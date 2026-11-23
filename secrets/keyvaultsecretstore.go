package secrets

import (
	"context"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"strings"
	"sync"
	"time"
	"unicode/utf8"

	"github.com/Azure/azure-sdk-for-go/sdk/azcore"
	"github.com/Azure/azure-sdk-for-go/sdk/security/keyvault/azsecrets"
	"oss.nandlabs.io/golly-azure/azcfg"
	secrets "oss.nandlabs.io/golly/secrets"
)

const (
	// AzureKeyVaultProvider is the provider name reported by Provider().
	AzureKeyVaultProvider = "azure-key-vault"

	// DefaultVaultDNSSuffix is the Key Vault DNS suffix of the Azure public
	// cloud. Sovereign clouds use e.g. "vault.azure.cn" or
	// "vault.usgovcloudapi.net".
	DefaultVaultDNSSuffix = "vault.azure.net"

	// ContentTypeJSON is the content type set on secrets written by the store.
	ContentTypeJSON = "application/json"

	// MaxTags is the Key Vault limit on tags per secret.
	MaxTags = 15
)

// Metadata keys populated by Get from Key Vault properties. Keys written by
// the caller in Credential.MetaData are preserved; these are added on top.
const (
	MetaVersionID   = "azure_version_id"
	MetaSecretID    = "azure_secret_id"
	MetaVaultURL    = "azure_vault_url"
	MetaContentType = "azure_content_type"
	MetaTags        = "azure_tags"
	MetaEnabled     = "azure_enabled"
	MetaCreated     = "azure_created"
	MetaUpdated     = "azure_updated"
	MetaExpires     = "azure_expires"
	MetaNotBefore   = "azure_not_before"
)

var (
	// ErrVaultURLRequired is returned when no vault URL can be resolved
	// from the configuration.
	ErrVaultURLRequired = errors.New("azure keyvault: vault URL or vault name is required")
	// ErrInvalidKey is returned when a key cannot be mapped to a valid Key
	// Vault secret name.
	ErrInvalidKey = errors.New("azure keyvault: invalid secret key")
	// ErrSecretNotFound is returned when the secret (or version) does not
	// exist.
	ErrSecretNotFound = errors.New("azure keyvault: secret not found")
	// ErrSecretDeleted is returned when writing to a name that is in the
	// soft-deleted (recoverable) state, or purging a secret that is still
	// being deleted. Recover or Purge it first.
	ErrSecretDeleted = errors.New("azure keyvault: secret is deleted but recoverable")
	// ErrTooManyTags is returned when a write would exceed MaxTags.
	ErrTooManyTags = errors.New("azure keyvault: too many tags")
)

// KeyVaultSecretStore implements the golly secrets.Store interface using
// Azure Key Vault secrets.
//
// Beyond Get/Write/Provider it offers List, GetVersion, Delete (soft delete),
// Recover and Purge. Those extra methods are not part of secrets.Store and
// therefore are NOT routed through secrets.Namespaced/WithAuthorizer; when a
// store is shared between tenants, only hand the namespaced secrets.Store to
// tenant code and enforce authorization at the call site for the others.
type KeyVaultSecretStore struct {
	client     *azsecrets.Client
	vaultURL   string
	encoder    KeyEncoder
	tagFilter  map[string]string
	tenantTags map[string]string
	cacheTTL   time.Duration

	mutex    sync.RWMutex
	cache    map[string]*secrets.Credential
	lastSync map[string]time.Time
}

// KeyVaultSecretStoreConfig holds configuration for creating a
// KeyVaultSecretStore.
//
// The vault URL is resolved in this order:
//  1. VaultURL
//  2. Azure.Endpoint (azcfg override, e.g. a private endpoint)
//  3. VaultName -> https://<VaultName>.<VaultDNSSuffix>/
//  4. Azure.AccountName treated as the vault name
type KeyVaultSecretStoreConfig struct {
	VaultURL       string            // Full vault URL, e.g. https://myvault.vault.azure.net/
	VaultName      string            // Short vault name
	VaultDNSSuffix string            // Defaults to DefaultVaultDNSSuffix
	Azure          *azcfg.Config     // Credential, client options, endpoint. nil = azcfg default config
	TagFilter      map[string]string // Tags a secret must carry to appear in List; also written when TenantTags is empty
	TenantTags     map[string]string // Tags written on every Write (see WithTenantTags)
	CacheTTL       time.Duration     // Cache TTL (0 = no caching)
	KeyEncoder     KeyEncoder        // Key <-> secret name mapping. nil = DefaultKeyEncoder

	// DisableChallengeResourceVerification is passed through to azsecrets;
	// needed only for non-standard vault domains.
	DisableChallengeResourceVerification bool
}

// Option configures a KeyVaultSecretStore at construction time.
type Option func(*KeyVaultSecretStore)

// WithTenantTags supplies a fixed set of tags that Write applies to every
// secret version it creates (Key Vault tags are set per version by
// SetSecret). Prefer this over reusing TagFilter for writes: TagFilter is a
// lookup filter and applying it to every write leaks the same tags across
// tenants.
//
//	tenantStore, _ := NewKeyVaultSecretStore(ctx, cfg,
//	    WithTenantTags(map[string]string{"tenant": tenantID}))
//	scoped := secrets.Namespace(tenantStore, "tenant/"+tenantID,
//	    secrets.WithAuthorizer(policy))
//
// When set, tenant tags replace (not merge with) TagFilter on writes; an
// empty map falls back to TagFilter.
func WithTenantTags(tags map[string]string) Option {
	return func(s *KeyVaultSecretStore) {
		if len(tags) == 0 {
			return
		}
		s.tenantTags = copyTags(tags)
	}
}

// WithKeyEncoder overrides the key <-> secret name mapping (default
// DefaultKeyEncoder).
func WithKeyEncoder(enc KeyEncoder) Option {
	return func(s *KeyVaultSecretStore) {
		if enc != nil {
			s.encoder = enc
		}
	}
}

// WithTagFilter sets the List tag filter (and write fallback tags).
func WithTagFilter(tags map[string]string) Option {
	return func(s *KeyVaultSecretStore) { s.tagFilter = copyTags(tags) }
}

// WithCacheTTL enables the in-memory read cache (0 disables it).
func WithCacheTTL(ttl time.Duration) Option {
	return func(s *KeyVaultSecretStore) { s.cacheTTL = ttl }
}

// ResolveVaultURL returns the vault URL cfg points at, following the order
// documented on KeyVaultSecretStoreConfig. The result always ends in '/'.
func ResolveVaultURL(cfg *KeyVaultSecretStoreConfig) (string, error) {
	if cfg == nil {
		return "", ErrVaultURLRequired
	}
	u := strings.TrimSpace(cfg.VaultURL)
	if u == "" && cfg.Azure != nil {
		u = strings.TrimSpace(cfg.Azure.Endpoint)
	}
	if u == "" {
		name := strings.TrimSpace(cfg.VaultName)
		if name == "" && cfg.Azure != nil {
			name = strings.TrimSpace(cfg.Azure.AccountName)
		}
		if name == "" {
			return "", ErrVaultURLRequired
		}
		suffix := strings.Trim(strings.TrimSpace(cfg.VaultDNSSuffix), ".")
		if suffix == "" {
			suffix = DefaultVaultDNSSuffix
		}
		u = "https://" + name + "." + suffix
	}
	if !strings.Contains(u, "://") {
		u = "https://" + u
	}
	if !strings.HasSuffix(u, "/") {
		u += "/"
	}
	return u, nil
}

// NewKeyVaultSecretStore creates a new Azure Key Vault-backed store. The
// credential and azcore client options come from cfg.Azure (falling back to
// azcfg.GetConfigOrDefault). Extra options are applied after cfg.
func NewKeyVaultSecretStore(ctx context.Context, cfg *KeyVaultSecretStoreConfig, opts ...Option) (*KeyVaultSecretStore, error) {
	_ = ctx // reserved; client construction does no I/O
	if cfg == nil {
		cfg = &KeyVaultSecretStoreConfig{}
	}
	azc := cfg.Azure
	if azc == nil {
		azc = azcfg.GetConfigOrDefault(nil, azcfg.DefaultConfigName)
	}
	resolved := *cfg
	resolved.Azure = azc
	vaultURL, err := ResolveVaultURL(&resolved)
	if err != nil {
		return nil, err
	}
	cred, err := azc.Credential()
	if err != nil {
		return nil, fmt.Errorf("azure keyvault: failed to resolve credential: %w", err)
	}
	client, err := azsecrets.NewClient(vaultURL, cred, &azsecrets.ClientOptions{
		ClientOptions:                        azc.ClientOpts(),
		DisableChallengeResourceVerification: cfg.DisableChallengeResourceVerification,
	})
	if err != nil {
		return nil, fmt.Errorf("azure keyvault: failed to create client: %w", err)
	}

	s := newStore(client, vaultURL)
	s.tagFilter = copyTags(cfg.TagFilter)
	s.cacheTTL = cfg.CacheTTL
	if cfg.KeyEncoder != nil {
		s.encoder = cfg.KeyEncoder
	}
	WithTenantTags(cfg.TenantTags)(s)
	for _, o := range opts {
		o(s)
	}
	return s, nil
}

func newStore(client *azsecrets.Client, vaultURL string) *KeyVaultSecretStore {
	return &KeyVaultSecretStore{
		client:   client,
		vaultURL: vaultURL,
		encoder:  DefaultKeyEncoder,
		cache:    make(map[string]*secrets.Credential),
		lastSync: make(map[string]time.Time),
	}
}

// envelope is the JSON document stored as the secret value. It matches the
// format used by the golly-aws and golly-gcp stores; binary values that are
// not valid UTF-8 are stored base64-encoded in value_b64.
type envelope struct {
	Value       *string                `json:"value,omitempty"`
	ValueB64    string                 `json:"value_b64,omitempty"`
	Version     string                 `json:"version"`
	LastUpdated int64                  `json:"last_updated"`
	MetaData    map[string]interface{} `json:"metadata,omitempty"`
}

// Get retrieves the latest version of a credential.
func (s *KeyVaultSecretStore) Get(key string, ctx context.Context) (*secrets.Credential, error) {
	if s.cacheTTL > 0 {
		s.mutex.RLock()
		cached, ok := s.cache[key]
		fresh := ok && time.Since(s.lastSync[key]) < s.cacheTTL
		s.mutex.RUnlock()
		if fresh {
			return cached, nil
		}
	}
	cred, err := s.GetVersion(key, "", ctx)
	if err != nil {
		return nil, err
	}
	s.putCache(key, cred)
	return cred, nil
}

// GetVersion retrieves a specific Key Vault version of a credential
// (version "" means latest). The result is never cached.
func (s *KeyVaultSecretStore) GetVersion(key, version string, ctx context.Context) (*secrets.Credential, error) {
	name, err := s.encoder.Encode(key)
	if err != nil {
		return nil, err
	}
	resp, err := s.client.GetSecret(ctx, name, version, nil)
	if err != nil {
		return nil, mapError("get", key, err)
	}
	return s.toCredential(&resp.Secret), nil
}

func (s *KeyVaultSecretStore) toCredential(sec *azsecrets.Secret) *secrets.Credential {
	cred := &secrets.Credential{MetaData: make(map[string]interface{})}
	raw := ""
	if sec.Value != nil {
		raw = *sec.Value
	}

	var env envelope
	if err := json.Unmarshal([]byte(raw), &env); err == nil && (env.Value != nil || env.ValueB64 != "") {
		switch {
		case env.Value != nil:
			cred.Value = []byte(*env.Value)
		default:
			if b, err := base64.StdEncoding.DecodeString(env.ValueB64); err == nil {
				cred.Value = b
			}
		}
		cred.Version = env.Version
		if env.LastUpdated > 0 {
			cred.LastUpdated = time.Unix(env.LastUpdated, 0)
		}
		for k, v := range env.MetaData {
			cred.MetaData[k] = v
		}
	} else {
		cred.Value = []byte(raw)
	}

	if sec.ID != nil {
		kvVersion := versionFromID(string(*sec.ID))
		cred.MetaData[MetaVersionID] = kvVersion
		cred.MetaData[MetaSecretID] = string(*sec.ID)
		if cred.Version == "" {
			cred.Version = kvVersion
		}
	}
	cred.MetaData[MetaVaultURL] = s.vaultURL
	if sec.ContentType != nil {
		cred.MetaData[MetaContentType] = *sec.ContentType
	}
	if len(sec.Tags) > 0 {
		cred.MetaData[MetaTags] = fromAzTags(sec.Tags)
	}
	if a := sec.Attributes; a != nil {
		if a.Enabled != nil {
			cred.MetaData[MetaEnabled] = *a.Enabled
		}
		if a.Created != nil {
			cred.MetaData[MetaCreated] = *a.Created
		}
		if a.Updated != nil {
			cred.MetaData[MetaUpdated] = *a.Updated
			if cred.LastUpdated.IsZero() {
				cred.LastUpdated = *a.Updated
			}
		}
		if a.Expires != nil {
			cred.MetaData[MetaExpires] = *a.Expires
		}
		if a.NotBefore != nil {
			cred.MetaData[MetaNotBefore] = *a.NotBefore
		}
	}
	if cred.LastUpdated.IsZero() {
		cred.LastUpdated = time.Now()
	}
	return cred
}

// Write stores a credential. Key Vault SetSecret is an upsert: it creates
// the secret if missing and otherwise adds a new version, so every Write
// produces a new Key Vault version (retrievable via GetVersion).
//
// Writing to a name that is soft-deleted fails with ErrSecretDeleted until
// the secret is recovered (Recover) or purged (Purge).
func (s *KeyVaultSecretStore) Write(key string, credential *secrets.Credential, ctx context.Context) error {
	if credential == nil {
		return fmt.Errorf("azure keyvault: credential must not be nil")
	}
	name, err := s.encoder.Encode(key)
	if err != nil {
		return err
	}

	lastUpdated := credential.LastUpdated
	if lastUpdated.IsZero() {
		lastUpdated = time.Now()
	}
	env := envelope{
		Version:     credential.Version,
		LastUpdated: lastUpdated.Unix(),
		MetaData:    credential.MetaData,
	}
	if utf8.Valid(credential.Value) {
		v := string(credential.Value)
		env.Value = &v
	} else {
		env.ValueB64 = base64.StdEncoding.EncodeToString(credential.Value)
	}
	body, err := json.Marshal(env)
	if err != nil {
		return fmt.Errorf("azure keyvault: failed to marshal credential: %w", err)
	}

	writeTags := s.tenantTags
	if len(writeTags) == 0 {
		writeTags = s.tagFilter
	}
	if len(writeTags) > MaxTags {
		return fmt.Errorf("%w: %d tags (max %d)", ErrTooManyTags, len(writeTags), MaxTags)
	}

	value := string(body)
	ct := ContentTypeJSON
	params := azsecrets.SetSecretParameters{
		Value:       &value,
		ContentType: &ct,
		Tags:        toAzTags(writeTags),
	}
	if _, err := s.client.SetSecret(ctx, name, params, nil); err != nil {
		return mapError("write", key, err)
	}

	s.putCache(key, credential)
	return nil
}

// List returns the golly keys of all (enabled or disabled) secrets in the
// vault whose names decode with the configured KeyEncoder and whose tags
// match every TagFilter entry. Secrets whose names were not produced by the
// encoder (e.g. created by other tools) are skipped. Soft-deleted secrets
// are not listed.
func (s *KeyVaultSecretStore) List(ctx context.Context) ([]string, error) {
	pager := s.client.NewListSecretPropertiesPager(nil)
	var results []string
	for pager.More() {
		page, err := pager.NextPage(ctx)
		if err != nil {
			return nil, mapError("list", "", err)
		}
		for _, p := range page.Value {
			if p == nil || p.ID == nil {
				continue
			}
			if p.Managed != nil && *p.Managed {
				continue // certificate-backed secrets
			}
			if !matchTags(p.Tags, s.tagFilter) {
				continue
			}
			key, ok := s.encoder.Decode(nameFromID(string(*p.ID)))
			if !ok {
				continue
			}
			results = append(results, key)
		}
	}
	return results, nil
}

// Delete soft-deletes a secret (all versions).
//
// Key Vault soft delete is always on: the secret moves to the deleted state
// for the vault's retention period (7-90 days), during which its name cannot
// be reused (Write returns ErrSecretDeleted). Use Recover to restore it or
// Purge to remove it permanently (requires the purge permission and is
// refused when the vault has purge protection enabled). Deletion completes
// asynchronously, so a Purge issued immediately after Delete may return
// ErrSecretDeleted ("being deleted"); retry after a short delay.
//
// Delete is not part of secrets.Store and bypasses secrets.Namespaced
// authorization; enforce policy at the call site.
func (s *KeyVaultSecretStore) Delete(key string, ctx context.Context) error {
	name, err := s.encoder.Encode(key)
	if err != nil {
		return err
	}
	if _, err := s.client.DeleteSecret(ctx, name, nil); err != nil {
		return mapError("delete", key, err)
	}
	s.dropCache(key)
	return nil
}

// Purge permanently deletes a soft-deleted secret. See Delete.
func (s *KeyVaultSecretStore) Purge(key string, ctx context.Context) error {
	name, err := s.encoder.Encode(key)
	if err != nil {
		return err
	}
	if _, err := s.client.PurgeDeletedSecret(ctx, name, nil); err != nil {
		return mapError("purge", key, err)
	}
	s.dropCache(key)
	return nil
}

// Recover restores a soft-deleted secret to its latest version. Recovery
// completes asynchronously on the service side.
func (s *KeyVaultSecretStore) Recover(key string, ctx context.Context) error {
	name, err := s.encoder.Encode(key)
	if err != nil {
		return err
	}
	if _, err := s.client.RecoverDeletedSecret(ctx, name, nil); err != nil {
		return mapError("recover", key, err)
	}
	return nil
}

// SecretName returns the Key Vault secret name a golly key maps to.
func (s *KeyVaultSecretStore) SecretName(key string) (string, error) {
	return s.encoder.Encode(key)
}

// Provider returns the provider name.
func (s *KeyVaultSecretStore) Provider() string { return AzureKeyVaultProvider }

// VaultURL returns the resolved vault URL.
func (s *KeyVaultSecretStore) VaultURL() string { return s.vaultURL }

// ClearCache clears the in-memory cache.
func (s *KeyVaultSecretStore) ClearCache() {
	s.mutex.Lock()
	defer s.mutex.Unlock()
	s.cache = make(map[string]*secrets.Credential)
	s.lastSync = make(map[string]time.Time)
}

// GetClient returns the underlying azsecrets client for advanced operations.
// Note that it addresses secrets by Key Vault name; use SecretName to map a
// golly key.
func (s *KeyVaultSecretStore) GetClient() *azsecrets.Client { return s.client }

func (s *KeyVaultSecretStore) putCache(key string, cred *secrets.Credential) {
	if s.cacheTTL <= 0 {
		return
	}
	s.mutex.Lock()
	s.cache[key] = cred
	s.lastSync[key] = time.Now()
	s.mutex.Unlock()
}

func (s *KeyVaultSecretStore) dropCache(key string) {
	s.mutex.Lock()
	delete(s.cache, key)
	delete(s.lastSync, key)
	s.mutex.Unlock()
}

// mapError wraps Key Vault errors with package / golly sentinels while
// keeping the original *azcore.ResponseError in the chain.
func mapError(op, key string, err error) error {
	var re *azcore.ResponseError
	if errors.As(err, &re) {
		switch re.StatusCode {
		case http.StatusNotFound:
			return fmt.Errorf("%w: %s %q: %w", ErrSecretNotFound, op, key, err)
		case http.StatusConflict:
			return fmt.Errorf("%w: %s %q: %w", ErrSecretDeleted, op, key, err)
		case http.StatusUnauthorized, http.StatusForbidden:
			return fmt.Errorf("%w: azure keyvault %s %q: %w", secrets.ErrForbidden, op, key, err)
		}
	}
	if key == "" {
		return fmt.Errorf("azure keyvault: %s failed: %w", op, err)
	}
	return fmt.Errorf("azure keyvault: %s %q failed: %w", op, key, err)
}

// nameFromID extracts the secret name from a Key Vault secret identifier
// (https://<vault>/secrets/<name>[/<version>]). azsecrets.ID.Name panics on
// identifiers without a name segment, so parse defensively.
func nameFromID(id string) string {
	if i := strings.Index(id, "://"); i >= 0 {
		id = id[i+3:]
	}
	parts := strings.Split(id, "/")
	if len(parts) < 3 {
		return ""
	}
	return parts[2]
}

// versionFromID extracts the version segment of a secret identifier, or "".
func versionFromID(id string) string {
	if i := strings.Index(id, "://"); i >= 0 {
		id = id[i+3:]
	}
	parts := strings.Split(id, "/")
	if len(parts) < 4 {
		return ""
	}
	return parts[3]
}

func copyTags(in map[string]string) map[string]string {
	if len(in) == 0 {
		return nil
	}
	out := make(map[string]string, len(in))
	for k, v := range in {
		out[k] = v
	}
	return out
}

func toAzTags(in map[string]string) map[string]*string {
	if len(in) == 0 {
		return nil
	}
	out := make(map[string]*string, len(in))
	for k, v := range in {
		v := v
		out[k] = &v
	}
	return out
}

func fromAzTags(in map[string]*string) map[string]string {
	out := make(map[string]string, len(in))
	for k, v := range in {
		if v != nil {
			out[k] = *v
		} else {
			out[k] = ""
		}
	}
	return out
}

func matchTags(have map[string]*string, want map[string]string) bool {
	for k, v := range want {
		got, ok := have[k]
		if !ok || got == nil || *got != v {
			return false
		}
	}
	return true
}
