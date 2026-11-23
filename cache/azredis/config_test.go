package azredis

import (
	"context"
	"crypto/tls"
	"errors"
	"testing"

	"github.com/alicebob/miniredis/v2"
	"github.com/redis/go-redis/v9"
)

func TestEndpointHelpers(t *testing.T) {
	if got, want := CacheEndpoint("mycache"), "mycache.redis.cache.windows.net:6380"; got != want {
		t.Fatalf("CacheEndpoint = %q, want %q", got, want)
	}
	if got, want := ManagedRedisEndpoint("amr", "eastus"), "amr.eastus.redis.azure.net:10000"; got != want {
		t.Fatalf("ManagedRedisEndpoint = %q, want %q", got, want)
	}
}

func TestWithDefaultPort(t *testing.T) {
	cases := map[string]string{
		"host":            "host:6380",
		"host:10000":      "host:10000",
		"10.0.0.1":        "10.0.0.1:6380",
		"[::1]:6380":      "[::1]:6380",
		"a.redis.net:123": "a.redis.net:123",
	}
	for in, want := range cases {
		if got := withDefaultPort(in, DefaultTLSPort); got != want {
			t.Errorf("withDefaultPort(%q) = %q, want %q", in, got, want)
		}
	}
}

func TestNew_EmptyAddress(t *testing.T) {
	if _, err := New(&Config{Addrs: []string{""}}); !errors.Is(err, ErrInvalidConfig) {
		t.Fatalf("err = %v, want ErrInvalidConfig", err)
	}
}

func TestNew_TLSDefaults(t *testing.T) {
	c, err := New(&Config{Addrs: []string{"mycache.redis.cache.windows.net"}})
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	defer func() { _ = c.Close() }()
	rc, ok := c.rdb.(*redis.Client)
	if !ok {
		t.Fatalf("single address should yield *redis.Client, got %T", c.rdb)
	}
	o := rc.Options()
	if o.Addr != "mycache.redis.cache.windows.net:6380" {
		t.Fatalf("Addr = %q, want default TLS port 6380", o.Addr)
	}
	if o.TLSConfig == nil || o.TLSConfig.MinVersion != tls.VersionTLS12 {
		t.Fatalf("TLS 1.2 should be enabled by default, got %+v", o.TLSConfig)
	}
}

func TestNew_CustomTLSAndPlaintextPort(t *testing.T) {
	custom := &tls.Config{MinVersion: tls.VersionTLS13, ServerName: "x"}
	c, err := New(&Config{Addrs: []string{"h"}, TLSConfig: custom})
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	if got := c.rdb.(*redis.Client).Options().TLSConfig; got != custom {
		t.Fatalf("custom TLS config not used")
	}
	_ = c.Close()

	c, err = New(&Config{Addrs: []string{"h"}, DisableTLS: true})
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	o := c.rdb.(*redis.Client).Options()
	if o.TLSConfig != nil || o.Addr != "h:6379" {
		t.Fatalf("DisableTLS: Addr=%q TLS=%v", o.Addr, o.TLSConfig)
	}
	_ = c.Close()
}

func TestNew_ClusterMode(t *testing.T) {
	c, err := New(&Config{Addrs: []string{ManagedRedisEndpoint("amr", "westeurope")}, ClusterMode: true})
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	defer func() { _ = c.Close() }()
	if _, ok := c.rdb.(*redis.ClusterClient); !ok {
		t.Fatalf("ClusterMode should yield *redis.ClusterClient, got %T", c.rdb)
	}
}

func TestClient_AccessKeyAuth(t *testing.T) {
	mr := miniredis.RunT(t)
	mr.RequireAuth("primary-access-key")

	bad, _ := New(&Config{Addrs: []string{mr.Addr()}, Password: "wrong", DisableTLS: true})
	defer func() { _ = bad.Close() }()
	if err := bad.Ping(context.Background()); err == nil {
		t.Fatalf("Ping with wrong access key should fail")
	}

	good, _ := New(&Config{Addrs: []string{mr.Addr()}, Password: "primary-access-key", DisableTLS: true})
	defer func() { _ = good.Close() }()
	if err := good.Ping(context.Background()); err != nil {
		t.Fatalf("Ping with access key: %v", err)
	}
}

func TestClient_CredentialsProviderError(t *testing.T) {
	mr := miniredis.RunT(t)
	sentinel := errors.New("no token")
	c, _ := New(&Config{
		Addrs:      []string{mr.Addr()},
		DisableTLS: true,
		CredentialsProvider: func(context.Context) (string, error) {
			return "", sentinel
		},
	})
	defer func() { _ = c.Close() }()
	if err := c.Ping(context.Background()); !errors.Is(err, sentinel) {
		t.Fatalf("Ping err = %v, want wrapped sentinel", err)
	}
}

func TestClient_CloseTwice(t *testing.T) {
	c, _ := withMiniRedis(t)
	if err := c.Close(); err != nil {
		t.Fatalf("Close: %v", err)
	}
	if err := c.Close(); err != nil {
		t.Fatalf("second Close: %v", err)
	}
}

func TestClient_GetOnClosedClient(t *testing.T) {
	c, _ := withMiniRedis(t)
	_ = c.Close()
	ctx := context.Background()
	if _, ok := c.Get(ctx, "k"); ok {
		t.Fatal("Get on closed client should miss")
	}
	if c.Delete(ctx, "k") || c.Has(ctx, "k") || c.Len(ctx) != 0 {
		t.Fatal("ops on closed client should report false / 0")
	}
}

func TestClient_ClusterMode_ClearAndLen(t *testing.T) {
	mr := miniredis.RunT(t)
	c, err := New(&Config{Addrs: []string{mr.Addr()}, ClusterMode: true, DisableTLS: true})
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	defer func() { _ = c.Close() }()
	ctx := context.Background()
	for _, k := range []string{"a", "b", "c"} {
		if err := c.Set(ctx, k, []byte("v")); err != nil {
			t.Fatalf("Set(%s): %v", k, err)
		}
	}
	if got := c.Len(ctx); got != 3 {
		t.Fatalf("Len = %d, want 3", got)
	}
	if err := c.Clear(ctx); err != nil {
		t.Fatalf("Clear: %v", err)
	}
	if got := c.Len(ctx); got != 0 {
		t.Fatalf("Len after Clear = %d, want 0", got)
	}
}
