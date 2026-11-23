// Command azopenai demonstrates the golly genai provider for Azure OpenAI /
// Azure AI Foundry.
//
// Environment:
//
//	AZURE_OPENAI_ENDPOINT              (required) e.g. https://<resource>.openai.azure.com
//	AZURE_OPENAI_API_KEY               (optional) API key; when unset Microsoft Entra ID
//	                                   (DefaultAzureCredential) is used
//	AZURE_OPENAI_MODEL                 golly model name (default gpt-4o-mini)
//	AZURE_OPENAI_DEPLOYMENT            deployment name for the model (default = model)
//	AZURE_OPENAI_EMBEDDING_DEPLOYMENT  optional embedding deployment; enables step 11
//	AZURE_OPENAI_API_VERSION           optional data-plane api-version
package main

import (
	"context"
	"errors"
	"fmt"
	"log"
	"os"

	"oss.nandlabs.io/golly-azure/azcfg"
	"oss.nandlabs.io/golly-azure/azopenai"
	"oss.nandlabs.io/golly/data"
	"oss.nandlabs.io/golly/genai"
)

func main() {
	endpoint := os.Getenv("AZURE_OPENAI_ENDPOINT")
	apiKey := os.Getenv("AZURE_OPENAI_API_KEY")
	model := envOrDefault("AZURE_OPENAI_MODEL", "gpt-4o-mini")
	deployment := envOrDefault("AZURE_OPENAI_DEPLOYMENT", model)
	embedDeployment := os.Getenv("AZURE_OPENAI_EMBEDDING_DEPLOYMENT")

	if endpoint == "" {
		log.Fatal("AZURE_OPENAI_ENDPOINT is required")
	}

	fmt.Println("=== golly Azure OpenAI Example ===")
	fmt.Printf("Endpoint: %s | Model: %s | Deployment: %s\n\n", endpoint, model, deployment)

	// Step 1: Configure Azure credentials via azcfg
	step("1. Configure Azure credentials")
	cfg := azcfg.NewConfig()
	cfg.SetEndpoint(endpoint)
	if apiKey != "" {
		fmt.Println("   Using API key authentication")
	} else {
		fmt.Println("   Using Microsoft Entra ID (DefaultAzureCredential)")
	}

	// Step 2: Create the provider, mapping the golly model name to the deployment
	step("2. Create Azure OpenAI provider")
	deployments := map[string]string{model: deployment}
	embedModel := ""
	if embedDeployment != "" {
		embedModel = "text-embedding-3-small"
		deployments[embedModel] = embedDeployment
	}
	provider, err := azopenai.NewAzureOpenAIProvider(&azopenai.ProviderConfig{
		Config:         cfg,
		APIKey:         apiKey,
		Deployments:    deployments,
		Models:         []string{model},
		EmbeddingModel: embedModel,
	})
	check(err, "NewAzureOpenAIProvider")
	fmt.Printf("   Provider: %s v%s\n", provider.Name(), provider.Version())
	fmt.Printf("   Description: %s\n", provider.Description())
	fmt.Printf("   Models: %v\n", provider.Models())
	if info, ok := provider.ModelInfoFor(model); ok {
		fmt.Printf("   Catalog: %s caps=%v ctx=%d\n", info.DisplayName, info.Capabilities, info.MaxInputTokens)
	}

	ctx := context.Background()

	// Step 3: Simple text generation
	step("3. Simple text generation")
	message := genai.NewTextMessage(genai.RoleUser, "What is the capital of France? Answer in one sentence.")
	options := genai.NewOptionsBuilder().
		SetMaxTokens(256).
		SetTemperature(0.3).
		Build()
	resp, err := provider.Generate(ctx, model, message, options)
	check(err, "Generate")
	printResponse("Simple", resp)

	// Step 4: Generation with system instructions
	step("4. Generation with system instructions")
	message2 := genai.NewTextMessage(genai.RoleUser, "Explain quantum entanglement.")
	options2 := genai.NewOptionsBuilder().
		SetMaxTokens(512).
		SetTemperature(0.5).
		Add(genai.OptionSystemInstructions, "You are a science teacher. Explain concepts simply, suitable for a 10-year-old. Use analogies.").
		Build()
	resp2, err := provider.Generate(ctx, model, message2, options2)
	check(err, "Generate with system instructions")
	printResponse("System Instructions", resp2)

	// Step 5: Structured output (JSON schema)
	step("5. Structured output (JSON schema)")
	schema := &data.Schema{
		Type:  "object",
		Title: "country",
		Properties: map[string]*data.Schema{
			"name":       {Type: "string"},
			"capital":    {Type: "string"},
			"population": {Type: "integer"},
		},
		Required: []string{"name", "capital", "population"},
	}
	message3 := genai.NewTextMessage(genai.RoleUser, "Describe Japan.")
	options3 := genai.NewOptionsBuilder().
		SetMaxTokens(256).
		SetSchema(schema).
		Add(azopenai.OptionStrictSchema, true).
		Build()
	resp3, err := provider.Generate(ctx, model, message3, options3)
	check(err, "Generate JSON")
	printResponse("JSON Output", resp3)

	// Step 6: Streaming generation
	step("6. Streaming generation")
	message4 := genai.NewTextMessage(genai.RoleUser, "Write a haiku about cloud computing.")
	options4 := genai.NewOptionsBuilder().
		SetMaxTokens(128).
		SetTemperature(0.7).
		Build()
	responseChan, errorChan := provider.GenerateStream(ctx, model, message4, options4)
	fmt.Print("   Stream: ")
	for resp := range responseChan {
		for _, c := range resp.Candidates {
			if c.Message != nil {
				for _, p := range c.Message.Parts {
					if p.Text != nil {
						fmt.Print(p.Text.Content)
					}
				}
			}
			if c.FinishReason != genai.FinishReasonInProgress && c.FinishReason != "" {
				fmt.Printf("\n   Finish reason: %s\n", c.FinishReason)
			}
		}
		if resp.Meta.InputTokens > 0 || resp.Meta.OutputTokens > 0 {
			fmt.Printf("   Tokens: input=%d, output=%d, total=%d\n",
				resp.Meta.InputTokens, resp.Meta.OutputTokens, resp.Meta.TotalTokens)
		}
	}
	if err := <-errorChan; err != nil {
		fmt.Printf("   Stream error: %v\n", err)
	}
	fmt.Println()

	// Step 7: Generation with stop sequences
	step("7. Generation with stop sequences")
	message5 := genai.NewTextMessage(genai.RoleUser, "Count from 1 to 20, one per line.")
	options5 := genai.NewOptionsBuilder().
		SetMaxTokens(512).
		SetTemperature(0.0).
		SetStopWords("10").
		Build()
	resp5, err := provider.Generate(ctx, model, message5, options5)
	check(err, "Generate with stop sequences")
	printResponse("Stop Sequences", resp5)

	// Step 8: Tool calling round trip
	step("8. Tool calling")
	weatherTool := genai.Tool{Function: &genai.FunctionDecl{
		Name:        "get_weather",
		Description: "Get the current weather for a city",
		Parameters: &data.Schema{
			Type:       "object",
			Properties: map[string]*data.Schema{"city": {Type: "string"}},
			Required:   []string{"city"},
		},
	}}
	toolOpts := genai.NewOptionsBuilder().
		SetMaxTokens(256).
		SetTools(weatherTool).
		SetToolChoice(genai.NewToolChoice(genai.ToolChoiceAuto)).
		Build()
	resp6, err := provider.Generate(ctx, model, genai.NewTextMessage(genai.RoleUser, "What's the weather in Paris?"), toolOpts)
	check(err, "Generate with tools")
	for _, c := range resp6.Candidates {
		for _, p := range c.Message.Parts {
			if p.FuncCall == nil {
				continue
			}
			fmt.Printf("   Tool call: %s(%v) id=%s\n", p.FuncCall.FunctionName, p.FuncCall.Arguments, p.FuncCall.Id)
			// Reply with the tool result; Part.Name carries the tool_call_id.
			result := `{"temperature":"21C","condition":"sunny"}`
			followUp := &genai.Message{Role: genai.RoleUser, Parts: []genai.Part{
				{Name: p.FuncCall.Id, FuncResponse: &genai.FuncResponsePart{Text: &result}},
			}}
			fmt.Printf("   Tool result message prepared for %s: %d part(s)\n", p.FuncCall.Id, len(followUp.Parts))
		}
		fmt.Printf("   Finish: %s\n", c.FinishReason)
	}

	// Step 9: Multipart message
	step("9. Multipart message (multiple text parts)")
	msg := &genai.Message{
		Role: genai.RoleUser,
		Parts: []genai.Part{
			{Name: "context", MimeType: "text/plain", Text: &genai.TextPart{Content: "The Eiffel Tower was built for the 1889 World's Fair in Paris."}},
			{Name: "question", MimeType: "text/plain", Text: &genai.TextPart{Content: "When was the Eiffel Tower built and for what event?"}},
		},
	}
	resp7, err := provider.Generate(ctx, model, msg, genai.NewOptionsBuilder().SetMaxTokens(256).Build())
	check(err, "Generate multipart")
	printResponse("Multipart", resp7)

	// Step 10: Error handling
	step("10. Error handling (unknown deployment)")
	_, err = provider.Generate(ctx, "does-not-exist", genai.NewTextMessage(genai.RoleUser, "hi"), nil)
	if err != nil {
		fmt.Printf("   Error: %v\n", err)
		var apiErr *azopenai.APIError
		if errors.As(err, &apiErr) {
			fmt.Printf("   Status=%d Code=%s Retryable=%v\n", apiErr.StatusCode, apiErr.Code, apiErr.Retryable())
		}
	}

	// Step 11: Embeddings
	if embedModel != "" {
		step("11. Embeddings")
		eresp, err := provider.Embed(ctx, &genai.EmbedRequest{
			Model: embedModel,
			Inputs: []genai.Part{
				{Text: &genai.TextPart{Content: "golly is a Go library"}},
				{Text: &genai.TextPart{Content: "Azure OpenAI hosts GPT models"}},
			},
		})
		check(err, "Embed")
		for i, v := range eresp.Vectors {
			fmt.Printf("   Vector %d: dims=%d\n", i, len(v))
		}
		if eresp.Meta != nil {
			fmt.Printf("   Tokens: %d\n", eresp.Meta.TotalTokens)
		}
	}

	// Step 12: Close provider
	step("12. Close provider")
	check(provider.Close(), "Close")
	fmt.Println("   Provider closed")

	fmt.Println("\n=== Example Complete ===")
}

// printResponse prints a GenResponse with candidate text and metadata.
func printResponse(label string, resp *genai.GenResponse) {
	if resp == nil {
		fmt.Printf("   [%s] No response\n", label)
		return
	}
	for _, c := range resp.Candidates {
		if c.Message != nil {
			for _, p := range c.Message.Parts {
				if p.Text != nil {
					text := p.Text.Content
					if len(text) > 300 {
						text = text[:300] + "..."
					}
					fmt.Printf("   Response: %s\n", text)
				}
			}
		}
		fmt.Printf("   Finish: %s\n", c.FinishReason)
	}
	fmt.Printf("   Tokens: input=%d, output=%d, total=%d\n",
		resp.Meta.InputTokens, resp.Meta.OutputTokens, resp.Meta.TotalTokens)
	if resp.Meta.CachedTokens > 0 {
		fmt.Printf("   Cached: %d\n", resp.Meta.CachedTokens)
	}
	if resp.Meta.ThinkingTokens > 0 {
		fmt.Printf("   Reasoning: %d\n", resp.Meta.ThinkingTokens)
	}
	if resp.Meta.TotalTime > 0 {
		fmt.Printf("   Latency: %dms\n", resp.Meta.TotalTime)
	}
}

func step(name string) {
	fmt.Printf("\n-- %s --\n", name)
}

func check(err error, action string) {
	if err != nil {
		log.Fatalf("FATAL: %s: %v", action, err)
	}
}

func envOrDefault(key, fallback string) string {
	if v := os.Getenv(key); v != "" {
		return v
	}
	return fallback
}
