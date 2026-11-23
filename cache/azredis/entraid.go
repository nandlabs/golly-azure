package azredis

import (
	"context"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"strings"
	"sync"
	"time"

	"github.com/Azure/azure-sdk-for-go/sdk/azcore"
	"github.com/Azure/azure-sdk-for-go/sdk/azcore/policy"
	"github.com/redis/go-redis/v9/auth"
	"oss.nandlabs.io/golly-azure/azcfg"
)

// DefaultEntraIDScope is the OAuth2 scope for Azure Cache for Redis and
// Azure Managed Redis data-plane access.
const DefaultEntraIDScope = "https://redis.azure.com/.default"

// Defaults for EntraIDOptions.
const (
	DefaultRefreshBefore = 5 * time.Minute
	DefaultRetryInterval = 10 * time.Second
	defaultFetchTimeout  = 30 * time.Second
)

// ErrNoObjectID is returned when the Redis username cannot be determined:
// EntraIDOptions.ObjectID is empty and the access token carries no "oid"
// claim.
var ErrNoObjectID = errors.New("azredis: cannot determine Entra ID object id (set EntraIDOptions.ObjectID)")

// EntraIDOptions configures an EntraIDProvider.
type EntraIDOptions struct {
	// Credential issues the access tokens. When nil, Config.Credential()
	// is used.
	Credential azcore.TokenCredential

	// Config resolves the credential when Credential is nil. A nil Config
	// resolves to azcfg.GetConfigOrDefault(nil, azcfg.DefaultConfigName)
	// (DefaultAzureCredential unless a "default" config is registered).
	Config *azcfg.Config

	// Scopes overrides the token scopes (default DefaultEntraIDScope).
	Scopes []string

	// ObjectID is the Redis username: the object (principal) ID of the
	// managed identity, service principal or user that was added to the
	// cache's data access configuration. When empty it is read from the
	// "oid" claim of the access token.
	ObjectID string

	// RefreshBefore is how long before token expiry the background loop
	// fetches a new token and re-authenticates connections. Defaults to
	// DefaultRefreshBefore; clamped to half the token lifetime.
	RefreshBefore time.Duration

	// RetryInterval is the delay between refresh attempts after a failure
	// (or when the credential hands back the same cached token). Defaults
	// to DefaultRetryInterval.
	RetryInterval time.Duration
}

// EntraIDProvider obtains Microsoft Entra ID access tokens for Azure Redis
// and keeps them fresh.
//
// It can be used two ways:
//
//   - As a streaming provider (auth.StreamingCredentialsProvider) via
//     Config.StreamingCredentialsProvider or NewWithEntraID. A background
//     goroutine refreshes the token RefreshBefore its expiry and pushes the
//     new credentials to every subscribed connection, which go-redis
//     re-AUTHs in place. This is the recommended mode: Azure closes
//     connections whose token has expired.
//   - As a per-connection callback via Config.CredentialsProvider =
//     p.Token (with Config.Username set to the object ID). Tokens are
//     cached and refreshed lazily, but long-lived connections are not
//     re-authenticated.
//
// EntraIDProvider is safe for concurrent use. Call Close to stop the
// refresh loop.
type EntraIDProvider struct {
	cred          azcore.TokenCredential
	scopes        []string
	objectID      string
	refreshBefore time.Duration
	retryInterval time.Duration
	now           func() time.Time

	mu        sync.Mutex
	token     azcore.AccessToken
	username  string
	refreshAt time.Time
	listeners map[auth.CredentialsListener]struct{}
	running   bool
	closed    bool
	stop      chan struct{}
	done      chan struct{}
	closeOnce sync.Once
}

var _ auth.StreamingCredentialsProvider = (*EntraIDProvider)(nil)

// NewEntraIDProvider returns an EntraIDProvider. It performs no I/O; the
// first token is fetched on first use.
func NewEntraIDProvider(opts EntraIDOptions) (*EntraIDProvider, error) {
	cred := opts.Credential
	if cred == nil {
		cfg := opts.Config
		if cfg == nil {
			cfg = azcfg.GetConfigOrDefault(nil, azcfg.DefaultConfigName)
		}
		var err error
		if cred, err = cfg.Credential(); err != nil {
			return nil, fmt.Errorf("azredis: entra id credential: %w", err)
		}
	}
	scopes := opts.Scopes
	if len(scopes) == 0 {
		scopes = []string{DefaultEntraIDScope}
	}
	rb := opts.RefreshBefore
	if rb <= 0 {
		rb = DefaultRefreshBefore
	}
	ri := opts.RetryInterval
	if ri <= 0 {
		ri = DefaultRetryInterval
	}
	return &EntraIDProvider{
		cred:          cred,
		scopes:        scopes,
		objectID:      opts.ObjectID,
		refreshBefore: rb,
		retryInterval: ri,
		now:           time.Now,
		listeners:     make(map[auth.CredentialsListener]struct{}),
		stop:          make(chan struct{}),
		done:          make(chan struct{}),
	}, nil
}

// Credentials returns the Redis username (object ID) and a valid access
// token, fetching a new token when none is cached or the cached one is
// within RefreshBefore of expiry.
func (p *EntraIDProvider) Credentials(ctx context.Context) (username, password string, err error) {
	p.mu.Lock()
	defer p.mu.Unlock()
	if p.token.Token == "" || !p.now().Before(p.refreshAt) {
		if _, err := p.fetchLocked(ctx); err != nil {
			// A still-valid cached token beats failing the connection.
			if p.token.Token == "" || !p.now().Before(p.token.ExpiresOn) {
				return "", "", err
			}
		}
	}
	return p.username, p.token.Token, nil
}

// Token returns a valid access token. Its signature matches
// Config.CredentialsProvider.
func (p *EntraIDProvider) Token(ctx context.Context) (string, error) {
	_, tok, err := p.Credentials(ctx)
	return tok, err
}

// Username returns the Redis username (object ID), fetching a token if
// needed to read its "oid" claim.
func (p *EntraIDProvider) Username(ctx context.Context) (string, error) {
	u, _, err := p.Credentials(ctx)
	return u, err
}

// Subscribe implements auth.StreamingCredentialsProvider. It returns the
// current credentials, registers listener for refreshed ones and starts
// the background refresh loop on first use. Subscribing the same listener
// twice is idempotent; every returned UnsubscribeFunc removes it.
func (p *EntraIDProvider) Subscribe(listener auth.CredentialsListener) (auth.Credentials, auth.UnsubscribeFunc, error) {
	ctx, cancel := context.WithTimeout(context.Background(), defaultFetchTimeout)
	defer cancel()
	user, tok, err := p.Credentials(ctx)
	if err != nil {
		return nil, nil, err
	}
	p.mu.Lock()
	if p.closed {
		p.mu.Unlock()
		return nil, nil, errors.New("azredis: entra id provider closed")
	}
	if listener != nil {
		p.listeners[listener] = struct{}{}
	}
	if !p.running {
		p.running = true
		go p.loop()
	}
	p.mu.Unlock()

	unsub := func() error {
		p.mu.Lock()
		delete(p.listeners, listener)
		p.mu.Unlock()
		return nil
	}
	return auth.NewBasicCredentials(user, tok), unsub, nil
}

// Close stops the background refresh loop. Safe to call multiple times.
func (p *EntraIDProvider) Close() error {
	p.closeOnce.Do(func() {
		p.mu.Lock()
		p.closed = true
		running := p.running
		p.mu.Unlock()
		close(p.stop)
		if running {
			<-p.done
		}
	})
	return nil
}

// loop refreshes the token ahead of expiry and notifies listeners.
func (p *EntraIDProvider) loop() {
	defer close(p.done)
	for {
		p.mu.Lock()
		wait := p.refreshAt.Sub(p.now())
		p.mu.Unlock()
		if wait < 0 {
			wait = 0
		}
		t := time.NewTimer(wait)
		select {
		case <-p.stop:
			t.Stop()
			return
		case <-t.C:
		}

		ctx, cancel := context.WithTimeout(context.Background(), defaultFetchTimeout)
		p.mu.Lock()
		changed, err := p.fetchLocked(ctx)
		if err != nil || !changed {
			// Retry soon, but never past the current token's expiry.
			retryAt := p.now().Add(p.retryInterval)
			if exp := p.token.ExpiresOn; !exp.IsZero() && retryAt.After(exp) && exp.After(p.now()) {
				retryAt = exp
			}
			p.refreshAt = retryAt
		}
		creds := auth.NewBasicCredentials(p.username, p.token.Token)
		ls := make([]auth.CredentialsListener, 0, len(p.listeners))
		for l := range p.listeners {
			ls = append(ls, l)
		}
		p.mu.Unlock()
		cancel()

		for _, l := range ls {
			switch {
			case err != nil:
				l.OnError(err)
			case changed:
				l.OnNext(creds)
			}
		}
	}
}

// fetchLocked obtains a token from the credential. It reports whether the
// token differs from the cached one. p.mu must be held.
func (p *EntraIDProvider) fetchLocked(ctx context.Context) (changed bool, err error) {
	tok, err := p.cred.GetToken(ctx, policy.TokenRequestOptions{Scopes: p.scopes})
	if err != nil {
		return false, fmt.Errorf("azredis: get entra id token: %w", err)
	}
	if tok.Token == "" {
		return false, errors.New("azredis: get entra id token: empty token")
	}
	user := p.objectID
	if user == "" {
		if user = objectIDFromJWT(tok.Token); user == "" {
			return false, ErrNoObjectID
		}
	}
	changed = tok.Token != p.token.Token
	now := p.now()
	margin := p.refreshBefore
	if life := tok.ExpiresOn.Sub(now); margin > life/2 {
		margin = life / 2
	}
	p.token = tok
	p.username = user
	if changed {
		p.refreshAt = tok.ExpiresOn.Add(-margin)
	}
	return changed, nil
}

// objectIDFromJWT returns the "oid" claim of a JWT access token, or "" if
// the token is not a parseable JWT. The signature is not verified — the
// server does that; we only need the claim to pick the username.
func objectIDFromJWT(token string) string {
	parts := strings.Split(token, ".")
	if len(parts) != 3 {
		return ""
	}
	payload, err := base64.RawURLEncoding.DecodeString(strings.TrimRight(parts[1], "="))
	if err != nil {
		return ""
	}
	var claims struct {
		OID string `json:"oid"`
	}
	if json.Unmarshal(payload, &claims) != nil {
		return ""
	}
	return claims.OID
}
