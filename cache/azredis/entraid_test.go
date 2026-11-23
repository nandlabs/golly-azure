package azredis

import (
	"context"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"sync"
	"testing"
	"time"

	"github.com/Azure/azure-sdk-for-go/sdk/azcore"
	"github.com/Azure/azure-sdk-for-go/sdk/azcore/policy"
	"github.com/alicebob/miniredis/v2"
	"github.com/redis/go-redis/v9/auth"
	"oss.nandlabs.io/golly-azure/azcfg"
)

const testOID = "11111111-2222-3333-4444-555555555555"

// fakeJWT builds an unsigned JWT-shaped token carrying an oid claim and a
// sequence number (so successive tokens differ).
func fakeJWT(oid string, seq int) string {
	enc := base64.RawURLEncoding
	hdr := enc.EncodeToString([]byte(`{"alg":"none","typ":"JWT"}`))
	claims := map[string]any{"seq": seq}
	if oid != "" {
		claims["oid"] = oid
	}
	body, _ := json.Marshal(claims)
	return hdr + "." + enc.EncodeToString(body) + ".sig"
}

// fakeCredential is an azcore.TokenCredential returning a fresh token per
// call (or a scripted error), with a configurable lifetime.
type fakeCredential struct {
	mu        sync.Mutex
	oid       string
	lifetime  time.Duration
	calls     int
	scopes    []string
	err       error
	sameToken bool // return the same token every call (simulates a cache)
}

func (f *fakeCredential) GetToken(_ context.Context, opts policy.TokenRequestOptions) (azcore.AccessToken, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.calls++
	f.scopes = opts.Scopes
	if f.err != nil {
		return azcore.AccessToken{}, f.err
	}
	seq := f.calls
	if f.sameToken {
		seq = 1
	}
	return azcore.AccessToken{Token: fakeJWT(f.oid, seq), ExpiresOn: time.Now().Add(f.lifetime)}, nil
}

func (f *fakeCredential) callCount() int {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.calls
}

func (f *fakeCredential) setErr(err error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.err = err
}

// recordingListener captures OnNext / OnError calls.
type recordingListener struct {
	mu    sync.Mutex
	next  []auth.Credentials
	errs  []error
	gotCh chan struct{}
}

func newRecordingListener() *recordingListener {
	return &recordingListener{gotCh: make(chan struct{}, 64)}
}

func (r *recordingListener) OnNext(c auth.Credentials) {
	r.mu.Lock()
	r.next = append(r.next, c)
	r.mu.Unlock()
	r.gotCh <- struct{}{}
}

func (r *recordingListener) OnError(err error) {
	r.mu.Lock()
	r.errs = append(r.errs, err)
	r.mu.Unlock()
	r.gotCh <- struct{}{}
}

func (r *recordingListener) counts() (int, int) {
	r.mu.Lock()
	defer r.mu.Unlock()
	return len(r.next), len(r.errs)
}

func waitFor(t *testing.T, d time.Duration, cond func() bool) {
	t.Helper()
	deadline := time.Now().Add(d)
	for time.Now().Before(deadline) {
		if cond() {
			return
		}
		time.Sleep(5 * time.Millisecond)
	}
	t.Fatalf("condition not met within %v", d)
}

func TestObjectIDFromJWT(t *testing.T) {
	if got := objectIDFromJWT(fakeJWT(testOID, 1)); got != testOID {
		t.Fatalf("oid = %q, want %q", got, testOID)
	}
	for _, bad := range []string{"", "opaque-token", "a.!!!.c", "a." + base64.RawURLEncoding.EncodeToString([]byte("nope")) + ".c"} {
		if got := objectIDFromJWT(bad); got != "" {
			t.Fatalf("objectIDFromJWT(%q) = %q, want empty", bad, got)
		}
	}
}

func TestEntraIDProvider_CredentialsCachedAndDefaults(t *testing.T) {
	fc := &fakeCredential{oid: testOID, lifetime: time.Hour}
	p, err := NewEntraIDProvider(EntraIDOptions{Credential: fc})
	if err != nil {
		t.Fatalf("NewEntraIDProvider: %v", err)
	}
	defer func() { _ = p.Close() }()
	ctx := context.Background()

	user, tok, err := p.Credentials(ctx)
	if err != nil {
		t.Fatalf("Credentials: %v", err)
	}
	if user != testOID || tok == "" {
		t.Fatalf("Credentials = (%q, %q)", user, tok)
	}
	if len(fc.scopes) != 1 || fc.scopes[0] != DefaultEntraIDScope {
		t.Fatalf("scopes = %v, want [%s]", fc.scopes, DefaultEntraIDScope)
	}
	tok2, _ := p.Token(ctx)
	u2, _ := p.Username(ctx)
	if tok2 != tok || u2 != user {
		t.Fatalf("cached token not reused")
	}
	if n := fc.callCount(); n != 1 {
		t.Fatalf("GetToken calls = %d, want 1 (cached)", n)
	}
	if p.refreshBefore != DefaultRefreshBefore || p.retryInterval != DefaultRetryInterval {
		t.Fatalf("defaults not applied")
	}
}

func TestEntraIDProvider_RefreshesNearExpiry(t *testing.T) {
	fc := &fakeCredential{oid: testOID, lifetime: time.Hour}
	p, _ := NewEntraIDProvider(EntraIDOptions{Credential: fc, RefreshBefore: 10 * time.Minute})
	defer func() { _ = p.Close() }()
	base := time.Now()
	p.now = func() time.Time { return base }
	ctx := context.Background()

	_, tok1, _ := p.Credentials(ctx)
	// 49 minutes later: still > RefreshBefore from expiry → cached.
	p.now = func() time.Time { return base.Add(49 * time.Minute) }
	_, tok, _ := p.Credentials(ctx)
	if tok != tok1 {
		t.Fatalf("token refreshed too early")
	}
	// 51 minutes later: within RefreshBefore → refetch.
	p.now = func() time.Time { return base.Add(51 * time.Minute) }
	_, tok, _ = p.Credentials(ctx)
	if tok == tok1 {
		t.Fatalf("token not refreshed within RefreshBefore of expiry")
	}
}

func TestEntraIDProvider_ObjectIDOverrideAndMissing(t *testing.T) {
	ctx := context.Background()
	fc := &fakeCredential{lifetime: time.Hour} // token without oid
	p, _ := NewEntraIDProvider(EntraIDOptions{Credential: fc})
	if _, _, err := p.Credentials(ctx); !errors.Is(err, ErrNoObjectID) {
		t.Fatalf("err = %v, want ErrNoObjectID", err)
	}
	_ = p.Close()

	p, _ = NewEntraIDProvider(EntraIDOptions{Credential: fc, ObjectID: "explicit-oid", Scopes: []string{"custom/.default"}})
	defer func() { _ = p.Close() }()
	u, _, err := p.Credentials(ctx)
	if err != nil || u != "explicit-oid" {
		t.Fatalf("Credentials = (%q, %v), want explicit-oid", u, err)
	}
	if fc.scopes[0] != "custom/.default" {
		t.Fatalf("custom scopes not passed: %v", fc.scopes)
	}
}

func TestEntraIDProvider_CredentialError(t *testing.T) {
	sentinel := errors.New("aad down")
	fc := &fakeCredential{oid: testOID, lifetime: time.Hour, err: sentinel}
	p, _ := NewEntraIDProvider(EntraIDOptions{Credential: fc})
	defer func() { _ = p.Close() }()
	if _, err := p.Token(context.Background()); !errors.Is(err, sentinel) {
		t.Fatalf("err = %v, want sentinel", err)
	}
	if _, _, err := p.Subscribe(newRecordingListener()); !errors.Is(err, sentinel) {
		t.Fatalf("Subscribe err = %v, want sentinel", err)
	}
}

func TestEntraIDProvider_StaleTokenServedWhenRefreshFails(t *testing.T) {
	fc := &fakeCredential{oid: testOID, lifetime: time.Hour}
	p, _ := NewEntraIDProvider(EntraIDOptions{Credential: fc})
	defer func() { _ = p.Close() }()
	base := time.Now()
	p.now = func() time.Time { return base }
	_, tok1, _ := p.Credentials(context.Background())

	fc.setErr(errors.New("transient"))
	p.now = func() time.Time { return base.Add(57 * time.Minute) } // in refresh window, not expired
	_, tok, err := p.Credentials(context.Background())
	if err != nil || tok != tok1 {
		t.Fatalf("expected cached token while still valid, got (%q, %v)", tok, err)
	}
	p.now = func() time.Time { return base.Add(61 * time.Minute) } // expired
	if _, _, err := p.Credentials(context.Background()); err == nil {
		t.Fatalf("expected error once cached token expired")
	}
}

func TestEntraIDProvider_EmptyToken(t *testing.T) {
	p, _ := NewEntraIDProvider(EntraIDOptions{Credential: emptyCred{}})
	defer func() { _ = p.Close() }()
	if _, err := p.Token(context.Background()); err == nil {
		t.Fatal("expected error for empty token")
	}
}

type emptyCred struct{}

func (emptyCred) GetToken(context.Context, policy.TokenRequestOptions) (azcore.AccessToken, error) {
	return azcore.AccessToken{ExpiresOn: time.Now().Add(time.Hour)}, nil
}

func TestEntraIDProvider_FromAzcfg(t *testing.T) {
	fc := &fakeCredential{oid: testOID, lifetime: time.Hour}
	cfg := &azcfg.Config{TokenCredential: fc}
	p, err := NewEntraIDProvider(EntraIDOptions{Config: cfg})
	if err != nil {
		t.Fatalf("NewEntraIDProvider: %v", err)
	}
	defer func() { _ = p.Close() }()
	if u, _ := p.Username(context.Background()); u != testOID {
		t.Fatalf("username = %q", u)
	}

	// Incomplete client-secret config surfaces the azcfg error.
	if _, err := NewEntraIDProvider(EntraIDOptions{Config: &azcfg.Config{ClientSecret: "s"}}); err == nil {
		t.Fatal("expected credential error for incomplete client secret config")
	}
}

func TestEntraIDProvider_StreamingRefreshNotifiesListeners(t *testing.T) {
	fc := &fakeCredential{oid: testOID, lifetime: 200 * time.Millisecond}
	p, _ := NewEntraIDProvider(EntraIDOptions{Credential: fc, RefreshBefore: 150 * time.Millisecond})
	defer func() { _ = p.Close() }()

	l := newRecordingListener()
	creds, unsub, err := p.Subscribe(l)
	if err != nil {
		t.Fatalf("Subscribe: %v", err)
	}
	u, first := creds.BasicAuth()
	if u != testOID || first == "" {
		t.Fatalf("initial creds = (%q, %q)", u, first)
	}
	// Idempotent re-subscribe of the same listener.
	if _, _, err := p.Subscribe(l); err != nil {
		t.Fatalf("re-Subscribe: %v", err)
	}

	// Lifetime 200ms, margin clamped to 100ms → refresh ~100ms in.
	waitFor(t, 2*time.Second, func() bool { n, _ := l.counts(); return n >= 1 })
	l.mu.Lock()
	_, next := l.next[0].BasicAuth()
	l.mu.Unlock()
	if next == first {
		t.Fatalf("refreshed token should differ from the first")
	}

	if err := unsub(); err != nil {
		t.Fatalf("unsubscribe: %v", err)
	}
	if err := unsub(); err != nil {
		t.Fatalf("second unsubscribe should be a no-op: %v", err)
	}
	n0, _ := l.counts()
	time.Sleep(300 * time.Millisecond)
	if n, _ := l.counts(); n > n0+1 { // allow one in-flight notification
		t.Fatalf("listener still notified after unsubscribe: %d -> %d", n0, n)
	}
}

func TestEntraIDProvider_StreamingErrorAndSameToken(t *testing.T) {
	fc := &fakeCredential{oid: testOID, lifetime: 100 * time.Millisecond, sameToken: true}
	p, _ := NewEntraIDProvider(EntraIDOptions{
		Credential:    fc,
		RefreshBefore: 50 * time.Millisecond,
		RetryInterval: 10 * time.Millisecond,
	})
	defer func() { _ = p.Close() }()
	l := newRecordingListener()
	if _, _, err := p.Subscribe(l); err != nil {
		t.Fatalf("Subscribe: %v", err)
	}
	// Same token returned → loop retries without notifying OnNext.
	waitFor(t, 2*time.Second, func() bool { return fc.callCount() >= 3 })
	if n, _ := l.counts(); n != 0 {
		t.Fatalf("OnNext called %d times for an unchanged token", n)
	}
	// Errors reach OnError.
	fc.setErr(errors.New("refresh failed"))
	waitFor(t, 2*time.Second, func() bool { _, e := l.counts(); return e >= 1 })
}

func TestEntraIDProvider_CloseStopsLoopAndRejectsSubscribe(t *testing.T) {
	fc := &fakeCredential{oid: testOID, lifetime: time.Hour}
	p, _ := NewEntraIDProvider(EntraIDOptions{Credential: fc})
	if _, _, err := p.Subscribe(newRecordingListener()); err != nil {
		t.Fatalf("Subscribe: %v", err)
	}
	if err := p.Close(); err != nil {
		t.Fatalf("Close: %v", err)
	}
	if err := p.Close(); err != nil {
		t.Fatalf("second Close: %v", err)
	}
	if _, _, err := p.Subscribe(newRecordingListener()); err == nil {
		t.Fatal("Subscribe after Close should fail")
	}
	// Close on a provider whose loop never started must not block.
	p2, _ := NewEntraIDProvider(EntraIDOptions{Credential: fc})
	done := make(chan struct{})
	go func() { _ = p2.Close(); close(done) }()
	select {
	case <-done:
	case <-time.After(time.Second):
		t.Fatal("Close blocked on never-started provider")
	}
}

func TestNewWithEntraID_AuthenticatesAgainstRedis(t *testing.T) {
	mr := miniredis.RunT(t)
	fc := &fakeCredential{oid: testOID, lifetime: time.Hour}
	// The first token handed out has seq=1; the provider fetches exactly
	// once in NewWithEntraID and reuses it for the connection.
	mr.RequireUserAuth(testOID, fakeJWT(testOID, 1))

	c, err := NewWithEntraID(context.Background(), &Config{
		Addrs:      []string{mr.Addr()},
		DisableTLS: true,
		Username:   "ignored",
		Password:   "ignored",
	}, EntraIDOptions{Credential: fc})
	if err != nil {
		t.Fatalf("NewWithEntraID: %v", err)
	}
	ctx := context.Background()
	if err := c.Ping(ctx); err != nil {
		t.Fatalf("Ping: %v", err)
	}
	if err := c.Set(ctx, "k", []byte("v")); err != nil {
		t.Fatalf("Set: %v", err)
	}
	if v, ok := c.Get(ctx, "k"); !ok || string(v) != "v" {
		t.Fatalf("Get = (%q, %v)", v, ok)
	}
	if err := c.Close(); err != nil {
		t.Fatalf("Close: %v", err)
	}
}

func TestNewWithEntraID_TokenRotationKeepsConnectionUsable(t *testing.T) {
	mr := miniredis.RunT(t)
	fc := &fakeCredential{oid: testOID, lifetime: 300 * time.Millisecond}
	mr.RequireUserAuth(testOID, fakeJWT(testOID, 1))

	c, err := NewWithEntraID(context.Background(), &Config{
		Addrs:      []string{mr.Addr()},
		DisableTLS: true,
		PoolSize:   1,
	}, EntraIDOptions{Credential: fc, RefreshBefore: 200 * time.Millisecond})
	if err != nil {
		t.Fatalf("NewWithEntraID: %v", err)
	}
	defer func() { _ = c.Close() }()
	ctx := context.Background()
	if err := c.Ping(ctx); err != nil {
		t.Fatalf("Ping: %v", err)
	}
	// Accept the next token server-side; the provider pushes it to the
	// pooled connection, which go-redis re-AUTHs.
	mr.RequireUserAuth(testOID, fakeJWT(testOID, 2))
	waitFor(t, 3*time.Second, func() bool { return fc.callCount() >= 2 })
	for i := 0; i < 5; i++ {
		if err := c.Ping(ctx); err != nil {
			t.Fatalf("Ping after rotation: %v", err)
		}
		time.Sleep(20 * time.Millisecond)
	}
}

func TestNewWithEntraID_Errors(t *testing.T) {
	ctx := context.Background()
	if _, err := NewWithEntraID(ctx, nil, EntraIDOptions{}); !errors.Is(err, ErrInvalidConfig) {
		t.Fatalf("nil cfg err = %v", err)
	}
	fc := &fakeCredential{oid: testOID, lifetime: time.Hour, err: fmt.Errorf("boom")}
	if _, err := NewWithEntraID(ctx, &Config{Addrs: []string{"h"}}, EntraIDOptions{Credential: fc}); err == nil {
		t.Fatal("expected token error")
	}
	if _, err := NewWithEntraID(ctx, &Config{Addrs: []string{"h"}}, EntraIDOptions{Config: &azcfg.Config{ClientSecret: "s"}}); err == nil {
		t.Fatal("expected credential error")
	}
	ok := &fakeCredential{oid: testOID, lifetime: time.Hour}
	if _, err := NewWithEntraID(ctx, &Config{Addrs: []string{""}}, EntraIDOptions{Credential: ok}); !errors.Is(err, ErrInvalidConfig) {
		t.Fatalf("empty addr err = %v", err)
	}
}

func TestClient_TokenAsCredentialsProvider(t *testing.T) {
	mr := miniredis.RunT(t)
	fc := &fakeCredential{oid: testOID, lifetime: time.Hour}
	mr.RequireUserAuth(testOID, fakeJWT(testOID, 1))
	p, _ := NewEntraIDProvider(EntraIDOptions{Credential: fc})
	defer func() { _ = p.Close() }()
	c, err := New(&Config{
		Addrs:               []string{mr.Addr()},
		DisableTLS:          true,
		Username:            testOID,
		CredentialsProvider: p.Token,
	})
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	defer func() { _ = c.Close() }()
	if err := c.Ping(context.Background()); err != nil {
		t.Fatalf("Ping: %v", err)
	}
}
