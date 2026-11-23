# azopenai

Azure OpenAI / Azure AI Foundry implementation of the
[golly genai](https://pkg.go.dev/oss.nandlabs.io/golly/genai) `Provider`,
`Embedder` and `CapabilityProvider` interfaces.

> Uses the deployment-scoped **Chat Completions** and **Embeddings** APIs via
> [`openai-go/v3`](https://github.com/openai/openai-go) and its `azure`
> subpackage. Works with Azure OpenAI resources (`*.openai.azure.com`) and
> Azure AI Foundry resources (`*.services.ai.azure.com`).

---

- [Installation](#installation)
- [Features](#features)
- [Architecture](#architecture)
- [Configuration](#configuration)
- [Deployments vs. Model Names](#deployments-vs-model-names)
- [Usage](#usage)
- [Model Router / Capabilities](#model-router--capabilities)
- [Supported Content Types](#supported-content-types)
- [Options](#options)
- [Structured Output](#structured-output)
- [Tool Use (Function Calling)](#tool-use-function-calling)
- [Streaming](#streaming)
- [Embeddings](#embeddings)
- [Error Handling](#error-handling)
- [API Reference](#api-reference)
- [Prerequisites](#prerequisites)
- [Contributing](#contributing)

---

## Installation

```bash
go get oss.nandlabs.io/golly-azure/azopenai
```

## Features

- **Generate** — synchronous inference via Chat Completions
- **GenerateStream** — SSE streaming with incremental text, assembled tool calls and trailing usage
- **Deployment mapping** — stable golly model names (`gpt-4o`) mapped to Azure deployment names
- **Text, images, audio and PDF** — images (inline bytes or URL), WAV/MP3 input audio, PDF files
- **Tool use** — function declarations, `tool_choice` (auto / none / required / named), parallel tool calls
- **Structured output** — JSON-schema `response_format` (optionally strict) and JSON mode
- **Reasoning models** — o-series and GPT-5: `reasoning_effort`, `max_completion_tokens`, sampling params dropped automatically
- **System prompts** — via options or system-role messages
- **Token usage** — input, output, total, cached and reasoning tokens, latency and time-to-first-token
- **Embeddings** — batched `text-embedding-3-*` / `ada-002` with optional `dimensions`
- **Model catalog** — `genai.CapabilityProvider` for the golly v1.8 model router
- **Typed errors** — `*APIError` with sentinel matching and `genai.RetryableError` for router fallover
- **Config resolution** — Microsoft Entra ID credentials via `azcfg` (DefaultAzureCredential, service principal, managed identity) or API key

## Architecture

```
┌──────────────────────────────────────────────────────────────┐
│  Application / golly genai Router                            │
│                                                              │
│  genai.Provider · genai.Embedder · genai.CapabilityProvider  │
│  ┌────────────────────────────────────────────────────────┐  │
│  │  azopenai.AzureOpenAIProvider                          │  │
│  │                                                        │  │
│  │  model name ──Deployments──▶ deployment name           │  │
│  │                                                        │  │
│  │  ┌───────────────────┐   ┌──────────────────────────┐  │  │
│  │  │ chatAPI           │   │ embeddingAPI             │  │  │
│  │  │ • New             │   │ • New                    │  │  │
│  │  │ • NewStreaming    │   │                          │  │  │
│  │  └───────────────────┘   └──────────────────────────┘  │  │
│  │  utils.go (convert) · models.go (catalog)              │  │
│  │  errors.go (APIError) · pkg.go (azcfg, client options) │  │
│  └────────────────────────────────────────────────────────┘  │
│                              │ openai-go/v3 + azure           │
│                              ▼                               │
│   https://<resource>.openai.azure.com/openai/deployments/    │
│        {deployment}/chat/completions | /embeddings           │
└──────────────────────────────────────────────────────────────┘
```

## Configuration

### Using `ProviderConfig`

```go
provider, err := azopenai.NewAzureOpenAIProvider(&azopenai.ProviderConfig{
    // Resource endpoint (or set azcfg.Config.Endpoint / AZURE_OPENAI_ENDPOINT)
    Endpoint: "https://my-resource.openai.azure.com",

    // Use a named azcfg configuration for Entra ID credentials...
    CfgName: "my-azure",
    // ...or an explicit one
    // Config: cfg,
    // ...or an API key instead of Entra ID
    // APIKey: os.Getenv("AZURE_OPENAI_API_KEY"),

    // golly model name -> Azure deployment name
    Deployments: map[string]string{
        "gpt-4o":                 "prod-gpt4o",
        "o4-mini":                "reasoning",
        "text-embedding-3-small": "embeddings",
    },
    EmbeddingModel: "text-embedding-3-small", // default for Embed

    APIVersion: "2025-04-01-preview", // optional; default DefaultAPIVersion
})
```

| Field             | Description                                                                 |
| ----------------- | --------------------------------------------------------------------------- |
| `CfgName`         | Named `azcfg.Config` from `azcfg.Manager`                                   |
| `Config`          | Explicit `*azcfg.Config` (overrides `CfgName`)                              |
| `Endpoint`        | Resource endpoint; a trailing `/openai` or `/openai/v1` is stripped         |
| `APIVersion`      | `api-version` query parameter                                               |
| `APIKey`          | Use `Api-Key` auth instead of Entra ID                                      |
| `TokenScopes`     | Override the Entra scope (default `https://cognitiveservices.azure.com/.default`) |
| `Deployments`     | Model name → deployment name                                                |
| `Models`          | Models served; defaults to the `Deployments` keys                           |
| `EmbeddingModel`  | Default embedding model when `EmbedRequest.Model` is empty                  |
| `LegacyMaxTokens` | Send `max_tokens` instead of `max_completion_tokens`                        |
| `HTTPClient`      | Custom `*http.Client`                                                       |
| `MaxRetries`      | SDK retry count (default 2); `0` lets the golly router handle fallover      |
| `RequestTimeout`  | Per-attempt timeout                                                         |
| `UnsafeAllowHTTP` | Allow plaintext HTTP to loopback endpoints (local dev/tests only)           |
| `RequestOptions`  | Extra `openai-go` `option.RequestOption`s                                   |

### Configuration Resolution

**Credentials** (when `APIKey` is empty):

1. **Explicit `Config`** — `Config.Credential()`
2. **Named config (`CfgName`)** — from `azcfg.Manager`
3. **`"default"` registered config**
4. **`azcfg.NewConfig()`** — `DefaultAzureCredential` (env, workload identity, managed identity, Azure CLI)

**Endpoint:** `ProviderConfig.Endpoint` → `azcfg.Config.Endpoint` → `AZURE_OPENAI_ENDPOINT`.

**API version:** `ProviderConfig.APIVersion` → `AZURE_OPENAI_API_VERSION` → `OPENAI_API_VERSION` → `DefaultAPIVersion`.

Entra ID callers need the **Cognitive Services OpenAI User** role on the resource.

## Deployments vs. Model Names

Azure addresses models by **deployment name**, which is chosen per resource.
The provider keeps golly-facing names stable (they match the model catalog and
the router) and translates them at request time:

```go
p.Generate(ctx, "gpt-4o", msg, opts) // POST /openai/deployments/prod-gpt4o/chat/completions
p.Deployment("gpt-4o")                // "prod-gpt4o"
p.Deployment("unmapped")              // "unmapped" (used verbatim)
```

When `Models` or `Deployments` is set, `ModelCatalog()` advertises only those
models, so the router never picks a model that has no deployment on the
resource. Name deployments after the model (or map them) so the provider can
recognise reasoning models.

## Usage

### Basic Text Generation

```go
package main

import (
    "context"
    "fmt"
    "log"

    "oss.nandlabs.io/golly-azure/azopenai"
    "oss.nandlabs.io/golly/genai"
)

func main() {
    provider, err := azopenai.NewAzureOpenAIProvider(&azopenai.ProviderConfig{
        Endpoint:    "https://my-resource.openai.azure.com",
        Deployments: map[string]string{"gpt-4o": "prod-gpt4o"},
    })
    if err != nil {
        log.Fatal(err)
    }
    defer provider.Close()

    msg := genai.NewTextMessage(genai.RoleUser, "What is Azure OpenAI?")
    options := genai.NewOptionsBuilder().
        SetMaxTokens(1024).
        SetTemperature(0.7).
        Add(genai.OptionSystemInstructions, "You are a helpful assistant.").
        Build()

    resp, err := provider.Generate(context.Background(), "gpt-4o", msg, options)
    if err != nil {
        log.Fatal(err)
    }
    for _, candidate := range resp.Candidates {
        for _, part := range candidate.Message.Parts {
            if part.Text != nil {
                fmt.Println(part.Text.Content)
            }
        }
    }
    fmt.Printf("Tokens: in=%d out=%d total=%d cached=%d\n",
        resp.Meta.InputTokens, resp.Meta.OutputTokens, resp.Meta.TotalTokens, resp.Meta.CachedTokens)
}
```

A full walkthrough lives in [`examples/azopenai`](../examples/azopenai/main.go).

## Model Router / Capabilities

The provider implements `genai.CapabilityProvider` (golly ≥ v1.8.0).
`ModelCatalog()` covers GPT-5 / 5.1 (incl. mini, nano, chat), GPT-4.1 (incl.
mini, nano), GPT-4o / 4o-mini, o1, o3, o3-mini, o4-mini and the
`text-embedding-3-*` / `ada-002` embedding models, each with capabilities
(text, chat, streaming, vision, tool calling, JSON mode, reasoning,
embeddings), context window, max output and Global Standard list pricing
(as of 2026-07). `ModelInfoFor(name)` looks up a single model.

```go
provider, _ := azopenai.NewAzureOpenAIProvider(&azopenai.ProviderConfig{
    Deployments: map[string]string{"gpt-4.1": "gpt41", "o4-mini": "o4mini"},
})
set := genai.NewProviderSet(provider)
registry, _ := genai.NewModelRegistry(set, genai.RoutingConfig{}) // reads ModelCatalog()
router, _ := genai.NewRouter(
    genai.WithProviders(set),
    genai.WithRegistry(registry),
    genai.WithStrategy(genai.NewCapabilityStrategy()),
)
task := genai.Task{RequiredCapabilities: []genai.Capability{genai.CapReasoning}}
```

Override pricing (e.g. Data Zone or PTU) or disable models through
`genai.RoutingConfig`.

## Supported Content Types

| Part                                  | Sent as                                   |
| ------------------------------------- | ----------------------------------------- |
| `Text`                                | text content part                         |
| `Bin` image (PNG/JPEG/GIF/WebP)       | `image_url` with a base64 data URL        |
| `File` image (https or `data:` URL)   | `image_url`                               |
| `Bin` `audio/wav`, `audio/mpeg`       | `input_audio` (audio-capable deployments) |
| `Bin` `application/pdf`               | `file` with `file_data`                   |
| `Bin` `text/*`, `application/json`    | text content                              |
| other `File`                          | text reference `[File: <uri>]`            |
| other `Bin`                           | `genai.GetUnsupportedMimeErr`             |

Set `Attributes["detail"]` (`low` / `high` / `auto`) on an image part to
control vision detail.

## Options

| Option                | Builder / key                         | Notes                                        |
| --------------------- | ------------------------------------- | -------------------------------------------- |
| `max_tokens`          | `SetMaxTokens(n)`                     | sent as `max_completion_tokens`              |
| `temperature`         | `SetTemperature(f)`                   | dropped for reasoning models                 |
| `top_p`               | `SetTopP(f)`                          | dropped for reasoning models                 |
| `presence_penalty`    | `SetPresencePenalty(f)`               | dropped for reasoning models                 |
| `frequency_penalty`   | `SetFrequencyPenalty(f)`              | dropped for reasoning models                 |
| `log_probs`           | `SetLogProbs(b)`                      | dropped for reasoning models                 |
| `candidate_count`     | `SetCandidateCount(n)`                | `n`                                          |
| `seed`                | `SetSeed(n)`                          |                                              |
| `stop_words`          | `SetStopWords(s...)`                  | up to 4                                      |
| `system_instructions` | `SetSystemInstructions(s)`            | system message                               |
| `output_mime`         | `SetOutputMime("application/json")`   | JSON mode                                    |
| `schema`              | `SetSchema(schema)`                   | JSON-schema structured output                |
| `tools`/`tool_choice` | `SetTools(...)`, `SetToolChoice(...)` | function calling                             |
| `reasoning_effort`    | `Add(azopenai.OptionReasoningEffort, "high")` | o-series / GPT-5                     |
| `parallel_tool_calls` | `Add(azopenai.OptionParallelToolCalls, false)` |                                     |
| `user`                | `Add(azopenai.OptionUser, "id")`      | end-user id                                  |
| `json_schema_strict`  | `Add(azopenai.OptionStrictSchema, true)` | strict schema adherence                   |
| `json_schema_name`    | `Add(azopenai.OptionSchemaName, "x")` | defaults to schema Title, then `response`    |

## Structured Output

```go
schema := &data.Schema{
    Type: "object", Title: "country",
    Properties: map[string]*data.Schema{
        "name":    {Type: "string"},
        "capital": {Type: "string"},
    },
    Required: []string{"name", "capital"},
}
opts := genai.NewOptionsBuilder().
    SetSchema(schema).
    Add(azopenai.OptionStrictSchema, true). // injects additionalProperties:false
    Build()
resp, _ := provider.Generate(ctx, "gpt-4o", genai.NewTextMessage(genai.RoleUser, "Describe Japan"), opts)
```

Without a schema, `SetOutputMime("application/json")` enables JSON mode
(`json_object`); the prompt must mention "JSON".

## Tool Use (Function Calling)

### Declaring Tools

```go
opts := genai.NewOptionsBuilder().
    SetTools(genai.Tool{Function: &genai.FunctionDecl{
        Name:        "get_weather",
        Description: "Get the weather for a city",
        Parameters:  citySchema,
    }}).
    SetToolChoice(genai.NewToolChoice(genai.ToolChoiceAuto)).
    Build()
```

### Receiving a Tool Call

```go
for _, c := range resp.Candidates {
    if c.FinishReason == genai.FinishReasonToolCall {
        for _, part := range c.Message.Parts {
            if part.FuncCall != nil {
                fmt.Println(part.FuncCall.Id, part.FuncCall.FunctionName, part.FuncCall.Arguments)
            }
        }
    }
}
```

Arguments that are not valid JSON are kept in `Attributes["raw_arguments"]`.

### Sending a Tool Result

`FuncResponse` parts in a user (or function-role) message become `tool`
messages; `Part.Name` carries the `tool_call_id`:

```go
result := `{"temperature":"22C"}`
msg := &genai.Message{Role: genai.RoleUser, Parts: []genai.Part{
    {Name: "call_abc123", FuncResponse: &genai.FuncResponsePart{Text: &result}},
}}
```

Assistant messages with `FuncCall` parts are sent back as `tool_calls`.

## Streaming

`GenerateStream` returns two channels:

| Channel               | Type             | Description                                 |
| --------------------- | ---------------- | ------------------------------------------- |
| `<-chan *GenResponse` | response channel | Text deltas, final events, usage            |
| `<-chan error`        | error channel    | API or stream errors (at most one)          |

| Event                       | Content                                                            |
| --------------------------- | ------------------------------------------------------------------ |
| Text delta                  | Partial text, `FinishReasonInProgress`                             |
| Final event (per candidate) | Assembled `FuncCall` parts (if any) and the mapped `FinishReason`  |
| Usage                       | `Meta` token counts and `TotalTime` (`stream_options.include_usage`) |
| Azure prompt-filter preamble, role-only deltas, tool-call fragments | Skipped              |

`Meta.TimeToFirst` is set on the first content event.

## Embeddings

```go
resp, err := provider.Embed(ctx, &genai.EmbedRequest{
    Model: "text-embedding-3-small",
    Inputs: []genai.Part{
        {Text: &genai.TextPart{Content: "hello"}, Attributes: map[string]any{"dimensions": 256}},
        {Text: &genai.TextPart{Content: "world"}},
    },
})
// resp.Vectors[i] is []float32, in input order; resp.Meta.InputTokens has usage.
```

All inputs go in one request. Only text parts are supported.

## Error Handling

Non-2xx responses are returned as `*azopenai.APIError` (status, code, message,
deployment), which matches sentinel errors via `errors.Is` and implements
`genai.RetryableError` so the golly router only falls over on transient
failures:

| Sentinel                   | Condition                                   | Retryable |
| -------------------------- | ------------------------------------------- | --------- |
| `ErrRateLimited`           | 429                                         | yes       |
| `ErrServiceUnavailable`    | 5xx, 408                                    | yes       |
| `ErrBadRequest`            | 400, 422                                    | no        |
| `ErrContentFiltered`       | code `content_filter`                       | no        |
| `ErrContextLengthExceeded` | code `context_length_exceeded`              | no        |
| `ErrUnauthorized`          | 401                                         | no        |
| `ErrForbidden`             | 403                                         | no        |
| `ErrDeploymentNotFound`    | 404                                         | no        |

```go
_, err := provider.Generate(ctx, "gpt-4o", msg, opts)
var apiErr *azopenai.APIError
switch {
case errors.Is(err, azopenai.ErrRateLimited):
    // back off
case errors.As(err, &apiErr):
    log.Printf("status=%d code=%s", apiErr.StatusCode, apiErr.Code)
}
```

Responses blocked at output time come back as a candidate with
`FinishReasonContentFilter`; model refusals are text parts with
`Attributes["refusal"] = true`.

## API Reference

### Types

| Type                  | Description                                                       |
| --------------------- | ----------------------------------------------------------------- |
| `AzureOpenAIProvider` | Implements `genai.Provider`, `genai.Embedder`, `genai.CapabilityProvider` |
| `ProviderConfig`      | Configuration for creating a provider instance                    |
| `APIError`            | Typed service error; implements `genai.RetryableError`            |

### Constants

| Constant            | Value                | Description               |
| ------------------- | -------------------- | ------------------------- |
| `ProviderName`      | `azure-openai`       | Provider name             |
| `ProviderVersion`   | `1.0.0`              | Provider version          |
| `DefaultMaxTokens`  | `4096`               | Default max tokens        |
| `DefaultAPIVersion` | `2025-04-01-preview` | Default data-plane version |

### Methods

| Method                                                | Description                         |
| ----------------------------------------------------- | ----------------------------------- |
| `NewAzureOpenAIProvider(config) (*Provider, error)`   | Create a new provider               |
| `Name() / Description() / Version()`                  | Provider metadata                   |
| `Models() []string`                                   | Configured model names              |
| `Deployment(model) string`                            | Deployment name for a model         |
| `Generate(ctx, model, msg, opts) (*GenResponse, err)` | Synchronous generation              |
| `GenerateStream(ctx, model, msg, opts)`               | Streaming generation                |
| `Embed(ctx, req) (*EmbedResponse, error)`             | Batch embeddings                    |
| `ModelCatalog() / ModelInfoFor(model)`                | Model metadata for the router       |
| `Close() error`                                       | No-op                               |

### Finish Reasons

| `finish_reason`  | `genai.FinishReason` |
| ---------------- | -------------------- |
| `stop`           | `Stop`               |
| `length`         | `Length`             |
| `tool_calls`     | `ToolCall`           |
| `function_call`  | `FunctionCall`       |
| `content_filter` | `ContentFilter`      |
| *(streaming, none yet)* | `InProgress`  |

## Prerequisites

- Go 1.26+
- An Azure OpenAI or Azure AI Foundry resource with model deployments
- Either an API key or an Entra ID identity with the *Cognitive Services OpenAI User* role

## Contributing

See [CONTRIBUTING.md](../CONTRIBUTING.md) for guidelines.
