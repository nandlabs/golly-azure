package blob

import (
	"context"
	"fmt"
	"io"
	"os"
	"strings"
	"testing"
	"time"

	"oss.nandlabs.io/golly-azure/azcfg"
	"oss.nandlabs.io/golly-azure/blob/blobtest"
	"oss.nandlabs.io/golly/vfs"
)

// azuriteBase registers an azcfg.Config targeting the Azurite emulator and
// returns the azblob:// base URL of a fresh container. The test is skipped
// unless AZURITE_BLOB_ENDPOINT is set, e.g.
//
//	docker run -p 10000:10000 mcr.microsoft.com/azure-storage/azurite azurite-blob --blobHost 0.0.0.0
//	AZURITE_BLOB_ENDPOINT=http://127.0.0.1:10000/devstoreaccount1 go test ./blob/...
//
// AZURITE_ACCOUNT_NAME / AZURITE_ACCOUNT_KEY override the well-known
// devstoreaccount1 credentials.
func azuriteBase(t *testing.T) string {
	t.Helper()
	endpoint := os.Getenv("AZURITE_BLOB_ENDPOINT")
	if endpoint == "" {
		t.Skip("AZURITE_BLOB_ENDPOINT not set; skipping Azurite emulator test")
	}
	account := envOr("AZURITE_ACCOUNT_NAME", blobtest.AccountName)
	key := envOr("AZURITE_ACCOUNT_KEY", blobtest.AccountKey)

	cfg := azcfg.NewConfig()
	cfg.SetEndpoint(endpoint)
	cfg.SetSharedKey(account, key)
	azcfg.Manager.Register(account, cfg)
	t.Cleanup(func() { azcfg.Manager.Unregister(account) })

	container := fmt.Sprintf("golly-%d", time.Now().UnixNano())
	base := "azblob://" + account + "/" + container
	if _, err := vfs.GetManager().MkdirAllRaw(base); err != nil {
		t.Fatalf("create container: %v", err)
	}
	t.Cleanup(func() {
		if svc, err := newServiceClient(cfg, account); err == nil {
			_, _ = svc.DeleteContainer(context.Background(), container, nil)
		}
	})
	return base
}

func envOr(key, def string) string {
	if v := os.Getenv(key); v != "" {
		return v
	}
	return def
}

func TestEmulator_EndToEnd(t *testing.T) {
	base := azuriteBase(t)
	mgr := vfs.GetManager()
	ctx := context.Background()

	for name, body := range map[string]string{
		"/data/hello.txt":       "Hello from Azurite!",
		"/data/sub/report.csv":  "a,b\n1,2",
		"/data/sub/deep/x.json": `{"x":1}`,
	} {
		f, err := mgr.CreateRaw(base + name)
		if err != nil {
			t.Fatalf("Create %s: %v", name, err)
		}
		if _, err := f.WriteString(body); err != nil {
			t.Fatal(err)
		}
		if err := f.Close(); err != nil {
			t.Fatalf("Close %s: %v", name, err)
		}
	}

	f, _ := mgr.OpenRaw(base + "/data/hello.txt")
	if s, err := vfs.ReadAllString(f); err != nil || s != "Hello from Azurite!" {
		t.Fatalf("read = %q, %v", s, err)
	}
	if b, err := vfs.ReadRange(ctx, f, 6, 4); err != nil || string(b) != "from" {
		t.Fatalf("ReadRange = %q, %v", b, err)
	}
	if _, err := vfs.ReadRange(ctx, f, 1000, 1); err != io.EOF {
		t.Fatalf("ReadRange past EOF = %v", err)
	}
	if err := f.AddProperty("team", "golly"); err != nil {
		t.Fatalf("AddProperty: %v", err)
	}
	if v, err := f.GetProperty("team"); err != nil || v != "golly" {
		t.Fatalf("GetProperty = %q, %v", v, err)
	}

	files, err := mgr.ListRaw(base + "/data/")
	if err != nil || len(files) != 2 {
		t.Fatalf("List = %d, %v", len(files), err)
	}
	var walked []string
	if err := mgr.WalkRaw(base+"/data/", func(f vfs.VFile) error {
		walked = append(walked, f.Url().Path)
		return nil
	}); err != nil || len(walked) != 3 {
		t.Fatalf("Walk = %v, %v", walked, err)
	}

	if err := mgr.CopyRaw(base+"/data/", base+"/backup/"); err != nil {
		t.Fatalf("Copy dir: %v", err)
	}
	if err := mgr.MoveRaw(base+"/backup/hello.txt", base+"/moved.txt"); err != nil {
		t.Fatalf("Move: %v", err)
	}
	m, _ := mgr.OpenRaw(base + "/moved.txt")
	if s, _ := vfs.ReadAllString(m); !strings.HasPrefix(s, "Hello") {
		t.Fatalf("moved content = %q", s)
	}
	if err := mgr.DeleteRaw(base + "/data/"); err != nil {
		t.Fatalf("Delete dir: %v", err)
	}
	if files, _ := mgr.ListRaw(base + "/data/"); len(files) != 0 {
		t.Fatalf("data/ not empty after delete: %d", len(files))
	}
}
