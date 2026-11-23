package entra

import (
	"context"
	"crypto"
	"crypto/rand"
	"crypto/rsa"
	"crypto/sha256"
	"encoding/base64"
	"encoding/binary"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"oss.nandlabs.io/golly/auth"
)

const (
	testTenant  = "72f988bf-86f1-41af-91ab-2d7cd011db47"
	otherTenant = "11111111-2222-3333-4444-555555555555"
	testClient  = "6e74172b-be56-4843-9ff4-e66a39bb12e3"
	testOID     = "00000000-aaaa-bbbb-cccc-000000000001"
)

type testSigner struct {
	kid string
	key *rsa.PrivateKey
}

var (
	keyOnce sync.Once
	keyPool []*rsa.PrivateKey
)

// newTestSigner hands out pre-generated 2048-bit keys so the suite
// does not pay key generation per test.
func newTestSigner(t *testing.T, kid string) *testSigner {
	t.Helper()
	keyOnce.Do(func() {
		for i := 0; i < 3; i++ {
			k, err := rsa.GenerateKey(rand.Reader, 2048)
			if err != nil {
				panic(err)
			}
			keyPool = append(keyPool, k)
		}
	})
	idx := 0
	for _, c := range kid {
		idx += int(c)
	}
	return &testSigner{kid: kid, key: keyPool[idx%len(keyPool)]}
}

func (s *testSigner) jwk() map[string]string {
	pub := s.key.PublicKey
	eBuf := make([]byte, 4)
	binary.BigEndian.PutUint32(eBuf, uint32(pub.E))
	i := 0
	for i < len(eBuf)-1 && eBuf[i] == 0 {
		i++
	}
	// Entra JWKs carry kid + x5t, kty=RSA, use=sig and no alg.
	return map[string]string{
		"kid": s.kid,
		"x5t": s.kid,
		"kty": "RSA",
		"use": "sig",
		"n":   base64.RawURLEncoding.EncodeToString(pub.N.Bytes()),
		"e":   base64.RawURLEncoding.EncodeToString(eBuf[i:]),
	}
}

func (s *testSigner) mintWithHeader(t *testing.T, hdr map[string]any, claims map[string]any) string {
	t.Helper()
	hb, _ := json.Marshal(hdr)
	pb, _ := json.Marshal(claims)
	signingInput := base64.RawURLEncoding.EncodeToString(hb) + "." + base64.RawURLEncoding.EncodeToString(pb)
	sum := sha256.Sum256([]byte(signingInput))
	sig, err := rsa.SignPKCS1v15(rand.Reader, s.key, crypto.SHA256, sum[:])
	if err != nil {
		t.Fatalf("SignPKCS1v15: %v", err)
	}
	return signingInput + "." + base64.RawURLEncoding.EncodeToString(sig)
}

func (s *testSigner) mint(t *testing.T, claims map[string]any) string {
	return s.mintWithHeader(t, map[string]any{"alg": auth.AlgRS256, "typ": "JWT", "kid": s.kid}, claims)
}

type jwksServer struct {
	*httptest.Server
	hits      atomic.Int64
	discoHits atomic.Int64
	body      atomic.Value // string
	status    atomic.Int64
}

func newJWKSServer(t *testing.T, signers ...*testSigner) *jwksServer {
	t.Helper()
	js := &jwksServer{}
	js.status.Store(http.StatusOK)
	js.setSigners(signers...)
	mux := http.NewServeMux()
	mux.HandleFunc("/keys", func(w http.ResponseWriter, r *http.Request) {
		js.hits.Add(1)
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(int(js.status.Load()))
		_, _ = w.Write([]byte(js.body.Load().(string)))
	})
	mux.HandleFunc("/"+testTenant+"/v2.0/.well-known/openid-configuration", func(w http.ResponseWriter, r *http.Request) {
		js.discoHits.Add(1)
		_ = json.NewEncoder(w).Encode(map[string]string{
			"issuer":   "https://login.microsoftonline.com/" + testTenant + "/v2.0",
			"jwks_uri": js.URL + "/keys",
		})
	})
	mux.HandleFunc("/"+testTenant+"/discovery/v2.0/keys", func(w http.ResponseWriter, r *http.Request) {
		js.hits.Add(1)
		_, _ = w.Write([]byte(js.body.Load().(string)))
	})
	js.Server = httptest.NewServer(mux)
	t.Cleanup(js.Close)
	return js
}

func (js *jwksServer) setSigners(signers ...*testSigner) {
	keys := make([]map[string]string, 0, len(signers))
	for _, s := range signers {
		keys = append(keys, s.jwk())
	}
	b, _ := json.Marshal(map[string]any{"keys": keys})
	js.body.Store(string(b))
}

func v2Claims(tid, aud, sub string) map[string]any {
	now := time.Now()
	return map[string]any{
		"iss":                DefaultAuthorityHost + "/" + tid + "/v2.0",
		"aud":                aud,
		"sub":                sub,
		"tid":                tid,
		"oid":                testOID,
		"ver":                TokenVersion2,
		"azp":                "client-app",
		"preferred_username": "alice@contoso.com",
		"scp":                "User.Read Files.Read",
		"roles":              []string{"Admin", "Reader"},
		"groups":             []string{"g-1", "g-2"},
		"iat":                now.Add(-time.Minute).Unix(),
		"nbf":                now.Add(-time.Minute).Unix(),
		"exp":                now.Add(time.Hour).Unix(),
	}
}

func v1Claims(tid, aud, sub string) map[string]any {
	c := v2Claims(tid, aud, sub)
	c["iss"] = DefaultV1IssuerHost + "/" + tid + "/"
	c["ver"] = TokenVersion1
	delete(c, "azp")
	c["appid"] = "client-app-v1"
	delete(c, "preferred_username")
	c["upn"] = "alice@contoso.com"
	return c
}

func newVerifier(t *testing.T, tenant string, js *jwksServer, opts ...Option) *Verifier {
	t.Helper()
	full := append([]Option{WithJWKSURL(js.URL + "/keys"), WithMinRefreshInterval(0)}, opts...)
	v, err := NewVerifier(tenant, testClient, full...)
	if err != nil {
		t.Fatalf("NewVerifier: %v", err)
	}
	return v
}

func TestVerifyToken_V2_Success(t *testing.T) {
	s := newTestSigner(t, "kid-1")
	js := newJWKSServer(t, s)
	v := newVerifier(t, testTenant, js)

	got, err := v.VerifyToken(context.Background(), s.mint(t, v2Claims(testTenant, testClient, "sub-1")))
	if err != nil {
		t.Fatalf("VerifyToken: %v", err)
	}
	if got.Subject != "sub-1" {
		t.Errorf("subject = %q", got.Subject)
	}
	if ObjectID(got) != testOID || TenantID(got) != testTenant || TokenVersion(got) != TokenVersion2 {
		t.Errorf("entra claims not surfaced: %v", got.Extra)
	}
	if ClientAppID(got) != "client-app" || Username(got) != "alice@contoso.com" {
		t.Errorf("azp/username: %q %q", ClientAppID(got), Username(got))
	}
	if sc := Scopes(got); len(sc) != 2 || sc[0] != "User.Read" {
		t.Errorf("scopes = %v", sc)
	}
	if r := Roles(got); len(r) != 2 || r[0] != "Admin" {
		t.Errorf("roles = %v", r)
	}
	if g := Groups(got); len(g) != 2 {
		t.Errorf("groups = %v", g)
	}
}

func TestVerifyToken_V1_Success_AppIDURIAudience(t *testing.T) {
	s := newTestSigner(t, "kid-1")
	js := newJWKSServer(t, s)
	v := newVerifier(t, testTenant, js)

	// v1 tokens typically carry the App ID URI as aud.
	got, err := v.VerifyToken(context.Background(), s.mint(t, v1Claims(testTenant, "api://"+testClient, "sub-1")))
	if err != nil {
		t.Fatalf("VerifyToken: %v", err)
	}
	if ClientAppID(got) != "client-app-v1" || Username(got) != "alice@contoso.com" {
		t.Errorf("appid/upn: %q %q", ClientAppID(got), Username(got))
	}
}

func TestVerifyToken_V1HeaderX5tOnly(t *testing.T) {
	s := newTestSigner(t, "kid-x5t")
	js := newJWKSServer(t, s)
	v := newVerifier(t, testTenant, js)
	tok := s.mintWithHeader(t, map[string]any{"alg": "RS256", "typ": "JWT", "x5t": s.kid}, v1Claims(testTenant, testClient, "s"))
	if _, err := v.VerifyToken(context.Background(), tok); err != nil {
		t.Fatalf("x5t-only header: %v", err)
	}
}

func TestVerifyToken_VersionInferredFromIssuer(t *testing.T) {
	s := newTestSigner(t, "kid-1")
	js := newJWKSServer(t, s)
	v := newVerifier(t, testTenant, js)
	c1 := v1Claims(testTenant, testClient, "s")
	delete(c1, "ver")
	if _, err := v.VerifyToken(context.Background(), s.mint(t, c1)); err != nil {
		t.Fatalf("v1 without ver: %v", err)
	}
	c2 := v2Claims(testTenant, testClient, "s")
	delete(c2, "ver")
	if _, err := v.VerifyToken(context.Background(), s.mint(t, c2)); err != nil {
		t.Fatalf("v2 without ver: %v", err)
	}
}

func TestVerifyToken_TokenVersionsRestricted(t *testing.T) {
	s := newTestSigner(t, "kid-1")
	js := newJWKSServer(t, s)
	v := newVerifier(t, testTenant, js, WithTokenVersions(TokenVersion2))
	_, err := v.VerifyToken(context.Background(), s.mint(t, v1Claims(testTenant, testClient, "s")))
	if !errors.Is(err, auth.ErrInvalidToken) {
		t.Fatalf("v1 with v2-only verifier: got %v", err)
	}
	c := v2Claims(testTenant, testClient, "s")
	c["ver"] = "3.0"
	v2 := newVerifier(t, testTenant, js, WithTokenVersions("3.0"))
	if _, err := v2.VerifyToken(context.Background(), s.mint(t, c)); !errors.Is(err, auth.ErrInvalidToken) {
		t.Fatalf("unknown version: got %v", err)
	}
}

func TestVerifyToken_IssuerMismatch(t *testing.T) {
	s := newTestSigner(t, "kid-1")
	js := newJWKSServer(t, s)
	v := newVerifier(t, testTenant, js)

	cases := map[string]func(map[string]any){
		"v2 issuer with v1 ver": func(c map[string]any) { c["ver"] = TokenVersion1 },
		"issuer of other tenant": func(c map[string]any) {
			c["iss"] = DefaultAuthorityHost + "/" + otherTenant + "/v2.0"
		},
		"foreign host": func(c map[string]any) { c["iss"] = "https://evil.example/" + testTenant + "/v2.0" },
	}
	for name, mut := range cases {
		t.Run(name, func(t *testing.T) {
			c := v2Claims(testTenant, testClient, "s")
			mut(c)
			if _, err := v.VerifyToken(context.Background(), s.mint(t, c)); !errors.Is(err, auth.ErrIssuerMismatch) {
				t.Fatalf("got %v, want ErrIssuerMismatch", err)
			}
		})
	}
}

func TestVerifyToken_SingleTenant_RejectsOtherTenant(t *testing.T) {
	s := newTestSigner(t, "kid-1")
	js := newJWKSServer(t, s)
	v := newVerifier(t, testTenant, js)
	// Self-consistent token from another tenant (iss and tid agree).
	_, err := v.VerifyToken(context.Background(), s.mint(t, v2Claims(otherTenant, testClient, "s")))
	if !errors.Is(err, ErrTenantNotAllowed) || !errors.Is(err, auth.ErrIssuerMismatch) {
		t.Fatalf("got %v, want ErrTenantNotAllowed+ErrIssuerMismatch", err)
	}
}

func TestVerifyToken_MissingTid(t *testing.T) {
	s := newTestSigner(t, "kid-1")
	js := newJWKSServer(t, s)
	v := newVerifier(t, testTenant, js)
	c := v2Claims(testTenant, testClient, "s")
	delete(c, "tid")
	if _, err := v.VerifyToken(context.Background(), s.mint(t, c)); !errors.Is(err, auth.ErrInvalidToken) {
		t.Fatalf("got %v", err)
	}
}

func TestVerifyToken_MultiTenant_AllowList(t *testing.T) {
	s := newTestSigner(t, "kid-1")
	js := newJWKSServer(t, s)
	v := newVerifier(t, TenantOrganizations, js, WithAllowedTenants(strings.ToUpper(otherTenant)))

	if _, err := v.VerifyToken(context.Background(), s.mint(t, v2Claims(otherTenant, testClient, "s"))); err != nil {
		t.Fatalf("allowed tenant: %v", err)
	}
	if _, err := v.VerifyToken(context.Background(), s.mint(t, v1Claims(otherTenant, testClient, "s"))); err != nil {
		t.Fatalf("allowed tenant v1: %v", err)
	}
	_, err := v.VerifyToken(context.Background(), s.mint(t, v2Claims(testTenant, testClient, "s")))
	if !errors.Is(err, ErrTenantNotAllowed) {
		t.Fatalf("non-allowed tenant: got %v", err)
	}
	// Issuer must still match the tid claim in multi-tenant mode.
	c := v2Claims(otherTenant, testClient, "s")
	c["iss"] = DefaultAuthorityHost + "/" + testTenant + "/v2.0"
	if _, err := v.VerifyToken(context.Background(), s.mint(t, c)); !errors.Is(err, auth.ErrIssuerMismatch) {
		t.Fatalf("iss/tid disagreement: got %v", err)
	}
}

func TestVerifyToken_MultiTenant_AnyTenant(t *testing.T) {
	s := newTestSigner(t, "kid-1")
	js := newJWKSServer(t, s)
	v := newVerifier(t, TenantCommon, js, WithAnyTenant())
	for _, tid := range []string{testTenant, otherTenant} {
		if _, err := v.VerifyToken(context.Background(), s.mint(t, v2Claims(tid, testClient, "s"))); err != nil {
			t.Fatalf("tenant %s: %v", tid, err)
		}
	}
}

func TestVerifyToken_Consumers(t *testing.T) {
	s := newTestSigner(t, "kid-1")
	js := newJWKSServer(t, s)
	v := newVerifier(t, TenantConsumers, js)
	if _, err := v.VerifyToken(context.Background(), s.mint(t, v2Claims(ConsumersTenantID, testClient, "s"))); err != nil {
		t.Fatalf("consumers: %v", err)
	}
	if _, err := v.VerifyToken(context.Background(), s.mint(t, v2Claims(testTenant, testClient, "s"))); !errors.Is(err, ErrTenantNotAllowed) {
		t.Fatalf("org tenant under consumers: %v", err)
	}
}

func TestVerifyToken_Audience(t *testing.T) {
	s := newTestSigner(t, "kid-1")
	js := newJWKSServer(t, s)
	v := newVerifier(t, testTenant, js, WithAudiences("https://api.contoso.com"))
	if _, err := v.VerifyToken(context.Background(), s.mint(t, v2Claims(testTenant, "https://api.contoso.com", "s"))); err != nil {
		t.Fatalf("custom audience: %v", err)
	}
	if _, err := v.VerifyToken(context.Background(), s.mint(t, v2Claims(testTenant, "someone-else", "s"))); !errors.Is(err, auth.ErrAudMismatch) {
		t.Fatalf("wrong aud: got %v", err)
	}
	c := v2Claims(testTenant, testClient, "s")
	c["aud"] = []string{"x", testClient}
	if _, err := v.VerifyToken(context.Background(), s.mint(t, c)); err != nil {
		t.Fatalf("aud array: %v", err)
	}
}

func TestVerifyToken_TimeClaims(t *testing.T) {
	s := newTestSigner(t, "kid-1")
	js := newJWKSServer(t, s)
	v := newVerifier(t, testTenant, js, WithLeeway(0))

	exp := v2Claims(testTenant, testClient, "s")
	exp["exp"] = time.Now().Add(-time.Minute).Unix()
	if _, err := v.VerifyToken(context.Background(), s.mint(t, exp)); !errors.Is(err, auth.ErrExpired) {
		t.Fatalf("expired: %v", err)
	}
	nbf := v2Claims(testTenant, testClient, "s")
	nbf["nbf"] = time.Now().Add(time.Hour).Unix()
	if _, err := v.VerifyToken(context.Background(), s.mint(t, nbf)); !errors.Is(err, auth.ErrNotYetValid) {
		t.Fatalf("nbf: %v", err)
	}
	iat := v2Claims(testTenant, testClient, "s")
	iat["iat"] = time.Now().Add(time.Hour).Unix()
	delete(iat, "nbf")
	if _, err := v.VerifyToken(context.Background(), s.mint(t, iat)); !errors.Is(err, auth.ErrNotYetValid) {
		t.Fatalf("iat: %v", err)
	}
	// WithNow moves the clock past expiry.
	later := newVerifier(t, testTenant, js, WithLeeway(0), WithNow(func() time.Time { return time.Now().Add(2 * time.Hour) }))
	if _, err := later.VerifyToken(context.Background(), s.mint(t, v2Claims(testTenant, testClient, "s"))); !errors.Is(err, auth.ErrExpired) {
		t.Fatalf("WithNow: %v", err)
	}
}

func TestVerifyToken_EmptySubject(t *testing.T) {
	s := newTestSigner(t, "kid-1")
	js := newJWKSServer(t, s)
	v := newVerifier(t, testTenant, js)
	if _, err := v.VerifyToken(context.Background(), s.mint(t, v2Claims(testTenant, testClient, ""))); !errors.Is(err, auth.ErrInvalidToken) {
		t.Fatalf("got %v", err)
	}
}

func TestVerifyToken_BadSignatureAndAlg(t *testing.T) {
	s := newTestSigner(t, "kid-1")
	js := newJWKSServer(t, s)
	v := newVerifier(t, testTenant, js)
	tok := s.mint(t, v2Claims(testTenant, testClient, "s"))
	if _, err := v.VerifyToken(context.Background(), tok[:len(tok)-4]+"AAAA"); !errors.Is(err, auth.ErrInvalidSig) {
		t.Fatalf("tampered: %v", err)
	}
	hs := s.mintWithHeader(t, map[string]any{"alg": "HS256", "kid": s.kid}, v2Claims(testTenant, testClient, "s"))
	if _, err := v.VerifyToken(context.Background(), hs); !errors.Is(err, auth.ErrAlgNotAllowed) {
		t.Fatalf("HS256: %v", err)
	}
	if _, err := v.VerifyToken(context.Background(), "not-a-jwt"); !errors.Is(err, auth.ErrInvalidToken) {
		t.Fatalf("garbage: %v", err)
	}
	// Token signed by a key that's not published under the claimed kid.
	other := newTestSigner(t, "kid-zz")
	forged := (&testSigner{kid: s.kid, key: other.key}).mint(t, v2Claims(testTenant, testClient, "s"))
	if other.key != s.key {
		if _, err := v.VerifyToken(context.Background(), forged); !errors.Is(err, auth.ErrInvalidSig) {
			t.Fatalf("forged: %v", err)
		}
	}
}

func TestVerifyToken_UnknownKid_RefreshesJWKS(t *testing.T) {
	oldS := newTestSigner(t, "kid-old")
	newS := newTestSigner(t, "kid-new")
	js := newJWKSServer(t, oldS)
	v := newVerifier(t, testTenant, js)
	if _, err := v.VerifyToken(context.Background(), oldS.mint(t, v2Claims(testTenant, testClient, "s"))); err != nil {
		t.Fatalf("prime: %v", err)
	}
	prime := js.hits.Load()
	js.setSigners(oldS, newS)
	if _, err := v.VerifyToken(context.Background(), newS.mint(t, v2Claims(testTenant, testClient, "s"))); err != nil {
		t.Fatalf("rotated: %v", err)
	}
	if js.hits.Load() <= prime {
		t.Fatal("expected JWKS refetch on unknown kid")
	}
	// Cached thereafter.
	h := js.hits.Load()
	for i := 0; i < 3; i++ {
		if _, err := v.VerifyToken(context.Background(), newS.mint(t, v2Claims(testTenant, testClient, "s"))); err != nil {
			t.Fatal(err)
		}
	}
	if js.hits.Load() != h {
		t.Fatalf("expected cache hits, got %d extra fetches", js.hits.Load()-h)
	}
}

func TestVerifyToken_UnknownKid_RateLimited(t *testing.T) {
	s := newTestSigner(t, "kid-1")
	js := newJWKSServer(t, s)
	now := time.Now()
	clock := func() time.Time { return now }
	v := newVerifier(t, testTenant, js, WithMinRefreshInterval(time.Minute), WithNow(clock))
	if _, err := v.VerifyToken(context.Background(), s.mint(t, v2Claims(testTenant, testClient, "s"))); err != nil {
		t.Fatalf("prime: %v", err)
	}
	h := js.hits.Load()
	ghost := &testSigner{kid: "ghost", key: s.key}
	for i := 0; i < 5; i++ {
		if _, err := v.VerifyToken(context.Background(), ghost.mint(t, v2Claims(testTenant, testClient, "s"))); !errors.Is(err, auth.ErrKeyNotFound) {
			t.Fatalf("ghost kid: %v", err)
		}
	}
	if js.hits.Load() != h {
		t.Fatalf("unknown kid should be rate-limited; %d extra fetches", js.hits.Load()-h)
	}
	now = now.Add(2 * time.Minute)
	_, _ = v.VerifyToken(context.Background(), ghost.mint(t, v2Claims(testTenant, testClient, "s")))
	if js.hits.Load() != h+1 {
		t.Fatalf("expected one fetch after interval, got %d", js.hits.Load()-h)
	}
}

func TestVerifyToken_KeyTTLExpiry(t *testing.T) {
	s := newTestSigner(t, "kid-1")
	js := newJWKSServer(t, s)
	now := time.Now()
	v := newVerifier(t, testTenant, js, WithKeyTTL(time.Minute), WithMinRefreshInterval(time.Hour),
		WithNow(func() time.Time { return now }))
	if err := v.Refresh(context.Background()); err != nil {
		t.Fatal(err)
	}
	h := js.hits.Load()
	now = now.Add(2 * time.Minute)
	tokenClaims := v2Claims(testTenant, testClient, "s")
	tokenClaims["exp"] = now.Add(time.Hour).Unix()
	if _, err := v.VerifyToken(context.Background(), s.mint(t, tokenClaims)); err != nil {
		t.Fatal(err)
	}
	if js.hits.Load() != h+1 {
		t.Fatalf("expired cache should refetch, hits delta %d", js.hits.Load()-h)
	}
}

func TestVerifyToken_JWKSErrors(t *testing.T) {
	s := newTestSigner(t, "kid-1")
	js := newJWKSServer(t, s)
	v := newVerifier(t, testTenant, js)
	tok := s.mint(t, v2Claims(testTenant, testClient, "s"))

	js.status.Store(http.StatusInternalServerError)
	if _, err := v.VerifyToken(context.Background(), tok); !errors.Is(err, auth.ErrKeyNotFound) {
		t.Fatalf("500: %v", err)
	}
	js.status.Store(http.StatusOK)
	for _, body := range []string{`{`, `{"keys":[]}`, `{"keys":[{"kid":"a","kty":"EC"}]}`, `{"keys":[{"kid":"a","kty":"RSA","n":"","e":""}]}`,
		`{"keys":[{"kid":"a","kty":"RSA","n":"AQAB","e":"AQABAQAB"}]}`, `{"keys":[{"kid":"a","kty":"RSA","n":"!!","e":"AQAB"}]}`,
		`{"keys":[{"kid":"a","kty":"RSA","n":"AQAB","e":"!!"}]}`, `{"keys":[{"kid":"a","kty":"RSA","use":"enc","n":"AQAB","e":"AQAB"}]}`} {
		js.body.Store(body)
		if err := v.Refresh(context.Background()); err == nil {
			t.Errorf("body %s: expected refresh error", body)
		}
	}
}

func TestVerifier_OpenIDDiscovery(t *testing.T) {
	s := newTestSigner(t, "kid-1")
	js := newJWKSServer(t, s)
	v, err := NewVerifier(testTenant, testClient, WithAuthorityHost(js.URL+"/"), WithOpenIDDiscovery(), WithMinRefreshInterval(0))
	if err != nil {
		t.Fatal(err)
	}
	c := v2Claims(testTenant, testClient, "s")
	c["iss"] = js.URL + "/" + testTenant + "/v2.0"
	if _, err := v.VerifyToken(context.Background(), s.mint(t, c)); err != nil {
		t.Fatalf("VerifyToken via discovery: %v", err)
	}
	if js.discoHits.Load() != 1 {
		t.Fatalf("discovery hits = %d", js.discoHits.Load())
	}
	if v.JWKSURL() != js.URL+"/keys" {
		t.Fatalf("JWKSURL = %q", v.JWKSURL())
	}
	// Discovery runs once only.
	_ = v.Refresh(context.Background())
	if js.discoHits.Load() != 1 {
		t.Fatalf("discovery should be cached")
	}
}

func TestVerifier_DefaultKeysURLAndSovereignHosts(t *testing.T) {
	s := newTestSigner(t, "kid-1")
	js := newJWKSServer(t, s)
	v, err := NewVerifier(testTenant, testClient, WithAuthorityHost(js.URL), WithV1IssuerHost("https://sts.chinacloudapi.cn/"),
		WithHTTPClient(js.Client()), WithMinRefreshInterval(0))
	if err != nil {
		t.Fatal(err)
	}
	if v.JWKSURL() != js.URL+"/"+testTenant+"/discovery/v2.0/keys" {
		t.Fatalf("JWKSURL = %q", v.JWKSURL())
	}
	c := v1Claims(testTenant, testClient, "s")
	c["iss"] = "https://sts.chinacloudapi.cn/" + testTenant + "/"
	if _, err := v.VerifyToken(context.Background(), s.mint(t, c)); err != nil {
		t.Fatalf("sovereign v1: %v", err)
	}
}

func TestVerifier_DiscoveryErrors(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/bad":
			_, _ = w.Write([]byte("{"))
		case "/empty":
			_, _ = w.Write([]byte("{}"))
		default:
			http.NotFound(w, r)
		}
	}))
	defer srv.Close()
	for _, p := range []string{"/bad", "/empty", "/missing"} {
		v, err := NewVerifier(testTenant, testClient, WithDiscoveryURL(srv.URL+p))
		if err != nil {
			t.Fatal(err)
		}
		if err := v.Refresh(context.Background()); err == nil {
			t.Errorf("%s: expected error", p)
		}
	}
}

func TestVerifier_KeysetAndVerify(t *testing.T) {
	s := newTestSigner(t, "kid-1")
	js := newJWKSServer(t, s)
	v := newVerifier(t, testTenant, js)
	ks, err := v.Keyset("kid-1")
	if err != nil || ks.Alg() != auth.AlgRS256 {
		t.Fatalf("Keyset: %v", err)
	}
	if _, err := v.Keyset("nope"); !errors.Is(err, auth.ErrKeyNotFound) {
		t.Fatalf("Keyset unknown: %v", err)
	}
	// Plug Keyset into auth.Verify directly.
	tok := s.mint(t, v2Claims(testTenant, testClient, "s"))
	if _, err := auth.Verify(tok, auth.VerifyOptions{Algs: []string{auth.AlgRS256}, Keyset: v.Keyset}); err != nil {
		t.Fatalf("auth.Verify with Keyset: %v", err)
	}
	// Direct Verify error paths.
	if err := v.Verify([]byte("nodot"), nil); !errors.Is(err, auth.ErrInvalidToken) {
		t.Errorf("nodot: %v", err)
	}
	if err := v.Verify([]byte("!!!.x"), nil); !errors.Is(err, auth.ErrInvalidToken) {
		t.Errorf("bad b64: %v", err)
	}
	nokid := base64.RawURLEncoding.EncodeToString([]byte(`{"alg":"RS256"}`))
	if err := v.Verify([]byte(nokid+".x"), nil); !errors.Is(err, auth.ErrKeyNotFound) {
		t.Errorf("no kid: %v", err)
	}
	if v.Alg() != auth.AlgRS256 || v.Tenant() != testTenant {
		t.Error("Alg/Tenant")
	}
}

func TestNewVerifier_Validation(t *testing.T) {
	if _, err := NewVerifier("", testClient); err == nil {
		t.Error("empty tenant")
	}
	if _, err := NewVerifier(testTenant, ""); err == nil {
		t.Error("empty client")
	}
	if _, err := NewVerifier("contoso.onmicrosoft.com", testClient); err == nil {
		t.Error("domain tenant should be rejected")
	}
	if _, err := NewVerifier("zzzzzzzz-86f1-41af-91ab-2d7cd011db47", testClient); err == nil {
		t.Error("non-hex guid")
	}
	if _, err := NewVerifier(TenantCommon, testClient); err == nil {
		t.Error("multi-tenant without allow-list must fail")
	}
	if _, err := NewVerifier(TenantOrganizations, testClient, WithAllowedTenants(testTenant)); err != nil {
		t.Errorf("multi-tenant with allow-list: %v", err)
	}
	v, err := NewVerifier(strings.ToUpper(testTenant), testClient, WithTokenVersions(), WithKeyTTL(-1), WithLeeway(-1), WithHTTPClient(nil), WithNow(nil), nil)
	if err != nil {
		t.Fatal(err)
	}
	if v.ttl != DefaultKeyTTL || v.leeway != DefaultLeeway || len(v.versions) != 2 {
		t.Error("invalid option values should be ignored")
	}
}

// Compile-time assertion: *Verifier satisfies auth.Verifier.
var _ auth.Verifier = (*Verifier)(nil)
