# azcfg

Centralized Azure configuration for golly-azure packages.

`azcfg` holds the tenant, subscription, credentials, endpoints and cloud
selection used to build Azure SDK clients, and keeps them in a named registry
so packages can resolve a configuration from a URL (`azblob://<account>/...`,
`servicebus://<namespace>/...`) or by name.

## Features

- Named registry (`azcfg.Manager`) with URL host / host+path / name resolution
- `DefaultAzureCredential` by default (env vars, workload identity, managed identity, Azure CLI / azd)
- Service principal (client secret), managed identity (system or user-assigned), or any custom `azcore.TokenCredential`
- Connection strings and shared keys for services that support them
- Sovereign clouds (`cloud.AzureGovernment`, `cloud.AzureChina`) and custom endpoints (Azurite, emulators)
- Credential is built once and cached per `Config`

## Usage

```go
import (
    "oss.nandlabs.io/golly-azure/azcfg"
)

cfg := azcfg.NewConfig()
cfg.SetTenantID("<tenant-id>")
cfg.SetSubscriptionID("<subscription-id>")

// Pick one auth mode (default: DefaultAzureCredential)
cfg.SetClientSecret("<tenant-id>", "<client-id>", "<client-secret>")
// cfg.SetManagedIdentity("")            // system-assigned
// cfg.SetManagedIdentity("<client-id>") // user-assigned
// cfg.SetCredential(myTokenCredential)

azcfg.Manager.Register("default", cfg)
```

### Resolving a config

```go
u, _ := url.Parse("azblob://mystorageaccount/container/path")
cfg := azcfg.GetConfig(u, "default")      // host, host+path, then name
cfg  = azcfg.GetConfigOrDefault(u, "x")   // ... then "default", then NewConfig()

cred, err := cfg.Credential()
opts := cfg.ClientOpts() // azcore policy.ClientOptions with the cloud applied
```

### Emulators

```go
cfg := azcfg.NewConfig()
cfg.SetConnectionString("UseDevelopmentStorage=true") // Azurite
azcfg.Manager.Register("devstoreaccount1", cfg)
```
