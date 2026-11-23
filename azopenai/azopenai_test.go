package azopenai

import (
	"context"
	"errors"
	"net/http"
	"strings"
	"testing"

	"oss.nandlabs.io/golly-azure/azcfg"
	"oss.nandlabs.io/golly/genai"
)

func TestProviderMetadata(t *testing.T) {
	f := newFakeAzure(t, func(w http.ResponseWriter, r *recordedRequest) {})
	p := f.provider(func(c *ProviderConfig) {
		c.Deployments = map[string]string{"gpt-4o": "prod-4o", "gpt-4.1": "prod-41"}
	})
	if p.Name() != ProviderName || p.Description() != ProviderDescription || p.Version() != ProviderVersion {
		t.Errorf("unexpected metadata: %s %s %s", p.Name(), p.Description(), p.Version())
	}
	if got := p.Models(); len(got) != 2 || got[0] != "gpt-4.1" || got[1] != "gpt-4o" {
		t.Errorf("Models() = %v, want sorted deployment keys", got)
	}
	if p.Deployment("gpt-4o") != "prod-4o" || p.Deployment("other") != "other" {
		t.Error("Deployment mapping incorrect")
	}
	if err := p.Close(); err != nil {
		t.Errorf("Close: %v", err)
	}

	p2 := f.provider(func(c *ProviderConfig) {
		c.Description, c.Version, c.Models = "custom", "9.9", []string{"gpt-5"}
	})
	if p2.Description() != "custom" || p2.Version() != "9.9" || len(p2.Models()) != 1 {
		t.Error("custom description/version/models not applied")
	}
}

func TestGenerate_Basic(t *testing.T) {
	f := newFakeAzure(t, func(w http.ResponseWriter, r *recordedRequest) {
		writeJSON(w, 200, chatCompletionJSON("Paris is the capital.", "stop"))
	})
	p := f.provider(func(c *ProviderConfig) { c.Deployments = map[string]string{"gpt-4o": "prod-4o"} })

	opts := genai.NewOptionsBuilder().
		SetMaxTokens(256).
		SetTemperature(0.3).
		SetTopP(0.8).
		SetStopWords("END").
		SetSeed(42).
		SetCandidateCount(1).
		SetPresencePenalty(0.1).
		SetFrequencyPenalty(0.2).
		Add(genai.OptionSystemInstructions, "Be terse.").
		Add(OptionUser, "user-1").
		Build()
	resp, err := p.Generate(context.Background(), "gpt-4o", genai.NewTextMessage(genai.RoleUser, "Capital of France?"), opts)
	if err != nil {
		t.Fatalf("Generate: %v", err)
	}

	req := f.last()
	if req.Method != http.MethodPost || req.Path != "/openai/deployments/prod-4o/chat/completions" {
		t.Errorf("unexpected request %s %s", req.Method, req.Path)
	}
	if got := req.Query["api-version"]; len(got) != 1 || got[0] != DefaultAPIVersion {
		t.Errorf("api-version = %v", got)
	}
	if req.Header.Get("Api-Key") != "test-key" {
		t.Errorf("Api-Key header = %q", req.Header.Get("Api-Key"))
	}
	b := req.Body
	if b["model"] != "prod-4o" {
		t.Errorf("model = %v", b["model"])
	}
	if b["max_completion_tokens"] != float64(256) || b["max_tokens"] != nil {
		t.Errorf("max tokens fields wrong: %v / %v", b["max_completion_tokens"], b["max_tokens"])
	}
	if v, _ := b["temperature"].(float64); v < 0.29 || v > 0.31 {
		t.Errorf("temperature = %v", b["temperature"])
	}
	if b["seed"] != float64(42) || b["n"] != float64(1) || b["user"] != "user-1" {
		t.Errorf("seed/n/user wrong: %v", b)
	}
	if stop, _ := b["stop"].([]any); len(stop) != 1 || stop[0] != "END" {
		t.Errorf("stop = %v", b["stop"])
	}
	if b["presence_penalty"] == nil || b["frequency_penalty"] == nil || b["top_p"] == nil {
		t.Error("penalties/top_p missing")
	}
	msgs := messagesOf(t, b)
	if len(msgs) != 2 || msgs[0]["role"] != "system" || msgs[0]["content"] != "Be terse." ||
		msgs[1]["role"] != "user" || msgs[1]["content"] != "Capital of France?" {
		t.Errorf("messages = %v", msgs)
	}

	if textOf(resp) != "Paris is the capital." {
		t.Errorf("text = %q", textOf(resp))
	}
	c := resp.Candidates[0]
	if c.FinishReason != genai.FinishReasonStop || c.Message.Role != genai.RoleAssistant {
		t.Errorf("candidate = %+v", c)
	}
	m := resp.Meta
	if m.InputTokens != 11 || m.OutputTokens != 7 || m.TotalTokens != 18 || m.CachedTokens != 3 || m.ThinkingTokens != 2 {
		t.Errorf("meta = %+v", m)
	}
}

func TestGenerate_TokenCredential(t *testing.T) {
	f := newFakeAzure(t, func(w http.ResponseWriter, r *recordedRequest) {
		writeJSON(w, 200, chatCompletionJSON("ok", "stop"))
	})
	cfg := azcfg.NewConfig()
	cfg.SetCredential(fakeCred{token: "entra-token"})
	cfg.SetEndpoint(f.srv.URL + "/openai/")
	zero := 0
	p, err := NewAzureOpenAIProvider(&ProviderConfig{
		Config:          cfg,
		APIVersion:      "2024-10-21",
		UnsafeAllowHTTP: true,
		MaxRetries:      &zero,
		LegacyMaxTokens: true,
		TokenScopes:     []string{"https://cognitiveservices.azure.com/.default"},
	})
	if err != nil {
		t.Fatalf("NewAzureOpenAIProvider: %v", err)
	}
	opts := genai.NewOptionsBuilder().SetMaxTokens(10).Build()
	if _, err := p.Generate(context.Background(), "gpt-4o-mini", genai.NewTextMessage(genai.RoleUser, "hi"), opts); err != nil {
		t.Fatalf("Generate: %v", err)
	}
	req := f.last()
	if got := req.Header.Get("Authorization"); got != "Bearer entra-token" {
		t.Errorf("Authorization = %q", got)
	}
	if req.Header.Get("Api-Key") != "" {
		t.Error("Api-Key must not be sent with token auth")
	}
	if req.Path != "/openai/deployments/gpt-4o-mini/chat/completions" {
		t.Errorf("path = %s (endpoint /openai suffix should be normalized)", req.Path)
	}
	if req.Query["api-version"][0] != "2024-10-21" {
		t.Errorf("api-version = %v", req.Query["api-version"])
	}
	if req.Body["max_tokens"] != float64(10) || req.Body["max_completion_tokens"] != nil {
		t.Errorf("legacy max_tokens not used: %v", req.Body)
	}
}

func TestGenerate_NamedConfigAndEnv(t *testing.T) {
	f := newFakeAzure(t, func(w http.ResponseWriter, r *recordedRequest) {
		writeJSON(w, 200, chatCompletionJSON("ok", "stop"))
	})
	t.Setenv(EnvEndpoint, f.srv.URL)
	t.Setenv(EnvAPIVersion, "")
	t.Setenv(EnvOpenAIAPIVersion, "2099-01-01")
	cfg := azcfg.NewConfig()
	cfg.SetCredential(fakeCred{token: "named"})
	azcfg.Manager.Register("azopenai-test", cfg)
	t.Cleanup(func() { azcfg.Manager.Unregister("azopenai-test") })

	zero := 0
	p, err := NewAzureOpenAIProvider(&ProviderConfig{CfgName: "azopenai-test", UnsafeAllowHTTP: true, MaxRetries: &zero})
	if err != nil {
		t.Fatalf("NewAzureOpenAIProvider: %v", err)
	}
	if _, err := p.Generate(context.Background(), "gpt-4o", genai.NewTextMessage(genai.RoleUser, "hi"), nil); err != nil {
		t.Fatalf("Generate: %v", err)
	}
	req := f.last()
	if req.Header.Get("Authorization") != "Bearer named" || req.Query["api-version"][0] != "2099-01-01" {
		t.Errorf("named config / env not applied: auth=%q version=%v", req.Header.Get("Authorization"), req.Query["api-version"])
	}
}

func TestNewProvider_MissingEndpoint(t *testing.T) {
	t.Setenv(EnvEndpoint, "")
	_, err := NewAzureOpenAIProvider(&ProviderConfig{APIKey: "k", Config: azcfg.NewConfig()})
	if !errors.Is(err, ErrMissingEndpoint) {
		t.Fatalf("err = %v, want ErrMissingEndpoint", err)
	}
	if _, err := NewAzureOpenAIProvider(nil); err == nil {
		// nil config falls back to env/default config; with no endpoint it must fail.
		t.Fatal("expected error for nil config without endpoint")
	}
}

func TestNewProvider_CredentialError(t *testing.T) {
	cfg := azcfg.NewConfig()
	cfg.ClientSecret = "secret" // missing tenant/client id
	cfg.SetEndpoint("https://example.openai.azure.com")
	_, err := NewAzureOpenAIProvider(&ProviderConfig{Config: cfg})
	if !errors.Is(err, azcfg.ErrIncompleteClientSecret) {
		t.Fatalf("err = %v, want ErrIncompleteClientSecret", err)
	}
}

func TestNormalizeEndpoint(t *testing.T) {
	cases := map[string]string{
		"https://r.openai.azure.com":             "https://r.openai.azure.com",
		"https://r.openai.azure.com/":            "https://r.openai.azure.com",
		"https://r.openai.azure.com/openai":      "https://r.openai.azure.com",
		"https://r.openai.azure.com/openai/v1/":  "https://r.openai.azure.com",
		" https://r.services.ai.azure.com/ ":     "https://r.services.ai.azure.com",
		"https://r.services.ai.azure.com/OpenAI": "https://r.services.ai.azure.com",
		"":                                       "",
	}
	for in, want := range cases {
		if got := normalizeEndpoint(in); got != want {
			t.Errorf("normalizeEndpoint(%q) = %q, want %q", in, got, want)
		}
	}
}

func TestGenerate_ReasoningModel(t *testing.T) {
	f := newFakeAzure(t, func(w http.ResponseWriter, r *recordedRequest) {
		writeJSON(w, 200, chatCompletionJSON("42", "stop"))
	})
	p := f.provider(func(c *ProviderConfig) { c.Deployments = map[string]string{"o4-mini": "reasoner"} })
	opts := genai.NewOptionsBuilder().
		SetTemperature(0.2).SetTopP(0.5).SetMaxTokens(1000).SetLogProbs(true).
		Add(OptionReasoningEffort, "high").
		Build()
	if _, err := p.Generate(context.Background(), "o4-mini", genai.NewTextMessage(genai.RoleUser, "q"), opts); err != nil {
		t.Fatal(err)
	}
	b := f.last().Body
	for _, k := range []string{"temperature", "top_p", "logprobs"} {
		if _, ok := b[k]; ok {
			t.Errorf("%s must be dropped for reasoning models", k)
		}
	}
	if b["reasoning_effort"] != "high" || b["max_completion_tokens"] != float64(1000) {
		t.Errorf("reasoning params wrong: %v", b)
	}
}

func TestGenerate_Validation(t *testing.T) {
	f := newFakeAzure(t, func(w http.ResponseWriter, r *recordedRequest) {})
	p := f.provider()
	if _, err := p.Generate(context.Background(), "", genai.NewTextMessage(genai.RoleUser, "x"), nil); err == nil {
		t.Error("expected error for empty model")
	}
	if _, err := p.Generate(context.Background(), "gpt-4o", nil, nil); err == nil {
		t.Error("expected error for nil message without system instructions")
	}
	bad := &genai.Message{Role: genai.RoleUser, Parts: []genai.Part{{MimeType: "application/zip", Bin: &genai.BinPart{Data: []byte{1}}}}}
	if _, err := p.Generate(context.Background(), "gpt-4o", bad, nil); err == nil {
		t.Error("expected unsupported mime error")
	}
	rc, ec := p.GenerateStream(context.Background(), "", genai.NewTextMessage(genai.RoleUser, "x"), nil)
	if _, err := collectStream(t, rc, ec); err == nil {
		t.Error("expected stream build error")
	}
	if f.count() != 0 {
		t.Errorf("no request should reach the server, got %d", f.count())
	}
}

func TestGenerate_Tools(t *testing.T) {
	f := newFakeAzure(t, func(w http.ResponseWriter, r *recordedRequest) {
		writeJSON(w, 200, `{"id":"x","object":"chat.completion","created":1,"model":"gpt-4o",
"choices":[{"index":0,"finish_reason":"tool_calls","message":{"role":"assistant","content":null,
"tool_calls":[{"id":"call_1","type":"function","function":{"name":"get_weather","arguments":"{\"city\":\"Paris\"}"}},
{"id":"call_2","type":"function","function":{"name":"get_weather","arguments":"not-json"}}]}}],
"usage":{"prompt_tokens":5,"completion_tokens":5,"total_tokens":10}}`)
	})
	p := f.provider()
	tool := weatherTool()
	opts := genai.NewOptionsBuilder().
		SetTools(tool).
		SetToolChoice(genai.NewNamedToolChoice("get_weather")).
		Add(OptionParallelToolCalls, false).
		Build()
	resp, err := p.Generate(context.Background(), "gpt-4o", genai.NewTextMessage(genai.RoleUser, "weather?"), opts)
	if err != nil {
		t.Fatal(err)
	}
	b := f.last().Body
	tools, _ := b["tools"].([]any)
	if len(tools) != 1 {
		t.Fatalf("tools = %v", b["tools"])
	}
	fn := tools[0].(map[string]any)["function"].(map[string]any)
	if fn["name"] != "get_weather" || fn["description"] != "Get the weather" {
		t.Errorf("function = %v", fn)
	}
	params := fn["parameters"].(map[string]any)
	if params["type"] != "object" || params["properties"].(map[string]any)["city"] == nil {
		t.Errorf("parameters = %v", params)
	}
	tc := b["tool_choice"].(map[string]any)
	if tc["type"] != "function" || tc["function"].(map[string]any)["name"] != "get_weather" {
		t.Errorf("tool_choice = %v", tc)
	}
	if b["parallel_tool_calls"] != false {
		t.Errorf("parallel_tool_calls = %v", b["parallel_tool_calls"])
	}

	c := resp.Candidates[0]
	if c.FinishReason != genai.FinishReasonToolCall || len(c.Message.Parts) != 2 {
		t.Fatalf("candidate = %+v", c)
	}
	call := c.Message.Parts[0].FuncCall
	if call.Id != "call_1" || call.FunctionName != "get_weather" || call.Arguments["city"] != "Paris" {
		t.Errorf("call = %+v", call)
	}
	if raw := c.Message.Parts[1].Attributes[AttrRawArguments]; raw != "not-json" {
		t.Errorf("raw arguments = %v", raw)
	}
}

func TestGenerate_ToolChoiceWithoutTools(t *testing.T) {
	f := newFakeAzure(t, func(w http.ResponseWriter, r *recordedRequest) {
		writeJSON(w, 200, chatCompletionJSON("ok", "stop"))
	})
	p := f.provider()
	opts := genai.NewOptionsBuilder().SetToolChoice(genai.NewToolChoice(genai.ToolChoiceRequired)).Build()
	if _, err := p.Generate(context.Background(), "gpt-4o", genai.NewTextMessage(genai.RoleUser, "x"), opts); err != nil {
		t.Fatal(err)
	}
	if _, ok := f.last().Body["tool_choice"]; ok {
		t.Error("tool_choice must be omitted when no tools are declared")
	}
}

func TestGenerate_StructuredOutput(t *testing.T) {
	f := newFakeAzure(t, func(w http.ResponseWriter, r *recordedRequest) {
		writeJSON(w, 200, chatCompletionJSON(`{"name":"Japan"}`, "stop"))
	})
	p := f.provider()

	opts := genai.NewOptionsBuilder().
		SetSchema(countrySchema()).
		Add(OptionStrictSchema, true).
		Build()
	resp, err := p.Generate(context.Background(), "gpt-4o", genai.NewTextMessage(genai.RoleUser, "Japan"), opts)
	if err != nil {
		t.Fatal(err)
	}
	if textOf(resp) != `{"name":"Japan"}` {
		t.Errorf("text = %q", textOf(resp))
	}
	rf := f.last().Body["response_format"].(map[string]any)
	if rf["type"] != "json_schema" {
		t.Fatalf("response_format = %v", rf)
	}
	js := rf["json_schema"].(map[string]any)
	if js["name"] != "Country_Info" || js["strict"] != true || js["description"] != "A country" {
		t.Errorf("json_schema = %v", js)
	}
	schema := js["schema"].(map[string]any)
	if schema["additionalProperties"] != false {
		t.Errorf("strict mode must close objects: %v", schema)
	}

	// JSON mode without schema.
	opts2 := genai.NewOptionsBuilder().SetOutputMime("application/json").Build()
	if _, err := p.Generate(context.Background(), "gpt-4o", genai.NewTextMessage(genai.RoleUser, "json"), opts2); err != nil {
		t.Fatal(err)
	}
	rf2 := f.last().Body["response_format"].(map[string]any)
	if rf2["type"] != "json_object" {
		t.Errorf("response_format = %v", rf2)
	}
}

func TestGenerate_MultiTurnToolRoundTrip(t *testing.T) {
	f := newFakeAzure(t, func(w http.ResponseWriter, r *recordedRequest) {
		writeJSON(w, 200, chatCompletionJSON("Sunny", "stop"))
	})
	p := f.provider()

	result := `{"temp":"22C"}`
	userTurn := &genai.Message{Role: genai.RoleUser, Parts: []genai.Part{
		{Name: "call_1", FuncResponse: &genai.FuncResponsePart{Text: &result}},
		{Name: "text", Text: &genai.TextPart{Content: "Summarize."}},
	}}
	if _, err := p.Generate(context.Background(), "gpt-4o", userTurn, nil); err != nil {
		t.Fatal(err)
	}
	msgs := messagesOf(t, f.last().Body)
	if len(msgs) != 2 || msgs[0]["role"] != "tool" || msgs[0]["tool_call_id"] != "call_1" || msgs[0]["content"] != result {
		t.Errorf("tool message = %v", msgs)
	}
	if msgs[1]["role"] != "user" || msgs[1]["content"] != "Summarize." {
		t.Errorf("user message = %v", msgs[1])
	}

	asst := &genai.Message{Role: genai.RoleAssistant, Parts: []genai.Part{
		{Text: &genai.TextPart{Content: "Calling tool"}},
		{FuncCall: &genai.FuncCallPart{Id: "call_9", FunctionName: "get_weather", Arguments: map[string]any{"city": "Oslo"}}},
	}}
	if _, err := p.Generate(context.Background(), "gpt-4o", asst, nil); err != nil {
		t.Fatal(err)
	}
	m := messagesOf(t, f.last().Body)[0]
	if m["role"] != "assistant" || m["content"] != "Calling tool" {
		t.Errorf("assistant = %v", m)
	}
	calls := m["tool_calls"].([]any)
	call := calls[0].(map[string]any)
	if call["id"] != "call_9" || call["type"] != "function" ||
		call["function"].(map[string]any)["arguments"] != `{"city":"Oslo"}` {
		t.Errorf("tool call = %v", call)
	}

	data := []byte("raw-bytes")
	fn := &genai.Message{Role: genai.RoleFunction, Parts: []genai.Part{
		{Name: "call_a", FuncResponse: &genai.FuncResponsePart{Data: data}},
		{Name: "call_b", Text: &genai.TextPart{Content: "text result"}},
	}}
	if _, err := p.Generate(context.Background(), "gpt-4o", fn, nil); err != nil {
		t.Fatal(err)
	}
	fm := messagesOf(t, f.last().Body)
	if len(fm) != 2 || fm[0]["content"] != "raw-bytes" || fm[1]["tool_call_id"] != "call_b" {
		t.Errorf("function role messages = %v", fm)
	}

	sys := &genai.Message{Role: genai.RoleSystem, Parts: []genai.Part{{Text: &genai.TextPart{Content: "sys"}}}}
	opts := genai.NewOptionsBuilder().Add(genai.OptionSystemInstructions, "first").Build()
	if _, err := p.Generate(context.Background(), "gpt-4o", sys, opts); err != nil {
		t.Fatal(err)
	}
	sm := messagesOf(t, f.last().Body)
	if len(sm) != 2 || sm[0]["content"] != "first" || sm[1]["content"] != "sys" || sm[1]["role"] != "system" {
		t.Errorf("system messages = %v", sm)
	}
}

func TestGenerate_Multimodal(t *testing.T) {
	f := newFakeAzure(t, func(w http.ResponseWriter, r *recordedRequest) {
		writeJSON(w, 200, chatCompletionJSON("ok", "stop"))
	})
	p := f.provider()
	msg := &genai.Message{Role: genai.RoleUser, Parts: []genai.Part{
		{Text: &genai.TextPart{Content: "Describe"}},
		{MimeType: "image/png", Bin: &genai.BinPart{Data: []byte("png")}, Attributes: map[string]any{AttrImageDetail: "low"}},
		{MimeType: "image/jpeg", File: &genai.FilePart{URI: "https://example.com/a.jpg"}},
		{MimeType: "audio/wav", Bin: &genai.BinPart{Data: []byte("wav")}},
		{Name: "r.pdf", MimeType: "application/pdf", Bin: &genai.BinPart{Data: []byte("pdf")}},
		{MimeType: "text/csv", Bin: &genai.BinPart{Data: []byte("a,b")}},
		{MimeType: "application/octet-stream", File: &genai.FilePart{URI: "https://example.com/x.bin"}},
	}}
	if _, err := p.Generate(context.Background(), "gpt-4o", msg, nil); err != nil {
		t.Fatal(err)
	}
	content := messagesOf(t, f.last().Body)[0]["content"].([]any)
	if len(content) != 7 {
		t.Fatalf("content parts = %d: %v", len(content), content)
	}
	get := func(i int) map[string]any { return content[i].(map[string]any) }
	if get(0)["type"] != "text" {
		t.Errorf("part0 = %v", get(0))
	}
	img := get(1)["image_url"].(map[string]any)
	if !strings.HasPrefix(img["url"].(string), "data:image/png;base64,") || img["detail"] != "low" {
		t.Errorf("image bin = %v", img)
	}
	if get(2)["image_url"].(map[string]any)["url"] != "https://example.com/a.jpg" {
		t.Errorf("image url = %v", get(2))
	}
	if ia := get(3)["input_audio"].(map[string]any); ia["format"] != "wav" {
		t.Errorf("audio = %v", ia)
	}
	file := get(4)["file"].(map[string]any)
	if file["filename"] != "r.pdf" || !strings.HasPrefix(file["file_data"].(string), "data:application/pdf;base64,") {
		t.Errorf("file = %v", file)
	}
	if get(5)["text"] != "a,b" || !strings.Contains(get(6)["text"].(string), "x.bin") {
		t.Errorf("text fallbacks = %v / %v", get(5), get(6))
	}
}

func TestGenerate_EmptyAndRefusal(t *testing.T) {
	f := newFakeAzure(t, func(w http.ResponseWriter, r *recordedRequest) {
		writeJSON(w, 200, `{"id":"x","object":"chat.completion","created":1,"model":"gpt-4o",
"choices":[{"index":0,"finish_reason":"content_filter","message":{"role":"assistant","content":null,"refusal":"I can't help with that."}}]}`)
	})
	p := f.provider()
	resp, err := p.Generate(context.Background(), "gpt-4o", &genai.Message{Role: genai.RoleUser}, nil)
	if err != nil {
		t.Fatal(err)
	}
	if msgs := messagesOf(t, f.last().Body); len(msgs) != 1 || msgs[0]["content"] != "" {
		t.Errorf("empty user message = %v", msgs)
	}
	c := resp.Candidates[0]
	if c.FinishReason != genai.FinishReasonContentFilter || c.Message.Parts[0].Attributes[AttrRefusal] != true {
		t.Errorf("refusal candidate = %+v", c)
	}
}

func TestGenerate_Errors(t *testing.T) {
	cases := []struct {
		name      string
		status    int
		body      string
		sentinel  error
		retryable bool
	}{
		{"rate limit", 429, `{"error":{"code":"429","message":"Rate limit exceeded"}}`, ErrRateLimited, true},
		{"not found", 404, `{"error":{"code":"DeploymentNotFound","message":"The API deployment for this resource does not exist."}}`, ErrDeploymentNotFound, false},
		{"content filter", 400, `{"error":{"code":"content_filter","message":"filtered","param":"prompt"}}`, ErrContentFiltered, false},
		{"context length", 400, `{"error":{"code":"context_length_exceeded","message":"too long"}}`, ErrContextLengthExceeded, false},
		{"unauthorized", 401, `{"error":{"code":"401","message":"Access denied"}}`, ErrUnauthorized, false},
		{"forbidden", 403, `{"error":{"code":"PermissionDenied","message":"no role"}}`, ErrForbidden, false},
		{"server", 503, `{"error":{"code":"ServiceUnavailable","message":"busy"}}`, ErrServiceUnavailable, true},
		{"bad request", 422, `{"error":{"code":"invalid","message":"bad"}}`, ErrBadRequest, false},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			f := newFakeAzure(t, func(w http.ResponseWriter, r *recordedRequest) { writeJSON(w, tc.status, tc.body) })
			p := f.provider(func(c *ProviderConfig) { c.Deployments = map[string]string{"gpt-4o": "dep"} })
			_, err := p.Generate(context.Background(), "gpt-4o", genai.NewTextMessage(genai.RoleUser, "x"), nil)
			var apiErr *APIError
			if !errors.As(err, &apiErr) {
				t.Fatalf("err = %v (%T), want *APIError", err, err)
			}
			if apiErr.StatusCode != tc.status || apiErr.Deployment != "dep" || apiErr.Op != "chat completion" {
				t.Errorf("apiErr = %+v", apiErr)
			}
			if !errors.Is(err, tc.sentinel) {
				t.Errorf("errors.Is(%v, %v) = false", err, tc.sentinel)
			}
			if apiErr.Retryable() != tc.retryable {
				t.Errorf("Retryable() = %v, want %v", apiErr.Retryable(), tc.retryable)
			}
			var re genai.RetryableError
			if !errors.As(err, &re) {
				t.Error("APIError must satisfy genai.RetryableError")
			}
			if !strings.Contains(err.Error(), "status ") || apiErr.Unwrap() == nil {
				t.Errorf("Error() = %q", err.Error())
			}
		})
	}
}

func TestMapError_NonAPI(t *testing.T) {
	if mapError("op", "d", nil) != nil {
		t.Error("nil must map to nil")
	}
	err := mapError("op", "d", context.Canceled)
	if !errors.Is(err, context.Canceled) || !strings.Contains(err.Error(), "canceled") {
		t.Errorf("err = %v", err)
	}
	other := errors.New("boom")
	if err := mapError("op", "d", other); !errors.Is(err, other) {
		t.Errorf("err = %v", err)
	}
	e := &APIError{StatusCode: 999}
	if e.Is(errors.New("x")) || !strings.Contains(e.Error(), "status 999") {
		t.Error("unexpected APIError behaviour")
	}
}

func weatherTool() genai.Tool {
	return genai.Tool{Function: &genai.FunctionDecl{
		Name:        "get_weather",
		Description: "Get the weather",
		Parameters:  citySchema(),
	}}
}
