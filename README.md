<h1 align="center">Golly Azure</h1>

<p align="center">
  <strong>Microsoft Azure service integrations for the <a href="https://github.com/nandlabs/golly">Golly</a> ecosystem</strong>
</p>

<p align="center">
  <a href="https://goreportcard.com/report/oss.nandlabs.io/golly-azure"><img src="https://img.shields.io/badge/go%20report-A+-brightgreen.svg?style=flat" alt="Go Report"></a>
  <a href="https://github.com/nandlabs/golly-azure/actions?query=event%3Apush+branch%3Amain+"><img src="https://img.shields.io/github/actions/workflow/status/nandlabs/golly-azure/go_ci.yml?branch=main&event=push&color=228B22" alt="Build Status"></a>
  <a href="https://github.com/nandlabs/golly-azure/releases/latest"><img src="https://img.shields.io/github/v/release/nandlabs/golly-azure?label=latest&color=228B22" alt="Release"></a>
  <a href="https://github.com/nandlabs/golly-azure/releases/latest"><img src="https://img.shields.io/github/release-date/nandlabs/golly-azure?label=released&color=00ADD8" alt="Release Date"></a>
  <a href="https://pkg.go.dev/oss.nandlabs.io/golly-azure"><img src="https://godoc.org/oss.nandlabs.io/golly-azure?status.svg" alt="GoDoc"></a>
  <a href="https://github.com/nandlabs/golly-azure/blob/main/LICENSING.md"><img src="https://img.shields.io/github/license/nandlabs/golly-azure?color=blue" alt="License"></a>
</p>

<p align="center">
  <a href="#installation">Installation</a> •
  <a href="#packages">Packages</a> •
  <a href="#testing">Testing</a> •
  <a href="#contributing">Contributing</a>
</p>

---

## Overview

Golly Azure provides Microsoft Azure implementations for the full set of [Golly](https://github.com/nandlabs/golly) capability interfaces — VFS, Messaging, Cache, Secrets, GenAI, Auth, Authz, Vectorstore, and Chrono leader election. It uses the official [Azure SDK for Go](https://github.com/Azure/azure-sdk-for-go) (plus [openai-go](https://github.com/openai/openai-go) for Azure OpenAI) and follows Golly's provider pattern: blank-import a package to auto-register it, then use standard Golly managers with `azblob://` or `servicebus://` URLs.

It is the Azure counterpart of [golly-aws](https://github.com/nandlabs/golly-aws) and [golly-gcp](https://github.com/nandlabs/golly-gcp):

| Capability          | golly-aws                     | golly-gcp                    | golly-azure                                |
| ------------------- | ----------------------------- | ---------------------------- | ------------------------------------------ |
| Config              | `awscfg`                      | `gcpsvc`                     | `azcfg`                                    |
| VFS                 | `s3`                          | `gs`                         | `blob`                                     |
| Messaging           | `sqs`, `sns`                  | `pubsub`                     | `servicebus`                               |
| Secrets             | `secrets` (Secrets Manager)   | `secrets` (Secret Manager)   | `secrets` (Key Vault)                      |
| GenAI               | `bedrock`                     | `genai`                      | `azopenai`                                 |
| Auth                | `auth/cognito`                | `auth/firebase`              | `auth/entra`                               |
| Authz               | `authz/awsiam`                | `authz/gcpiam`               | `authz/azrbac`                             |
| Cache               | `cache/elasticache`           | `cache/memorystore`          | `cache/azredis`                            |
| Leader election     | `chrono/dynamolock`, `s3lock` | `chrono/firestorelock`, `gcslock` | `chrono/tablelock`, `bloblock`        |
| Vectorstore         | `opensearch`, `bedrockkb`     | `vertex`, `alloydb`          | `aisearch`, `cosmosdb`                     |

## Compatibility

| golly-azure | golly  | azcore   | azidentity |
| ----------- | ------ | -------- | ---------- |
| v0.1.0      | v1.8.1 | v1.24.0  | v1.15.0    |

## Installation

```bash
go get oss.nandlabs.io/golly-azure@latest
```

## Packages

### ⚙️ Configuration

| Package                  | Description                                                                                                                                   |
| ------------------------ | --------------------------------------------------------------------------------------------------------------------------------------------- |
| [azcfg](azcfg/README.md) | Centralized Azure config with named registry and URL-based resolution — `DefaultAzureCredential`, service principal, managed identity, connection strings, sovereign clouds |

### 🔐 Auth & Authorization

| Package                                | Description                                                                                                                             |
| -------------------------------------- | --------------------------------------------------------------------------------------------------------------------------------------- |
| [auth/entra](auth/entra/README.md)     | Microsoft Entra ID JWT verifier (v1/v2 tokens, single- or multi-tenant allow-list, JWKS-cached RS256) + Azure Table Storage `auth.SessionStore` |
| [authz/azrbac](authz/azrbac/README.md) | Azure RBAC `authz.Policy` — cached role-assignment / role-definition / deny-assignment evaluator + `RBACResource{Ctx,Scope}` live permissions check |

### 🤖 AI & Intelligence

| Package                                                   | Description                                                                                                                                         |
| --------------------------------------------------------- | --------------------------------------------------------------------------------------------------------------------------------------------------- |
| [azopenai](azopenai/README.md)                            | Azure OpenAI / AI Foundry GenAI provider — chat, streaming, tool calling, structured output, Embedder, deployment mapping, and a model catalog (`CapabilityProvider`) for the golly v1.8.0 model router |
| [vectorstore/aisearch](vectorstore/aisearch/README.md)    | Azure AI Search backend — batched upsert/delete, vector + hybrid search, OData metadata filters, `CreateIndex` helper; API key or Entra ID           |
| [vectorstore/cosmosdb](vectorstore/cosmosdb/README.md)    | Azure Cosmos DB for NoSQL backend — `VectorDistance` search, parameterised SQL filters, partition-key aware                                           |

### 🗃️ Storage

| Package                | Description                                                                                                                                                   |
| ---------------------- | ------------------------------------------------------------------------------------------------------------------------------------------------------------- |
| [blob](blob/README.md) | Azure Blob Storage implementation of the golly VFS interface (`azblob://`) — read, write, copy, move, list, walk; VFileSystemCtx, Lister, RangeReader, sentinel-error mapping, Azurite support |

### 🧠 Cache

| Package                                  | Description                                                                                                                         |
| ---------------------------------------- | ----------------------------------------------------------------------------------------------------------------------------------- |
| [cache/azredis](cache/azredis/README.md) | Azure Cache for Redis / Azure Managed Redis backend for `cache.Cache[K,V]` + `Sweeper` + `Loader` (single-flight); access key or Entra ID auth with background token refresh |

### 🕐 Scheduling & Leader Election

| Package                                        | Description                                                                                                  |
| ---------------------------------------------- | ------------------------------------------------------------------------------------------------------------ |
| [chrono/tablelock](chrono/tablelock/README.md) | `chrono.LeaderElector` on an Azure Table Storage entity — `AddEntity` acquire + `If-Match` ETag renew/steal/release |
| [chrono/bloblock](chrono/bloblock/README.md)   | `chrono.LeaderElector` on an Azure blob — `If-None-Match` acquire + `If-Match` ETag renew/steal               |

### 🔑 Secrets

| Package                      | Description                                                                                                                                                  |
| ---------------------------- | ------------------------------------------------------------------------------------------------------------------------------------------------------------ |
| [secrets](secrets/README.md) | Azure Key Vault implementation of the golly secrets store — get, write, list, versions, soft delete / recover / purge; `WithTenantTags`; `NewNamespacedKeyVaultSecretStore` with Authorizer wiring; reversible key-name encoding |

### 📡 Messaging

| Package                            | Description                                                                                                                                                                              |
| ---------------------------------- | ---------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------- |
| [servicebus](servicebus/README.md) | Azure Service Bus provider for queues and topics/subscriptions — send, batch send, receive, listeners; sessions, dead-lettering, `ListenerRemover`, `Producer/ReceiverCtx`, `Keyed → SessionID`, `ObservableProvider`, broker-targeted options |

> 📖 Full API documentation available at [pkg.go.dev](https://pkg.go.dev/oss.nandlabs.io/golly-azure)

## Quick Start

```go
package main

import (
    "fmt"
    "log"

    "oss.nandlabs.io/golly-azure/azcfg"
    _ "oss.nandlabs.io/golly-azure/blob"       // registers azblob://
    _ "oss.nandlabs.io/golly-azure/servicebus" // registers servicebus://
    "oss.nandlabs.io/golly/vfs"
)

func main() {
    // Uses DefaultAzureCredential (env, workload/managed identity, Azure CLI).
    azcfg.Manager.Register("default", azcfg.NewConfig())

    f, err := vfs.GetManager().CreateRaw("azblob://mystorageaccount/mycontainer/hello.txt")
    if err != nil {
        log.Fatal(err)
    }
    defer f.Close()
    if _, err := f.Write([]byte("hello from golly-azure")); err != nil {
        log.Fatal(err)
    }
    fmt.Println("written")
}
```

More end-to-end samples live in [examples/](examples/) (`blob`, `servicebus`, `azopenai`).

## Testing

All unit tests run offline (fakes, `httptest` servers wired via azcore `ClientOptions.Transport`, and miniredis):

```bash
make test          # go test -race ./...
make test-cover    # with coverage
```

Emulator-backed integration tests are skipped unless the following variables are set:

| Variable                            | Emulator                                    | Packages                          |
| ----------------------------------- | ------------------------------------------- | --------------------------------- |
| `AZURITE_BLOB_ENDPOINT`             | [Azurite](https://github.com/Azure/Azurite) blob (`http://127.0.0.1:10000/devstoreaccount1`) | `blob`, `chrono/bloblock` |
| `AZURITE_TABLES_CONNECTION_STRING`  | Azurite tables                              | `chrono/tablelock`, `auth/entra`  |
| `SERVICEBUS_TEST_CONNECTION_STRING` | [Service Bus emulator](https://learn.microsoft.com/azure/service-bus-messaging/overview-emulator) | `servicebus` |
| `COSMOS_EMULATOR_ENDPOINT` / `COSMOS_EMULATOR_KEY` | [Cosmos DB emulator](https://learn.microsoft.com/azure/cosmos-db/emulator) | `vectorstore/cosmosdb` |

---

## Contributing

We welcome contributions to the project. If you find a bug or would like to
request a new feature, please open an issue on
[GitHub](https://github.com/nandlabs/golly-azure/issues). See
[CONTRIBUTING.md](CONTRIBUTING.md) for details.

## License

Licensed under either of

- Apache License, Version 2.0 ([LICENSE-APACHE](LICENSE-APACHE) or <http://www.apache.org/licenses/LICENSE-2.0>)
- MIT license ([LICENSE-MIT](LICENSE-MIT) or <http://opensource.org/licenses/MIT>)

at your option.

### Contribution

Unless you explicitly state otherwise, any contribution intentionally submitted
for inclusion in the work by you, as defined in the Apache-2.0 license, shall be
dual licensed as above, without any additional terms or conditions.
