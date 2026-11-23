# azredis

An `oss.nandlabs.io/golly/cache.Cache[string, []byte]` backend for
**Azure Cache for Redis** and **Azure Managed Redis**, built on
go-redis/v9's universal client. It also implements the optional
`cache.Sweeper` (no-op — Redis expires keys itself) and
`cache.Loader` (in-process single-flight `GetOrLoad`) capabilities.

## Features

- Works with Basic / Standard / Premium caches, clustered Premium caches
  and Azure Managed Redis (OSS or Enterprise cluster policy).
- **Access-key** auth (`Config.Password`) or **Microsoft Entra ID** auth
  (`NewWithEntraID` / `EntraIDProvider`).
- Entra ID tokens for `https://redis.azure.com/.default` come from any
  `azcore.TokenCredential` — by default resolved through `azcfg`
  (managed identity, service principal, `DefaultAzureCredential`).
- The Redis username is the principal's **object ID**, read from the
  token's `oid` claim (or set explicitly).
- Tokens are refreshed in the background before they expire and pushed to
  go-redis via `StreamingCredentialsProvider`, which re-AUTHs pooled
  connections in place — Azure closes connections whose token expired.
- TLS 1.2+ on by default; port-less addresses default to **6380**.

## Ports

| Service                                   | Endpoint                                  | Port    |
| ----------------------------------------- | ----------------------------------------- | ------- |
| Azure Cache for Redis (TLS)               | `<name>.redis.cache.windows.net`          | `6380`  |
| Azure Cache for Redis (non-TLS, disabled) | `<name>.redis.cache.windows.net`          | `6379`  |
| Azure Managed Redis / Enterprise          | `<name>.<region>.redis.azure.net`         | `10000` |

Helpers: `azredis.CacheEndpoint(name)` and
`azredis.ManagedRedisEndpoint(name, region)`.

## Quick start — access key

```go
import "oss.nandlabs.io/golly-azure/cache/azredis"

c, err := azredis.New(&azredis.Config{
    Addrs:    []string{azredis.CacheEndpoint("my-cache")},
    Password: os.Getenv("REDIS_ACCESS_KEY"),
})
if err != nil { /* ... */ }
defer c.Close()
if err := c.Ping(ctx); err != nil { /* auth / TLS problem */ }

_ = c.SetWithTTL(ctx, "greeting", []byte("hello"), time.Minute)
v, ok := c.Get(ctx, "greeting")
```

## Quick start — Microsoft Entra ID

Enable Entra ID authentication on the cache and add the managed identity /
service principal to its data access configuration (or an access policy
assignment on Managed Redis). Then:

```go
import (
    "oss.nandlabs.io/golly-azure/azcfg"
    "oss.nandlabs.io/golly-azure/cache/azredis"
)

cfg := azcfg.NewConfig()
cfg.SetManagedIdentity("") // or SetClientSecret(...); nil Config = DefaultAzureCredential

c, err := azredis.NewWithEntraID(ctx, &azredis.Config{
    Addrs: []string{azredis.ManagedRedisEndpoint("my-amr", "eastus")}, // :10000
}, azredis.EntraIDOptions{
    Config: cfg,
    // ObjectID: "…", // optional; defaults to the token's oid claim
    // RefreshBefore: 5 * time.Minute,
})
if err != nil { /* credential or token problem */ }
defer c.Close() // also stops the token refresh loop
```

Lower-level wiring is available when you need it:

```go
p, _ := azredis.NewEntraIDProvider(azredis.EntraIDOptions{Credential: myCred})
defer p.Close()

// Streaming (recommended): re-authenticates live connections.
c, _ := azredis.New(&azredis.Config{
    Addrs:                        []string{azredis.CacheEndpoint("my-cache")},
    StreamingCredentialsProvider: p,
})

// Per-connection only (no re-auth of long-lived connections).
oid, _ := p.Username(ctx)
c2, _ := azredis.New(&azredis.Config{
    Addrs:               []string{azredis.CacheEndpoint("my-cache")},
    Username:            oid,
    CredentialsProvider: p.Token,
})
```

## Clustering

Set `Config.ClusterMode = true` for clustered Premium caches and for Azure
Managed Redis with the **OSS** cluster policy so go-redis discovers shards
from the single endpoint. Leave it false for the **Enterprise** cluster
policy (a single proxied endpoint). Supplying more than one address also
selects the cluster client.

## Configuration

| Field                          | Description                                                         |
| ------------------------------ | ------------------------------------------------------------------- |
| `Addrs`                        | `host[:port]` endpoints; missing port → 6380 (6379 with DisableTLS) |
| `ClusterMode`                  | Force the cluster-aware client with a single address                |
| `Username` / `Password`        | Static credentials (access key: password only)                      |
| `CredentialsProvider`          | Per-connection password callback (paired with `Username`)           |
| `StreamingCredentialsProvider` | Rotating credentials with connection re-auth (`*EntraIDProvider`)   |
| `TLSConfig` / `DisableTLS`     | TLS override / plaintext for local testing only                     |
| `PoolSize`, `*Timeout`         | go-redis passthroughs                                               |

`EntraIDOptions`: `Credential`, `Config` (azcfg), `Scopes`
(default `https://redis.azure.com/.default`), `ObjectID`,
`RefreshBefore` (default 5m, clamped to half the token lifetime),
`RetryInterval` (default 10s).

## Notes

- `GetOrLoad` single-flight is per process; use a distributed lock for
  cross-replica coordination.
- `Clear` issues `FLUSHDB` (on every master when clustered). It may be
  blocked by restrictive access policies.
- `Sweep` always returns 0: Redis handles expiry server-side.

## Testing

Unit tests run offline against [miniredis](https://github.com/alicebob/miniredis)
with a fake `azcore.TokenCredential`:

```sh
GOWORK=off go test -race ./cache/azredis/...
```
