package secrets

import (
	"context"

	secrets "oss.nandlabs.io/golly/secrets"
)

// NewNamespacedKeyVaultSecretStore is a convenience constructor that
// assembles a namespaced Key Vault-backed store in one call. It builds the
// base [KeyVaultSecretStore] via [NewKeyVaultSecretStore] and wraps it with
// [secrets.Namespace], applying prefix and any [secrets.NamespaceOption]
// (for example [secrets.WithAuthorizer]).
//
// Per-tenant tags are taken from cfg.TenantTags. Namespaced keys contain
// '/' (prefix + "/" + key), which the default KeyEncoder escapes, so
// "tenants/acme/api-key" is stored as the Key Vault secret
// "tenants-2Facme-2Fapi--key".
//
// The returned value is a [secrets.Store]; the Key Vault specific methods
// (List, Delete, Purge, Recover, GetClient, ClearCache) are intentionally
// hidden because they are not covered by the Authorizer. Use
// [NewKeyVaultSecretStore] directly when you need them.
//
//	store, err := kvsecrets.NewNamespacedKeyVaultSecretStore(ctx,
//	    &kvsecrets.KeyVaultSecretStoreConfig{
//	        VaultName:  "my-vault",
//	        TenantTags: map[string]string{"tenant": tenantID},
//	    },
//	    "tenants/"+tenantID,
//	    secrets.WithAuthorizer(auth),
//	)
func NewNamespacedKeyVaultSecretStore(
	ctx context.Context,
	cfg *KeyVaultSecretStoreConfig,
	prefix string,
	opts ...secrets.NamespaceOption,
) (secrets.Store, error) {
	base, err := NewKeyVaultSecretStore(ctx, cfg)
	if err != nil {
		return nil, err
	}
	return secrets.Namespace(base, prefix, opts...), nil
}
