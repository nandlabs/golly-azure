package blob

import (
	"io"
	"net/url"
	"testing"

	"github.com/Azure/azure-sdk-for-go/sdk/azcore/policy"
	"oss.nandlabs.io/golly-azure/azcfg"
	"oss.nandlabs.io/golly-azure/blob/blobtest"
	"oss.nandlabs.io/golly/vfs"
)

const testContainer = "data"

// newTestServer starts a fake Blob service, creates testContainer and
// registers an azcfg.Config for blobtest.AccountName pointing at it.
func newTestServer(t *testing.T) *blobtest.Server {
	t.Helper()
	srv := blobtest.NewServer()
	t.Cleanup(srv.Close)
	srv.CreateContainer(testContainer)
	registerAccount(t, blobtest.AccountName, srv)
	return srv
}

// registerAccount registers an azcfg.Config for account that targets srv
// with the Azurite shared key and retries disabled.
func registerAccount(t *testing.T, account string, srv *blobtest.Server) {
	t.Helper()
	cfg := azcfg.NewConfig()
	cfg.SetEndpoint(srv.ServiceURL())
	cfg.SetSharedKey(blobtest.AccountName, blobtest.AccountKey)
	cfg.ClientOptions.Retry = policy.RetryOptions{MaxRetries: -1}
	azcfg.Manager.Register(account, cfg)
	t.Cleanup(func() { azcfg.Manager.Unregister(account) })
}

// testURL returns azblob://devstoreaccount1/data/<key>.
func testURL(key string) string {
	return "azblob://" + blobtest.AccountName + "/" + testContainer + "/" + key
}

func mustURL(t *testing.T, raw string) *url.URL {
	t.Helper()
	u, err := url.Parse(raw)
	if err != nil {
		t.Fatalf("url.Parse(%q): %v", raw, err)
	}
	return u
}

func newFS() *BlobFS {
	fs := &BlobFS{}
	fs.BaseVFS = &vfs.BaseVFS{VFileSystem: fs}
	return fs
}

func readAll(t *testing.T, f vfs.VFile) string {
	t.Helper()
	b, err := io.ReadAll(f)
	if err != nil {
		t.Fatalf("ReadAll: %v", err)
	}
	return string(b)
}

func keysOf(files []vfs.VFile) map[string]bool {
	out := map[string]bool{}
	for _, f := range files {
		out[f.(*BlobFile).urlOpts.Key] = true
	}
	return out
}
