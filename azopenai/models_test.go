package azopenai

import (
	"testing"

	"oss.nandlabs.io/golly/genai"
)

func newCatalogProvider(models ...string) *AzureOpenAIProvider {
	return newProvider(&ProviderConfig{Models: models}, nil, nil)
}

func TestModelCatalog_NonEmpty(t *testing.T) {
	p := newCatalogProvider()
	cat := p.ModelCatalog()
	if len(cat) != len(azureCatalog) || len(cat) == 0 {
		t.Fatalf("catalog has %d entries, want %d", len(cat), len(azureCatalog))
	}
	seen := map[string]bool{}
	for _, m := range cat {
		if m.Provider != ProviderName {
			t.Errorf("%s: Provider = %q", m.Name, m.Provider)
		}
		if m.Name == "" || m.DisplayName == "" || len(m.Capabilities) == 0 || m.MaxInputTokens <= 0 {
			t.Errorf("incomplete entry: %+v", m)
		}
		if seen[m.Name] {
			t.Errorf("duplicate model %s", m.Name)
		}
		seen[m.Name] = true
	}
	for _, want := range []string{"gpt-4o", "gpt-4o-mini", "gpt-4.1", "o3", "o4-mini", "gpt-5", "gpt-5-mini", "text-embedding-3-small", "text-embedding-3-large"} {
		if !seen[want] {
			t.Errorf("catalog missing %s", want)
		}
	}
}

func TestModelCatalog_ReturnsCopy(t *testing.T) {
	p := newCatalogProvider()
	cat := p.ModelCatalog()
	cat[0].Name = "mutated"
	if p.ModelCatalog()[0].Name == "mutated" || azureCatalog[0].Name == "mutated" {
		t.Error("ModelCatalog must return a defensive copy")
	}
}

func TestModelCatalog_RestrictedToDeployedModels(t *testing.T) {
	p := newProvider(&ProviderConfig{Deployments: map[string]string{"gpt-4o": "a", "my-finetune": "b"}}, nil, nil)
	cat := p.ModelCatalog()
	if len(cat) != 1 || cat[0].Name != "gpt-4o" {
		t.Fatalf("catalog = %+v, want only gpt-4o", cat)
	}
	if _, ok := p.ModelInfoFor("gpt-4.1"); ok {
		t.Error("undeployed model must not be advertised")
	}
}

func TestModelInfoFor(t *testing.T) {
	p := newCatalogProvider()
	info, ok := p.ModelInfoFor("gpt-4o")
	if !ok || !info.Has(genai.CapVision) || !info.Has(genai.CapToolCalling) || info.InputCostPerMTok <= 0 {
		t.Errorf("gpt-4o info = %+v, %v", info, ok)
	}
	if info, ok := p.ModelInfoFor("o3"); !ok || !info.Has(genai.CapReasoning) {
		t.Errorf("o3 info = %+v", info)
	}
	if _, ok := p.ModelInfoFor("nope"); ok {
		t.Error("unknown model must not resolve")
	}
}

func TestEmbeddingModels_OnlyEmbeddings(t *testing.T) {
	p := newCatalogProvider()
	for _, name := range []string{"text-embedding-3-small", "text-embedding-3-large", "text-embedding-ada-002"} {
		info, ok := p.ModelInfoFor(name)
		if !ok || len(info.Capabilities) != 1 || info.Capabilities[0] != genai.CapEmbeddings {
			t.Errorf("%s capabilities = %v", name, info.Capabilities)
		}
	}
}

func TestIsReasoningModel(t *testing.T) {
	p := newCatalogProvider()
	cases := map[string]bool{
		"gpt-4o": false, "gpt-5": true, "gpt-5-chat": false, "o3-mini": true,
		"o1-preview": true, "gpt-5-codex": true, "my-gpt-5-chat-latest": false, "custom-deployment": false,
	}
	for m, want := range cases {
		if got := p.isReasoningModel(m); got != want {
			t.Errorf("isReasoningModel(%q) = %v, want %v", m, got, want)
		}
	}
}

func TestCapabilityProvider_Conformance(t *testing.T) {
	var p genai.Provider = newCatalogProvider()
	if _, ok := p.(genai.CapabilityProvider); !ok {
		t.Fatal("AzureOpenAIProvider must implement genai.CapabilityProvider")
	}
}

// TestRouter_Integration builds a ProviderSet and ModelRegistry from the
// provider and checks a CapabilityStrategy ranks a reasoning task onto an
// Azure model.
func TestRouter_Integration(t *testing.T) {
	p := newCatalogProvider()
	set := genai.NewProviderSet(p)
	registry, err := genai.NewModelRegistry(set, genai.RoutingConfig{})
	if err != nil {
		t.Fatalf("NewModelRegistry: %v", err)
	}
	if got := len(registry.All()); got != len(p.ModelCatalog()) {
		t.Errorf("registry has %d models, want %d", got, len(p.ModelCatalog()))
	}
	if _, ok := registry.Get(ProviderName, "gpt-4.1"); !ok {
		t.Error("registry missing gpt-4.1")
	}
	ranked := genai.NewCapabilityStrategy().Rank(genai.Task{RequiredCapabilities: []genai.Capability{genai.CapReasoning, genai.CapVision}}, registry)
	if len(ranked) == 0 {
		t.Fatal("no candidates ranked for a reasoning+vision task")
	}
	for _, c := range ranked {
		info, ok := registry.Get(c.Provider, c.Model)
		if !ok || !info.Has(genai.CapReasoning) {
			t.Errorf("ranked %s/%s lacks reasoning", c.Provider, c.Model)
		}
	}
}
