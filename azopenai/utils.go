package azopenai

import (
	"encoding/base64"
	"encoding/json"
	"fmt"
	"regexp"
	"strings"
	"time"

	"github.com/openai/openai-go/v3"
	"github.com/openai/openai-go/v3/shared"
	"oss.nandlabs.io/golly/genai"
	"oss.nandlabs.io/golly/ioutils"
)

// Provider-specific option keys understood in addition to the standard golly
// genai options. Set them with genai.OptionsBuilder.Add(key, value).
const (
	// OptionReasoningEffort sets reasoning_effort for reasoning models
	// ("minimal", "low", "medium", "high"). Value: string.
	OptionReasoningEffort = "reasoning_effort"
	// OptionParallelToolCalls toggles parallel_tool_calls. Value: bool.
	OptionParallelToolCalls = "parallel_tool_calls"
	// OptionUser sets the end-user identifier forwarded to the service. Value: string.
	OptionUser = "user"
	// OptionStrictSchema enables strict JSON-schema adherence for structured
	// output (requires every property to be listed in "required"). When true,
	// "additionalProperties": false is injected into every object schema.
	// Value: bool.
	OptionStrictSchema = "json_schema_strict"
	// OptionSchemaName names the JSON schema sent for structured output.
	// Defaults to the schema Title, then "response". Value: string.
	OptionSchemaName = "json_schema_name"
)

// Part attribute keys.
const (
	// AttrRefusal marks a text part that carries a model refusal.
	AttrRefusal = "refusal"
	// AttrRawArguments carries the raw tool-call arguments string when it is
	// not valid JSON.
	AttrRawArguments = "raw_arguments"
	// AttrDimensions on the first embed input selects the output dimensions
	// (text-embedding-3-* only). Value: int.
	AttrDimensions = "dimensions"
	// AttrImageDetail on an image part sets the vision detail level
	// ("low", "high", "auto"). Value: string.
	AttrImageDetail = "detail"
)

var schemaNameSanitizer = regexp.MustCompile(`[^a-zA-Z0-9_-]`)

// buildParams constructs ChatCompletionNewParams from golly genai types.
func (p *AzureOpenAIProvider) buildParams(model string, message *genai.Message, options *genai.Options) (openai.ChatCompletionNewParams, error) {
	params := openai.ChatCompletionNewParams{}
	if model == "" {
		return params, fmt.Errorf("model is required")
	}
	params.Model = p.Deployment(model)

	msgs, err := buildMessages(model, message, options)
	if err != nil {
		return params, err
	}
	if len(msgs) == 0 {
		return params, fmt.Errorf("message is required")
	}
	params.Messages = msgs

	if options == nil {
		return params, nil
	}

	reasoning := p.isReasoningModel(model)

	if options.Has(genai.OptionMaxTokens) {
		v := int64(options.GetMaxTokens(DefaultMaxTokens))
		if p.legacyMaxTokens {
			params.MaxTokens = openai.Int(v)
		} else {
			params.MaxCompletionTokens = openai.Int(v)
		}
	}

	// Reasoning models (o-series, GPT-5) reject sampling parameters.
	if reasoning {
		for _, k := range []string{genai.OptionTemperature, genai.OptionTopP, genai.OptionPresencePenalty, genai.OptionFrequencyPenalty, genai.OptionLogProbs} {
			if options.Has(k) {
				logger.DebugF("azopenai: dropping option %s unsupported by reasoning model %s", k, model)
			}
		}
	} else {
		if options.Has(genai.OptionTemperature) {
			params.Temperature = openai.Float(float64(options.GetTemperature(0.7)))
		}
		if options.Has(genai.OptionTopP) {
			params.TopP = openai.Float(float64(options.GetTopP(0.9)))
		}
		if options.Has(genai.OptionPresencePenalty) {
			params.PresencePenalty = openai.Float(options.GetPresencePenalty(0))
		}
		if options.Has(genai.OptionFrequencyPenalty) {
			params.FrequencyPenalty = openai.Float(options.GetFrequencyPenalty(0))
		}
		if options.Has(genai.OptionLogProbs) {
			params.Logprobs = openai.Bool(options.GetLogProbs(false))
		}
	}

	if options.Has(genai.OptionCandidateCount) {
		params.N = openai.Int(int64(options.GetCandidateCount(1)))
	}
	if options.Has(genai.OptionSeed) {
		params.Seed = openai.Int(int64(options.GetSeed(0)))
	}
	if stop := options.GetStopWords(nil); len(stop) > 0 {
		params.Stop = openai.ChatCompletionNewParamsStopUnion{OfStringArray: stop}
	}
	if v := options.GetString(OptionReasoningEffort); v != "" {
		params.ReasoningEffort = shared.ReasoningEffort(v)
	}
	if v := options.GetString(OptionUser); v != "" {
		params.User = openai.String(v)
	}

	rf, err := buildResponseFormat(options)
	if err != nil {
		return params, err
	}
	params.ResponseFormat = rf

	tools, err := buildTools(options.GetTools())
	if err != nil {
		return params, err
	}
	if len(tools) > 0 {
		params.Tools = tools
		if choice := options.GetToolChoice(); choice != nil {
			tc, err := buildToolChoice(choice)
			if err != nil {
				return params, err
			}
			params.ToolChoice = tc
		}
		if options.Has(OptionParallelToolCalls) {
			params.ParallelToolCalls = openai.Bool(options.GetBool(OptionParallelToolCalls))
		}
	}

	return params, nil
}

// buildResponseFormat maps OptionSchema / OptionOutputMime to response_format.
// A schema selects json_schema structured output; an application/json output
// MIME without a schema selects JSON mode (json_object).
func buildResponseFormat(options *genai.Options) (openai.ChatCompletionNewParamsResponseFormatUnion, error) {
	var rf openai.ChatCompletionNewParamsResponseFormatUnion
	if schema := options.GetSchema(); schema != nil {
		m, err := schemaToMap(schema)
		if err != nil {
			return rf, fmt.Errorf("response schema: %w", err)
		}
		strict := options.Has(OptionStrictSchema) && options.GetBool(OptionStrictSchema)
		if strict {
			closeObjects(m)
		}
		name := options.GetString(OptionSchemaName)
		if name == "" {
			name = schema.Title
		}
		js := shared.ResponseFormatJSONSchemaJSONSchemaParam{
			Name:   sanitizeSchemaName(name),
			Schema: m,
		}
		if schema.Description != "" {
			js.Description = openai.String(schema.Description)
		}
		if options.Has(OptionStrictSchema) {
			js.Strict = openai.Bool(strict)
		}
		rf.OfJSONSchema = &shared.ResponseFormatJSONSchemaParam{JSONSchema: js}
		return rf, nil
	}
	if strings.EqualFold(options.GetOutputMime(""), ioutils.MimeApplicationJSON) {
		rf.OfJSONObject = &shared.ResponseFormatJSONObjectParam{}
	}
	return rf, nil
}

// sanitizeSchemaName coerces a name into the ^[a-zA-Z0-9_-]{1,64}$ form the
// service requires.
func sanitizeSchemaName(name string) string {
	name = schemaNameSanitizer.ReplaceAllString(strings.TrimSpace(name), "_")
	if name == "" {
		return "response"
	}
	if len(name) > 64 {
		name = name[:64]
	}
	return name
}

// closeObjects recursively sets additionalProperties=false on object schemas,
// as strict structured output requires.
func closeObjects(v any) {
	switch t := v.(type) {
	case map[string]any:
		if typ, _ := t["type"].(string); typ == "object" {
			if _, ok := t["additionalProperties"]; !ok {
				t["additionalProperties"] = false
			}
		}
		for _, child := range t {
			closeObjects(child)
		}
	case []any:
		for _, child := range t {
			closeObjects(child)
		}
	}
}

// buildTools translates golly tool declarations into function tools.
func buildTools(tools []genai.Tool) ([]openai.ChatCompletionToolUnionParam, error) {
	if len(tools) == 0 {
		return nil, nil
	}
	out := make([]openai.ChatCompletionToolUnionParam, 0, len(tools))
	for i, t := range tools {
		if t.Function == nil {
			return nil, fmt.Errorf("tool[%d] has no Function declaration", i)
		}
		if t.Function.Name == "" {
			return nil, fmt.Errorf("tool[%d] has an empty function name", i)
		}
		params, err := schemaToMap(t.Function.Parameters)
		if err != nil {
			return nil, fmt.Errorf("tool[%d] (%s) parameters schema: %w", i, t.Function.Name, err)
		}
		def := shared.FunctionDefinitionParam{
			Name:       t.Function.Name,
			Parameters: shared.FunctionParameters(params),
		}
		if t.Function.Description != "" {
			def.Description = openai.String(t.Function.Description)
		}
		out = append(out, openai.ChatCompletionFunctionTool(def))
	}
	return out, nil
}

// buildToolChoice maps a golly ToolChoice to tool_choice.
func buildToolChoice(choice *genai.ToolChoice) (openai.ChatCompletionToolChoiceOptionUnionParam, error) {
	var tc openai.ChatCompletionToolChoiceOptionUnionParam
	switch choice.Mode {
	case genai.ToolChoiceAuto, "":
		tc.OfAuto = openai.String("auto")
	case genai.ToolChoiceNone:
		tc.OfAuto = openai.String("none")
	case genai.ToolChoiceRequired:
		tc.OfAuto = openai.String("required")
	case genai.ToolChoiceNamed:
		if choice.Name == "" {
			return tc, fmt.Errorf("named tool choice requires a non-empty Name")
		}
		tc = openai.ToolChoiceOptionFunctionToolChoice(openai.ChatCompletionNamedToolChoiceFunctionParam{Name: choice.Name})
	default:
		return tc, fmt.Errorf("unsupported tool choice mode: %q", choice.Mode)
	}
	return tc, nil
}

// schemaToMap converts a *data.Schema (or any JSON-Schema shaped value) into a
// plain map. A nil schema yields an empty object schema.
func schemaToMap(schema any) (map[string]any, error) {
	if schema == nil {
		return map[string]any{"type": "object", "properties": map[string]any{}}, nil
	}
	raw, err := json.Marshal(schema)
	if err != nil {
		return nil, err
	}
	var m map[string]any
	if err := json.Unmarshal(raw, &m); err != nil {
		return nil, err
	}
	if m == nil {
		return map[string]any{"type": "object", "properties": map[string]any{}}, nil
	}
	if _, ok := m["type"]; !ok {
		m["type"] = "object"
	}
	return m, nil
}

// buildMessages converts the system instructions and the golly message into
// Chat Completions messages. A single golly message can expand into several
// wire messages: function responses become one "tool" message each.
func buildMessages(model string, message *genai.Message, options *genai.Options) ([]openai.ChatCompletionMessageParamUnion, error) {
	var msgs []openai.ChatCompletionMessageParamUnion

	if options != nil {
		if sys := options.GetSystemInstructions(); sys != "" {
			msgs = append(msgs, openai.SystemMessage(sys))
		}
	}
	if message == nil {
		return msgs, nil
	}

	switch message.Role {
	case genai.RoleSystem:
		if text := joinText(message.Parts); text != "" {
			msgs = append(msgs, openai.SystemMessage(text))
		}
	case genai.RoleAssistant:
		msgs = append(msgs, convertAssistantMessage(message))
	case genai.RoleFunction:
		for i := range message.Parts {
			part := &message.Parts[i]
			if part.FuncResponse != nil {
				msgs = append(msgs, toolMessage(part))
			} else if part.Text != nil {
				msgs = append(msgs, openai.ToolMessage(part.Text.Content, part.Name))
			}
		}
	default:
		toolMsgs, user, err := convertUserMessage(model, message)
		if err != nil {
			return nil, err
		}
		msgs = append(msgs, toolMsgs...)
		if user != nil {
			msgs = append(msgs, *user)
		}
	}
	return msgs, nil
}

// joinText concatenates the text parts of a message.
func joinText(parts []genai.Part) string {
	var texts []string
	for _, part := range parts {
		if part.Text != nil && part.Text.Content != "" {
			texts = append(texts, part.Text.Content)
		}
	}
	return strings.Join(texts, "\n")
}

// toolMessage converts a FuncResponse part into a "tool" message. Following
// the golly convention, Part.Name carries the tool_call_id.
func toolMessage(part *genai.Part) openai.ChatCompletionMessageParamUnion {
	var content string
	switch {
	case part.FuncResponse.Text != nil:
		content = *part.FuncResponse.Text
	case part.FuncResponse.Data != nil:
		content = string(part.FuncResponse.Data)
	case part.FuncResponse.FileURI != nil:
		content = *part.FuncResponse.FileURI
	}
	return openai.ToolMessage(content, part.Name)
}

// convertAssistantMessage converts an assistant message (prior turn) with
// text and/or tool calls.
func convertAssistantMessage(msg *genai.Message) openai.ChatCompletionMessageParamUnion {
	asst := openai.ChatCompletionAssistantMessageParam{}
	if text := joinText(msg.Parts); text != "" {
		asst.Content.OfString = openai.String(text)
	}
	for _, part := range msg.Parts {
		if part.FuncCall == nil {
			continue
		}
		args, err := json.Marshal(part.FuncCall.Arguments)
		if err != nil || part.FuncCall.Arguments == nil {
			args = []byte("{}")
		}
		asst.ToolCalls = append(asst.ToolCalls, openai.ChatCompletionMessageToolCallUnionParam{
			OfFunction: &openai.ChatCompletionMessageFunctionToolCallParam{
				ID: part.FuncCall.Id,
				Function: openai.ChatCompletionMessageFunctionToolCallFunctionParam{
					Name:      part.FuncCall.FunctionName,
					Arguments: string(args),
				},
			},
		})
	}
	if !asst.Content.OfString.Valid() && len(asst.ToolCalls) == 0 {
		asst.Content.OfString = openai.String("")
	}
	return openai.ChatCompletionMessageParamUnion{OfAssistant: &asst}
}

// convertUserMessage splits a user message into tool messages (for
// FuncResponse parts) and a single user message with the remaining content.
func convertUserMessage(model string, msg *genai.Message) ([]openai.ChatCompletionMessageParamUnion, *openai.ChatCompletionMessageParamUnion, error) {
	var toolMsgs []openai.ChatCompletionMessageParamUnion
	var content []openai.ChatCompletionContentPartUnionParam
	onlyText := true

	for i := range msg.Parts {
		part := &msg.Parts[i]
		if part.FuncResponse != nil {
			toolMsgs = append(toolMsgs, toolMessage(part))
			continue
		}
		cp, err := convertContentPart(model, part)
		if err != nil {
			return nil, nil, err
		}
		if cp == nil {
			continue
		}
		if cp.OfText == nil {
			onlyText = false
		}
		content = append(content, *cp)
	}

	if len(content) == 0 {
		if len(toolMsgs) > 0 {
			return toolMsgs, nil, nil
		}
		u := openai.UserMessage("")
		return nil, &u, nil
	}
	if onlyText && len(content) == 1 {
		u := openai.UserMessage(content[0].OfText.Text)
		return toolMsgs, &u, nil
	}
	u := openai.UserMessage(content)
	return toolMsgs, &u, nil
}

// convertContentPart converts a golly Part into a user content part.
func convertContentPart(model string, part *genai.Part) (*openai.ChatCompletionContentPartUnionParam, error) {
	mime := strings.ToLower(part.MimeType)
	switch {
	case part.Text != nil:
		cp := openai.TextContentPart(part.Text.Content)
		return &cp, nil

	case part.Bin != nil && ioutils.IsImageMime(mime):
		cp := openai.ImageContentPart(openai.ChatCompletionContentPartImageImageURLParam{
			URL:    dataURL(mime, part.Bin.Data),
			Detail: imageDetail(part),
		})
		return &cp, nil

	case part.Bin != nil && isAudioMime(mime):
		cp := openai.InputAudioContentPart(openai.ChatCompletionContentPartInputAudioInputAudioParam{
			Data:   base64.StdEncoding.EncodeToString(part.Bin.Data),
			Format: audioFormat(mime),
		})
		return &cp, nil

	case part.Bin != nil && mime == "application/pdf":
		name := part.Name
		if name == "" {
			name = "document.pdf"
		}
		cp := openai.FileContentPart(openai.ChatCompletionContentPartFileFileParam{
			FileData: openai.String(dataURL(mime, part.Bin.Data)),
			Filename: openai.String(name),
		})
		return &cp, nil

	case part.Bin != nil && (strings.HasPrefix(mime, "text/") || mime == ioutils.MimeApplicationJSON):
		cp := openai.TextContentPart(string(part.Bin.Data))
		return &cp, nil

	case part.Bin != nil:
		return nil, genai.GetUnsupportedMimeErr.Err(part.MimeType, model)

	case part.File != nil && (ioutils.IsImageMime(mime) || strings.HasPrefix(part.File.URI, "data:image/")):
		cp := openai.ImageContentPart(openai.ChatCompletionContentPartImageImageURLParam{
			URL:    part.File.URI,
			Detail: imageDetail(part),
		})
		return &cp, nil

	case part.File != nil:
		// Chat Completions cannot fetch arbitrary file URLs; pass a reference.
		cp := openai.TextContentPart(fmt.Sprintf("[File: %s]", part.File.URI))
		return &cp, nil

	case part.FuncCall != nil:
		// Tool calls in a user turn are meaningless; ignore them.
		return nil, nil

	default:
		return nil, nil
	}
}

func imageDetail(part *genai.Part) string {
	if v, ok := part.Attributes[AttrImageDetail].(string); ok {
		return v
	}
	return ""
}

func dataURL(mime string, data []byte) string {
	return "data:" + mime + ";base64," + base64.StdEncoding.EncodeToString(data)
}

func isAudioMime(mime string) bool {
	switch mime {
	case "audio/wav", "audio/x-wav", "audio/wave", "audio/mpeg", "audio/mp3":
		return true
	}
	return false
}

func audioFormat(mime string) string {
	if strings.Contains(mime, "wav") {
		return "wav"
	}
	return "mp3"
}

// mapFinishReason maps a Chat Completions finish_reason to a golly FinishReason.
func mapFinishReason(reason string) genai.FinishReason {
	switch reason {
	case "stop":
		return genai.FinishReasonStop
	case "length":
		return genai.FinishReasonLength
	case "tool_calls":
		return genai.FinishReasonToolCall
	case "function_call":
		return genai.FinishReasonFunctionCall
	case "content_filter":
		return genai.FinishReasonContentFilter
	case "":
		return genai.FinishReasonInProgress
	default:
		return genai.FinishReasonUnknown
	}
}

// usageToMeta converts CompletionUsage into ResponseMeta token counts.
func usageToMeta(u openai.CompletionUsage, meta *genai.ResponseMeta) {
	meta.InputTokens = int(u.PromptTokens)
	meta.OutputTokens = int(u.CompletionTokens)
	meta.TotalTokens = int(u.TotalTokens)
	meta.CachedTokens = int(u.PromptTokensDetails.CachedTokens)
	meta.ThinkingTokens = int(u.CompletionTokensDetails.ReasoningTokens)
}

func hasUsage(u openai.CompletionUsage) bool {
	return u.TotalTokens > 0 || u.PromptTokens > 0 || u.CompletionTokens > 0
}

// textPart builds an assistant text part.
func textPart(text string) genai.Part {
	return genai.Part{
		Name:     "text",
		MimeType: ioutils.MimeTextPlain,
		Text:     &genai.TextPart{Content: text},
	}
}

// refusalPart builds a text part flagged as a refusal.
func refusalPart(text string) genai.Part {
	p := textPart(text)
	p.Name = AttrRefusal
	p.Attributes = map[string]interface{}{AttrRefusal: true}
	return p
}

// funcCallPart builds a FuncCall part, decoding the JSON arguments.
func funcCallPart(id, name, arguments string) genai.Part {
	part := genai.Part{
		Name: name,
		FuncCall: &genai.FuncCallPart{
			Id:           id,
			FunctionName: name,
			Arguments:    map[string]interface{}{},
		},
	}
	if strings.TrimSpace(arguments) != "" {
		if err := json.Unmarshal([]byte(arguments), &part.FuncCall.Arguments); err != nil {
			logger.WarnF("azopenai: failed to decode tool call arguments for %s: %v", name, err)
			part.FuncCall.Arguments = map[string]interface{}{}
			part.Attributes = map[string]interface{}{AttrRawArguments: arguments}
		}
	}
	return part
}

// toGenResponse converts a ChatCompletion into a golly GenResponse.
func toGenResponse(resp *openai.ChatCompletion) *genai.GenResponse {
	genResp := &genai.GenResponse{}
	if resp == nil {
		return genResp
	}
	usageToMeta(resp.Usage, &genResp.Meta)

	for _, choice := range resp.Choices {
		msg := &genai.Message{Role: genai.RoleAssistant, Parts: []genai.Part{}}
		if choice.Message.Content != "" {
			msg.Parts = append(msg.Parts, textPart(choice.Message.Content))
		}
		if choice.Message.Refusal != "" {
			msg.Parts = append(msg.Parts, refusalPart(choice.Message.Refusal))
		}
		for _, tc := range choice.Message.ToolCalls {
			if tc.Type != "" && tc.Type != "function" {
				continue
			}
			msg.Parts = append(msg.Parts, funcCallPart(tc.ID, tc.Function.Name, tc.Function.Arguments))
		}
		genResp.Candidates = append(genResp.Candidates, genai.Candidate{
			Index:        int(choice.Index),
			Message:      msg,
			FinishReason: mapFinishReason(choice.FinishReason),
		})
	}
	return genResp
}

// toolCallAcc accumulates streamed tool-call fragments.
type toolCallAcc struct {
	id   string
	name string
	args strings.Builder
}

// streamAccumulator converts ChatCompletionChunks into golly GenResponses,
// assembling tool calls that arrive in fragments.
type streamAccumulator struct {
	start     time.Time
	gotFirst  bool
	toolCalls map[int64]map[int64]*toolCallAcc
	toolOrder map[int64][]int64
}

func newStreamAccumulator(start time.Time) *streamAccumulator {
	return &streamAccumulator{
		start:     start,
		toolCalls: map[int64]map[int64]*toolCallAcc{},
		toolOrder: map[int64][]int64{},
	}
}

// add folds one chunk into the accumulator and returns the GenResponse to
// emit, or nil when the chunk carries nothing to surface (e.g. Azure's
// prompt_filter_results preamble, role-only deltas or tool-call fragments).
func (a *streamAccumulator) add(chunk openai.ChatCompletionChunk) *genai.GenResponse {
	var out *genai.GenResponse
	ensure := func() *genai.GenResponse {
		if out == nil {
			out = &genai.GenResponse{}
		}
		return out
	}

	for _, choice := range chunk.Choices {
		var parts []genai.Part
		if choice.Delta.Content != "" {
			parts = append(parts, textPart(choice.Delta.Content))
		}
		if choice.Delta.Refusal != "" {
			parts = append(parts, refusalPart(choice.Delta.Refusal))
		}
		for _, tc := range choice.Delta.ToolCalls {
			a.addToolDelta(choice.Index, tc)
		}

		finish := genai.FinishReasonInProgress
		if choice.FinishReason != "" {
			finish = mapFinishReason(choice.FinishReason)
			parts = append(parts, a.drainToolCalls(choice.Index)...)
			if len(parts) == 0 {
				parts = append(parts, textPart(""))
			}
		}
		if len(parts) == 0 {
			continue
		}
		r := ensure()
		r.Candidates = append(r.Candidates, genai.Candidate{
			Index:        int(choice.Index),
			Message:      &genai.Message{Role: genai.RoleAssistant, Parts: parts},
			FinishReason: finish,
		})
	}

	if hasUsage(chunk.Usage) {
		r := ensure()
		usageToMeta(chunk.Usage, &r.Meta)
		r.Meta.TotalTime = time.Since(a.start).Milliseconds()
	}

	if out != nil && len(out.Candidates) > 0 && !a.gotFirst {
		a.gotFirst = true
		out.Meta.TimeToFirst = time.Since(a.start).Milliseconds()
	}
	return out
}

func (a *streamAccumulator) addToolDelta(choice int64, tc openai.ChatCompletionChunkChoiceDeltaToolCall) {
	calls, ok := a.toolCalls[choice]
	if !ok {
		calls = map[int64]*toolCallAcc{}
		a.toolCalls[choice] = calls
	}
	acc, ok := calls[tc.Index]
	if !ok {
		acc = &toolCallAcc{}
		calls[tc.Index] = acc
		a.toolOrder[choice] = append(a.toolOrder[choice], tc.Index)
	}
	if tc.ID != "" {
		acc.id = tc.ID
	}
	if tc.Function.Name != "" {
		acc.name = tc.Function.Name
	}
	acc.args.WriteString(tc.Function.Arguments)
}

func (a *streamAccumulator) drainToolCalls(choice int64) []genai.Part {
	order := a.toolOrder[choice]
	if len(order) == 0 {
		return nil
	}
	calls := a.toolCalls[choice]
	parts := make([]genai.Part, 0, len(order))
	for _, idx := range order {
		acc := calls[idx]
		parts = append(parts, funcCallPart(acc.id, acc.name, acc.args.String()))
	}
	delete(a.toolCalls, choice)
	delete(a.toolOrder, choice)
	return parts
}
