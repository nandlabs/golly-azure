package azopenai

import (
	"context"
	"errors"
	"net/http"
	"strings"
	"testing"
	"time"

	"oss.nandlabs.io/golly/data"
	"oss.nandlabs.io/golly/genai"
)

func TestGenerateStream_Text(t *testing.T) {
	f := newFakeAzure(t, func(w http.ResponseWriter, r *recordedRequest) {
		writeSSE(w,
			// Azure preamble: prompt filter results with no choices.
			`{"id":"","object":"","created":0,"model":"","choices":[],"prompt_filter_results":[{"prompt_index":0,"content_filter_results":{}}]}`,
			`{"id":"c","object":"chat.completion.chunk","created":1,"model":"gpt-4o","choices":[{"index":0,"delta":{"role":"assistant","content":""},"finish_reason":null}]}`,
			`{"id":"c","object":"chat.completion.chunk","created":1,"model":"gpt-4o","choices":[{"index":0,"delta":{"content":"Hello"},"finish_reason":null}]}`,
			`{"id":"c","object":"chat.completion.chunk","created":1,"model":"gpt-4o","choices":[{"index":0,"delta":{"content":", world"},"finish_reason":null}]}`,
			`{"id":"c","object":"chat.completion.chunk","created":1,"model":"gpt-4o","choices":[{"index":0,"delta":{},"finish_reason":"stop"}]}`,
			`{"id":"c","object":"chat.completion.chunk","created":1,"model":"gpt-4o","choices":[],"usage":{"prompt_tokens":4,"completion_tokens":3,"total_tokens":7,"prompt_tokens_details":{"cached_tokens":1}}}`,
		)
	})
	p := f.provider(func(c *ProviderConfig) { c.Deployments = map[string]string{"gpt-4o": "stream-dep"} })

	rc, ec := p.GenerateStream(context.Background(), "gpt-4o", genai.NewTextMessage(genai.RoleUser, "hi"), nil)
	resps, err := collectStream(t, rc, ec)
	if err != nil {
		t.Fatalf("stream error: %v", err)
	}

	req := f.last()
	if req.Path != "/openai/deployments/stream-dep/chat/completions" || req.Body["stream"] != true {
		t.Errorf("request = %s %v", req.Path, req.Body["stream"])
	}
	if so, _ := req.Body["stream_options"].(map[string]any); so["include_usage"] != true {
		t.Errorf("stream_options = %v", req.Body["stream_options"])
	}

	var text strings.Builder
	var finish genai.FinishReason
	var meta genai.ResponseMeta
	for _, r := range resps {
		text.WriteString(textOf(r))
		for _, c := range r.Candidates {
			if c.FinishReason != genai.FinishReasonInProgress {
				finish = c.FinishReason
			}
		}
		if r.Meta.TotalTokens > 0 {
			meta = r.Meta
		}
	}
	if text.String() != "Hello, world" {
		t.Errorf("text = %q", text.String())
	}
	if finish != genai.FinishReasonStop {
		t.Errorf("finish = %q", finish)
	}
	if meta.InputTokens != 4 || meta.OutputTokens != 3 || meta.CachedTokens != 1 {
		t.Errorf("meta = %+v", meta)
	}
	// preamble and role-only chunks are skipped: 2 deltas + finish + usage.
	if len(resps) != 4 {
		t.Errorf("got %d responses, want 4", len(resps))
	}
}

func TestGenerateStream_ToolCalls(t *testing.T) {
	f := newFakeAzure(t, func(w http.ResponseWriter, r *recordedRequest) {
		writeSSE(w,
			`{"id":"c","object":"chat.completion.chunk","created":1,"model":"gpt-4o","choices":[{"index":0,"delta":{"role":"assistant","tool_calls":[{"index":0,"id":"call_1","type":"function","function":{"name":"get_weather","arguments":""}}]},"finish_reason":null}]}`,
			`{"id":"c","object":"chat.completion.chunk","created":1,"model":"gpt-4o","choices":[{"index":0,"delta":{"tool_calls":[{"index":0,"function":{"arguments":"{\"ci"}}]},"finish_reason":null}]}`,
			`{"id":"c","object":"chat.completion.chunk","created":1,"model":"gpt-4o","choices":[{"index":0,"delta":{"tool_calls":[{"index":1,"id":"call_2","type":"function","function":{"name":"get_time","arguments":"{}"}}]},"finish_reason":null}]}`,
			`{"id":"c","object":"chat.completion.chunk","created":1,"model":"gpt-4o","choices":[{"index":0,"delta":{"tool_calls":[{"index":0,"function":{"arguments":"ty\":\"Rome\"}"}}]},"finish_reason":null}]}`,
			`{"id":"c","object":"chat.completion.chunk","created":1,"model":"gpt-4o","choices":[{"index":0,"delta":{},"finish_reason":"tool_calls"}],"usage":{"prompt_tokens":9,"completion_tokens":4,"total_tokens":13}}`,
		)
	})
	p := f.provider()
	opts := genai.NewOptionsBuilder().SetTools(weatherTool()).SetToolChoice(genai.NewToolChoice(genai.ToolChoiceAuto)).Build()
	rc, ec := p.GenerateStream(context.Background(), "gpt-4o", genai.NewTextMessage(genai.RoleUser, "weather"), opts)
	resps, err := collectStream(t, rc, ec)
	if err != nil {
		t.Fatal(err)
	}
	if len(resps) != 1 {
		t.Fatalf("got %d responses, want 1 (fragments are accumulated)", len(resps))
	}
	r := resps[0]
	c := r.Candidates[0]
	if c.FinishReason != genai.FinishReasonToolCall || len(c.Message.Parts) != 2 {
		t.Fatalf("candidate = %+v", c)
	}
	first := c.Message.Parts[0].FuncCall
	if first.Id != "call_1" || first.FunctionName != "get_weather" || first.Arguments["city"] != "Rome" {
		t.Errorf("first call = %+v", first)
	}
	if c.Message.Parts[1].FuncCall.FunctionName != "get_time" {
		t.Errorf("second call = %+v", c.Message.Parts[1].FuncCall)
	}
	if r.Meta.TotalTokens != 13 || r.Meta.TimeToFirst < 0 {
		t.Errorf("meta = %+v", r.Meta)
	}
	if f.last().Body["tool_choice"] != "auto" {
		t.Errorf("tool_choice = %v", f.last().Body["tool_choice"])
	}
}

func TestGenerateStream_HTTPError(t *testing.T) {
	f := newFakeAzure(t, func(w http.ResponseWriter, r *recordedRequest) {
		writeJSON(w, 429, `{"error":{"code":"429","message":"slow down"}}`)
	})
	p := f.provider()
	rc, ec := p.GenerateStream(context.Background(), "gpt-4o", genai.NewTextMessage(genai.RoleUser, "x"), nil)
	resps, err := collectStream(t, rc, ec)
	if len(resps) != 0 {
		t.Errorf("unexpected responses: %v", resps)
	}
	if !errors.Is(err, ErrRateLimited) {
		t.Fatalf("err = %v, want ErrRateLimited", err)
	}
	var apiErr *APIError
	if !errors.As(err, &apiErr) || !apiErr.Retryable() || apiErr.Op != "streaming chat completion" {
		t.Errorf("apiErr = %+v", apiErr)
	}
}

func TestGenerateStream_ContextCanceled(t *testing.T) {
	release := make(chan struct{})
	f := newFakeAzure(t, func(w http.ResponseWriter, r *recordedRequest) {
		w.Header().Set("Content-Type", "text/event-stream")
		w.WriteHeader(200)
		for i := 0; i < 50; i++ {
			_, _ = w.Write([]byte(`data: {"id":"c","object":"chat.completion.chunk","created":1,"model":"m","choices":[{"index":0,"delta":{"content":"x"},"finish_reason":null}]}` + "\n\n"))
		}
		w.(http.Flusher).Flush()
		select {
		case <-release:
		case <-time.After(5 * time.Second):
		}
	})
	defer close(release)
	p := f.provider()
	ctx, cancel := context.WithCancel(context.Background())
	rc, ec := p.GenerateStream(ctx, "gpt-4o", genai.NewTextMessage(genai.RoleUser, "x"), nil)
	<-rc // receive one, then cancel without draining
	cancel()
	for range rc {
	}
	if err := <-ec; err == nil {
		t.Error("expected an error after cancellation")
	}
}

func TestStreamAccumulator_MultiChoiceAndRefusal(t *testing.T) {
	acc := newStreamAccumulator(time.Now())
	chunk := mustChunk(t, `{"id":"c","object":"chat.completion.chunk","created":1,"model":"m","choices":[
{"index":0,"delta":{"content":"A"},"finish_reason":null},
{"index":1,"delta":{"refusal":"no"},"finish_reason":"content_filter"}]}`)
	r := acc.add(chunk)
	if r == nil || len(r.Candidates) != 2 {
		t.Fatalf("resp = %+v", r)
	}
	if r.Candidates[1].FinishReason != genai.FinishReasonContentFilter || r.Candidates[1].Message.Parts[0].Attributes[AttrRefusal] != true {
		t.Errorf("refusal candidate = %+v", r.Candidates[1])
	}
	if r.Meta.TimeToFirst < 0 {
		t.Error("TimeToFirst must be set on first content")
	}
	// Finish without content produces an empty text part.
	r2 := acc.add(mustChunk(t, `{"id":"c","object":"chat.completion.chunk","created":1,"model":"m","choices":[{"index":0,"delta":{},"finish_reason":"length"}]}`))
	if r2.Candidates[0].FinishReason != genai.FinishReasonLength || len(r2.Candidates[0].Message.Parts) != 1 {
		t.Errorf("finish candidate = %+v", r2.Candidates[0])
	}
}

func citySchema() *data.Schema {
	return &data.Schema{
		Type:       "object",
		Properties: map[string]*data.Schema{"city": {Type: "string", Description: "City name"}},
		Required:   []string{"city"},
	}
}

func countrySchema() *data.Schema {
	return &data.Schema{
		Type:        "object",
		Title:       "Country Info",
		Description: "A country",
		Properties: map[string]*data.Schema{
			"name":    {Type: "string"},
			"capital": {Type: "object", Properties: map[string]*data.Schema{"city": {Type: "string"}}, Required: []string{"city"}},
		},
		Required: []string{"name", "capital"},
	}
}
