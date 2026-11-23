package entra

import (
	"context"
	"crypto"
	"crypto/rsa"
	"crypto/sha256"
	"encoding/base64"
	"encoding/binary"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"math/big"
	"net/http"
	"strings"
	"sync"
	"time"

	"oss.nandlabs.io/golly/auth"
)

// Well-known tenant segments accepted by NewVerifier in place of a
// tenant GUID.
//
//   - TenantCommon and TenantOrganizations put the verifier in
//     multi-tenant mode: tokens from any Entra tenant are cryptographically
//     acceptable, and the tid claim is then checked against the allow-list
//     configured via WithAllowedTenants (or accepted unconditionally with
//     WithAnyTenant).
//   - TenantConsumers pins the verifier to the Microsoft-account (MSA)
//     tenant, ConsumersTenantID.
const (
	TenantCommon        = "common"
	TenantOrganizations = "organizations"
	TenantConsumers     = "consumers"

	// ConsumersTenantID is the fixed tenant id under which personal
	// Microsoft accounts are issued tokens.
	ConsumersTenantID = "9188040d-6c67-4c5b-b112-36a304b66dad"
)

// Token versions as surfaced in the "ver" claim. Entra mints v1.0
// tokens (issuer https://sts.windows.net/<tid>/) for resources whose app
// manifest sets accessTokenAcceptedVersion to null/1 and v2.0 tokens
// (issuer https://login.microsoftonline.com/<tid>/v2.0) otherwise.
const (
	TokenVersion1 = "1.0"
	TokenVersion2 = "2.0"
)

// DefaultAuthorityHost is the public-cloud Entra authority. Override it
// with WithAuthorityHost for sovereign clouds
// (https://login.microsoftonline.us, https://login.chinacloudapi.cn).
const DefaultAuthorityHost = "https://login.microsoftonline.com"

// DefaultV1IssuerHost is the host component of v1.0 token issuers in the
// public cloud. Override with WithV1IssuerHost for sovereign clouds.
const DefaultV1IssuerHost = "https://sts.windows.net"

// DefaultKeyTTL is the fallback cache lifetime for JWKS entries when
// WithKeyTTL is not supplied. Entra rotates signing keys a few times a
// year and publishes the next key well in advance, so an hour is a
// conservative default.
const DefaultKeyTTL = time.Hour

// DefaultLeeway is the clock-skew allowance used when validating exp,
// nbf and iat during VerifyToken.
const DefaultLeeway = 60 * time.Second

// DefaultMinRefreshInterval bounds how often a token carrying an
// unknown kid can force a JWKS re-fetch. Without it an attacker could
// turn every forged request into an outbound call to Entra.
const DefaultMinRefreshInterval = 10 * time.Second

// ErrTenantNotAllowed is returned (wrapped together with
// auth.ErrIssuerMismatch) when a token's tid claim does not match the
// configured tenant or is not on the multi-tenant allow-list.
var ErrTenantNotAllowed = errors.New("auth/entra: tenant not allowed")

// Verifier is a Microsoft Entra ID (formerly Azure AD) JWT verifier.
// It caches the tenant's JWKS per-kid and refreshes them on demand when
// a token references an unknown kid. Verifier is safe for concurrent
// use and implements auth.Verifier.
type Verifier struct {
	tenant       string // as configured (GUID or common/organizations/consumers)
	tenantID     string // pinned tenant GUID; "" in multi-tenant mode
	multi        bool
	anyTenant    bool
	allowed      map[string]struct{}
	audiences    map[string]struct{}
	versions     map[string]struct{}
	authority    string
	v1Host       string
	jwksURL      string
	jwksExplicit bool
	discovery    bool
	discoveryURL string
	ttl          time.Duration
	leeway       time.Duration
	minRefresh   time.Duration
	httpc        *http.Client
	now          func() time.Time

	mu          sync.RWMutex
	keys        map[string]*keyEntry
	lastRefresh time.Time
	resolved    bool // discovery has resolved jwksURL
}

type keyEntry struct {
	pub     *rsa.PublicKey
	expires time.Time
}

// Option customizes a Verifier at construction time.
type Option func(*Verifier)

// WithJWKSURL overrides the JWKS endpoint (and disables OpenID
// discovery). Primarily used by tests.
func WithJWKSURL(url string) Option {
	return func(v *Verifier) {
		if url != "" {
			v.jwksURL = url
			v.jwksExplicit = true
		}
	}
}

// WithOpenIDDiscovery makes the verifier resolve the JWKS endpoint from
// the tenant's OpenID Connect discovery document
// (<authority>/<tenant>/v2.0/.well-known/openid-configuration) on the
// first key fetch instead of using the well-known
// <authority>/<tenant>/discovery/v2.0/keys URL directly.
func WithOpenIDDiscovery() Option { return func(v *Verifier) { v.discovery = true } }

// WithDiscoveryURL sets an explicit OpenID discovery document URL and
// enables discovery. Ignored when WithJWKSURL is also supplied.
func WithDiscoveryURL(url string) Option {
	return func(v *Verifier) {
		if url != "" {
			v.discoveryURL = url
			v.discovery = true
		}
	}
}

// WithAuthorityHost overrides the Entra authority host used to build
// the v2.0 issuer, JWKS and discovery URLs (e.g.
// "https://login.microsoftonline.us" for Azure Government).
func WithAuthorityHost(host string) Option {
	return func(v *Verifier) {
		if host != "" {
			v.authority = strings.TrimRight(host, "/")
		}
	}
}

// WithV1IssuerHost overrides the host used to build v1.0 issuers
// (default https://sts.windows.net).
func WithV1IssuerHost(host string) Option {
	return func(v *Verifier) {
		if host != "" {
			v.v1Host = strings.TrimRight(host, "/")
		}
	}
}

// WithHTTPClient overrides the HTTP client used to fetch JWKS and the
// discovery document.
func WithHTTPClient(c *http.Client) Option {
	return func(v *Verifier) {
		if c != nil {
			v.httpc = c
		}
	}
}

// WithKeyTTL sets the cache lifetime for fetched JWKS entries.
func WithKeyTTL(d time.Duration) Option {
	return func(v *Verifier) {
		if d > 0 {
			v.ttl = d
		}
	}
}

// WithLeeway sets the clock-skew allowance used by VerifyToken.
func WithLeeway(d time.Duration) Option {
	return func(v *Verifier) {
		if d >= 0 {
			v.leeway = d
		}
	}
}

// WithMinRefreshInterval sets the minimum delay between two JWKS
// fetches triggered by an unknown kid. Zero disables the rate limit.
func WithMinRefreshInterval(d time.Duration) Option {
	return func(v *Verifier) {
		if d >= 0 {
			v.minRefresh = d
		}
	}
}

// WithNow overrides the time source used by VerifyToken and the key
// cache. Primarily used by tests.
func WithNow(fn func() time.Time) Option {
	return func(v *Verifier) {
		if fn != nil {
			v.now = fn
		}
	}
}

// WithAudiences adds extra accepted aud values on top of the client id
// passed to NewVerifier (and its default "api://<clientID>" App ID URI).
// Use it for custom App ID URIs such as "https://api.contoso.com".
func WithAudiences(aud ...string) Option {
	return func(v *Verifier) {
		for _, a := range aud {
			if a != "" {
				v.audiences[a] = struct{}{}
			}
		}
	}
}

// WithAllowedTenants sets the tenant-id allow-list used in multi-tenant
// mode (tenant "common" or "organizations"). Tenant ids are compared
// case-insensitively. In single-tenant mode the option is ignored: the
// configured tenant is the only one accepted.
func WithAllowedTenants(tenantIDs ...string) Option {
	return func(v *Verifier) {
		for _, t := range tenantIDs {
			if t != "" {
				v.allowed[strings.ToLower(t)] = struct{}{}
			}
		}
	}
}

// WithAnyTenant accepts tokens from every Entra tenant in multi-tenant
// mode. This is only appropriate for genuinely multi-tenant SaaS apps
// that perform their own per-tenant onboarding check on the tid claim.
func WithAnyTenant() Option { return func(v *Verifier) { v.anyTenant = true } }

// WithTokenVersions restricts the accepted token versions to the given
// "ver" values (TokenVersion1 and/or TokenVersion2). By default both
// are accepted.
func WithTokenVersions(vers ...string) Option {
	return func(v *Verifier) {
		if len(vers) == 0 {
			return
		}
		set := make(map[string]struct{}, len(vers))
		for _, s := range vers {
			set[s] = struct{}{}
		}
		v.versions = set
	}
}

// NewVerifier constructs an Entra ID JWT verifier.
//
// tenant is either the directory (tenant) GUID, which pins the verifier
// to that tenant, or one of TenantCommon / TenantOrganizations (multi-
// tenant mode; requires WithAllowedTenants or WithAnyTenant) or
// TenantConsumers. clientID is the application (client) id of the API
// the tokens are minted for; aud must equal it or "api://<clientID>"
// (extend with WithAudiences).
func NewVerifier(tenant, clientID string, opts ...Option) (*Verifier, error) {
	tenant = strings.TrimSpace(tenant)
	if tenant == "" {
		return nil, errors.New("auth/entra: tenant is required")
	}
	if clientID == "" {
		return nil, errors.New("auth/entra: clientID is required")
	}
	v := &Verifier{
		tenant:    tenant,
		allowed:   make(map[string]struct{}),
		audiences: map[string]struct{}{clientID: {}, "api://" + clientID: {}},
		versions: map[string]struct{}{
			TokenVersion1: {},
			TokenVersion2: {},
		},
		authority:  DefaultAuthorityHost,
		v1Host:     DefaultV1IssuerHost,
		ttl:        DefaultKeyTTL,
		leeway:     DefaultLeeway,
		minRefresh: DefaultMinRefreshInterval,
		httpc:      http.DefaultClient,
		now:        time.Now,
		keys:       make(map[string]*keyEntry),
	}
	switch strings.ToLower(tenant) {
	case TenantCommon, TenantOrganizations:
		v.multi = true
	case TenantConsumers:
		v.tenantID = ConsumersTenantID
	default:
		if !isGUID(tenant) {
			return nil, fmt.Errorf("auth/entra: tenant %q must be a tenant GUID or one of %q, %q, %q",
				tenant, TenantCommon, TenantOrganizations, TenantConsumers)
		}
		v.tenantID = strings.ToLower(tenant)
	}
	for _, o := range opts {
		if o != nil {
			o(v)
		}
	}
	if v.multi && !v.anyTenant && len(v.allowed) == 0 {
		return nil, fmt.Errorf("auth/entra: multi-tenant mode (%q) requires WithAllowedTenants or WithAnyTenant", tenant)
	}
	if !v.jwksExplicit {
		v.jwksURL = v.authority + "/" + tenant + "/discovery/v2.0/keys"
		if v.discovery && v.discoveryURL == "" {
			v.discoveryURL = v.authority + "/" + tenant + "/v2.0/.well-known/openid-configuration"
		}
	}
	if v.jwksExplicit || !v.discovery {
		v.resolved = true
	}
	return v, nil
}

// Alg reports the algorithm this verifier validates. Entra signs with
// RS256.
func (v *Verifier) Alg() string { return auth.AlgRS256 }

// Tenant returns the tenant segment the verifier was configured with.
func (v *Verifier) Tenant() string { return v.tenant }

// JWKSURL returns the JWKS endpoint currently in use. When discovery is
// enabled and has not run yet it returns the default keys URL.
func (v *Verifier) JWKSURL() string {
	v.mu.RLock()
	defer v.mu.RUnlock()
	return v.jwksURL
}

// header is the subset of the JOSE header Entra emits. v1 tokens carry
// x5t (equal to kid); some Microsoft Graph access tokens carry a nonce
// and are not verifiable by third parties.
type header struct {
	Alg string `json:"alg"`
	Kid string `json:"kid"`
	X5t string `json:"x5t"`
}

func (h header) keyID() string {
	if h.Kid != "" {
		return h.Kid
	}
	return h.X5t
}

// Verify implements auth.Verifier by extracting the kid (or x5t) from
// the JWS header carried in signingInput, resolving the RSA public key
// (with a JWKS refresh on unknown kid), and validating the RS256
// signature. It does not perform claim validation — use VerifyToken.
func (v *Verifier) Verify(signingInput, sig []byte) error {
	dot := indexByte(signingInput, '.')
	if dot < 0 {
		return auth.ErrInvalidToken
	}
	hdr, err := decodeHeader(string(signingInput[:dot]))
	if err != nil {
		return auth.ErrInvalidToken
	}
	if hdr.Alg != auth.AlgRS256 {
		return auth.ErrAlgNotAllowed
	}
	kid := hdr.keyID()
	if kid == "" {
		return fmt.Errorf("%w: token missing kid", auth.ErrKeyNotFound)
	}
	pub, err := v.keyForKid(context.Background(), kid)
	if err != nil {
		return fmt.Errorf("%w: %v", auth.ErrKeyNotFound, err)
	}
	sum := sha256.Sum256(signingInput)
	if err := rsa.VerifyPKCS1v15(pub, crypto.SHA256, sum[:], sig); err != nil {
		return auth.ErrInvalidSig
	}
	return nil
}

// Keyset returns an auth.Verifier bound to a specific kid, suitable for
// auth.VerifyOptions.Keyset.
func (v *Verifier) Keyset(kid string) (auth.Verifier, error) {
	pub, err := v.keyForKid(context.Background(), kid)
	if err != nil {
		return nil, fmt.Errorf("%w: %v", auth.ErrKeyNotFound, err)
	}
	return auth.NewRSVerifier(auth.AlgRS256, pub)
}

// VerifyToken parses, signature-checks and claim-validates a compact
// Entra-issued JWT (id token or access token). It enforces:
//   - alg = RS256, signature valid against the tenant JWKS
//   - exp in the future and nbf/iat in the past (with leeway)
//   - ver is an accepted token version (default 1.0 and 2.0)
//   - iss matches the version's issuer format for the token's tenant:
//     v2.0: <authority>/<tid>/v2.0, v1.0: https://sts.windows.net/<tid>/
//   - tid equals the pinned tenant, or — in multi-tenant mode — is on
//     the allow-list (unless WithAnyTenant)
//   - aud contains the client id, "api://<clientID>", or an audience
//     added with WithAudiences
//   - sub is non-empty
//
// Entra-specific claims (tid, oid, ver, scp, roles, groups, azp, appid,
// preferred_username, upn, ...) are surfaced via Claims.Extra; see the
// ObjectID, TenantID, Scopes, Roles and Groups helpers.
func (v *Verifier) VerifyToken(ctx context.Context, token string) (*auth.Claims, error) {
	if ctx == nil {
		ctx = context.Background()
	}
	// Prime the JWKS cache using the caller-supplied context so the
	// network fetch respects any request-scoped deadline.
	if kid := peekKid(token); kid != "" {
		if _, err := v.keyForKid(ctx, kid); err != nil {
			return nil, fmt.Errorf("%w: %v", auth.ErrKeyNotFound, err)
		}
	}
	claims, err := auth.Verify(token, auth.VerifyOptions{
		Algs:             []string{auth.AlgRS256},
		VerifierFallback: v,
		// Issuer and audience are validated below: the issuer depends
		// on the token version and tid, and several audiences are
		// acceptable.
		Leeway: v.leeway,
		Now:    v.now,
	})
	if err != nil {
		return nil, err
	}
	if claims.IssuedAt != nil && claims.IssuedAt.After(v.now().Add(v.leeway)) {
		return nil, auth.ErrNotYetValid
	}
	if err := v.checkIssuer(claims); err != nil {
		return nil, err
	}
	if !v.audienceOK(claims.Audience) {
		return nil, auth.ErrAudMismatch
	}
	if claims.Subject == "" {
		return nil, fmt.Errorf("%w: sub claim is empty", auth.ErrInvalidToken)
	}
	return claims, nil
}

// checkIssuer validates ver, tid and iss together.
func (v *Verifier) checkIssuer(c *auth.Claims) error {
	tid := strings.ToLower(stringClaim(c.Extra, "tid"))
	if tid == "" {
		return fmt.Errorf("%w: tid claim is missing", auth.ErrInvalidToken)
	}
	ver := stringClaim(c.Extra, "ver")
	if ver == "" {
		// Older id tokens omit ver; infer from the issuer shape.
		if strings.HasPrefix(c.Issuer, v.v1Host+"/") {
			ver = TokenVersion1
		} else {
			ver = TokenVersion2
		}
	}
	if _, ok := v.versions[ver]; !ok {
		return fmt.Errorf("%w: token version %q not accepted", auth.ErrInvalidToken, ver)
	}
	var want string
	switch ver {
	case TokenVersion1:
		want = v.v1Host + "/" + tid + "/"
	case TokenVersion2:
		want = v.authority + "/" + tid + "/v2.0"
	default:
		return fmt.Errorf("%w: unknown token version %q", auth.ErrInvalidToken, ver)
	}
	if !strings.EqualFold(c.Issuer, want) {
		return auth.ErrIssuerMismatch
	}
	if v.multi {
		if v.anyTenant {
			return nil
		}
		if _, ok := v.allowed[tid]; !ok {
			return fmt.Errorf("%w: %w: tid %q", auth.ErrIssuerMismatch, ErrTenantNotAllowed, tid)
		}
		return nil
	}
	if tid != v.tenantID {
		return fmt.Errorf("%w: %w: tid %q", auth.ErrIssuerMismatch, ErrTenantNotAllowed, tid)
	}
	return nil
}

func (v *Verifier) audienceOK(aud auth.Audience) bool {
	for _, a := range aud {
		if _, ok := v.audiences[a]; ok {
			return true
		}
	}
	return false
}

// keyForKid returns the cached public key for kid, refreshing the JWKS
// on a cache miss or when the cached entry has expired. Refreshes caused
// by unknown kids are rate-limited by the minimum refresh interval.
func (v *Verifier) keyForKid(ctx context.Context, kid string) (*rsa.PublicKey, error) {
	now := v.now()
	v.mu.RLock()
	entry, ok := v.keys[kid]
	v.mu.RUnlock()
	if ok && now.Before(entry.expires) {
		return entry.pub, nil
	}

	v.mu.Lock()
	defer v.mu.Unlock()
	// Another goroutine may have refreshed while we waited.
	entry, ok = v.keys[kid]
	if ok && now.Before(entry.expires) {
		return entry.pub, nil
	}
	if !ok && v.minRefresh > 0 && !v.lastRefresh.IsZero() &&
		now.Sub(v.lastRefresh) < v.minRefresh && !v.cacheExpiredLocked(now) {
		return nil, fmt.Errorf("kid %q not present in JWKS (refresh rate-limited)", kid)
	}
	if err := v.refreshLocked(ctx); err != nil {
		return nil, err
	}
	entry, ok = v.keys[kid]
	if !ok {
		return nil, fmt.Errorf("kid %q not present in JWKS", kid)
	}
	return entry.pub, nil
}

// cacheExpiredLocked reports whether the cache is empty or its entries
// have expired. Caller must hold v.mu.
func (v *Verifier) cacheExpiredLocked(now time.Time) bool {
	for _, e := range v.keys {
		return !now.Before(e.expires)
	}
	return true
}

// Refresh forces a JWKS re-fetch, bypassing the rate limit. Useful at
// start-up to fail fast on misconfiguration.
func (v *Verifier) Refresh(ctx context.Context) error {
	if ctx == nil {
		ctx = context.Background()
	}
	v.mu.Lock()
	defer v.mu.Unlock()
	return v.refreshLocked(ctx)
}

// jwks is the shape returned by the Entra keys endpoint.
type jwks struct {
	Keys []jwk `json:"keys"`
}

// jwk carries the fields we care about from a JSON Web Key.
type jwk struct {
	Kid string `json:"kid"`
	Kty string `json:"kty"`
	Alg string `json:"alg"`
	Use string `json:"use"`
	X5t string `json:"x5t"`
	N   string `json:"n"`
	E   string `json:"e"`
}

// refreshLocked fetches the JWKS (resolving it through discovery first
// if needed) and replaces the in-memory cache. Caller must hold the
// write lock so concurrent misses collapse into a single fetch.
func (v *Verifier) refreshLocked(ctx context.Context) error {
	v.lastRefresh = v.now()
	if !v.resolved {
		u, err := v.discover(ctx)
		if err != nil {
			return err
		}
		v.jwksURL = u
		v.resolved = true
	}
	body, err := v.get(ctx, v.jwksURL)
	if err != nil {
		return fmt.Errorf("auth/entra: JWKS fetch: %w", err)
	}
	var set jwks
	if err := json.Unmarshal(body, &set); err != nil {
		return fmt.Errorf("auth/entra: decode JWKS: %w", err)
	}
	if len(set.Keys) == 0 {
		return errors.New("auth/entra: JWKS response contains no keys")
	}
	next := make(map[string]*keyEntry, len(set.Keys))
	exp := v.now().Add(v.ttl)
	for _, k := range set.Keys {
		if k.Kty != "" && k.Kty != "RSA" {
			continue
		}
		if k.Use != "" && k.Use != "sig" {
			continue
		}
		if k.Alg != "" && k.Alg != auth.AlgRS256 {
			continue
		}
		pub, err := parseRSAPublicKeyFromJWK(k)
		if err != nil {
			return fmt.Errorf("auth/entra: kid %q: %w", k.Kid, err)
		}
		e := &keyEntry{pub: pub, expires: exp}
		if k.Kid != "" {
			next[k.Kid] = e
		}
		if k.X5t != "" && k.X5t != k.Kid {
			next[k.X5t] = e
		}
	}
	if len(next) == 0 {
		return errors.New("auth/entra: JWKS contained no usable RS256 keys")
	}
	v.keys = next
	return nil
}

// discover fetches the OpenID discovery document and returns jwks_uri.
func (v *Verifier) discover(ctx context.Context) (string, error) {
	body, err := v.get(ctx, v.discoveryURL)
	if err != nil {
		return "", fmt.Errorf("auth/entra: OpenID discovery: %w", err)
	}
	var doc struct {
		JWKSURI string `json:"jwks_uri"`
	}
	if err := json.Unmarshal(body, &doc); err != nil {
		return "", fmt.Errorf("auth/entra: decode OpenID discovery document: %w", err)
	}
	if doc.JWKSURI == "" {
		return "", errors.New("auth/entra: OpenID discovery document has no jwks_uri")
	}
	return doc.JWKSURI, nil
}

func (v *Verifier) get(ctx context.Context, url string) ([]byte, error) {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, url, nil)
	if err != nil {
		return nil, err
	}
	req.Header.Set("Accept", "application/json")
	resp, err := v.httpc.Do(req)
	if err != nil {
		return nil, err
	}
	defer func() { _ = resp.Body.Close() }()
	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		return nil, fmt.Errorf("GET %s returned %s", url, resp.Status)
	}
	// JWKS documents are a few KiB; cap reads defensively.
	return io.ReadAll(io.LimitReader(resp.Body, 1<<20))
}

// parseRSAPublicKeyFromJWK reconstructs an *rsa.PublicKey from the
// base64url-encoded modulus (n) and exponent (e) fields of a JWK.
func parseRSAPublicKeyFromJWK(k jwk) (*rsa.PublicKey, error) {
	if k.N == "" || k.E == "" {
		return nil, errors.New("JWK missing n or e")
	}
	nb, err := base64.RawURLEncoding.DecodeString(strings.TrimRight(k.N, "="))
	if err != nil {
		return nil, fmt.Errorf("decode n: %w", err)
	}
	eb, err := base64.RawURLEncoding.DecodeString(strings.TrimRight(k.E, "="))
	if err != nil {
		return nil, fmt.Errorf("decode e: %w", err)
	}
	if len(eb) == 0 || len(eb) > 4 {
		return nil, fmt.Errorf("invalid exponent length (%d bytes)", len(eb))
	}
	padded := make([]byte, 4)
	copy(padded[4-len(eb):], eb)
	e := int(binary.BigEndian.Uint32(padded))
	if e <= 1 {
		return nil, errors.New("invalid exponent")
	}
	return &rsa.PublicKey{N: new(big.Int).SetBytes(nb), E: e}, nil
}

func decodeHeader(seg string) (header, error) {
	var h header
	hb, err := base64.RawURLEncoding.DecodeString(seg)
	if err != nil {
		return h, err
	}
	err = json.Unmarshal(hb, &h)
	return h, err
}

// peekKid extracts the kid (or x5t) header from a compact JWT without
// any signature check. It returns "" on any parse error — the
// subsequent Verify pass surfaces the specific problem.
func peekKid(token string) string {
	dot := strings.IndexByte(token, '.')
	if dot < 0 {
		return ""
	}
	h, err := decodeHeader(token[:dot])
	if err != nil {
		return ""
	}
	return h.keyID()
}

func indexByte(b []byte, c byte) int {
	for i, x := range b {
		if x == c {
			return i
		}
	}
	return -1
}

// isGUID reports whether s has the 8-4-4-4-12 hex GUID shape.
func isGUID(s string) bool {
	if len(s) != 36 {
		return false
	}
	for i, r := range s {
		switch i {
		case 8, 13, 18, 23:
			if r != '-' {
				return false
			}
		default:
			if !isHex(r) {
				return false
			}
		}
	}
	return true
}

func isHex(r rune) bool {
	switch {
	case r >= '0' && r <= '9', r >= 'a' && r <= 'f', r >= 'A' && r <= 'F':
		return true
	}
	return false
}
