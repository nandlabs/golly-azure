// Package azopenai implements the golly genai.Provider interface for Azure
// OpenAI and Azure AI Foundry (Azure OpenAI in Foundry Models).
//
// It talks to the deployment-scoped Chat Completions and Embeddings REST
// endpoints using github.com/openai/openai-go/v3 together with its azure
// subpackage. Authentication uses either a Microsoft Entra ID token credential
// resolved through the golly-azure azcfg package (DefaultAzureCredential,
// service principal, managed identity, or any azcore.TokenCredential) or an
// API key.
//
// Azure addresses models by deployment name rather than by model id. The
// provider keeps golly-facing model names (for example "gpt-4o") stable and
// maps them to deployment names through ProviderConfig.Deployments, so the
// same model names can be used with the golly model router regardless of how
// the Azure resource names its deployments.
//
// Supported features: synchronous and streaming generation, system prompts,
// multimodal input (images, audio, PDF), tool / function calling, JSON mode
// and JSON-schema structured output, token usage accounting (including cached
// and reasoning tokens), batch embeddings (genai.Embedder), a static model
// catalog (genai.CapabilityProvider) and typed, retry-aware errors
// (genai.RetryableError).
package azopenai
