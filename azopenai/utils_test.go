package azopenai

import (
	"encoding/json"
	"strings"
	"testing"

	"github.com/openai/openai-go/v3"
	"oss.nandlabs.io/golly/genai"
)

func mustChunk(t *testing.T, s string) openai.ChatCompletionChunk {
	t.Helper()
	var c openai.ChatCompletionChunk
	if err := json.Unmarshal([]byte(s), &c); err != nil {
		t.Fatalf("unmarshal chunk: %v", err)
	}
	return c
}

func TestBuildToolChoice(t *testing.T) {
	for mode, want := range map[genai.ToolChoiceMode]string{
		genai.ToolChoiceAuto: "auto", genai.ToolChoiceNone: "none", genai.ToolChoiceRequired: "required", "": "auto",
	} {
		tc, err := buildToolChoice(&genai.ToolChoice{Mode: mode})
		if err != nil || tc.OfAuto.Value != want {
			t.Errorf("mode %q: got %v, %v", mode, tc.OfAuto, err)
		}
	}
	if _, err := buildToolChoice(&genai.ToolChoice{Mode: genai.ToolChoiceNamed}); err == nil {
		t.Error("named without name must fail")
	}
	if _, err := buildToolChoice(&genai.ToolChoice{Mode: "bogus"}); err == nil {
		t.Error("unknown mode must fail")
	}
}

func TestBuildTools_Errors(t *testing.T) {
	if _, err := buildTools([]genai.Tool{{}}); err == nil {
		t.Error("tool without function must fail")
	}
	if _, err := buildTools([]genai.Tool{{Function: &genai.FunctionDecl{}}}); err == nil {
		t.Error("tool without name must fail")
	}
	tools, err := buildTools([]genai.Tool{{Function: &genai.FunctionDecl{Name: "noargs"}}})
	if err != nil || len(tools) != 1 {
		t.Fatalf("tools = %v, %v", tools, err)
	}
	raw, _ := json.Marshal(tools[0])
	if !strings.Contains(string(raw), `"type":"object"`) {
		t.Errorf("nil parameters must become an object schema: %s", raw)
	}
}

func TestSchemaHelpers(t *testing.T) {
	m, err := schemaToMap(nil)
	if err != nil || m["type"] != "object" {
		t.Errorf("nil schema = %v, %v", m, err)
	}
	m, err = schemaToMap(map[string]any{"properties": map[string]any{}})
	if err != nil || m["type"] != "object" {
		t.Errorf("type must default to object: %v", m)
	}
	if _, err := schemaToMap(func() {}); err == nil {
		t.Error("unmarshalable schema must fail")
	}
	if got := sanitizeSchemaName("  my schema!  "); got != "my_schema_" {
		t.Errorf("sanitize = %q", got)
	}
	if got := sanitizeSchemaName(""); got != "response" {
		t.Errorf("sanitize empty = %q", got)
	}
	if got := sanitizeSchemaName(strings.Repeat("a", 80)); len(got) != 64 {
		t.Errorf("sanitize long = %d", len(got))
	}
	obj := map[string]any{"type": "object", "properties": map[string]any{
		"items": map[string]any{"type": "array", "items": map[string]any{"type": "object"}},
	}, "anyOf": []any{map[string]any{"type": "object"}}}
	closeObjects(obj)
	inner := obj["properties"].(map[string]any)["items"].(map[string]any)["items"].(map[string]any)
	if obj["additionalProperties"] != false || inner["additionalProperties"] != false ||
		obj["anyOf"].([]any)[0].(map[string]any)["additionalProperties"] != false {
		t.Errorf("closeObjects = %v", obj)
	}
}

func TestBuildResponseFormat_SchemaName(t *testing.T) {
	opts := genai.NewOptionsBuilder().SetSchema(citySchema()).Add(OptionSchemaName, "city").Build()
	rf, err := buildResponseFormat(opts)
	if err != nil || rf.OfJSONSchema == nil || rf.OfJSONSchema.JSONSchema.Name != "city" {
		t.Fatalf("rf = %+v, %v", rf, err)
	}
	if rf.OfJSONSchema.JSONSchema.Strict.Valid() {
		t.Error("strict must be unset when the option is absent")
	}
	none, _ := buildResponseFormat(genai.NewOptionsBuilder().SetOutputMime("text/plain").Build())
	if none.OfJSONObject != nil || none.OfJSONSchema != nil {
		t.Error("text/plain must not set a response format")
	}
}

func TestMapFinishReason(t *testing.T) {
	cases := map[string]genai.FinishReason{
		"stop": genai.FinishReasonStop, "length": genai.FinishReasonLength,
		"tool_calls": genai.FinishReasonToolCall, "function_call": genai.FinishReasonFunctionCall,
		"content_filter": genai.FinishReasonContentFilter, "": genai.FinishReasonInProgress,
		"weird": genai.FinishReasonUnknown,
	}
	for in, want := range cases {
		if got := mapFinishReason(in); got != want {
			t.Errorf("mapFinishReason(%q) = %q, want %q", in, got, want)
		}
	}
}

func TestContentPartHelpers(t *testing.T) {
	if audioFormat("audio/mpeg") != "mp3" || audioFormat("audio/x-wav") != "wav" {
		t.Error("audioFormat")
	}
	if !isAudioMime("audio/mp3") || isAudioMime("audio/ogg") {
		t.Error("isAudioMime")
	}
	cp, err := convertContentPart("m", &genai.Part{FuncCall: &genai.FuncCallPart{}})
	if cp != nil || err != nil {
		t.Error("func call in user turn must be ignored")
	}
	cp, _ = convertContentPart("m", &genai.Part{})
	if cp != nil {
		t.Error("empty part must be ignored")
	}
	cp, _ = convertContentPart("m", &genai.Part{File: &genai.FilePart{URI: "data:image/png;base64,AAA"}})
	if cp == nil || cp.OfImageURL == nil {
		t.Error("data:image URI must become an image part")
	}
	cp, _ = convertContentPart("m", &genai.Part{MimeType: "application/pdf", Bin: &genai.BinPart{Data: []byte("x")}})
	if cp.OfFile == nil || cp.OfFile.File.Filename.Value != "document.pdf" {
		t.Error("pdf default filename")
	}
	if _, err := convertContentPart("my-model", &genai.Part{MimeType: "video/mp4", Bin: &genai.BinPart{Data: []byte("x")}}); err == nil ||
		!strings.Contains(err.Error(), "my-model") {
		t.Errorf("unsupported mime err = %v", err)
	}
}

func TestBuildMessages_Variants(t *testing.T) {
	// Function response only (no user content) produces only tool messages.
	txt := "r"
	uri := "https://x/y"
	msg := &genai.Message{Role: genai.RoleUser, Parts: []genai.Part{
		{Name: "c1", FuncResponse: &genai.FuncResponsePart{Text: &txt}},
		{Name: "c2", FuncResponse: &genai.FuncResponsePart{FileURI: &uri}},
	}}
	msgs, err := buildMessages("m", msg, nil)
	if err != nil || len(msgs) != 2 || msgs[0].OfTool == nil || msgs[1].OfTool.Content.OfString.Value != uri {
		t.Fatalf("msgs = %+v, %v", msgs, err)
	}
	// Empty assistant message keeps an empty content string.
	am := convertAssistantMessage(&genai.Message{Role: genai.RoleAssistant})
	if am.OfAssistant == nil || !am.OfAssistant.Content.OfString.Valid() {
		t.Error("empty assistant message must carry empty content")
	}
	// Assistant tool call with nil args marshals to {}.
	am = convertAssistantMessage(&genai.Message{Role: genai.RoleAssistant, Parts: []genai.Part{
		{FuncCall: &genai.FuncCallPart{Id: "1", FunctionName: "f"}},
	}})
	if am.OfAssistant.ToolCalls[0].OfFunction.Function.Arguments != "{}" {
		t.Errorf("args = %q", am.OfAssistant.ToolCalls[0].OfFunction.Function.Arguments)
	}
	// System message with no text yields nothing.
	msgs, _ = buildMessages("m", &genai.Message{Role: genai.RoleSystem}, nil)
	if len(msgs) != 0 {
		t.Errorf("msgs = %v", msgs)
	}
}

func TestToGenResponse_Nil(t *testing.T) {
	r := toGenResponse(nil)
	if r == nil || len(r.Candidates) != 0 {
		t.Errorf("r = %+v", r)
	}
}

func TestFuncCallPart_EmptyArgs(t *testing.T) {
	p := funcCallPart("id", "fn", "  ")
	if p.FuncCall == nil || len(p.FuncCall.Arguments) != 0 || p.Attributes != nil {
		t.Errorf("part = %+v", p)
	}
}
