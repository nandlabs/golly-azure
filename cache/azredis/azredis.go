package azredis

import (
	"context"
	"crypto/tls"
	"errors"
	"fmt"
	"net"
	"strconv"
	"sync"
	"time"

	"github.com/redis/go-redis/v9"
	"github.com/redis/go-redis/v9/auth"
	"oss.nandlabs.io/golly/cache"
)

// Well-known Azure Redis ports.
const (
	// DefaultTLSPort is the TLS port of Azure Cache for Redis. It is
	// appended to Config.Addrs entries that carry no port.
	DefaultTLSPort = 6380
	// DefaultNonTLSPort is the (disabled-by-default) plaintext port of
	// Azure Cache for Redis. Used for port-less addresses when
	// Config.DisableTLS is true.
	DefaultNonTLSPort = 6379
	// ManagedRedisPort is the TLS port of Azure Managed Redis (and Azure
	// Cache for Redis Enterprise).
	ManagedRedisPort = 10000
)

// CacheEndpoint returns the TLS endpoint (host:port) of an Azure Cache for
// Redis instance in the public cloud: <name>.redis.cache.windows.net:6380.
func CacheEndpoint(name string) string {
	return net.JoinHostPort(name+".redis.cache.windows.net", strconv.Itoa(DefaultTLSPort))
}

// ManagedRedisEndpoint returns the endpoint (host:port) of an Azure Managed
// Redis instance in the public cloud: <name>.<region>.redis.azure.net:10000.
func ManagedRedisEndpoint(name, region string) string {
	return net.JoinHostPort(name+"."+region+".redis.azure.net", strconv.Itoa(ManagedRedisPort))
}

// Config configures an Azure Redis connection.
type Config struct {
	// Addrs is the list of endpoints (host:port). A single entry is
	// treated as a primary endpoint unless ClusterMode is set; multiple
	// entries trigger go-redis's cluster-aware client. Entries without a
	// port get DefaultTLSPort (or DefaultNonTLSPort with DisableTLS).
	Addrs []string

	// ClusterMode forces the cluster-aware client even with a single
	// address. Set it for Azure Cache for Redis Premium with clustering
	// and Azure Managed Redis with the OSS cluster policy.
	ClusterMode bool

	// Username is the Redis user. Leave empty for access-key auth. With
	// Entra ID it is the principal's object ID (NewWithEntraID fills it
	// in automatically).
	Username string

	// Password is a static access key. Ignored if CredentialsProvider or
	// StreamingCredentialsProvider is set.
	Password string

	// CredentialsProvider returns a fresh password (e.g. an Entra ID
	// access token) on every connection attempt, paired with Username.
	// EntraIDProvider.Token fits this signature. Prefer
	// StreamingCredentialsProvider for Entra ID: it also re-authenticates
	// already-open connections before the token expires.
	CredentialsProvider func(ctx context.Context) (string, error)

	// StreamingCredentialsProvider supplies credentials that change over
	// the connection lifetime; go-redis re-AUTHs pooled connections when
	// it publishes new ones. *EntraIDProvider implements it. Takes
	// precedence over CredentialsProvider and Password.
	StreamingCredentialsProvider auth.StreamingCredentialsProvider

	// TLSConfig controls TLS in transit. The zero value enables TLS with
	// MinVersion = TLS 1.2 (Azure's minimum). Set a custom *tls.Config to
	// override.
	TLSConfig *tls.Config

	// DisableTLS forces plaintext. Default false. Use only against test
	// endpoints (miniredis, local fakes) — Azure disables the non-TLS
	// port by default.
	DisableTLS bool

	// PoolSize, ReadTimeout, WriteTimeout, DialTimeout — passthroughs to
	// go-redis's UniversalOptions. Zero values use go-redis defaults.
	PoolSize     int
	ReadTimeout  time.Duration
	WriteTimeout time.Duration
	DialTimeout  time.Duration
}

// Client is a cache.Cache[string, []byte] backed by Azure Cache for Redis
// or Azure Managed Redis. Values are stored as raw bytes; pair with a
// typed wrapper (or codec) at the call site for richer values.
//
// Client also satisfies the optional cache.Sweeper and cache.Loader
// capability interfaces (see capabilities.go).
type Client struct {
	rdb redis.UniversalClient

	// closers are released by Close after the redis client (e.g. an
	// EntraIDProvider created by NewWithEntraID).
	closers []func() error

	// flightMu guards flights. In-process single-flight coordination for
	// GetOrLoad; see capabilities.go.
	flightMu sync.Mutex
	flights  map[string]*flightCall
}

// Compile-time check that Client satisfies cache.Cache[string, []byte]
// and the optional Sweeper + Loader capability interfaces.
var (
	_ cache.Cache[string, []byte]  = (*Client)(nil)
	_ cache.Sweeper                = (*Client)(nil)
	_ cache.Loader[string, []byte] = (*Client)(nil)
)

// ErrInvalidConfig is returned by New when Config is unusable.
var ErrInvalidConfig = errors.New("azredis: invalid config")

// New returns a ready-to-use Client. Returns ErrInvalidConfig if Addrs is
// empty. The underlying redis client connects lazily — New itself does
// not perform I/O. Call Client.Ping(ctx) to validate connectivity.
func New(cfg *Config) (*Client, error) {
	if cfg == nil || len(cfg.Addrs) == 0 {
		return nil, fmt.Errorf("%w: at least one address required", ErrInvalidConfig)
	}

	defPort := DefaultTLSPort
	if cfg.DisableTLS {
		defPort = DefaultNonTLSPort
	}
	addrs := make([]string, 0, len(cfg.Addrs))
	for _, a := range cfg.Addrs {
		if a == "" {
			return nil, fmt.Errorf("%w: empty address", ErrInvalidConfig)
		}
		addrs = append(addrs, withDefaultPort(a, defPort))
	}

	opts := &redis.UniversalOptions{
		Addrs:         addrs,
		IsClusterMode: cfg.ClusterMode,
		Username:      cfg.Username,
		Password:      cfg.Password,
		PoolSize:      cfg.PoolSize,
		ReadTimeout:   cfg.ReadTimeout,
		WriteTimeout:  cfg.WriteTimeout,
		DialTimeout:   cfg.DialTimeout,
	}

	switch {
	case cfg.StreamingCredentialsProvider != nil:
		opts.StreamingCredentialsProvider = cfg.StreamingCredentialsProvider
	case cfg.CredentialsProvider != nil:
		username := cfg.Username
		opts.CredentialsProviderContext = func(ctx context.Context) (string, string, error) {
			token, err := cfg.CredentialsProvider(ctx)
			if err != nil {
				return "", "", fmt.Errorf("azredis: credentials provider: %w", err)
			}
			return username, token, nil
		}
	}

	if !cfg.DisableTLS {
		if cfg.TLSConfig != nil {
			opts.TLSConfig = cfg.TLSConfig
		} else {
			opts.TLSConfig = &tls.Config{MinVersion: tls.VersionTLS12}
		}
	}

	return &Client{
		rdb:     redis.NewUniversalClient(opts),
		flights: make(map[string]*flightCall),
	}, nil
}

// NewWithEntraID returns a Client that authenticates with Microsoft Entra
// ID. It builds an EntraIDProvider from opts, fetches the first token
// (to resolve the object-ID username and fail fast on credential
// problems), and wires the provider in as the streaming credentials
// provider so pooled connections are re-authenticated before each token
// expires. cfg.Username, Password, CredentialsProvider and
// StreamingCredentialsProvider are ignored. Closing the Client stops the
// provider's refresh loop.
func NewWithEntraID(ctx context.Context, cfg *Config, opts EntraIDOptions) (*Client, error) {
	if cfg == nil || len(cfg.Addrs) == 0 {
		return nil, fmt.Errorf("%w: at least one address required", ErrInvalidConfig)
	}
	p, err := NewEntraIDProvider(opts)
	if err != nil {
		return nil, err
	}
	user, _, err := p.Credentials(ctx)
	if err != nil {
		_ = p.Close()
		return nil, err
	}
	c2 := *cfg
	c2.Username = user
	c2.Password = ""
	c2.CredentialsProvider = nil
	c2.StreamingCredentialsProvider = p
	c, err := New(&c2)
	if err != nil {
		_ = p.Close()
		return nil, err
	}
	c.closers = append(c.closers, p.Close)
	return c, nil
}

// withDefaultPort appends port to addr when addr carries none.
func withDefaultPort(addr string, port int) string {
	if _, _, err := net.SplitHostPort(addr); err == nil {
		return addr
	}
	return net.JoinHostPort(addr, strconv.Itoa(port))
}

// Ping verifies connectivity by issuing a Redis PING. Useful right after
// New to surface auth/TLS misconfiguration up front.
func (c *Client) Ping(ctx context.Context) error {
	return c.rdb.Ping(ctx).Err()
}

// Get returns the value for key. Missing keys return (nil, false).
func (c *Client) Get(ctx context.Context, key string) ([]byte, bool) {
	v, err := c.rdb.Get(ctx, key).Bytes()
	if err != nil {
		return nil, false
	}
	return v, true
}

// Set stores value under key with no expiry.
func (c *Client) Set(ctx context.Context, key string, value []byte) error {
	return c.rdb.Set(ctx, key, value, 0).Err()
}

// SetWithTTL stores value under key with the given TTL. A ttl of
// cache.NoExpiry (zero) means the entry never expires.
func (c *Client) SetWithTTL(ctx context.Context, key string, value []byte, ttl time.Duration) error {
	return c.rdb.Set(ctx, key, value, ttl).Err()
}

// Delete removes the key. Returns true when the key existed.
func (c *Client) Delete(ctx context.Context, key string) bool {
	n, err := c.rdb.Del(ctx, key).Result()
	if err != nil {
		return false
	}
	return n > 0
}

// Has reports whether key is present (and not expired server-side).
func (c *Client) Has(ctx context.Context, key string) bool {
	n, err := c.rdb.Exists(ctx, key).Result()
	if err != nil {
		return false
	}
	return n > 0
}

// Clear empties the cache via FLUSHDB. This is a destructive admin
// operation — only call against caches that exclusively belong to this
// application. Note that Azure Managed Redis may restrict FLUSHDB
// depending on the cluster policy and access policy assigned to the user.
//
// For clustered deployments FLUSHDB runs on every master.
func (c *Client) Clear(ctx context.Context) error {
	switch v := c.rdb.(type) {
	case *redis.ClusterClient:
		return v.ForEachMaster(ctx, func(ctx context.Context, m *redis.Client) error {
			return m.FlushDB(ctx).Err()
		})
	default:
		return c.rdb.FlushDB(ctx).Err()
	}
}

// Len returns the total number of keys via DBSIZE. For clustered
// deployments it sums DBSIZE across masters. Approximate under concurrent
// writes.
func (c *Client) Len(ctx context.Context) int {
	switch v := c.rdb.(type) {
	case *redis.ClusterClient:
		var mu sync.Mutex
		total := int64(0)
		_ = v.ForEachMaster(ctx, func(ctx context.Context, m *redis.Client) error {
			n, err := m.DBSize(ctx).Result()
			if err == nil {
				mu.Lock()
				total += n
				mu.Unlock()
			}
			return nil
		})
		return int(total)
	default:
		n, err := c.rdb.DBSize(ctx).Result()
		if err != nil {
			return 0
		}
		return int(n)
	}
}

// Close releases the underlying redis client and its connection pool, and
// stops any EntraIDProvider created by NewWithEntraID. Safe to call
// multiple times.
func (c *Client) Close() error {
	err := c.rdb.Close()
	if errors.Is(err, redis.ErrClosed) {
		err = nil
	}
	for _, f := range c.closers {
		if cerr := f(); cerr != nil && err == nil {
			err = cerr
		}
	}
	return err
}
