package azopenai

import (
	"context"
	"errors"
	"fmt"
	"os"
	"strings"

	"github.com/openai/openai-go/v3"
	"github.com/openai/openai-go/v3/azure"
	"github.com/openai/openai-go/v3/option"
	"github.com/openai/openai-go/v3/packages/ssestream"
	"oss.nandlabs.io/golly-azure/azcfg"
	"oss.nandlabs.io/golly/l3"
)

var logger = l3.Get()

// Environment variables consulted when the endpoint / API version are not set
// explicitly on ProviderConfig or the resolved azcfg.Config.
const (
	// EnvEndpoint is the environment variable holding the Azure OpenAI endpoint.
	EnvEndpoint = "AZURE_OPENAI_ENDPOINT"
	// EnvAPIVersion is the environment variable holding the Azure OpenAI API version.
	EnvAPIVersion = "AZURE_OPENAI_API_VERSION"
	// EnvOpenAIAPIVersion is the alternative API-version variable used by the
	// official Azure OpenAI SDKs.
	EnvOpenAIAPIVersion = "OPENAI_API_VERSION"
)

// ErrMissingEndpoint is returned when no Azure OpenAI endpoint can be resolved.
var ErrMissingEndpoint = errors.New("azopenai: endpoint is required (ProviderConfig.Endpoint, azcfg.Config.Endpoint or " + EnvEndpoint + ")")

// chatAPI abstracts the Chat Completions service methods used by the provider.
// *openai.ChatCompletionService satisfies it; tests may substitute fakes.
type chatAPI interface {
	New(ctx context.Context, body openai.ChatCompletionNewParams, opts ...option.RequestOption) (*openai.ChatCompletion, error)
	NewStreaming(ctx context.Context, body openai.ChatCompletionNewParams, opts ...option.RequestOption) *ssestream.Stream[openai.ChatCompletionChunk]
}

// embeddingAPI abstracts the Embeddings service method used by the provider.
// *openai.EmbeddingService satisfies it.
type embeddingAPI interface {
	New(ctx context.Context, body openai.EmbeddingNewParams, opts ...option.RequestOption) (*openai.CreateEmbeddingResponse, error)
}

// resolveAzCfg picks the azcfg.Config to use: the explicit Config, then the
// named config from azcfg.Manager, then the "default" registered config, and
// finally a fresh azcfg.NewConfig() (DefaultAzureCredential).
func resolveAzCfg(config *ProviderConfig) *azcfg.Config {
	if config.Config != nil {
		return config.Config
	}
	if config.CfgName != "" {
		if cfg := azcfg.Manager.Get(config.CfgName); cfg != nil {
			return cfg
		}
	}
	if cfg := azcfg.Manager.Get(azcfg.DefaultConfigName); cfg != nil {
		return cfg
	}
	return azcfg.NewConfig()
}

// normalizeEndpoint trims trailing slashes and an "/openai" (or "/openai/v1")
// suffix: azure.WithEndpoint adds the "/openai/deployments/{name}" prefix
// itself, so users can paste either the resource root or the URL shown in the
// Azure portal.
func normalizeEndpoint(endpoint string) string {
	ep := strings.TrimRight(strings.TrimSpace(endpoint), "/")
	for _, suffix := range []string{"/openai/v1", "/openai"} {
		if strings.HasSuffix(strings.ToLower(ep), suffix) {
			ep = ep[:len(ep)-len(suffix)]
			break
		}
	}
	return ep
}

// buildClientOptions assembles the openai-go request options for the Azure
// endpoint, API version and authentication mode.
func buildClientOptions(config *ProviderConfig) ([]option.RequestOption, error) {
	cfg := resolveAzCfg(config)

	endpoint := config.Endpoint
	if endpoint == "" {
		endpoint = cfg.Endpoint
	}
	if endpoint == "" {
		endpoint = os.Getenv(EnvEndpoint)
	}
	endpoint = normalizeEndpoint(endpoint)
	if endpoint == "" {
		return nil, ErrMissingEndpoint
	}

	apiVersion := config.APIVersion
	if apiVersion == "" {
		apiVersion = os.Getenv(EnvAPIVersion)
	}
	if apiVersion == "" {
		apiVersion = os.Getenv(EnvOpenAIAPIVersion)
	}
	if apiVersion == "" {
		apiVersion = DefaultAPIVersion
	}

	opts := []option.RequestOption{azure.WithEndpoint(endpoint, apiVersion)}

	if config.APIKey != "" {
		opts = append(opts, azure.WithAPIKey(config.APIKey))
	} else {
		cred, err := cfg.Credential()
		if err != nil {
			return nil, fmt.Errorf("azopenai: failed to resolve Azure credential: %w", err)
		}
		var credOpts []azure.TokenCredentialOption
		if len(config.TokenScopes) > 0 {
			credOpts = append(credOpts, azure.WithTokenCredentialScopes(config.TokenScopes))
		}
		opts = append(opts, azure.WithTokenCredential(cred, credOpts...))
	}

	if config.HTTPClient != nil {
		opts = append(opts, option.WithHTTPClient(config.HTTPClient))
	}
	if config.MaxRetries != nil {
		opts = append(opts, option.WithMaxRetries(*config.MaxRetries))
	}
	if config.RequestTimeout > 0 {
		opts = append(opts, option.WithRequestTimeout(config.RequestTimeout))
	}
	if config.UnsafeAllowHTTP {
		opts = append(opts, azure.WithUnsafeAllowHTTP())
	}
	opts = append(opts, config.RequestOptions...)
	return opts, nil
}
