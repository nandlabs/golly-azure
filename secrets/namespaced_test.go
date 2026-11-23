package secrets

import (
	"context"
	"errors"
	"fmt"
	"testing"

	secrets "oss.nandlabs.io/golly/secrets"
)

// TestNamespaced_AuthorizerVetoesGet proves that wrapping the Key Vault
// store in secrets.Namespaced with a deny-all authorizer blocks Get before
// any Key Vault request is made.
func TestNamespaced_AuthorizerVetoesGet(t *testing.T) {
	fv := newFakeVault()
	inner := newTestStore(t, fv, nil)
	deny := func(_ context.Context, op secrets.Op, key string) error {
		if op != secrets.OpGet {
			t.Errorf("op = %q, want get", op)
		}
		if key != "tenant/acme/db-password" {
			t.Errorf("authorizer saw key %q", key)
		}
		return fmt.Errorf("%w: get blocked for %s", secrets.ErrForbidden, key)
	}
	scoped := secrets.Namespace(inner, "tenant/acme", secrets.WithAuthorizer(deny))
	if _, err := scoped.Get("db-password", ctx); !errors.Is(err, secrets.ErrForbidden) {
		t.Fatalf("Get: err = %v, want ErrForbidden", err)
	}
	if n := fv.totalCalls(); n != 0 {
		t.Errorf("expected 0 vault calls, got %d", n)
	}
}

func TestNamespaced_AuthorizerVetoesWrite(t *testing.T) {
	fv := newFakeVault()
	inner := newTestStore(t, fv, nil)
	deny := func(_ context.Context, op secrets.Op, key string) error {
		if op != secrets.OpWrite {
			t.Errorf("op = %q, want write", op)
		}
		return secrets.ErrForbidden
	}
	scoped := secrets.Namespace(inner, "tenant/acme", secrets.WithAuthorizer(deny))
	err := scoped.Write("db-password", &secrets.Credential{Value: []byte("s3cr3t")}, ctx)
	if !errors.Is(err, secrets.ErrForbidden) {
		t.Fatalf("Write: err = %v, want ErrForbidden", err)
	}
	if n := fv.totalCalls(); n != 0 {
		t.Errorf("expected 0 vault calls, got %d", n)
	}
}

// TestNamespaced_PermitForwardsEncodedKey checks that a permitted namespaced
// write lands in the vault under the escaped, prefixed name and that the
// tenant tags are attached.
func TestNamespaced_PermitForwardsEncodedKey(t *testing.T) {
	fv := newFakeVault()
	inner := newTestStore(t, fv, nil, WithTenantTags(map[string]string{"tenant": "acme"}))
	permit := func(context.Context, secrets.Op, string) error { return nil }
	scoped := secrets.Namespace(inner, "tenants/acme", secrets.WithAuthorizer(permit))

	if err := scoped.Write("api-key", &secrets.Credential{Value: []byte("v")}, ctx); err != nil {
		t.Fatalf("Write: %v", err)
	}
	stored := fv.latest("tenants-2Facme-2Fapi--key")
	if stored == nil {
		t.Fatalf("secret not stored under encoded name; calls=%v", fv.calls)
	}
	if stored.tags["tenant"] != "acme" {
		t.Errorf("tags = %v", stored.tags)
	}
	got, err := scoped.Get("api-key", ctx)
	if err != nil || string(got.Value) != "v" {
		t.Fatalf("Get = %v, %v", got, err)
	}
	keys, err := inner.List(ctx)
	if err != nil || len(keys) != 1 || keys[0] != "tenants/acme/api-key" {
		t.Errorf("List = %q, %v", keys, err)
	}
	if scoped.Provider() != AzureKeyVaultProvider {
		t.Errorf("Provider = %q", scoped.Provider())
	}
}

func TestNewNamespacedKeyVaultSecretStore(t *testing.T) {
	fv := newFakeVault()
	var seen []string
	auth := func(_ context.Context, op secrets.Op, key string) error {
		seen = append(seen, string(op)+":"+key)
		if key == "tenants/acme/forbidden" {
			return secrets.ErrForbidden
		}
		return nil
	}
	store, err := NewNamespacedKeyVaultSecretStore(ctx, &KeyVaultSecretStoreConfig{
		VaultURL:   fakeVaultURL,
		Azure:      testAzConfig(fv),
		TenantTags: map[string]string{"tenant": "acme"},
	}, "tenants/acme", secrets.WithAuthorizer(auth))
	if err != nil {
		t.Fatalf("constructor: %v", err)
	}
	if err := store.Write("db.pass", &secrets.Credential{Value: []byte("p")}, ctx); err != nil {
		t.Fatalf("Write: %v", err)
	}
	if v := fv.latest("tenants-2Facme-2Fdb-2Epass"); v == nil || v.tags["tenant"] != "acme" {
		t.Errorf("stored = %+v", v)
	}
	if _, err := store.Get("forbidden", ctx); !errors.Is(err, secrets.ErrForbidden) {
		t.Errorf("Get forbidden: %v", err)
	}
	if len(seen) != 2 || seen[0] != "write:tenants/acme/db.pass" {
		t.Errorf("authorizer calls = %v", seen)
	}

	if _, err := NewNamespacedKeyVaultSecretStore(ctx, &KeyVaultSecretStoreConfig{}, "p"); !errors.Is(err, ErrVaultURLRequired) {
		t.Errorf("missing vault: %v", err)
	}
}
