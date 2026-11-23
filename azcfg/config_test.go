package azcfg

import (
	"context"
	"errors"
	"net/url"
	"testing"

	"github.com/Azure/azure-sdk-for-go/sdk/azcore"
	"github.com/Azure/azure-sdk-for-go/sdk/azcore/cloud"
	"github.com/Azure/azure-sdk-for-go/sdk/azcore/policy"
)

type fakeCred struct{}

func (fakeCred) GetToken(context.Context, policy.TokenRequestOptions) (azcore.AccessToken, error) {
	return azcore.AccessToken{Token: "t"}, nil
}

func TestCredentialOverride(t *testing.T) {
	c := NewConfig()
	c.SetCredential(fakeCred{})
	got, err := c.Credential()
	if err != nil {
		t.Fatal(err)
	}
	if _, ok := got.(fakeCred); !ok {
		t.Fatalf("expected override credential, got %T", got)
	}
}

func TestClientSecretIncomplete(t *testing.T) {
	c := NewConfig()
	c.SetClientSecret("", "client", "secret")
	if _, err := c.Credential(); !errors.Is(err, ErrIncompleteClientSecret) {
		t.Fatalf("expected ErrIncompleteClientSecret, got %v", err)
	}
}

func TestClientSecretCached(t *testing.T) {
	c := NewConfig()
	c.SetClientSecret("tenant", "client", "secret")
	a, err := c.Credential()
	if err != nil {
		t.Fatal(err)
	}
	b, _ := c.Credential()
	if a != b {
		t.Fatal("expected cached credential")
	}
	c.SetTenantID("other")
	d, _ := c.Credential()
	if a == d {
		t.Fatal("expected credential rebuilt after config change")
	}
}

func TestManagedIdentity(t *testing.T) {
	c := NewConfig()
	c.SetManagedIdentity("client-id")
	if _, err := c.Credential(); err != nil {
		t.Fatal(err)
	}
}

func TestClientOptsCloud(t *testing.T) {
	c := NewConfig()
	if c.ClientOpts().Cloud.ActiveDirectoryAuthorityHost != "" {
		t.Fatal("expected zero cloud by default")
	}
	c.SetCloud(cloud.AzureChina)
	if c.ClientOpts().Cloud.ActiveDirectoryAuthorityHost != cloud.AzureChina.ActiveDirectoryAuthorityHost {
		t.Fatal("expected china cloud")
	}
}

func TestSetters(t *testing.T) {
	c := NewConfig()
	c.SetSubscriptionID("sub")
	c.SetResourceGroup("rg")
	c.SetLocation("eastus")
	c.SetConnectionString("cs")
	c.SetSharedKey("acct", "key")
	c.SetEndpoint("http://127.0.0.1:10000")
	c.SetAccountName("acct2")
	if c.SubscriptionID != "sub" || c.ResourceGroup != "rg" || c.Location != "eastus" ||
		c.ConnectionString != "cs" || c.AccountKey != "key" || c.AccountName != "acct2" || c.Endpoint == "" {
		t.Fatalf("setters not applied: %+v", c)
	}
}

func TestGetConfig(t *testing.T) {
	a, b, d := NewConfig(), NewConfig(), NewConfig()
	Manager.Register("acct", a)
	Manager.Register("acct/container", b)
	Manager.Register("fallback", d)
	defer func() {
		Manager.Unregister("acct")
		Manager.Unregister("acct/container")
		Manager.Unregister("fallback")
	}()

	if GetConfig(nil, "fallback") != d {
		t.Fatal("nil url should use name")
	}
	if GetConfig(&url.URL{Host: "acct", Path: "/x"}, "") != a {
		t.Fatal("host match expected")
	}
	if GetConfig(&url.URL{Host: "acctx", Path: "/container"}, "") != nil {
		t.Fatal("no match expected")
	}
	Manager.Register("ns/queue", b)
	defer Manager.Unregister("ns/queue")
	if GetConfig(&url.URL{Host: "ns", Path: "/queue"}, "") != b {
		t.Fatal("host+path match expected")
	}
	if GetConfig(&url.URL{Host: "none"}, "fallback") != d {
		t.Fatal("fallback expected")
	}
	if GetConfigOrDefault(&url.URL{Host: "none"}, "missing") == nil {
		t.Fatal("expected non-nil default config")
	}
}
