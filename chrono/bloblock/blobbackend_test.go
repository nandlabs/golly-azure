package bloblock

import (
	"context"
	"errors"
	"net/http"
	"testing"
	"time"

	"github.com/Azure/azure-sdk-for-go/sdk/azcore"
	"github.com/Azure/azure-sdk-for-go/sdk/azcore/policy"
	"github.com/Azure/azure-sdk-for-go/sdk/storage/azblob"
	"oss.nandlabs.io/golly-azure/blob/blobtest"
)

// newFakeClient returns an azblob client wired to an in-memory fake Blob
// service with the "locks" container pre-created.
func newFakeClient(t *testing.T) (*azblob.Client, *blobtest.Server) {
	t.Helper()
	srv := blobtest.NewServer()
	t.Cleanup(srv.Close)
	srv.CreateContainer("locks")
	c, err := azblob.NewClientFromConnectionString(srv.ConnectionString(), &azblob.ClientOptions{
		ClientOptions: policy.ClientOptions{Retry: policy.RetryOptions{MaxRetries: -1}},
	})
	if err != nil {
		t.Fatalf("client: %v", err)
	}
	return c, srv
}

func TestBlobBackend_ElectionAgainstFakeService(t *testing.T) {
	client, srv := newFakeClient(t)
	ctx := context.Background()
	clock := newMockClock(t0)
	opts := Options{Container: "locks", Blob: "sched.lock", Lease: 10 * time.Second}

	a := New(client, withOwner(opts, "a"))
	a.now = clock.Now
	b := New(client, withOwner(opts, "b"))
	b.now = clock.Now

	if err := a.tick(ctx); err != nil || !a.IsLeader(ctx) {
		t.Fatalf("a claim: %v leader=%v", err, a.IsLeader(ctx))
	}
	if data, ok := srv.GetBlob("locks", "sched.lock"); !ok || parseDoc(data).Owner != "a" {
		t.Fatalf("stored lock = %q", data)
	}
	if ct := srv.BlobContentType("locks", "sched.lock"); ct != "application/json" {
		t.Fatalf("content type = %q", ct)
	}
	if err := b.tick(ctx); err != nil || b.IsLeader(ctx) {
		t.Fatalf("b should lose: %v leader=%v", err, b.IsLeader(ctx))
	}
	clock.Advance(5 * time.Second)
	if err := a.tick(ctx); err != nil || !a.IsLeader(ctx) {
		t.Fatalf("a renew: %v", err)
	}
	clock.Advance(time.Minute)
	if err := b.tick(ctx); err != nil || !b.IsLeader(ctx) {
		t.Fatalf("b steal: %v", err)
	}
	if data, _ := srv.GetBlob("locks", "sched.lock"); parseDoc(data).Owner != "b" {
		t.Fatalf("owner after steal = %q", data)
	}
	// a's stale ETag must not clobber b's lock.
	a.started = true
	a.leader.Store(true)
	if err := a.Resign(ctx); err != nil {
		t.Fatalf("a stale resign: %v", err)
	}
	if _, ok := srv.GetBlob("locks", "sched.lock"); !ok {
		t.Fatal("stale resign deleted successor lock")
	}
	b.started = true
	if err := b.Resign(ctx); err != nil {
		t.Fatalf("b resign: %v", err)
	}
	if _, ok := srv.GetBlob("locks", "sched.lock"); ok {
		t.Fatal("lock not deleted on resign")
	}
}

func withOwner(o Options, owner string) Options {
	o.Owner = owner
	return o
}

func TestBlobBackend_ErrorNormalisation(t *testing.T) {
	client, srv := newFakeClient(t)
	ctx := context.Background()
	be := &blobBackend{c: client.ServiceClient().NewContainerClient("locks").NewBlockBlobClient("x")}

	if _, _, err := be.read(ctx); !errors.Is(err, errNotFound) {
		t.Fatalf("read missing = %v", err)
	}
	tag, err := be.create(ctx, []byte("{}"))
	if err != nil || tag == "" {
		t.Fatalf("create = %q, %v", tag, err)
	}
	if _, err := be.create(ctx, []byte("{}")); !errors.Is(err, errPreconditionFailed) {
		t.Fatalf("second create = %v", err)
	}
	if _, err := be.replace(ctx, []byte("{}"), "\"stale\""); !errors.Is(err, errPreconditionFailed) {
		t.Fatalf("stale replace = %v", err)
	}
	if err := be.remove(ctx, "\"stale\""); !errors.Is(err, errPreconditionFailed) {
		t.Fatalf("stale remove = %v", err)
	}
	body, cur, err := be.read(ctx)
	if err != nil || string(body) != "{}" || cur != tag {
		t.Fatalf("read = %q %q %v", body, cur, err)
	}
	if err := be.remove(ctx, cur); err != nil {
		t.Fatalf("remove: %v", err)
	}
	if err := be.remove(ctx, cur); !errors.Is(err, errNotFound) {
		t.Fatalf("remove missing = %v", err)
	}

	// Transport / server errors pass through untouched.
	srv.InjectError(http.MethodPut, http.StatusInternalServerError, "InternalError")
	_, err = be.create(ctx, []byte("{}"))
	var re *azcore.ResponseError
	if errors.Is(err, errPreconditionFailed) || !errors.As(err, &re) || re.StatusCode != 500 {
		t.Fatalf("500 create = %v", err)
	}
	// Missing container is not "lock not found".
	missing := &blobBackend{c: client.ServiceClient().NewContainerClient("nope").NewBlockBlobClient("x")}
	if _, _, err := missing.read(ctx); errors.Is(err, errNotFound) || err == nil {
		t.Fatalf("missing container read = %v", err)
	}
}

func TestErrorPredicates(t *testing.T) {
	if isPrecondition(nil) || isNotFound(nil) || normalize(nil) != nil {
		t.Fatal("nil handling")
	}
	if !isPrecondition(&azcore.ResponseError{StatusCode: 412}) {
		t.Fatal("412 should be a precondition failure")
	}
	if !isNotFound(&azcore.ResponseError{StatusCode: 404}) {
		t.Fatal("bare 404 should be not-found")
	}
	if isNotFound(&azcore.ResponseError{StatusCode: 404, ErrorCode: "ContainerNotFound"}) {
		t.Fatal("ContainerNotFound must not look like a missing lock")
	}
	plain := errors.New("x")
	if normalize(plain) != plain {
		t.Fatal("unknown errors must pass through")
	}
}

func TestNewWithBlockBlobClient(t *testing.T) {
	client, _ := newFakeClient(t)
	bb := client.ServiceClient().NewContainerClient("locks").NewBlockBlobClient("direct.lock")
	e := NewWithBlockBlobClient(bb, Options{Container: "locks", Blob: "direct.lock", Owner: "o", Lease: time.Second})
	if err := e.Start(context.Background()); err != nil {
		t.Fatal(err)
	}
	if !e.IsLeader(context.Background()) {
		t.Fatal("expected leader")
	}
	if err := e.Resign(context.Background()); err != nil {
		t.Fatal(err)
	}
}
