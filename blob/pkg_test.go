package blob

import (
	"context"
	"errors"
	"net/url"
	"strings"
	"testing"
	"time"

	"github.com/Azure/azure-sdk-for-go/sdk/azcore"
	"github.com/Azure/azure-sdk-for-go/sdk/azcore/cloud"
	"github.com/Azure/azure-sdk-for-go/sdk/azcore/policy"
	"oss.nandlabs.io/golly-azure/azcfg"
	"oss.nandlabs.io/golly-azure/blob/blobtest"
)

type staticCred struct{}

func (staticCred) GetToken(context.Context, policy.TokenRequestOptions) (azcore.AccessToken, error) {
	return azcore.AccessToken{Token: "t", ExpiresOn: time.Now().Add(time.Hour)}, nil
}

func TestNewServiceClient_Variants(t *testing.T) {
	srv := blobtest.NewServer()
	defer srv.Close()

	if _, err := newServiceClient(nil, "a"); err == nil {
		t.Error("nil config should error")
	}

	cs := azcfg.NewConfig()
	cs.SetConnectionString(srv.ConnectionString())
	c, err := newServiceClient(cs, "ignored")
	if err != nil || !strings.HasPrefix(c.URL(), srv.ServiceURL()) {
		t.Errorf("connection string client = %v, %v", c, err)
	}

	sk := azcfg.NewConfig()
	sk.SetSharedKey("", blobtest.AccountKey)
	c, err = newServiceClient(sk, "myacct")
	if err != nil || c.URL() != "https://myacct.blob.core.windows.net/" {
		t.Errorf("shared key client url = %v, %v", c, err)
	}
	bad := azcfg.NewConfig()
	bad.SetSharedKey("acct", "not-base64!!")
	if _, err := newServiceClient(bad, "acct"); err == nil {
		t.Error("invalid shared key should error")
	}

	sas := azcfg.NewConfig()
	sas.SetEndpoint("https://acct.blob.core.windows.net/?sv=2024&sig=abc")
	if c, err := newServiceClient(sas, "acct"); err != nil || !strings.Contains(c.URL(), "sig=abc") {
		t.Errorf("SAS client = %v, %v", c, err)
	}

	tok := azcfg.NewConfig()
	tok.SetCredential(staticCred{})
	tok.SetCloud(cloud.AzureChina)
	if c, err := newServiceClient(tok, "cn"); err != nil || c.URL() != "https://cn.blob.core.chinacloudapi.cn/" {
		t.Errorf("token client = %v, %v", c, err)
	}
	if _, err := newServiceClient(tok, ""); err == nil {
		t.Error("missing account should error")
	}

	broken := azcfg.NewConfig()
	broken.ClientSecret = "secret" // no tenant / client id
	if _, err := newServiceClient(broken, "acct"); !errors.Is(err, azcfg.ErrIncompleteClientSecret) {
		t.Errorf("credential error = %v", err)
	}
}

func TestStorageSuffix(t *testing.T) {
	cases := map[string]cloud.Configuration{
		"core.windows.net":       cloud.AzurePublic,
		"core.chinacloudapi.cn":  cloud.AzureChina,
		"core.usgovcloudapi.net": cloud.AzureGovernment,
	}
	for want, c := range cases {
		if got := storageSuffix(c); got != want {
			t.Errorf("storageSuffix = %q, want %q", got, want)
		}
	}
	if storageSuffix(cloud.Configuration{}) != "core.windows.net" {
		t.Error("zero cloud should default to public")
	}
}

func TestResolveConfig_Order(t *testing.T) {
	u, _ := url.Parse("azblob://resolveacct/c/k")
	opts, _ := parseURL(u)

	if got := resolveConfig(opts); got != fallbackConfig() || got == nil {
		t.Fatal("expected package fallback config")
	}

	def := azcfg.NewConfig()
	azcfg.Manager.Register(azcfg.DefaultConfigName, def)
	t.Cleanup(func() { azcfg.Manager.Unregister(azcfg.DefaultConfigName) })
	if resolveConfig(opts) != def {
		t.Fatal("expected default config")
	}

	scheme := azcfg.NewConfig()
	azcfg.Manager.Register(BlobScheme, scheme)
	t.Cleanup(func() { azcfg.Manager.Unregister(BlobScheme) })
	if resolveConfig(opts) != scheme {
		t.Fatal("expected scheme config")
	}

	acct := azcfg.NewConfig()
	azcfg.Manager.Register("resolveacct", acct)
	t.Cleanup(func() { azcfg.Manager.Unregister("resolveacct") })
	if resolveConfig(opts) != acct {
		t.Fatal("expected account config")
	}
}

func TestGetContainerClient_Error(t *testing.T) {
	broken := azcfg.NewConfig()
	broken.ClientSecret = "secret"
	azcfg.Manager.Register("brokenacct", broken)
	t.Cleanup(func() { azcfg.Manager.Unregister("brokenacct") })
	fs := newFS()
	raw := "azblob://brokenacct/c/k"
	if _, err := fs.OpenRaw(raw); err == nil {
		t.Error("OpenRaw should surface credential errors")
	}
	u, _ := url.Parse(raw)
	if _, err := fs.ListIter(context.Background(), u); err == nil {
		t.Error("ListIter should surface credential errors")
	}
	if err := fs.Walk(u, nil); err == nil {
		t.Error("Walk should surface credential errors")
	}
	if err := fs.Copy(u, u); err == nil {
		t.Error("Copy should surface credential errors")
	}
	if err := fs.Delete(u); err == nil {
		t.Error("Delete should surface credential errors")
	}
	if _, err := fs.Create(u); err == nil {
		t.Error("Create should surface credential errors")
	}
	if _, err := fs.MkdirAll(u); err == nil {
		t.Error("MkdirAll should surface credential errors")
	}
}
