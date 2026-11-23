package azopenai

import (
	"strings"

	"oss.nandlabs.io/golly/genai"
)

// Compile-time assertion that AzureOpenAIProvider advertises structured model
// metadata to the golly model router (golly >= v1.8.0). The router
// type-asserts each Provider to CapabilityProvider and folds its ModelCatalog
// into the ModelRegistry.
var _ genai.CapabilityProvider = (*AzureOpenAIProvider)(nil)

// Shorthand capability sets.
var (
	capsChatVision = []genai.Capability{
		genai.CapText, genai.CapChat, genai.CapStreaming, genai.CapVision,
		genai.CapToolCalling, genai.CapJSONMode,
	}
	capsReasoningVision = []genai.Capability{
		genai.CapText, genai.CapChat, genai.CapStreaming, genai.CapVision,
		genai.CapToolCalling, genai.CapJSONMode, genai.CapReasoning,
	}
	capsReasoningText = []genai.Capability{
		genai.CapText, genai.CapChat, genai.CapStreaming,
		genai.CapToolCalling, genai.CapJSONMode, genai.CapReasoning,
	}
)

// azureCatalog is the authoritative, static metadata for the Azure OpenAI
// model surface. Names are the canonical OpenAI model names (the golly-facing
// names); ProviderConfig.Deployments maps them to deployment names. The
// Provider field on every entry is ProviderName so router registry keys
// (provider/model) line up with this provider.
//
// pricing as of 2026-07 — public Azure OpenAI Global Standard pay-as-you-go
// list prices in USD per 1M tokens. Regional / Data Zone deployments and
// provisioned throughput are priced differently; operators can override via
// genai.RoutingConfig.CostOverrides.
var azureCatalog = []genai.ModelInfo{
	// ---- GPT-5 family ----
	{
		Name: "gpt-5.1", Provider: ProviderName, DisplayName: "GPT-5.1",
		Capabilities:   capsReasoningVision,
		MaxInputTokens: 272000, MaxOutputTokens: 128000,
		InputCostPerMTok: 1.25, OutputCostPerMTok: 10.0,
		Metadata: map[string]string{"family": "gpt-5", "reasoning": "true"},
	},
	{
		Name: "gpt-5", Provider: ProviderName, DisplayName: "GPT-5",
		Capabilities:   capsReasoningVision,
		MaxInputTokens: 272000, MaxOutputTokens: 128000,
		InputCostPerMTok: 1.25, OutputCostPerMTok: 10.0,
		Metadata: map[string]string{"family": "gpt-5", "reasoning": "true"},
	},
	{
		Name: "gpt-5-mini", Provider: ProviderName, DisplayName: "GPT-5 mini",
		Capabilities:   capsReasoningVision,
		MaxInputTokens: 272000, MaxOutputTokens: 128000,
		InputCostPerMTok: 0.25, OutputCostPerMTok: 2.0,
		Metadata: map[string]string{"family": "gpt-5", "tier": "mini", "reasoning": "true"},
	},
	{
		Name: "gpt-5-nano", Provider: ProviderName, DisplayName: "GPT-5 nano",
		Capabilities:   capsReasoningVision,
		MaxInputTokens: 272000, MaxOutputTokens: 128000,
		InputCostPerMTok: 0.05, OutputCostPerMTok: 0.40,
		Metadata: map[string]string{"family": "gpt-5", "tier": "nano", "reasoning": "true"},
	},
	{
		Name: "gpt-5-chat", Provider: ProviderName, DisplayName: "GPT-5 chat",
		Capabilities:   capsChatVision,
		MaxInputTokens: 128000, MaxOutputTokens: 16384,
		InputCostPerMTok: 1.25, OutputCostPerMTok: 10.0,
		Metadata: map[string]string{"family": "gpt-5", "tier": "chat"},
	},

	// ---- GPT-4.1 family ----
	{
		Name: "gpt-4.1", Provider: ProviderName, DisplayName: "GPT-4.1",
		Capabilities:   capsChatVision,
		MaxInputTokens: 1047576, MaxOutputTokens: 32768,
		InputCostPerMTok: 2.0, OutputCostPerMTok: 8.0,
		Metadata: map[string]string{"family": "gpt-4.1"},
	},
	{
		Name: "gpt-4.1-mini", Provider: ProviderName, DisplayName: "GPT-4.1 mini",
		Capabilities:   capsChatVision,
		MaxInputTokens: 1047576, MaxOutputTokens: 32768,
		InputCostPerMTok: 0.40, OutputCostPerMTok: 1.60,
		Metadata: map[string]string{"family": "gpt-4.1", "tier": "mini"},
	},
	{
		Name: "gpt-4.1-nano", Provider: ProviderName, DisplayName: "GPT-4.1 nano",
		Capabilities:   capsChatVision,
		MaxInputTokens: 1047576, MaxOutputTokens: 32768,
		InputCostPerMTok: 0.10, OutputCostPerMTok: 0.40,
		Metadata: map[string]string{"family": "gpt-4.1", "tier": "nano"},
	},

	// ---- GPT-4o family ----
	{
		Name: "gpt-4o", Provider: ProviderName, DisplayName: "GPT-4o",
		Capabilities:   capsChatVision,
		MaxInputTokens: 128000, MaxOutputTokens: 16384,
		InputCostPerMTok: 2.50, OutputCostPerMTok: 10.0,
		Metadata: map[string]string{"family": "gpt-4o"},
	},
	{
		Name: "gpt-4o-mini", Provider: ProviderName, DisplayName: "GPT-4o mini",
		Capabilities:   capsChatVision,
		MaxInputTokens: 128000, MaxOutputTokens: 16384,
		InputCostPerMTok: 0.15, OutputCostPerMTok: 0.60,
		Metadata: map[string]string{"family": "gpt-4o", "tier": "mini"},
	},

	// ---- o-series reasoning ----
	{
		Name: "o3", Provider: ProviderName, DisplayName: "o3",
		Capabilities:   capsReasoningVision,
		MaxInputTokens: 200000, MaxOutputTokens: 100000,
		InputCostPerMTok: 2.0, OutputCostPerMTok: 8.0,
		Metadata: map[string]string{"family": "o-series", "reasoning": "true"},
	},
	{
		Name: "o4-mini", Provider: ProviderName, DisplayName: "o4-mini",
		Capabilities:   capsReasoningVision,
		MaxInputTokens: 200000, MaxOutputTokens: 100000,
		InputCostPerMTok: 1.10, OutputCostPerMTok: 4.40,
		Metadata: map[string]string{"family": "o-series", "tier": "mini", "reasoning": "true"},
	},
	{
		Name: "o3-mini", Provider: ProviderName, DisplayName: "o3-mini",
		Capabilities:   capsReasoningText,
		MaxInputTokens: 200000, MaxOutputTokens: 100000,
		InputCostPerMTok: 1.10, OutputCostPerMTok: 4.40,
		Metadata: map[string]string{"family": "o-series", "tier": "mini", "reasoning": "true"},
	},
	{
		Name: "o1", Provider: ProviderName, DisplayName: "o1",
		Capabilities:   capsReasoningVision,
		MaxInputTokens: 200000, MaxOutputTokens: 100000,
		InputCostPerMTok: 15.0, OutputCostPerMTok: 60.0,
		Metadata: map[string]string{"family": "o-series", "reasoning": "true"},
	},

	// ---- Embeddings ----
	// Embedding models are routed through golly's EmbeddingRouter, not the
	// generate-shaped ModelRouter. They advertise only CapEmbeddings.
	{
		Name: "text-embedding-3-large", Provider: ProviderName, DisplayName: "Text Embedding 3 Large",
		Capabilities:     []genai.Capability{genai.CapEmbeddings},
		MaxInputTokens:   8191,
		InputCostPerMTok: 0.13,
		Metadata:         map[string]string{"family": "text-embedding-3", "dimensions": "3072"},
	},
	{
		Name: "text-embedding-3-small", Provider: ProviderName, DisplayName: "Text Embedding 3 Small",
		Capabilities:     []genai.Capability{genai.CapEmbeddings},
		MaxInputTokens:   8191,
		InputCostPerMTok: 0.02,
		Metadata:         map[string]string{"family": "text-embedding-3", "dimensions": "1536"},
	},
	{
		Name: "text-embedding-ada-002", Provider: ProviderName, DisplayName: "Text Embedding Ada 002",
		Capabilities:     []genai.Capability{genai.CapEmbeddings},
		MaxInputTokens:   8191,
		InputCostPerMTok: 0.10,
		Metadata:         map[string]string{"family": "text-embedding-ada", "dimensions": "1536"},
	},
}

// buildCatalog returns the catalog advertised by a provider instance. When
// the instance is restricted to specific models (ProviderConfig.Models or
// Deployments), only those catalog entries are advertised, so the router
// never selects a model that has no deployment on this Azure resource.
// Otherwise the full catalog is returned.
func buildCatalog(models []string) []genai.ModelInfo {
	if len(models) == 0 {
		out := make([]genai.ModelInfo, len(azureCatalog))
		copy(out, azureCatalog)
		return out
	}
	out := make([]genai.ModelInfo, 0, len(models))
	for _, m := range models {
		if info, ok := catalogLookup(m); ok {
			info.Name = m
			out = append(out, info)
		}
	}
	return out
}

// catalogLookup finds a model in the static catalog by exact name.
func catalogLookup(model string) (genai.ModelInfo, bool) {
	for i := range azureCatalog {
		if azureCatalog[i].Name == model {
			return azureCatalog[i], true
		}
	}
	return genai.ModelInfo{}, false
}

// ModelCatalog returns metadata for every model this provider instance serves
// as a defensive copy. Implements genai.CapabilityProvider.
func (p *AzureOpenAIProvider) ModelCatalog() []genai.ModelInfo {
	out := make([]genai.ModelInfo, len(p.catalog))
	copy(out, p.catalog)
	return out
}

// ModelInfoFor returns metadata for a single model, if this instance serves
// it. Implements genai.CapabilityProvider.
func (p *AzureOpenAIProvider) ModelInfoFor(model string) (genai.ModelInfo, bool) {
	for i := range p.catalog {
		if p.catalog[i].Name == model {
			return p.catalog[i], true
		}
	}
	return genai.ModelInfo{}, false
}

// isReasoningModel reports whether model is a reasoning model (o-series,
// GPT-5 reasoning variants), which reject sampling parameters such as
// temperature and top_p. The catalog is authoritative; unknown names fall
// back to a prefix heuristic on the model name.
func (p *AzureOpenAIProvider) isReasoningModel(model string) bool {
	if info, ok := catalogLookup(model); ok {
		return info.Has(genai.CapReasoning)
	}
	m := strings.ToLower(model)
	if strings.Contains(m, "chat") {
		return false
	}
	for _, prefix := range []string{"o1", "o3", "o4", "gpt-5"} {
		if strings.HasPrefix(m, prefix) {
			return true
		}
	}
	return false
}
