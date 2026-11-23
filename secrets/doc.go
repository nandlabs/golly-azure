// Package secrets provides Azure Key Vault integration for Golly credential
// management.
//
// This package implements the Store interface from
// oss.nandlabs.io/golly/secrets using Azure Key Vault secrets as the backend
// storage for credentials. It is the Azure counterpart of the golly-aws
// (Secrets Manager) and golly-gcp (Secret Manager) secrets packages and
// stores credentials in the same JSON format.
//
// # Features
//
//   - Full integration with Golly credential types and metadata
//   - Upsert writes: every Write creates a new Key Vault version
//   - Version access (GetVersion) and Key Vault metadata mapping
//   - Soft delete, recover and purge
//   - List with tag filtering
//   - Per-tenant tags (WithTenantTags) and namespaced stores with an
//     Authorizer (NewNamespacedKeyVaultSecretStore)
//   - Reversible key encoding so keys containing '/', '_', '.' work
//   - In-memory caching with configurable TTL
//   - Thread-safe concurrent operations
//   - Credentials resolved through azcfg (DefaultAzureCredential, client
//     secret, managed identity or a custom azcore.TokenCredential)
//
// # Basic Usage
//
//	store, err := secrets.NewKeyVaultSecretStore(ctx, &secrets.KeyVaultSecretStoreConfig{
//	    VaultName: "my-vault", // https://my-vault.vault.azure.net/
//	})
//
//	err = store.Write("db/password", &gsecrets.Credential{
//	    Value:       []byte("secret-value"),
//	    LastUpdated: time.Now(),
//	    Version:     "1.0",
//	}, ctx)
//
//	cred, err := store.Get("db/password", ctx)
//	keys, err := store.List(ctx)
//	err = store.Delete("db/password", ctx) // soft delete
//	err = store.Purge("db/password", ctx)  // permanent
//
// # Vault URL resolution
//
// KeyVaultSecretStoreConfig.VaultURL wins, then the azcfg Endpoint (e.g. a
// private endpoint), then VaultName (https://<name>.<VaultDNSSuffix>/, the
// suffix defaulting to vault.azure.net), then the azcfg AccountName used as
// a vault name. When cfg.Azure is nil the azcfg default configuration
// (azcfg.GetConfigOrDefault) supplies the credential and client options.
//
// # Secret names
//
// Key Vault secret names may contain only alphanumerics and '-' (1-127
// characters) and are case-insensitive. Golly keys are mapped with
// DefaultKeyEncoder: letters and digits are kept, '-' becomes "--" and
// every other byte becomes '-' plus two upper-case hex digits, so
// "tenants/acme/api_key" is stored as "tenants-2Facme-2Fapi-5Fkey". The
// mapping is reversible, which lets List return the original keys; vault
// secrets whose names were not produced by the encoder are skipped by List.
// Use WithKeyEncoder(RawKeyEncoder) to address pre-existing secrets by their
// literal names. Keys that differ only by case address the same secret.
//
// # Delete and purge
//
// Key Vault always soft-deletes: Delete moves the secret to the deleted
// state for the vault retention period (7-90 days). While deleted, the name
// cannot be reused (Write fails with ErrSecretDeleted). Recover restores
// it; Purge removes it permanently, requires the purge permission and is
// rejected on vaults with purge protection. Deletion is asynchronous, so a
// Purge immediately after Delete may fail with ErrSecretDeleted; retry.
//
// Delete, Purge, Recover, List and GetVersion are not part of
// secrets.Store, so they are not checked by secrets.Namespaced +
// WithAuthorizer. NewNamespacedKeyVaultSecretStore therefore returns only a
// secrets.Store.
//
// # Multi-tenant
//
//	store, err := secrets.NewNamespacedKeyVaultSecretStore(ctx,
//	    &secrets.KeyVaultSecretStoreConfig{
//	        VaultName:  "my-vault",
//	        TenantTags: map[string]string{"tenant": tenantID},
//	    },
//	    "tenants/"+tenantID,
//	    gsecrets.WithAuthorizer(policy),
//	)
//
// # Errors
//
// Errors wrap package sentinels and keep the underlying
// *azcore.ResponseError in the chain: ErrSecretNotFound (404),
// ErrSecretDeleted (409), golly secrets.ErrForbidden (401/403),
// ErrInvalidKey, ErrTooManyTags and ErrVaultURLRequired.
//
// # Azure Requirements
//
//   - A Key Vault and an identity the azcfg credential resolves to
//   - RBAC role "Key Vault Secrets Officer" (or access policy permissions
//     get, list, set, delete, recover, purge as needed); "Key Vault Secrets
//     User" suffices for read-only use
//
// # Thread Safety
//
// All operations are safe for concurrent use.
package secrets
