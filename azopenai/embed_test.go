package azopenai

import (
	"context"
	"errors"
	"net/http"
	"strings"
	"testing"

	"github.com/openai/openai-go/v3"
	"oss.nandlabs.io/golly/genai"
)

func textInputs(texts ...string) []genai.Part {
	parts := make([]genai.Part, 0, len(texts))
	for _, t := range texts {
		parts = append(parts, genai.Part{Text: &genai.TextPart{Content: t}})
	}
	return parts
}

func TestEmbed_Batch(t *testing.T) {
	f := newFakeAzure(t, func(w http.ResponseWriter, r *recordedRequest) {
		// Deliberately out of order to verify index-based ordering.
		writeJSON(w, 200, `{"object":"list","model":"text-embedding-3-small","data":[
{"object":"embedding","index":1,"embedding":[0.4,0.5]},
{"object":"embedding","index":0,"embedding":[0.1,0.2]}],
"usage":{"prompt_tokens":6,"total_tokens":6}}`)
	})
	p := f.provider(func(c *ProviderConfig) {
		c.Deployments = map[string]string{"text-embedding-3-small": "emb-small"}
	})
	inputs := textInputs("hello", "world")
	inputs[0].Attributes = map[string]any{AttrDimensions: 2}

	var _ genai.Embedder = p
	resp, err := p.Embed(context.Background(), &genai.EmbedRequest{Model: "text-embedding-3-small", Inputs: inputs})
	if err != nil {
		t.Fatalf("Embed: %v", err)
	}
	req := f.last()
	if req.Path != "/openai/deployments/emb-small/embeddings" {
		t.Errorf("path = %s", req.Path)
	}
	in, _ := req.Body["input"].([]any)
	if len(in) != 2 || in[0] != "hello" || req.Body["dimensions"] != float64(2) || req.Body["encoding_format"] != "float" {
		t.Errorf("body = %v", req.Body)
	}
	if len(resp.Vectors) != 2 || resp.Vectors[0][0] != float32(0.1) || resp.Vectors[1][1] != float32(0.5) {
		t.Errorf("vectors = %v", resp.Vectors)
	}
	if resp.Meta == nil || resp.Meta.InputTokens != 6 || resp.Meta.TotalTokens != 6 {
		t.Errorf("meta = %+v", resp.Meta)
	}
}

func TestEmbed_DefaultModel(t *testing.T) {
	f := newFakeAzure(t, func(w http.ResponseWriter, r *recordedRequest) {
		writeJSON(w, 200, `{"object":"list","model":"m","data":[{"object":"embedding","index":0,"embedding":[1]}],"usage":{"prompt_tokens":1,"total_tokens":1}}`)
	})
	p := f.provider(func(c *ProviderConfig) { c.EmbeddingModel = "text-embedding-3-large" })
	if _, err := p.Embed(context.Background(), &genai.EmbedRequest{Inputs: textInputs("x")}); err != nil {
		t.Fatal(err)
	}
	if f.last().Path != "/openai/deployments/text-embedding-3-large/embeddings" {
		t.Errorf("path = %s", f.last().Path)
	}
}

func TestEmbed_Errors(t *testing.T) {
	f := newFakeAzure(t, func(w http.ResponseWriter, r *recordedRequest) {
		if r.Body["model"] == "mismatch" {
			writeJSON(w, 200, `{"object":"list","model":"m","data":[],"usage":{"prompt_tokens":1,"total_tokens":1}}`)
			return
		}
		writeJSON(w, 404, `{"error":{"code":"DeploymentNotFound","message":"nope"}}`)
	})
	p := f.provider()
	ctx := context.Background()

	if _, err := p.Embed(ctx, nil); err == nil {
		t.Error("nil request must fail")
	}
	if _, err := p.Embed(ctx, &genai.EmbedRequest{Inputs: textInputs("x")}); err == nil {
		t.Error("missing model must fail")
	}
	if _, err := p.Embed(ctx, &genai.EmbedRequest{Model: "m"}); err == nil {
		t.Error("no inputs must fail")
	}
	bin := []genai.Part{{MimeType: "image/png", Bin: &genai.BinPart{Data: []byte{1}}}}
	if _, err := p.Embed(ctx, &genai.EmbedRequest{Model: "m", Inputs: bin}); err == nil || !strings.Contains(err.Error(), "input[0]") {
		t.Errorf("binary input err = %v", err)
	}
	_, err := p.Embed(ctx, &genai.EmbedRequest{Model: "missing", Inputs: textInputs("x")})
	if !errors.Is(err, ErrDeploymentNotFound) {
		t.Errorf("err = %v, want ErrDeploymentNotFound", err)
	}
	if _, err := p.Embed(ctx, &genai.EmbedRequest{Model: "mismatch", Inputs: textInputs("x")}); err == nil {
		t.Error("vector count mismatch must fail")
	}
}

func TestEmbedDimensions(t *testing.T) {
	cases := []struct {
		v    any
		want int64
		ok   bool
	}{
		{256, 256, true}, {int32(8), 8, true}, {int64(9), 9, true}, {float64(10), 10, true},
		{0, 0, false}, {"256", 0, false}, {nil, 0, false},
	}
	for _, c := range cases {
		got, ok := embedDimensions(genai.Part{Attributes: map[string]any{AttrDimensions: c.v}})
		if got != c.want || ok != c.ok {
			t.Errorf("embedDimensions(%v) = %d,%v want %d,%v", c.v, got, ok, c.want, c.ok)
		}
	}
	if _, err := toEmbedResponse((*openai.CreateEmbeddingResponse)(nil), 0); err == nil {
		t.Error("nil response must fail")
	}
}
