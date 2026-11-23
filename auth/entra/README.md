# auth/entra

Microsoft Entra ID (formerly Azure AD) adapter for [`oss.nandlabs.io/golly/auth`](https://pkg.go.dev/oss.nandlabs.io/golly/auth).

Ships these pluggable pieces:

- **`Verifier`** — a JWKS-backed JWT verifier for Entra ID id-tokens and
  access-tokens (implements `auth.Verifier`). It fetches the tenant's
  signing keys from
  `https://login.microsoftonline.com/<tenant>/discovery/v2.0/keys`
  (or resolves `jwks_uri` from the tenant's OpenID discovery document
  with `WithOpenIDDiscovery`), caches them per-kid with a configurable
  TTL (default 1h) and refreshes on demand when a token references an
  unknown kid (rate-limited to one fetch per 10s by default). It
  validates:
  - the RS256 signature (`kid`, or `x5t` for v1 headers),
  - `exp` / `nbf` / `iat` with a configurable leeway (default 60s),
  - `ver` — both `1.0` and `2.0` accepted by default (`WithTokenVersions`),
  - `iss` in the format matching the version:
    v1.0 `https://sts.windows.net/<tid>/`, v2.0 `https://login.microsoftonline.com/<tid>/v2.0`,
  - `tid` — equal to the pinned tenant, or on the allow-list in multi-tenant mode,
  - `aud` — the client id or `api://<clientID>` (extend with `WithAudiences`),
  - a non-empty `sub`.
- **Claim helpers** — `ObjectID`, `TenantID`, `TokenVersion`,
  `ClientAppID` (`azp`/`appid`), `Username`, `Scopes` (`scp`), `Roles`,
  `Groups`, `HasGroupOverage`, and **`Principal`** (`NewPrincipal(claims)`),
  an `authz.Principal` whose `ID()` is the object id, `Roles()` the app
  roles, `Capabilities()` the scopes and `Groups()` the group ids — ready
  for golly `authz.RBAC` or golly-azure `authz/azrbac`.
- **`TableSessionStore`** — an `auth.SessionStore` backed by Azure Table
  Storage (`aztables`; Azurite and the Cosmos DB Table API work too) with
  emulated TTL semantics and `ListByUser` / `PurgeExpired` helpers.

## Quick start — verify an Entra JWT in a `rest` handler

```go
import (
    "net/http"

    "oss.nandlabs.io/golly-azure/auth/entra"
    "oss.nandlabs.io/golly/rest"
)

func main() {
    v, err := entra.NewVerifier(
        "72f988bf-86f1-41af-91ab-2d7cd011db47", // directory (tenant) id
        "6e74172b-be56-4843-9ff4-e66a39bb12e3", // API app (client) id
    )
    if err != nil { panic(err) }
    r := rest.NewRouter()
    r.Get("/me", func(w http.ResponseWriter, req *http.Request) {
        tok := req.Header.Get("Authorization")[len("Bearer "):]
        claims, err := v.VerifyToken(req.Context(), tok)
        if err != nil { http.Error(w, err.Error(), http.StatusUnauthorized); return }
        w.Write([]byte("hello " + entra.Username(claims) + " (" + entra.ObjectID(claims) + ")"))
    })
    http.ListenAndServe(":8080", r)
}
```

## Tenancy modes

| `tenant` argument                 | Behaviour                                                                                  |
|-----------------------------------|--------------------------------------------------------------------------------------------|
| tenant GUID                       | Single-tenant: `tid` must equal it; issuer built from it.                                   |
| `organizations` / `common`        | Multi-tenant: issuer must match the token's own `tid`; `tid` must be on `WithAllowedTenants(...)`. Use `WithAnyTenant()` to accept every tenant (construction fails if neither is given). |
| `consumers`                       | Personal Microsoft accounts only (tid `9188040d-6c67-4c5b-b112-36a304b66dad`).              |

Domain-name tenants (`contoso.onmicrosoft.com`) are rejected — pass the
tenant GUID so the issuer can be validated exactly.

```go
v, _ := entra.NewVerifier(entra.TenantOrganizations, clientID,
    entra.WithAllowedTenants(customerA, customerB))
```

## Options

| Option                          | Effect                                                                 |
|---------------------------------|------------------------------------------------------------------------|
| `WithAudiences(aud...)`         | Accept extra `aud` values (custom App ID URIs).                         |
| `WithAllowedTenants(ids...)`    | Tenant allow-list for multi-tenant mode.                               |
| `WithAnyTenant()`               | Accept any tenant in multi-tenant mode.                                |
| `WithTokenVersions(v...)`       | Restrict to `entra.TokenVersion1` and/or `entra.TokenVersion2`.        |
| `WithOpenIDDiscovery()`         | Resolve the JWKS URL from `<authority>/<tenant>/v2.0/.well-known/openid-configuration`. |
| `WithDiscoveryURL(u)`           | Explicit discovery document URL (implies discovery).                   |
| `WithJWKSURL(u)`                | Explicit JWKS URL (disables discovery).                                |
| `WithAuthorityHost(h)`          | Sovereign clouds, e.g. `https://login.microsoftonline.us`.             |
| `WithV1IssuerHost(h)`           | v1.0 issuer host for sovereign clouds (default `https://sts.windows.net`). |
| `WithKeyTTL(d)` / `WithLeeway(d)` / `WithMinRefreshInterval(d)` | Cache lifetime, clock skew, unknown-kid refresh rate limit. |
| `WithHTTPClient(c)` / `WithNow(fn)` | Transport and clock overrides.                                     |

Errors map to golly's sentinels: `auth.ErrInvalidSig`, `auth.ErrExpired`,
`auth.ErrNotYetValid`, `auth.ErrIssuerMismatch` (also wrapping
`entra.ErrTenantNotAllowed` for tenant rejections), `auth.ErrAudMismatch`,
`auth.ErrKeyNotFound`, `auth.ErrInvalidToken`.

> Microsoft Graph access tokens (audience `00000003-0000-0000-c000-000000000000`)
> carry a `nonce` header and are not meant to be validated by third
> parties — request a token for your own API instead.

## Session store

```go
cfg := azcfg.NewConfig()
cfg.SetConnectionString(os.Getenv("AZURE_STORAGE_CONNECTION_STRING"))
store, err := entra.NewTableSessionStoreFromConfig(cfg, "sessions")
if err != nil { /* handle */ }
_ = store.CreateTableIfNotExists(ctx)
sm := auth.NewSessionManager(store, auth.SessionConfig{Absolute: 24 * time.Hour})
```

Or wrap an existing client: `entra.NewTableSessionStore(tableClient)`.
`NewTableSessionStoreFromConfig` uses `ConnectionString`, else a shared
key (`AccountName` + `AccountKey`), else `cfg.Credential()` (Entra ID /
managed identity — grant *Storage Table Data Contributor*). The service
URL is `cfg.Endpoint` or `https://<AccountName>.table.core.windows.net`.

### Entity schema

| Property       | Type       | Notes                                    |
|----------------|------------|------------------------------------------|
| `PartitionKey` | string     | session id                               |
| `RowKey`       | string     | constant `"session"`                     |
| `UserID`       | string     | mirror of `Session.Subject`              |
| `Payload`      | string     | JSON-encoded `auth.Session` (≤ 64 KiB)   |
| `ExpiresAt`    | DateTime   | mirror of `Session.ExpiresAt`            |
| `CreatedAt`    | DateTime   | mirror of `Session.CreatedAt`            |

Session ids must be valid PartitionKeys (no `/ \ # ? '` or control
characters; ids minted by `auth.SessionManager` always are).

### TTL semantics

Table Storage has no server-side TTL, so the store emulates it:

- `Get` treats a session whose `ExpiresAt` has passed as absent and
  deletes it best-effort.
- `ListByUser(ctx, uid)` skips expired sessions.
- `PurgeExpired(ctx)` deletes every expired entity
  (`ExpiresAt lt datetime'<now>'`) and returns the count — schedule it
  periodically (e.g. a golly `chrono` job).

`ListByUser` is a server-side filtered table scan (`UserID eq '<uid>'`)
because Table Storage has no secondary indexes; it's intended for
"sign out everywhere" and admin views, not hot paths.

## Testing

Unit tests are fully offline (httptest JWKS with generated RSA keys,
an in-memory table fake and a tiny Table REST fake). The Azurite smoke
test runs only when `AZURITE_TABLES_CONNECTION_STRING` is set.
