# Azure Key Vault Secrets Store

This package provides a Golly `secrets.Store` implementation backed by Azure
Key Vault secrets. It is the Azure counterpart of the `golly-aws` and
`golly-gcp` secrets packages and uses the same JSON storage format.

```go
import (
    gsecrets "oss.nandlabs.io/golly/secrets"
    kvsecrets "oss.nandlabs.io/golly-azure/secrets"
)
```

## Features

- **Full Credential Support**: Works with all Golly credential types and metadata
- **Upsert Writes**: Creates the secret if missing; every write adds a new Key Vault version
- **Version Access**: `GetVersion` reads any historical version; Key Vault metadata is mapped onto the credential
- **Soft Delete / Recover / Purge**: Explicit lifecycle methods with documented semantics
- **List with Tag Filtering**: Returns the original golly keys
- **Multi-tenant**: Per-tenant tags (`WithTenantTags`) and namespaced stores with an `Authorizer`
- **Safe Key Encoding**: Keys containing `/`, `_`, `.` (and any UTF-8) map reversibly to valid Key Vault names
- **In-Memory Caching**: Optional caching with TTL
- **azcfg Integration**: Credentials, cloud and client options come from `azcfg.Config`

## Creating a Key Vault Secrets Store

### Basic Configuration

```go
store, err := kvsecrets.NewKeyVaultSecretStore(ctx, &kvsecrets.KeyVaultSecretStoreConfig{
    VaultName: "my-vault", // https://my-vault.vault.azure.net/
})
```

### Explicit URL, Credential and Sovereign Clouds

```go
azc := azcfg.NewConfig()
azc.SetClientSecret(tenantID, clientID, clientSecret) // or SetManagedIdentity / SetCredential

store, err := kvsecrets.NewKeyVaultSecretStore(ctx, &kvsecrets.KeyVaultSecretStoreConfig{
    VaultURL: "https://my-vault.vault.azure.net/",
    Azure:    azc,
})

// Azure China
store, err = kvsecrets.NewKeyVaultSecretStore(ctx, &kvsecrets.KeyVaultSecretStoreConfig{
    VaultName:      "my-vault",
    VaultDNSSuffix: "vault.azure.cn",
    Azure:          azc,
})
```

The vault URL is resolved in this order:

1. `VaultURL`
2. `Azure.Endpoint` (azcfg override, e.g. a private endpoint)
3. `VaultName` → `https://<VaultName>.<VaultDNSSuffix>/` (suffix defaults to `vault.azure.net`)
4. `Azure.AccountName` used as the vault name

When `Azure` is nil, `azcfg.GetConfigOrDefault(nil, "default")` is used,
which falls back to `DefaultAzureCredential`.

### With Tag Filtering and Caching

```go
store, err := kvsecrets.NewKeyVaultSecretStore(ctx, &kvsecrets.KeyVaultSecretStoreConfig{
    VaultName: "my-vault",
    TagFilter: map[string]string{"app": "golly", "env": "production"},
    CacheTTL:  5 * time.Minute,
})
```

### Configuration Reference

| Field | Description |
| --- | --- |
| `VaultURL` | Full vault URL |
| `VaultName` | Short vault name |
| `VaultDNSSuffix` | DNS suffix for `VaultName` (default `vault.azure.net`) |
| `Azure` | `*azcfg.Config` for credential / client options / endpoint |
| `TagFilter` | Tags a secret must carry to appear in `List`; written when no tenant tags are set |
| `TenantTags` | Tags written on every `Write` |
| `CacheTTL` | Read cache TTL (0 = no caching) |
| `KeyEncoder` | Key ↔ secret-name mapping (default `DefaultKeyEncoder`) |
| `DisableChallengeResourceVerification` | Passed to azsecrets for non-standard vault domains |

Functional options: `WithTenantTags`, `WithTagFilter`, `WithCacheTTL`, `WithKeyEncoder`.

## Usage

### Writing a Credential

```go
cred := &gsecrets.Credential{
    Value:       []byte("secret-api-key"),
    LastUpdated: time.Now(),
    Version:     "1.0",
}
err := store.Write("payments/stripe_api.key", cred, ctx)
```

### Reading a Credential

```go
cred, err := store.Get("payments/stripe_api.key", ctx)
if errors.Is(err, kvsecrets.ErrSecretNotFound) { /* ... */ }

// A specific Key Vault version
old, err := store.GetVersion("payments/stripe_api.key", "3f1c...e9", ctx)
```

### Listing All Credentials

```go
keys, err := store.List(ctx) // original golly keys, e.g. "payments/stripe_api.key"
```

### Deleting, Recovering and Purging

```go
err := store.Delete("payments/stripe_api.key", ctx)  // soft delete
err  = store.Recover("payments/stripe_api.key", ctx) // undo soft delete
err  = store.Purge("payments/stripe_api.key", ctx)   // permanent (after Delete)
```

Key Vault always soft-deletes. A deleted secret stays recoverable for the
vault's retention period (7-90 days) and its name **cannot be reused** in
the meantime: `Write` returns `ErrSecretDeleted` until it is recovered or
purged. `Purge` needs the purge permission and is refused on vaults with
purge protection enabled. Deletion is asynchronous, so a `Purge` issued
immediately after `Delete` may return `ErrSecretDeleted` ("being deleted");
retry after a short delay.

`Delete`, `Purge`, `Recover`, `List` and `GetVersion` are not part of the
upstream `secrets.Store` interface and are therefore **not** checked by
`secrets.Namespaced` + `WithAuthorizer`. Enforce authorization at the call
site, and hand tenant code only the namespaced `secrets.Store`.

### Per-Tenant Tags and Namespaced Stores

`TagFilter` is a *lookup* filter; reusing it for writes leaks the same tags
across tenants. `WithTenantTags` (or `TenantTags` in the config) attaches a
distinct tag set to every secret version written by that instance and
replaces (does not merge with) `TagFilter` on writes:

```go
tenantStore, _ := kvsecrets.NewKeyVaultSecretStore(ctx, cfg,
    kvsecrets.WithTenantTags(map[string]string{"tenant": tenantID}),
)
scoped := gsecrets.Namespace(tenantStore, "tenant/"+tenantID,
    gsecrets.WithAuthorizer(policy))
```

Or in one call:

```go
auth := func(ctx context.Context, op gsecrets.Op, key string) error {
    user := userFromCtx(ctx)
    if !user.CanAccess(op, key) {
        return fmt.Errorf("%w: user %s", gsecrets.ErrForbidden, user.ID)
    }
    return nil
}

store, err := kvsecrets.NewNamespacedKeyVaultSecretStore(ctx,
    &kvsecrets.KeyVaultSecretStoreConfig{
        VaultName:  "my-vault",
        TenantTags: map[string]string{"tenant": tenantID},
    },
    "tenants/"+tenantID,
    gsecrets.WithAuthorizer(auth),
)
// store.Get("api-key", ctx) reads Key Vault secret "tenants-2F<tenantID>-2Fapi--key".
```

Key Vault allows at most 15 tags per secret (`ErrTooManyTags`).

## Secret Name Encoding

Key Vault secret names must match `^[0-9a-zA-Z-]{1,127}$` and are
case-insensitive. Golly keys are mapped with `DefaultKeyEncoder`:

| Character | Encoded as |
| --- | --- |
| `a-z`, `A-Z`, `0-9` | unchanged |
| `-` | `--` |
| any other byte | `-` + two upper-case hex digits (`/` → `-2F`, `_` → `-5F`, `.` → `-2E`) |

| Golly key | Key Vault name |
| --- | --- |
| `db-password` | `db--password` |
| `tenants/acme/api_key` | `tenants-2Facme-2Fapi-5Fkey` |
| `app.config` | `app-2Econfig` |

The encoding is reversible, so `List` returns the original keys. Vault
secrets whose names the encoder did not produce (e.g. `my-secret` created
in the portal; it would decode only if it were `my--secret`) are skipped by
`List`. To work with such secrets use `WithKeyEncoder(kvsecrets.RawKeyEncoder)`,
which uses keys verbatim and rejects invalid names, or supply your own
`KeyEncoder`. `store.SecretName(key)` returns the vault name for a key.

Encoded names longer than 127 characters are rejected with `ErrInvalidKey`
(each escaped byte costs three characters). Because Key Vault ignores case,
keys that differ only by case address the same secret.

## Storage Format

Credentials are stored as JSON with content type `application/json`:

```json
{
  "value": "secret-value",
  "version": "1.0",
  "last_updated": 1682505600,
  "metadata": {
    "type": "password",
    "username": "bob"
  }
}
```

Values that are not valid UTF-8 are stored base64-encoded in `value_b64`
instead of `value`. Secrets that are not in this format (e.g. created by
other tools) are returned verbatim as the credential value.

## Version and Metadata Mapping

Every `Write` creates a new Key Vault version. On `Get`:

| Credential field | Source |
| --- | --- |
| `Value`, `Version`, `MetaData` | JSON envelope |
| `Version` (if envelope has none) | Key Vault version id |
| `LastUpdated` | envelope `last_updated`, else Key Vault `updated` attribute |
| `MetaData["azure_version_id"]` | Key Vault version id |
| `MetaData["azure_secret_id"]` | full secret identifier URL |
| `MetaData["azure_vault_url"]` | vault URL |
| `MetaData["azure_content_type"]` | content type |
| `MetaData["azure_tags"]` | secret tags (`map[string]string`) |
| `MetaData["azure_enabled"]`, `["azure_created"]`, `["azure_updated"]`, `["azure_expires"]`, `["azure_not_before"]` | secret attributes |

## Error Handling

Errors wrap sentinels and keep the original `*azcore.ResponseError`:

| Sentinel | Cause |
| --- | --- |
| `ErrSecretNotFound` | HTTP 404 |
| `ErrSecretDeleted` | HTTP 409 (soft-deleted name, or delete in progress) |
| `gsecrets.ErrForbidden` | HTTP 401 / 403 |
| `ErrInvalidKey` | key cannot be encoded as a Key Vault name |
| `ErrTooManyTags` | more than 15 tags |
| `ErrVaultURLRequired` | no vault URL could be resolved |

## Azure Permissions

With Azure RBAC:

- **Key Vault Secrets User**: `Get`, `GetVersion`, `List`
- **Key Vault Secrets Officer**: all operations including `Write`, `Delete`, `Recover`, `Purge`

With access policies, grant the secret permissions `get`, `list`, `set`,
`delete`, `recover` and `purge` as required.

## Advanced Usage

```go
client := store.GetClient()            // *azsecrets.Client
name, _ := store.SecretName("a/b")     // "a-2Fb"
store.ClearCache()
```

## Testing

Unit tests run offline against an in-memory Key Vault REST fake plugged in
via `azcfg.Config.ClientOptions.Transport`, exercising the real `azsecrets`
client pipeline (including the bearer challenge).

## Thread Safety

The store is safe for concurrent use.
