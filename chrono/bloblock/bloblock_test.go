package bloblock

import (
	"context"
	"errors"
	"fmt"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/Azure/azure-sdk-for-go/sdk/azcore"
)

// fakeBackend is an in-memory substitute for the blob client with
// matching If-Match / If-None-Match precondition semantics on a single
// blob slot. Every mutating operation advances the ETag.
type fakeBackend struct {
	mu       sync.Mutex
	exists   bool
	body     []byte
	etag     azcore.ETag
	nextGen  int64
	calls    atomic.Int64
	failNext atomic.Int64
}

var errInjected = errors.New("injected transport error")

func newFakeBackend() *fakeBackend { return &fakeBackend{nextGen: 100} }

func (b *fakeBackend) advanceETag() {
	b.nextGen++
	b.etag = azcore.ETag(fmt.Sprintf("\"0x%d\"", b.nextGen))
}

func (b *fakeBackend) injected() bool {
	b.calls.Add(1)
	if n := b.failNext.Load(); n > 0 {
		b.failNext.Add(-1)
		return true
	}
	return false
}

func (b *fakeBackend) read(context.Context) ([]byte, azcore.ETag, error) {
	if b.injected() {
		return nil, "", errInjected
	}
	b.mu.Lock()
	defer b.mu.Unlock()
	if !b.exists {
		return nil, "", errNotFound
	}
	return append([]byte(nil), b.body...), b.etag, nil
}

func (b *fakeBackend) create(_ context.Context, body []byte) (azcore.ETag, error) {
	if b.injected() {
		return "", errInjected
	}
	b.mu.Lock()
	defer b.mu.Unlock()
	if b.exists {
		return "", errPreconditionFailed
	}
	b.exists = true
	b.body = append([]byte(nil), body...)
	b.advanceETag()
	return b.etag, nil
}

func (b *fakeBackend) replace(_ context.Context, body []byte, etag azcore.ETag) (azcore.ETag, error) {
	if b.injected() {
		return "", errInjected
	}
	b.mu.Lock()
	defer b.mu.Unlock()
	if !b.exists || b.etag != etag {
		return "", errPreconditionFailed
	}
	b.body = append([]byte(nil), body...)
	b.advanceETag()
	return b.etag, nil
}

func (b *fakeBackend) remove(_ context.Context, etag azcore.ETag) error {
	if b.injected() {
		return errInjected
	}
	b.mu.Lock()
	defer b.mu.Unlock()
	if !b.exists {
		return errNotFound
	}
	if b.etag != etag {
		return errPreconditionFailed
	}
	b.exists = false
	b.body = nil
	b.advanceETag()
	return nil
}

// snapshot returns a copy of the fake's stored state.
func (b *fakeBackend) snapshot() (exists bool, body []byte, etag azcore.ETag) {
	b.mu.Lock()
	defer b.mu.Unlock()
	return b.exists, append([]byte(nil), b.body...), b.etag
}

type mockClock struct {
	mu  sync.Mutex
	now time.Time
}

func newMockClock(t time.Time) *mockClock { return &mockClock{now: t} }

func (c *mockClock) Now() time.Time {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.now
}

func (c *mockClock) Advance(d time.Duration) {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.now = c.now.Add(d)
}

func newTestElector(be backend, owner string, clock *mockClock) *Elector {
	e := newWithBackend(be, Options{
		Container: "locks",
		Blob:      "chrono/sched.lock",
		Owner:     owner,
		Lease:     10 * time.Second,
	})
	e.now = clock.Now
	return e
}

var t0 = time.Unix(1_700_000_000, 0)

func TestClaim_FirstAcquirer(t *testing.T) {
	be := newFakeBackend()
	clock := newMockClock(t0)
	e := newTestElector(be, "instance-a", clock)

	if err := e.tick(context.Background()); err != nil {
		t.Fatalf("tick: %v", err)
	}
	if !e.IsLeader(context.Background()) {
		t.Fatalf("expected leader after first tick")
	}
	exists, body, _ := be.snapshot()
	if !exists {
		t.Fatalf("expected blob created")
	}
	doc := parseDoc(body)
	if doc.Owner != "instance-a" {
		t.Fatalf("owner = %q, want instance-a", doc.Owner)
	}
	if want := clock.Now().Add(30 * time.Second); !doc.LeaseUntil.Equal(want) {
		t.Fatalf("lease_until = %v, want %v", doc.LeaseUntil, want)
	}
}

func TestRenew_RetainsLeadershipAcrossLoops(t *testing.T) {
	be := newFakeBackend()
	clock := newMockClock(t0)
	e := newTestElector(be, "instance-a", clock)

	if err := e.tick(context.Background()); err != nil {
		t.Fatalf("tick1: %v", err)
	}
	_, body1, tag1 := be.snapshot()
	first := parseDoc(body1).LeaseUntil

	clock.Advance(4 * time.Second)
	if err := e.tick(context.Background()); err != nil {
		t.Fatalf("tick2: %v", err)
	}
	if !e.IsLeader(context.Background()) {
		t.Fatalf("expected still leader after renewal")
	}
	_, body2, tag2 := be.snapshot()
	if !parseDoc(body2).LeaseUntil.After(first) {
		t.Fatalf("lease_until did not advance")
	}
	if tag2 == tag1 || e.heldETag != tag2 {
		t.Fatalf("ETag did not advance / not tracked: %s %s %s", tag1, tag2, e.heldETag)
	}
}

func TestSteal_AfterLeaseExpiry(t *testing.T) {
	be := newFakeBackend()
	clock := newMockClock(t0)

	incumbent := newTestElector(be, "instance-a", clock)
	if err := incumbent.tick(context.Background()); err != nil {
		t.Fatalf("incumbent tick: %v", err)
	}
	clock.Advance(31 * time.Second)

	challenger := newTestElector(be, "instance-b", clock)
	if err := challenger.tick(context.Background()); err != nil {
		t.Fatalf("challenger tick: %v", err)
	}
	if !challenger.IsLeader(context.Background()) {
		t.Fatalf("challenger should have stolen expired lease")
	}
	_, body, _ := be.snapshot()
	if parseDoc(body).Owner != "instance-b" {
		t.Fatalf("owner should be challenger after steal")
	}

	// The former leader notices on its next tick.
	if err := incumbent.tick(context.Background()); err != nil {
		t.Fatalf("incumbent tick2: %v", err)
	}
	if incumbent.IsLeader(context.Background()) {
		t.Fatal("incumbent should have lost leadership")
	}
}

func TestSteal_CorruptBodyTreatedAsExpired(t *testing.T) {
	be := newFakeBackend()
	be.exists, be.body = true, []byte("not json")
	be.advanceETag()
	e := newTestElector(be, "instance-a", newMockClock(t0))
	if err := e.tick(context.Background()); err != nil {
		t.Fatal(err)
	}
	if !e.IsLeader(context.Background()) {
		t.Fatal("corrupt lock should be stolen")
	}
}

func TestSecondInstance_NotLeader_WhileFirstAlive(t *testing.T) {
	be := newFakeBackend()
	clock := newMockClock(t0)
	a := newTestElector(be, "instance-a", clock)
	b := newTestElector(be, "instance-b", clock)

	if err := a.tick(context.Background()); err != nil {
		t.Fatalf("a tick: %v", err)
	}
	clock.Advance(2 * time.Second)
	if err := b.tick(context.Background()); err != nil {
		t.Fatalf("b tick: %v", err)
	}
	if !a.IsLeader(context.Background()) || b.IsLeader(context.Background()) {
		t.Fatalf("expected a leader, b not")
	}
	_, body, _ := be.snapshot()
	if parseDoc(body).Owner != "instance-a" {
		t.Fatalf("stored owner should remain instance-a")
	}
}

// racingBackend lets the read succeed but makes the subsequent If-Match
// write lose (another instance wrote in between).
type racingBackend struct {
	*fakeBackend
	replaceErr error
	readErr    error
}

func (r *racingBackend) read(ctx context.Context) ([]byte, azcore.ETag, error) {
	if r.readErr != nil {
		return nil, "", r.readErr
	}
	return r.fakeBackend.read(ctx)
}

func (r *racingBackend) replace(ctx context.Context, body []byte, etag azcore.ETag) (azcore.ETag, error) {
	if r.replaceErr != nil {
		return "", r.replaceErr
	}
	return r.fakeBackend.replace(ctx, body, etag)
}

func TestTick_RaceAndErrorPaths(t *testing.T) {
	ctx := context.Background()
	clock := newMockClock(t0)

	// Lost race on renew → not leader, no error.
	fb := newFakeBackend()
	e := newTestElector(&racingBackend{fakeBackend: fb, replaceErr: errPreconditionFailed}, "a", clock)
	_ = e.tick(ctx) // claims
	e.leader.Store(true)
	if err := e.tick(ctx); err != nil || e.IsLeader(ctx) {
		t.Fatalf("lost race: err=%v leader=%v", err, e.IsLeader(ctx))
	}

	// Transport error on renew → error, leadership preserved.
	e = newTestElector(&racingBackend{fakeBackend: fb, replaceErr: errInjected}, "a", clock)
	e.leader.Store(true)
	if err := e.tick(ctx); !errors.Is(err, errInjected) || !e.IsLeader(ctx) {
		t.Fatalf("renew transport error: err=%v leader=%v", err, e.IsLeader(ctx))
	}

	// Blob vanished between create and read → not leader.
	e = newTestElector(&racingBackend{fakeBackend: fb, readErr: errNotFound}, "a", clock)
	e.leader.Store(true)
	if err := e.tick(ctx); err != nil || e.IsLeader(ctx) {
		t.Fatalf("vanished: err=%v leader=%v", err, e.IsLeader(ctx))
	}

	// Read transport error → error.
	e = newTestElector(&racingBackend{fakeBackend: fb, readErr: errInjected}, "a", clock)
	if err := e.tick(ctx); !errors.Is(err, errInjected) {
		t.Fatalf("read error = %v", err)
	}

	// Create transport error → error, prior state preserved.
	fb2 := newFakeBackend()
	fb2.failNext.Store(1)
	e = newTestElector(fb2, "a", clock)
	e.leader.Store(true)
	if err := e.tick(ctx); !errors.Is(err, errInjected) || !e.IsLeader(ctx) {
		t.Fatalf("create error: err=%v leader=%v", err, e.IsLeader(ctx))
	}
}

func TestResign_ClearsRecord_WhenLeader(t *testing.T) {
	be := newFakeBackend()
	e := newTestElector(be, "instance-a", newMockClock(t0))

	if err := e.Start(context.Background()); err != nil {
		t.Fatalf("Start: %v", err)
	}
	if !e.IsLeader(context.Background()) {
		t.Fatalf("expected leader after Start")
	}
	if err := e.Resign(context.Background()); err != nil {
		t.Fatalf("Resign: %v", err)
	}
	if e.IsLeader(context.Background()) {
		t.Fatalf("expected NOT leader after Resign")
	}
	if exists, _, _ := be.snapshot(); exists {
		t.Fatalf("expected blob cleared on Resign")
	}
}

func TestResign_NoOp_WhenNotLeader(t *testing.T) {
	be := newFakeBackend()
	clock := newMockClock(t0)
	a := newTestElector(be, "instance-a", clock)
	b := newTestElector(be, "instance-b", clock)

	if err := a.Start(context.Background()); err != nil {
		t.Fatal(err)
	}
	if err := b.Start(context.Background()); err != nil {
		t.Fatal(err)
	}
	if !a.IsLeader(context.Background()) || b.IsLeader(context.Background()) {
		t.Fatalf("expected a leader, b not")
	}
	if err := b.Resign(context.Background()); err != nil {
		t.Fatalf("b Resign: %v", err)
	}
	exists, body, _ := be.snapshot()
	if !exists || parseDoc(body).Owner != "instance-a" {
		t.Fatalf("b Resign clobbered a's ownership")
	}
	if err := a.Resign(context.Background()); err != nil {
		t.Fatalf("a Resign: %v", err)
	}
}

func TestResign_AfterSteal_KeepsSuccessorLock(t *testing.T) {
	be := newFakeBackend()
	clock := newMockClock(t0)
	a := newTestElector(be, "instance-a", clock)
	if err := a.Start(context.Background()); err != nil {
		t.Fatal(err)
	}
	clock.Advance(time.Minute)
	b := newTestElector(be, "instance-b", clock)
	if err := b.tick(context.Background()); err != nil || !b.IsLeader(context.Background()) {
		t.Fatalf("b should steal: %v", err)
	}
	// a still believes it is leader; its If-Match delete must not remove b's lock.
	if err := a.Resign(context.Background()); err != nil {
		t.Fatalf("a Resign: %v", err)
	}
	if exists, body, _ := be.snapshot(); !exists || parseDoc(body).Owner != "instance-b" {
		t.Fatal("successor lock removed by stale resign")
	}
}

func TestResign_Errors(t *testing.T) {
	// Not started → no-op.
	if err := newTestElector(newFakeBackend(), "a", newMockClock(t0)).Resign(context.Background()); err != nil {
		t.Fatalf("Resign before Start: %v", err)
	}
	// Already deleted → nil.
	be := newFakeBackend()
	e := newTestElector(be, "a", newMockClock(t0))
	_ = e.Start(context.Background())
	be.mu.Lock()
	be.exists = false
	be.mu.Unlock()
	if err := e.Resign(context.Background()); err != nil {
		t.Fatalf("Resign on missing blob: %v", err)
	}
	// Transport error surfaces.
	be = newFakeBackend()
	e = newTestElector(be, "a", newMockClock(t0))
	_ = e.Start(context.Background())
	be.failNext.Store(1)
	if err := e.Resign(context.Background()); !errors.Is(err, errInjected) {
		t.Fatalf("Resign transport error = %v", err)
	}
}

func TestIsLeader_ReturnsAtomicallyFast(t *testing.T) {
	be := &blockingBackend{ch: make(chan struct{})}
	defer close(be.ch)
	e := newTestElector(be, "instance-a", newMockClock(t0))
	e.leader.Store(true)

	done := make(chan bool, 1)
	go func() { done <- e.IsLeader(context.Background()) }()
	select {
	case v := <-done:
		if !v {
			t.Fatalf("IsLeader returned false, want true")
		}
	case <-time.After(100 * time.Millisecond):
		t.Fatalf("IsLeader did not return within 100ms — likely blocked on I/O")
	}
}

// blockingBackend blocks every backend call until ch closes.
type blockingBackend struct{ ch chan struct{} }

func (b *blockingBackend) wait(ctx context.Context) error {
	select {
	case <-ctx.Done():
		return ctx.Err()
	case <-b.ch:
		return nil
	}
}
func (b *blockingBackend) read(ctx context.Context) ([]byte, azcore.ETag, error) {
	return nil, "", b.wait(ctx)
}
func (b *blockingBackend) create(ctx context.Context, _ []byte) (azcore.ETag, error) {
	return "", b.wait(ctx)
}
func (b *blockingBackend) replace(ctx context.Context, _ []byte, _ azcore.ETag) (azcore.ETag, error) {
	return "", b.wait(ctx)
}
func (b *blockingBackend) remove(ctx context.Context, _ azcore.ETag) error { return b.wait(ctx) }

func TestStart_IsIdempotent_AndLoopRenews(t *testing.T) {
	be := newFakeBackend()
	e := newWithBackend(be, Options{Container: "c", Blob: "b", Owner: "a", Lease: 20 * time.Millisecond})
	if err := e.Start(context.Background()); err != nil {
		t.Fatalf("Start: %v", err)
	}
	if err := e.Start(context.Background()); err != nil {
		t.Fatalf("second Start: %v", err)
	}
	_, _, tag1 := be.snapshot()
	time.Sleep(60 * time.Millisecond)
	_, _, tag2 := be.snapshot()
	if tag1 == tag2 {
		t.Fatal("background loop did not renew")
	}
	if err := e.Resign(context.Background()); err != nil {
		t.Fatal(err)
	}
}

func TestValidate_RejectsBadOptions(t *testing.T) {
	cases := map[string]Options{
		"no container": {Blob: "x", Owner: "y", Lease: time.Second},
		"no blob":      {Container: "c", Owner: "y", Lease: time.Second},
		"no owner":     {Container: "c", Blob: "x", Lease: time.Second},
		"no lease":     {Container: "c", Blob: "x", Owner: "y"},
	}
	for name, opts := range cases {
		t.Run(name, func(t *testing.T) {
			if err := newWithBackend(newFakeBackend(), opts).Start(context.Background()); err == nil {
				t.Fatalf("expected validation error for %s", name)
			}
		})
	}
}

func TestParseDoc_RoundTrip(t *testing.T) {
	in := lockDoc{Owner: "x", LeaseUntil: t0.UTC()}
	b, err := marshalDoc(in)
	if err != nil {
		t.Fatal(err)
	}
	if out := parseDoc(b); out.Owner != in.Owner || !out.LeaseUntil.Equal(in.LeaseUntil) {
		t.Fatalf("round trip = %+v", out)
	}
	if out := parseDoc([]byte("{")); out.Owner != "" || !out.LeaseUntil.IsZero() {
		t.Fatal("corrupt body should parse to zero doc")
	}
}
