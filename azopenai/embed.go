package azopenai

import (
	"context"
	"fmt"
	"sort"

	"github.com/openai/openai-go/v3"
	"oss.nandlabs.io/golly/genai"
)

// Embed generates embedding vectors for one or more text inputs using the
// Azure OpenAI Embeddings endpoint (text-embedding-3-small/-large,
// text-embedding-ada-002). All inputs are sent in a single batched request
// and the response vectors are returned in input order.
//
// The model is EmbedRequest.Model, or ProviderConfig.EmbeddingModel when
// empty; it is mapped through ProviderConfig.Deployments like chat models.
// An optional "dimensions" attribute (AttrDimensions) on the first input
// shortens text-embedding-3 vectors. Only non-empty text parts are supported.
func (p *AzureOpenAIProvider) Embed(ctx context.Context, req *genai.EmbedRequest) (*genai.EmbedResponse, error) {
	if req == nil {
		return nil, fmt.Errorf("azopenai: embed request is nil")
	}
	model := req.Model
	if model == "" {
		model = p.embeddingModel
	}
	if model == "" {
		return nil, fmt.Errorf("azopenai: embed request requires a Model (or ProviderConfig.EmbeddingModel)")
	}
	if len(req.Inputs) == 0 {
		return nil, fmt.Errorf("azopenai: embed request has no inputs")
	}

	texts, err := embedInputsAsText(req.Inputs)
	if err != nil {
		return nil, err
	}

	deployment := p.Deployment(model)
	params := openai.EmbeddingNewParams{
		Input:          openai.EmbeddingNewParamsInputUnion{OfArrayOfStrings: texts},
		Model:          deployment,
		EncodingFormat: openai.EmbeddingNewParamsEncodingFormatFloat,
	}
	if dims, ok := embedDimensions(req.Inputs[0]); ok {
		params.Dimensions = openai.Int(dims)
	}

	resp, err := p.embeddings.New(ctx, params)
	if err != nil {
		return nil, mapError("embeddings", deployment, err)
	}
	return toEmbedResponse(resp, len(texts))
}

// embedInputsAsText extracts one text string per Part. Binary / file parts
// are rejected rather than silently dropped.
func embedInputsAsText(inputs []genai.Part) ([]string, error) {
	texts := make([]string, 0, len(inputs))
	for i, part := range inputs {
		if part.Text == nil || part.Text.Content == "" {
			return nil, fmt.Errorf("azopenai: embed input[%d]: only non-empty text parts are supported", i)
		}
		texts = append(texts, part.Text.Content)
	}
	return texts, nil
}

// embedDimensions reads the optional "dimensions" attribute.
func embedDimensions(part genai.Part) (int64, bool) {
	switch v := part.Attributes[AttrDimensions].(type) {
	case int:
		return int64(v), v > 0
	case int32:
		return int64(v), v > 0
	case int64:
		return v, v > 0
	case float64:
		return int64(v), v > 0
	}
	return 0, false
}

// toEmbedResponse converts the SDK response into a golly EmbedResponse,
// ordering vectors by their index and converting to float32.
func toEmbedResponse(resp *openai.CreateEmbeddingResponse, n int) (*genai.EmbedResponse, error) {
	if resp == nil {
		return nil, fmt.Errorf("azopenai: embeddings returned an empty response")
	}
	data := append([]openai.Embedding(nil), resp.Data...)
	sort.SliceStable(data, func(i, j int) bool { return data[i].Index < data[j].Index })
	if len(data) != n {
		return nil, fmt.Errorf("azopenai: embeddings returned %d vectors for %d inputs", len(data), n)
	}

	vectors := make([][]float32, len(data))
	for i, d := range data {
		v := make([]float32, len(d.Embedding))
		for j, f := range d.Embedding {
			v[j] = float32(f)
		}
		vectors[i] = v
	}
	return &genai.EmbedResponse{
		Vectors: vectors,
		Meta: &genai.ResponseMeta{
			InputTokens: int(resp.Usage.PromptTokens),
			TotalTokens: int(resp.Usage.TotalTokens),
		},
	}, nil
}
