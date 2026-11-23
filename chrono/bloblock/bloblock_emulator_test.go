package bloblock

import (
	"context"
	"fmt"
	"os"
	"testing"
	"time"

	"github.com/Azure/azure-sdk-for-go/sdk/storage/azblob"
	"github.com/Azure/azure-sdk-for-go/sdk/storage/azblob/bloberror"
)

// newEmulatorClient dials Azurite at AZURITE_BLOB_ENDPOINT (for example
// http://127.0.0.1:10000/devstoreaccount1) with the well-known
// devstoreaccount1 key (override with AZURITE_ACCOUNT_NAME /
// AZURITE_ACCOUNT_KEY). The test is skipped unless the endpoint is set.
// A fresh container is created per test and removed afterwards.
func newEmulatorClient(t *testing.T) (*azblob.Client, string) {
	t.Helper()
	endpoint := os.Getenv("AZURITE_BLOB_ENDPOINT")
	if endpoint == "" {
		t.Skip("AZURITE_BLOB_ENDPOINT not set; skipping Azurite smoke test")
	}
	name := envOr("AZURITE_ACCOUNT_NAME", "devstoreaccount1")
	key := envOr("AZURITE_ACCOUNT_KEY",
		"Eby8vdM02xNOcqFlqUwJPLlmEtlCDXJ1OUzFT50uSRZ6IFsuFq2UVErCz4I6tq/K1SZFPTOtr/KBHBeksoGMGw==")
	cred, err := azblob.NewSharedKeyCredential(name, key)
	if err != nil {
		t.Fatalf("shared key: %v", err)
	}
	client, err := azblob.NewClientWithSharedKeyCredential(endpoint, cred, nil)
	if err != nil {
		t.Fatalf("client: %v", err)
	}
	container := fmt.Sprintf("chrono-locks-%d", time.Now().UnixNano())
	ctx := context.Background()
	if _, err := client.CreateContainer(ctx, container, nil); err != nil && !bloberror.HasCode(err, bloberror.ContainerAlreadyExists) {
		t.Fatalf("create container: %v", err)
	}
	t.Cleanup(func() { _, _ = client.DeleteContainer(context.Background(), container, nil) })
	return client, container
}

func envOr(k, def string) string {
	if v := os.Getenv(k); v != "" {
		return v
	}
	return def
}

func TestSmoke_ClaimAndRenew(t *testing.T) {
	client, container := newEmulatorClient(t)
	ctx := context.Background()

	e := New(client, Options{Container: container, Blob: "sched.lock", Owner: "instance-a", Lease: 500 * time.Millisecond})
	if err := e.Start(ctx); err != nil {
		t.Fatalf("Start: %v", err)
	}
	time.Sleep(750 * time.Millisecond)
	if !e.IsLeader(ctx) {
		t.Fatalf("expected leader after smoke tick")
	}
	if err := e.Resign(ctx); err != nil {
		t.Fatalf("Resign: %v", err)
	}
}

func TestSmoke_SecondInstanceLoses(t *testing.T) {
	client, container := newEmulatorClient(t)
	ctx := context.Background()

	a := New(client, Options{Container: container, Blob: "sched.lock", Owner: "a", Lease: 500 * time.Millisecond})
	b := New(client, Options{Container: container, Blob: "sched.lock", Owner: "b", Lease: 500 * time.Millisecond})
	if err := a.Start(ctx); err != nil {
		t.Fatalf("a.Start: %v", err)
	}
	if err := b.Start(ctx); err != nil {
		t.Fatalf("b.Start: %v", err)
	}
	time.Sleep(300 * time.Millisecond)
	if !a.IsLeader(ctx) {
		t.Fatalf("expected a to be leader")
	}
	if b.IsLeader(ctx) {
		t.Fatalf("expected b to NOT be leader")
	}
	_ = a.Resign(ctx)
	_ = b.Resign(ctx)
}
