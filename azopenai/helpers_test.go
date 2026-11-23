package azopenai

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"

	"github.com/Azure/azure-sdk-for-go/sdk/azcore"
	"github.com/Azure/azure-sdk-for-go/sdk/azcore/policy"
	"oss.nandlabs.io/golly/genai"
)

// recordedRequest captures what the fake Azure OpenAI endpoint received.
type recordedRequest struct {
	Method string
	Path   string
	Query  map[string][]string
	Header http.Header
	Body   map[string]any
}

// fakeAzure is an httptest server emulating the deployment-scoped Azure
// OpenAI Chat Completions and Embeddings endpoints.
type fakeAzure struct {
	t       *testing.T
	srv     *httptest.Server
	mu      sync.Mutex
	reqs    []recordedRequest
	handler func(w http.ResponseWriter, r *recordedRequest)
}

func newFakeAzure(t *testing.T, handler func(w http.ResponseWriter, r *recordedRequest)) *fakeAzure {
	t.Helper()
	f := &fakeAzure{t: t, handler: handler}
	f.srv = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		raw, _ := io.ReadAll(r.Body)
		rec := recordedRequest{Method: r.Method, Path: r.URL.Path, Query: r.URL.Query(), Header: r.Header.Clone()}
		if len(raw) > 0 {
			if err := json.Unmarshal(raw, &rec.Body); err != nil {
				t.Errorf("request body is not JSON: %v", err)
			}
		}
		f.mu.Lock()
		f.reqs = append(f.reqs, rec)
		f.mu.Unlock()
		f.handler(w, &rec)
	}))
	t.Cleanup(f.srv.Close)
	return f
}

func (f *fakeAzure) last() recordedRequest {
	f.mu.Lock()
	defer f.mu.Unlock()
	if len(f.reqs) == 0 {
		f.t.Fatal("no request recorded")
	}
	return f.reqs[len(f.reqs)-1]
}

func (f *fakeAzure) count() int {
	f.mu.Lock()
	defer f.mu.Unlock()
	return len(f.reqs)
}

// provider builds an API-key provider pointed at the fake server.
func (f *fakeAzure) provider(mutate ...func(*ProviderConfig)) *AzureOpenAIProvider {
	f.t.Helper()
	zero := 0
	cfg := &ProviderConfig{
		Endpoint:        f.srv.URL,
		APIKey:          "test-key",
		UnsafeAllowHTTP: true,
		MaxRetries:      &zero,
	}
	for _, m := range mutate {
		m(cfg)
	}
	p, err := NewAzureOpenAIProvider(cfg)
	if err != nil {
		f.t.Fatalf("NewAzureOpenAIProvider: %v", err)
	}
	return p
}

func writeJSON(w http.ResponseWriter, status int, body string) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	_, _ = io.WriteString(w, body)
}

func writeSSE(w http.ResponseWriter, events ...string) {
	w.Header().Set("Content-Type", "text/event-stream")
	w.WriteHeader(http.StatusOK)
	fl, _ := w.(http.Flusher)
	for _, e := range events {
		_, _ = fmt.Fprintf(w, "data: %s\n\n", e)
		if fl != nil {
			fl.Flush()
		}
	}
	_, _ = io.WriteString(w, "data: [DONE]\n\n")
}

func chatCompletionJSON(content, finish string) string {
	return fmt.Sprintf(`{"id":"chatcmpl-1","object":"chat.completion","created":1,"model":"gpt-4o",
"choices":[{"index":0,"finish_reason":%q,"message":{"role":"assistant","content":%q}}],
"usage":{"prompt_tokens":11,"completion_tokens":7,"total_tokens":18,
"prompt_tokens_details":{"cached_tokens":3},"completion_tokens_details":{"reasoning_tokens":2}}}`, finish, content)
}

// fakeCred is a static azcore.TokenCredential.
type fakeCred struct{ token string }

func (c fakeCred) GetToken(context.Context, policy.TokenRequestOptions) (azcore.AccessToken, error) {
	return azcore.AccessToken{Token: c.token}, nil
}

// collectStream drains a GenerateStream call.
func collectStream(t *testing.T, rc <-chan *genai.GenResponse, ec <-chan error) ([]*genai.GenResponse, error) {
	t.Helper()
	var out []*genai.GenResponse
	for r := range rc {
		out = append(out, r)
	}
	return out, <-ec
}

func textOf(resp *genai.GenResponse) string {
	var b strings.Builder
	if resp == nil {
		return ""
	}
	for _, c := range resp.Candidates {
		if c.Message == nil {
			continue
		}
		for _, p := range c.Message.Parts {
			if p.Text != nil {
				b.WriteString(p.Text.Content)
			}
		}
	}
	return b.String()
}

func messagesOf(t *testing.T, body map[string]any) []map[string]any {
	t.Helper()
	raw, ok := body["messages"].([]any)
	if !ok {
		t.Fatalf("messages missing: %v", body)
	}
	out := make([]map[string]any, 0, len(raw))
	for _, m := range raw {
		out = append(out, m.(map[string]any))
	}
	return out
}
