package secrets

import (
	"context"
	"encoding/json"
	"errors"
	"reflect"
	"sort"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/Azure/azure-sdk-for-go/sdk/azcore"
	"oss.nandlabs.io/golly-azure/azcfg"
	secrets "oss.nandlabs.io/golly/secrets"
)

var ctx = context.Background()

func TestResolveVaultURL(t *testing.T) {
	tests := []struct {
		name string
		cfg  *KeyVaultSecretStoreConfig
		want string
		err  error
	}{
		{"nil", nil, "", ErrVaultURLRequired},
		{"empty", &KeyVaultSecretStoreConfig{}, "", ErrVaultURLRequired},
		{"vault name", &KeyVaultSecretStoreConfig{VaultName: "myvault"}, "https://myvault.vault.azure.net/", nil},
		{"vault name sovereign", &KeyVaultSecretStoreConfig{VaultName: "v", VaultDNSSuffix: ".vault.azure.cn"}, "https://v.vault.azure.cn/", nil},
		{"vault url wins", &KeyVaultSecretStoreConfig{VaultURL: "https://a.vault.azure.net", VaultName: "b",
			Azure: &azcfg.Config{Endpoint: "https://c.vault.azure.net/"}}, "https://a.vault.azure.net/", nil},
		{"endpoint overrides name", &KeyVaultSecretStoreConfig{VaultName: "b",
			Azure: &azcfg.Config{Endpoint: "https://private.vault.azure.net"}}, "https://private.vault.azure.net/", nil},
		{"account name fallback", &KeyVaultSecretStoreConfig{Azure: &azcfg.Config{AccountName: "acct"}}, "https://acct.vault.azure.net/", nil},
		{"scheme added", &KeyVaultSecretStoreConfig{VaultURL: "x.vault.azure.net"}, "https://x.vault.azure.net/", nil},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got, err := ResolveVaultURL(tt.cfg)
			if !errors.Is(err, tt.err) {
				t.Fatalf("err = %v, want %v", err, tt.err)
			}
			if got != tt.want {
				t.Errorf("got %q, want %q", got, tt.want)
			}
		})
	}
}

func TestNewKeyVaultSecretStore_Errors(t *testing.T) {
	if _, err := NewKeyVaultSecretStore(ctx, nil); !errors.Is(err, ErrVaultURLRequired) {
		t.Errorf("nil cfg: err = %v, want ErrVaultURLRequired", err)
	}
	bad := &azcfg.Config{ClientSecret: "s"} // missing tenant/client id
	_, err := NewKeyVaultSecretStore(ctx, &KeyVaultSecretStoreConfig{VaultName: "v", Azure: bad})
	if !errors.Is(err, azcfg.ErrIncompleteClientSecret) {
		t.Errorf("bad credential: err = %v, want ErrIncompleteClientSecret", err)
	}
}

func TestNewKeyVaultSecretStore_DefaultAzcfg(t *testing.T) {
	fv := newFakeVault()
	c := testAzConfig(fv)
	c.SetEndpoint(fakeVaultURL)
	azcfg.Manager.Register(azcfg.DefaultConfigName, c)
	t.Cleanup(func() { azcfg.Manager.Unregister(azcfg.DefaultConfigName) })

	s, err := NewKeyVaultSecretStore(ctx, nil)
	if err != nil {
		t.Fatalf("NewKeyVaultSecretStore: %v", err)
	}
	if s.VaultURL() != fakeVaultURL {
		t.Errorf("VaultURL = %q", s.VaultURL())
	}
	if err := s.Write("k", &secrets.Credential{Value: []byte("v")}, ctx); err != nil {
		t.Fatalf("Write: %v", err)
	}
}

func TestWriteGetRoundTrip(t *testing.T) {
	fv := newFakeVault()
	s := newTestStore(t, fv, nil)

	ts := time.Unix(1710000000, 0)
	in := &secrets.Credential{
		Value:       []byte("s3cr3t"),
		Version:     "1.0",
		LastUpdated: ts,
		MetaData:    map[string]interface{}{"type": "password", "username": "bob"},
	}
	key := "apps/billing/db_password.v2"
	if err := s.Write(key, in, ctx); err != nil {
		t.Fatalf("Write: %v", err)
	}

	name, _ := s.SecretName(key)
	if name != "apps-2Fbilling-2Fdb-5Fpassword-2Ev2" {
		t.Fatalf("secret name = %q", name)
	}
	stored := fv.latest(name)
	if stored == nil {
		t.Fatal("secret not stored under encoded name")
	}
	if stored.contentType != ContentTypeJSON {
		t.Errorf("contentType = %q", stored.contentType)
	}
	var env map[string]interface{}
	if err := json.Unmarshal([]byte(stored.value), &env); err != nil {
		t.Fatalf("stored value not JSON: %v", err)
	}
	if env["value"] != "s3cr3t" || env["version"] != "1.0" || env["last_updated"].(float64) != float64(ts.Unix()) {
		t.Errorf("envelope = %v", env)
	}

	got, err := s.Get(key, ctx)
	if err != nil {
		t.Fatalf("Get: %v", err)
	}
	if string(got.Value) != "s3cr3t" || got.Version != "1.0" || !got.LastUpdated.Equal(ts) {
		t.Errorf("got %+v", got)
	}
	if got.MetaData["username"] != "bob" || secrets.DetectCredentialType(got.MetaData) != secrets.CredentialTypePassword {
		t.Errorf("caller metadata lost: %v", got.MetaData)
	}
	if got.MetaData[MetaVersionID] != "ver1" {
		t.Errorf("version id = %v", got.MetaData[MetaVersionID])
	}
	if got.MetaData[MetaSecretID] != fakeVaultURL+"secrets/"+name+"/ver1" {
		t.Errorf("secret id = %v", got.MetaData[MetaSecretID])
	}
	if got.MetaData[MetaVaultURL] != fakeVaultURL || got.MetaData[MetaContentType] != ContentTypeJSON {
		t.Errorf("metadata = %v", got.MetaData)
	}
	if got.MetaData[MetaEnabled] != true {
		t.Errorf("enabled = %v", got.MetaData[MetaEnabled])
	}
	for _, k := range []string{MetaCreated, MetaUpdated} {
		if _, ok := got.MetaData[k].(time.Time); !ok {
			t.Errorf("%s missing: %v", k, got.MetaData[k])
		}
	}
}

func TestWrite_BinaryValue(t *testing.T) {
	fv := newFakeVault()
	s := newTestStore(t, fv, nil)
	bin := []byte{0x00, 0xff, 0xfe, 0x80}
	if err := s.Write("bin", &secrets.Credential{Value: bin}, ctx); err != nil {
		t.Fatalf("Write: %v", err)
	}
	if !strings.Contains(fv.latest("bin").value, "value_b64") {
		t.Errorf("binary value not base64 encoded: %s", fv.latest("bin").value)
	}
	got, err := s.Get("bin", ctx)
	if err != nil {
		t.Fatalf("Get: %v", err)
	}
	if !reflect.DeepEqual(got.Value, bin) {
		t.Errorf("value = %v, want %v", got.Value, bin)
	}
	if got.LastUpdated.IsZero() {
		t.Error("LastUpdated should default to write time")
	}
}

func TestWrite_NilCredential(t *testing.T) {
	s := newTestStore(t, newFakeVault(), nil)
	if err := s.Write("k", nil, ctx); err == nil {
		t.Fatal("expected error")
	}
}

func TestGet_RawSecret(t *testing.T) {
	fv := newFakeVault()
	fv.seed("plain", "just-a-string", map[string]string{"owner": "ops"}, false)
	s := newTestStore(t, fv, nil)

	got, err := s.Get("plain", ctx)
	if err != nil {
		t.Fatalf("Get: %v", err)
	}
	if string(got.Value) != "just-a-string" {
		t.Errorf("value = %q", got.Value)
	}
	// Without an envelope version, the Key Vault version is used.
	if got.Version != "v0" {
		t.Errorf("version = %q, want v0", got.Version)
	}
	if !got.LastUpdated.Equal(time.Unix(1700000000, 0)) {
		t.Errorf("LastUpdated = %v, want Key Vault updated time", got.LastUpdated)
	}
	if tags, _ := got.MetaData[MetaTags].(map[string]string); tags["owner"] != "ops" {
		t.Errorf("tags = %v", got.MetaData[MetaTags])
	}
	// JSON that is not an envelope is returned verbatim.
	fv.seed("jsonish", `{"user":"x"}`, nil, false)
	got, err = s.Get("jsonish", ctx)
	if err != nil || string(got.Value) != `{"user":"x"}` {
		t.Errorf("jsonish = %q, %v", got.Value, err)
	}
}

func TestVersioning(t *testing.T) {
	fv := newFakeVault()
	s := newTestStore(t, fv, nil)
	for _, v := range []string{"one", "two", "three"} {
		if err := s.Write("rotating", &secrets.Credential{Value: []byte(v)}, ctx); err != nil {
			t.Fatalf("Write: %v", err)
		}
	}
	latest, err := s.Get("rotating", ctx)
	if err != nil || string(latest.Value) != "three" || latest.MetaData[MetaVersionID] != "ver3" {
		t.Fatalf("latest = %+v, %v", latest, err)
	}
	// Caller supplied no Version, so the Key Vault version is surfaced.
	if latest.Version != "ver3" {
		t.Errorf("Version = %q", latest.Version)
	}
	old, err := s.GetVersion("rotating", "ver1", ctx)
	if err != nil || string(old.Value) != "one" {
		t.Fatalf("GetVersion(ver1) = %+v, %v", old, err)
	}
	if _, err := s.GetVersion("rotating", "nope", ctx); !errors.Is(err, ErrSecretNotFound) {
		t.Errorf("missing version err = %v", err)
	}
}

func TestGet_NotFound(t *testing.T) {
	s := newTestStore(t, newFakeVault(), nil)
	_, err := s.Get("missing", ctx)
	if !errors.Is(err, ErrSecretNotFound) {
		t.Fatalf("err = %v, want ErrSecretNotFound", err)
	}
	var re *azcore.ResponseError
	if !errors.As(err, &re) || re.ErrorCode != "SecretNotFound" {
		t.Errorf("original ResponseError not preserved: %v", err)
	}
}

func TestErrorMapping(t *testing.T) {
	for _, tc := range []struct {
		status int
		want   error
	}{
		{403, secrets.ErrForbidden},
		{401, secrets.ErrForbidden},
	} {
		fv := newFakeVault()
		fv.failWith = tc.status
		s := newTestStore(t, fv, nil)
		if _, err := s.Get("k", ctx); !errors.Is(err, tc.want) {
			t.Errorf("status %d: err = %v, want %v", tc.status, err, tc.want)
		}
	}
	fv := newFakeVault()
	fv.failWith = 400
	s := newTestStore(t, fv, nil)
	_, err := s.Get("k", ctx)
	if err == nil || errors.Is(err, ErrSecretNotFound) || errors.Is(err, secrets.ErrForbidden) {
		t.Errorf("400: err = %v", err)
	}
	if _, err := s.List(ctx); err == nil || !strings.Contains(err.Error(), "list failed") {
		t.Errorf("list err = %v", err)
	}
	if err := s.Write("k", &secrets.Credential{}, ctx); err == nil {
		t.Error("write: expected error")
	}
}

func TestInvalidKeys(t *testing.T) {
	fv := newFakeVault()
	s := newTestStore(t, fv, nil)
	long := strings.Repeat("a", 128)
	for _, k := range []string{"", long} {
		if _, err := s.Get(k, ctx); !errors.Is(err, ErrInvalidKey) {
			t.Errorf("Get(%q): %v", k, err)
		}
		if err := s.Write(k, &secrets.Credential{}, ctx); !errors.Is(err, ErrInvalidKey) {
			t.Errorf("Write(%q): %v", k, err)
		}
		if err := s.Delete(k, ctx); !errors.Is(err, ErrInvalidKey) {
			t.Errorf("Delete(%q): %v", k, err)
		}
		if err := s.Purge(k, ctx); !errors.Is(err, ErrInvalidKey) {
			t.Errorf("Purge(%q): %v", k, err)
		}
		if err := s.Recover(k, ctx); !errors.Is(err, ErrInvalidKey) {
			t.Errorf("Recover(%q): %v", k, err)
		}
	}
	if fv.totalCalls() != 0 {
		t.Errorf("invalid keys reached the vault: %v", fv.calls)
	}
}

func TestDeletePurgeRecover(t *testing.T) {
	fv := newFakeVault()
	s := newTestStore(t, fv, nil)
	key := "svc/token"
	if err := s.Write(key, &secrets.Credential{Value: []byte("v1")}, ctx); err != nil {
		t.Fatal(err)
	}
	if err := s.Delete(key, ctx); err != nil {
		t.Fatalf("Delete: %v", err)
	}
	if _, err := s.Get(key, ctx); !errors.Is(err, ErrSecretNotFound) {
		t.Errorf("Get after delete: %v", err)
	}
	// Name is reserved while soft-deleted.
	if err := s.Write(key, &secrets.Credential{Value: []byte("v2")}, ctx); !errors.Is(err, ErrSecretDeleted) {
		t.Errorf("Write while deleted: %v, want ErrSecretDeleted", err)
	}
	if err := s.Recover(key, ctx); err != nil {
		t.Fatalf("Recover: %v", err)
	}
	if got, err := s.Get(key, ctx); err != nil || string(got.Value) != "v1" {
		t.Fatalf("Get after recover = %v, %v", got, err)
	}
	if err := s.Delete(key, ctx); err != nil {
		t.Fatal(err)
	}
	if err := s.Purge(key, ctx); err != nil {
		t.Fatalf("Purge: %v", err)
	}
	if err := s.Write(key, &secrets.Credential{Value: []byte("v3")}, ctx); err != nil {
		t.Errorf("Write after purge: %v", err)
	}
	if err := s.Delete("absent", ctx); !errors.Is(err, ErrSecretNotFound) {
		t.Errorf("Delete absent: %v", err)
	}
	if err := s.Purge("absent", ctx); !errors.Is(err, ErrSecretNotFound) {
		t.Errorf("Purge absent: %v", err)
	}
	if err := s.Recover("absent", ctx); !errors.Is(err, ErrSecretNotFound) {
		t.Errorf("Recover absent: %v", err)
	}
}

func TestList(t *testing.T) {
	fv := newFakeVault()
	s := newTestStore(t, fv, nil)
	keys := []string{"a", "tenants/acme/db-pass", "x.y_z", "Mixed-Case", "üñí"}
	for _, k := range keys {
		if err := s.Write(k, &secrets.Credential{Value: []byte(k)}, ctx); err != nil {
			t.Fatalf("Write(%q): %v", k, err)
		}
	}
	// Foreign names that the encoder did not produce are skipped.
	fv.seed("created-by-portal", "x", nil, false)
	fv.seed("cert-backed", "x", nil, true)

	got, err := s.List(ctx)
	if err != nil {
		t.Fatalf("List: %v", err)
	}
	sort.Strings(got)
	want := append([]string(nil), keys...)
	sort.Strings(want)
	if !reflect.DeepEqual(got, want) {
		t.Errorf("List = %q, want %q", got, want)
	}
	if fv.count("GET secrets") < 2 {
		t.Errorf("expected paging, got %d list calls", fv.count("GET secrets"))
	}

	// Raw encoder sees the portal secret (but not the escaped ones that
	// still look valid, which is expected: raw lists every valid name).
	raw := newTestStore(t, fv, &KeyVaultSecretStoreConfig{KeyEncoder: RawKeyEncoder})
	rk, err := raw.List(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if !contains(rk, "created-by-portal") {
		t.Errorf("raw List = %q", rk)
	}
	if g, err := raw.Get("created-by-portal", ctx); err != nil || string(g.Value) != "x" {
		t.Errorf("raw Get = %v, %v", g, err)
	}
}

func TestList_TagFilter(t *testing.T) {
	fv := newFakeVault()
	fv.seed("prod1", "x", map[string]string{"env": "prod", "app": "golly"}, false)
	fv.seed("prod2", "x", map[string]string{"env": "prod"}, false)
	fv.seed("dev1", "x", map[string]string{"env": "dev", "app": "golly"}, false)
	s := newTestStore(t, fv, &KeyVaultSecretStoreConfig{TagFilter: map[string]string{"env": "prod", "app": "golly"}})
	got, err := s.List(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if !reflect.DeepEqual(got, []string{"prod1"}) {
		t.Errorf("List = %q", got)
	}
}

func TestWrite_UsesTenantTags(t *testing.T) {
	fv := newFakeVault()
	s := newTestStore(t, fv, &KeyVaultSecretStoreConfig{TagFilter: map[string]string{"env": "global", "app": "golly"}},
		WithTenantTags(map[string]string{"tenant": "acme"}))
	if err := s.Write("api-key", &secrets.Credential{Value: []byte("k")}, ctx); err != nil {
		t.Fatal(err)
	}
	if got := fv.latest("api--key").tags; !reflect.DeepEqual(got, map[string]string{"tenant": "acme"}) {
		t.Errorf("tags = %v, want only tenant tags", got)
	}

	// Config field works as well.
	s2 := newTestStore(t, fv, &KeyVaultSecretStoreConfig{TenantTags: map[string]string{"tenant": "globex"}})
	if err := s2.Write("other", &secrets.Credential{Value: []byte("k")}, ctx); err != nil {
		t.Fatal(err)
	}
	if got := fv.latest("other").tags; got["tenant"] != "globex" {
		t.Errorf("tags = %v", got)
	}
}

func TestWrite_FallsBackToTagFilter(t *testing.T) {
	fv := newFakeVault()
	s := newTestStore(t, fv, nil, WithTagFilter(map[string]string{"env": "global"}), WithTenantTags(nil))
	if err := s.Write("k", &secrets.Credential{Value: []byte("v")}, ctx); err != nil {
		t.Fatal(err)
	}
	if got := fv.latest("k").tags; !reflect.DeepEqual(got, map[string]string{"env": "global"}) {
		t.Errorf("tags = %v", got)
	}
}

func TestWrite_TooManyTags(t *testing.T) {
	tags := map[string]string{}
	for i := 0; i < MaxTags+1; i++ {
		tags[string(rune('a'+i))] = "v"
	}
	fv := newFakeVault()
	s := newTestStore(t, fv, nil, WithTenantTags(tags))
	if err := s.Write("k", &secrets.Credential{}, ctx); !errors.Is(err, ErrTooManyTags) {
		t.Errorf("err = %v", err)
	}
	if fv.totalCalls() != 0 {
		t.Error("request should not be sent")
	}
}

func TestCache(t *testing.T) {
	fv := newFakeVault()
	s := newTestStore(t, fv, nil, WithCacheTTL(time.Hour))
	if err := s.Write("k", &secrets.Credential{Value: []byte("v")}, ctx); err != nil {
		t.Fatal(err)
	}
	for i := 0; i < 3; i++ {
		if got, err := s.Get("k", ctx); err != nil || string(got.Value) != "v" {
			t.Fatalf("Get = %v, %v", got, err)
		}
	}
	if n := fv.count("GET secrets"); n != 0 {
		t.Errorf("cached Get hit the vault %d times", n)
	}
	s.ClearCache()
	if _, err := s.Get("k", ctx); err != nil {
		t.Fatal(err)
	}
	if _, err := s.Get("k", ctx); err != nil {
		t.Fatal(err)
	}
	if n := fv.count("GET secrets"); n != 1 {
		t.Errorf("after ClearCache expected 1 vault call, got %d", n)
	}
	if err := s.Delete("k", ctx); err != nil {
		t.Fatal(err)
	}
	if _, err := s.Get("k", ctx); !errors.Is(err, ErrSecretNotFound) {
		t.Errorf("Get after Delete served from cache: %v", err)
	}

	// TTL 0 disables caching.
	fv2 := newFakeVault()
	s2 := newTestStore(t, fv2, nil)
	_ = s2.Write("k", &secrets.Credential{Value: []byte("v")}, ctx)
	_, _ = s2.Get("k", ctx)
	_, _ = s2.Get("k", ctx)
	if n := fv2.count("GET secrets"); n != 2 {
		t.Errorf("no-cache: expected 2 vault calls, got %d", n)
	}
}

func TestConcurrentAccess(t *testing.T) {
	fv := newFakeVault()
	s := newTestStore(t, fv, nil, WithCacheTTL(time.Minute))
	var wg sync.WaitGroup
	for i := 0; i < 16; i++ {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			k := "k" + string(rune('a'+i%4))
			_ = s.Write(k, &secrets.Credential{Value: []byte("v")}, ctx)
			_, _ = s.Get(k, ctx)
			if i%5 == 0 {
				s.ClearCache()
			}
		}(i)
	}
	wg.Wait()
}

func TestAccessors(t *testing.T) {
	s := newTestStore(t, newFakeVault(), nil, WithKeyEncoder(RawKeyEncoder), WithKeyEncoder(nil))
	if s.Provider() != AzureKeyVaultProvider {
		t.Errorf("Provider = %q", s.Provider())
	}
	if s.GetClient() == nil {
		t.Error("GetClient nil")
	}
	if _, err := s.SecretName("a/b"); !errors.Is(err, ErrInvalidKey) {
		t.Error("WithKeyEncoder(Raw) not applied")
	}
	var _ secrets.Store = s
}

func TestIDHelpers(t *testing.T) {
	for id, want := range map[string][2]string{
		"https://v.vault.azure.net/secrets/name/ver": {"name", "ver"},
		"https://v.vault.azure.net/secrets/name":     {"name", ""},
		"https://v.vault.azure.net/":                 {"", ""},
		"":                                           {"", ""},
	} {
		if n, v := nameFromID(id), versionFromID(id); n != want[0] || v != want[1] {
			t.Errorf("%q -> (%q,%q), want %v", id, n, v, want)
		}
	}
	if got := fromAzTags(map[string]*string{"a": nil}); got["a"] != "" {
		t.Errorf("fromAzTags = %v", got)
	}
}

func contains(list []string, v string) bool {
	for _, s := range list {
		if s == v {
			return true
		}
	}
	return false
}
