package azopenai

import (
	"context"
	"fmt"
	"net/http"
	"sort"
	"time"

	"github.com/openai/openai-go/v3"
	"github.com/openai/openai-go/v3/option"
	"oss.nandlabs.io/golly-azure/azcfg"
	"oss.nandlabs.io/golly/genai"
)

const (
	// ProviderName is the name of the Azure OpenAI provider.
	ProviderName = "azure-openai"
	// ProviderVersion is the version of the Azure OpenAI provider.
	ProviderVersion = "1.0.0"
	// ProviderDescription is the description of the Azure OpenAI provider.
	ProviderDescription = "Azure OpenAI / Azure AI Foundry provider for chat completions and embeddings via openai-go"
	// DefaultMaxTokens is the default max tokens if not specified in options.
	DefaultMaxTokens = 4096
	// DefaultAPIVersion is the Azure OpenAI data-plane API version used when
	// none is configured. It supports reasoning models (o-series, GPT-5),
	// max_completion_tokens, reasoning_effort and JSON-schema structured output.
	DefaultAPIVersion = "2025-04-01-preview"
)

// Compile-time interface conformance checks.
var (
	_ genai.Provider = (*AzureOpenAIProvider)(nil)
	_ genai.Embedder = (*AzureOpenAIProvider)(nil)
)

// AzureOpenAIProvider implements genai.Provider (Chat Completions),
// genai.Embedder (Embeddings) and genai.CapabilityProvider (model catalog) for
// Azure OpenAI and Azure AI Foundry deployments.
type AzureOpenAIProvider struct {
	chat            chatAPI
	embeddings      embeddingAPI
	models          []string
	deployments     map[string]string
	embeddingModel  string
	legacyMaxTokens bool
	catalog         []genai.ModelInfo
	description     string
	version         string
}

// ProviderConfig contains configuration for the Azure OpenAI provider.
type ProviderConfig struct {
	// CfgName is the name of the azcfg.Config registered with azcfg.Manager.
	// When empty (or not found) the "default" registered config is used, and
	// failing that a fresh azcfg.NewConfig() (DefaultAzureCredential).
	CfgName string
	// Config is an explicit azcfg.Config. Overrides CfgName when set. Its
	// Endpoint and Credential() are used unless overridden below.
	Config *azcfg.Config
	// Endpoint is the Azure OpenAI / AI Foundry resource endpoint, e.g.
	// https://<resource>.openai.azure.com or
	// https://<resource>.services.ai.azure.com. Overrides azcfg.Config.Endpoint.
	// Falls back to the AZURE_OPENAI_ENDPOINT environment variable.
	Endpoint string
	// APIVersion is the data-plane api-version query parameter. Defaults to
	// AZURE_OPENAI_API_VERSION / OPENAI_API_VERSION, then DefaultAPIVersion.
	APIVersion string
	// APIKey switches authentication to an Azure OpenAI API key (Api-Key
	// header). When empty, Microsoft Entra ID token authentication via the
	// azcfg credential is used.
	APIKey string
	// TokenScopes overrides the Entra ID token scope
	// (default https://cognitiveservices.azure.com/.default).
	TokenScopes []string
	// Deployments maps golly model names (e.g. "gpt-4o") to Azure deployment
	// names (e.g. "prod-gpt4o"). Models without an entry are used verbatim as
	// the deployment name.
	Deployments map[string]string
	// Models is the list of model names served by this provider instance. When
	// empty, the keys of Deployments are used. When either is set, the model
	// catalog advertised to the golly router is restricted to these models.
	Models []string
	// EmbeddingModel is the default embedding model / deployment used by Embed
	// when EmbedRequest.Model is empty.
	EmbeddingModel string
	// LegacyMaxTokens sends the deprecated max_tokens field instead of
	// max_completion_tokens (needed only for very old API versions).
	LegacyMaxTokens bool
	// HTTPClient is an optional custom HTTP client. It must be an
	// *http.Client so the Azure credential redirect guard can wrap it.
	HTTPClient *http.Client
	// MaxRetries overrides the SDK retry count (default 2). Use a pointer to 0
	// to disable retries, e.g. when the golly router handles fallover.
	MaxRetries *int
	// RequestTimeout sets a per-attempt request timeout (0 = none).
	RequestTimeout time.Duration
	// UnsafeAllowHTTP permits plaintext HTTP to loopback endpoints only. For
	// local development and tests; never enable in production.
	UnsafeAllowHTTP bool
	// RequestOptions are appended verbatim to the openai-go client options.
	RequestOptions []option.RequestOption
	// Description is a custom description for the provider.
	Description string
	// Version is a custom version for the provider.
	Version string
}

// NewAzureOpenAIProvider creates a new AzureOpenAIProvider. It resolves the
// endpoint, API version and credentials from config, azcfg and environment
// variables, then builds an openai-go client configured for Azure.
func NewAzureOpenAIProvider(config *ProviderConfig) (*AzureOpenAIProvider, error) {
	if config == nil {
		config = &ProviderConfig{}
	}
	opts, err := buildClientOptions(config)
	if err != nil {
		return nil, err
	}
	client := openai.NewClient(opts...)
	return newProvider(config, &client.Chat.Completions, &client.Embeddings), nil
}

// newProvider wires a provider around the given service implementations.
func newProvider(config *ProviderConfig, chat chatAPI, emb embeddingAPI) *AzureOpenAIProvider {
	desc := config.Description
	if desc == "" {
		desc = ProviderDescription
	}
	ver := config.Version
	if ver == "" {
		ver = ProviderVersion
	}

	deployments := make(map[string]string, len(config.Deployments))
	for k, v := range config.Deployments {
		deployments[k] = v
	}

	models := append([]string(nil), config.Models...)
	if len(models) == 0 && len(deployments) > 0 {
		for k := range deployments {
			models = append(models, k)
		}
		sort.Strings(models)
	}

	return &AzureOpenAIProvider{
		chat:            chat,
		embeddings:      emb,
		models:          models,
		deployments:     deployments,
		embeddingModel:  config.EmbeddingModel,
		legacyMaxTokens: config.LegacyMaxTokens,
		catalog:         buildCatalog(models),
		description:     desc,
		version:         ver,
	}
}

// Name returns the name of the provider.
func (p *AzureOpenAIProvider) Name() string { return ProviderName }

// Description returns a brief description of the provider.
func (p *AzureOpenAIProvider) Description() string { return p.description }

// Version returns the version of the provider.
func (p *AzureOpenAIProvider) Version() string { return p.version }

// Models returns the list of model names served by this provider instance.
func (p *AzureOpenAIProvider) Models() []string { return p.models }

// Close releases provider resources. The HTTP client holds no state that
// requires explicit release, so this is a no-op.
func (p *AzureOpenAIProvider) Close() error { return nil }

// Deployment returns the Azure deployment name used for a golly model name.
// Unmapped models are returned unchanged.
func (p *AzureOpenAIProvider) Deployment(model string) string {
	if d, ok := p.deployments[model]; ok && d != "" {
		return d
	}
	return model
}

// Generate performs a synchronous Chat Completions call against the
// deployment mapped from model.
func (p *AzureOpenAIProvider) Generate(ctx context.Context, model string, message *genai.Message, options *genai.Options) (*genai.GenResponse, error) {
	params, err := p.buildParams(model, message, options)
	if err != nil {
		return nil, fmt.Errorf("azopenai: failed to build chat completion request: %w", err)
	}

	start := time.Now()
	resp, err := p.chat.New(ctx, params)
	if err != nil {
		return nil, mapError("chat completion", p.Deployment(model), err)
	}

	genResp := toGenResponse(resp)
	genResp.Meta.TotalTime = time.Since(start).Milliseconds()
	return genResp, nil
}

// GenerateStream performs a streaming Chat Completions call (SSE). Text
// deltas are emitted as they arrive with FinishReasonInProgress; tool-call
// argument fragments are accumulated and emitted, fully assembled, on the
// candidate's final event; token usage arrives in a trailing Meta-only event.
func (p *AzureOpenAIProvider) GenerateStream(ctx context.Context, model string, message *genai.Message, options *genai.Options) (<-chan *genai.GenResponse, <-chan error) {
	responseChan := make(chan *genai.GenResponse, 10)
	errorChan := make(chan error, 1)

	go func() {
		defer close(responseChan)
		defer close(errorChan)

		params, err := p.buildParams(model, message, options)
		if err != nil {
			errorChan <- fmt.Errorf("azopenai: failed to build chat completion request: %w", err)
			return
		}
		params.StreamOptions = openai.ChatCompletionStreamOptionsParam{IncludeUsage: openai.Bool(true)}

		start := time.Now()
		stream := p.chat.NewStreaming(ctx, params)
		if stream == nil {
			errorChan <- fmt.Errorf("azopenai: streaming chat completion returned nil stream")
			return
		}
		defer func() {
			if closeErr := stream.Close(); closeErr != nil {
				logger.ErrorF("azopenai: failed to close stream: %v", closeErr)
			}
		}()

		acc := newStreamAccumulator(start)
		for stream.Next() {
			genResp := acc.add(stream.Current())
			if genResp == nil {
				continue
			}
			select {
			case <-ctx.Done():
				errorChan <- ctx.Err()
				return
			case responseChan <- genResp:
			}
		}

		if err := stream.Err(); err != nil {
			errorChan <- mapError("streaming chat completion", p.Deployment(model), err)
		}
	}()

	return responseChan, errorChan
}
