package tablelock

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/Azure/azure-sdk-for-go/sdk/azcore"
	"github.com/Azure/azure-sdk-for-go/sdk/azcore/policy"
	"github.com/Azure/azure-sdk-for-go/sdk/data/aztables"
	"oss.nandlabs.io/golly-azure/azcfg"
)

// fakeTable is an in-memory substitute for *aztables.Client with the same
// ETag / conditional semantics as Azure Table Storage on a single entity.
// All operations acquire the same mutex, matching the service's per-entity
// strong consistency.
type fakeTable struct {
	mu       sync.Mutex
	exists   bool
	body     []byte
	etag     azcore.ETag
	seq      int
	calls    atomic.Int64
	failNext atomic.Int64 // if >0, next N calls fail with errInjected

	// beforeUpdate, when set, runs (under no lock) right before an
	// UpdateEntity is evaluated — used to simulate a racing stealer.
	beforeUpdate func()
}

var errInjected = errors.New("injected transport error")

func respErr(code int, ec aztables.TableErrorCode) error {
	return &azcore.ResponseError{StatusCode: code, ErrorCode: string(ec)}
}

func (f *fakeTable) injected() bool {
	f.calls.Add(1)
	if n := f.failNext.Load(); n > 0 {
		f.failNext.Add(-1)
		return true
	}
	return false
}

func (f *fakeTable) bump(body []byte) azcore.ETag {
	f.seq++
	f.exists = true
	f.body = append([]byte(nil), body...)
	f.etag = azcore.ETag(fmt.Sprintf("W/\"etag-%d\"", f.seq))
	return f.etag
}

func (f *fakeTable) AddEntity(_ context.Context, entity []byte, _ *aztables.AddEntityOptions) (aztables.AddEntityResponse, error) {
	if f.injected() {
		return aztables.AddEntityResponse{}, errInjected
	}
	f.mu.Lock()
	defer f.mu.Unlock()
	if f.exists {
		return aztables.AddEntityResponse{}, respErr(http.StatusConflict, aztables.EntityAlreadyExists)
	}
	return aztables.AddEntityResponse{ETag: f.bump(entity), Value: entity}, nil
}

func (f *fakeTable) GetEntity(_ context.Context, _, _ string, _ *aztables.GetEntityOptions) (aztables.GetEntityResponse, error) {
	if f.injected() {
		return aztables.GetEntityResponse{}, errInjected
	}
	f.mu.Lock()
	defer f.mu.Unlock()
	if !f.exists {
		return aztables.GetEntityResponse{}, respErr(http.StatusNotFound, aztables.ResourceNotFound)
	}
	return aztables.GetEntityResponse{ETag: f.etag, Value: append([]byte(nil), f.body...)}, nil
}

func (f *fakeTable) UpdateEntity(_ context.Context, entity []byte, opts *aztables.UpdateEntityOptions) (aztables.UpdateEntityResponse, error) {
	if f.injected() {
		return aztables.UpdateEntityResponse{}, errInjected
	}
	if f.beforeUpdate != nil {
		f.beforeUpdate()
	}
	f.mu.Lock()
	defer f.mu.Unlock()
	if !f.exists {
		return aztables.UpdateEntityResponse{}, respErr(http.StatusNotFound, aztables.ResourceNotFound)
	}
	if opts == nil || opts.IfMatch == nil || (*opts.IfMatch != "*" && *opts.IfMatch != f.etag) {
		return aztables.UpdateEntityResponse{}, respErr(http.StatusPreconditionFailed, aztables.UpdateConditionNotSatisfied)
	}
	return aztables.UpdateEntityResponse{ETag: f.bump(entity)}, nil
}

func (f *fakeTable) DeleteEntity(_ context.Context, _, _ string, opts *aztables.DeleteEntityOptions) (aztables.DeleteEntityResponse, error) {
	if f.injected() {
		return aztables.DeleteEntityResponse{}, errInjected
	}
	f.mu.Lock()
	defer f.mu.Unlock()
	if !f.exists {
		return aztables.DeleteEntityResponse{}, respErr(http.StatusNotFound, aztables.ResourceNotFound)
	}
	if opts != nil && opts.IfMatch != nil && *opts.IfMatch != "*" && *opts.IfMatch != f.etag {
		return aztables.DeleteEntityResponse{}, respErr(http.StatusPreconditionFailed, aztables.UpdateConditionNotSatisfied)
	}
	f.exists, f.body, f.etag = false, nil, ""
	return aztables.DeleteEntityResponse{}, nil
}

type snap struct {
	Exists     bool
	Owner      string
	LeaseUntil time.Time
	Version    int64
}

func (f *fakeTable) snapshot(t *testing.T) snap {
	t.Helper()
	f.mu.Lock()
	defer f.mu.Unlock()
	if !f.exists {
		return snap{}
	}
	o, lu, v, ok := parseEntity(f.body)
	if !ok {
		t.Fatalf("stored entity unparseable: %s", f.body)
	}
	return snap{Exists: true, Owner: o, LeaseUntil: lu, Version: v}
}

// putRaw seeds the fake with an arbitrary entity body.
func (f *fakeTable) putRaw(body []byte) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.bump(body)
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

func newTestElector(tc tableClient, owner string, clock *mockClock) *Elector {
	e := newWithClient(tc, Options{
		Key:   "sched",
		Owner: owner,
		Lease: 10 * time.Second,
	})
	e.now = clock.Now
	return e
}

var t0 = time.UnixMilli(1_700_000_000_000)

func TestClaim_FirstAcquirer(t *testing.T) {
	ft := &fakeTable{}
	clock := newMockClock(t0)
	e := newTestElector(ft, "instance-a", clock)

	if err := e.tick(context.Background()); err != nil {
		t.Fatalf("tick: %v", err)
	}
	if !e.IsLeader(context.Background()) {
		t.Fatalf("expected leader after first tick")
	}
	st := ft.snapshot(t)
	if st.Owner != "instance-a" {
		t.Fatalf("owner = %q, want instance-a", st.Owner)
	}
	if want := clock.Now().Add(30 * time.Second); !st.LeaseUntil.Equal(want) {
		t.Fatalf("LeaseUntil = %v, want %v", st.LeaseUntil, want)
	}
	if st.Version != 1 {
		t.Fatalf("Version = %d, want 1", st.Version)
	}
	if e.opts.PartitionKey != DefaultPartitionKey {
		t.Fatalf("PartitionKey default = %q, want %q", e.opts.PartitionKey, DefaultPartitionKey)
	}
}

func TestRenew_UsesHeldETag(t *testing.T) {
	ft := &fakeTable{}
	clock := newMockClock(t0)
	e := newTestElector(ft, "instance-a", clock)

	if err := e.tick(context.Background()); err != nil {
		t.Fatalf("tick1: %v", err)
	}
	first := ft.snapshot(t).LeaseUntil
	callsBefore := ft.calls.Load()

	clock.Advance(4 * time.Second)
	if err := e.tick(context.Background()); err != nil {
		t.Fatalf("tick2: %v", err)
	}
	if !e.IsLeader(context.Background()) {
		t.Fatalf("expected still leader after renewal")
	}
	st := ft.snapshot(t)
	if !st.LeaseUntil.After(first) {
		t.Fatalf("LeaseUntil did not advance: first=%v second=%v", first, st.LeaseUntil)
	}
	if st.Version != 2 {
		t.Fatalf("Version = %d, want 2", st.Version)
	}
	// Renewal is a single conditional UpdateEntity.
	if got := ft.calls.Load() - callsBefore; got != 1 {
		t.Fatalf("renew used %d calls, want 1", got)
	}
}

func TestSteal_AfterLeaseExpiry(t *testing.T) {
	ft := &fakeTable{}
	clock := newMockClock(t0)

	incumbent := newTestElector(ft, "instance-a", clock)
	if err := incumbent.tick(context.Background()); err != nil {
		t.Fatalf("incumbent tick: %v", err)
	}

	clock.Advance(31 * time.Second) // beyond 3*Lease

	challenger := newTestElector(ft, "instance-b", clock)
	if err := challenger.tick(context.Background()); err != nil {
		t.Fatalf("challenger tick: %v", err)
	}
	if !challenger.IsLeader(context.Background()) {
		t.Fatalf("challenger should have stolen expired lease")
	}
	if ft.snapshot(t).Owner != "instance-b" {
		t.Fatalf("owner should be challenger after steal")
	}

	// The former incumbent's renewal must fail its If-Match and demote it.
	if err := incumbent.tick(context.Background()); err != nil {
		t.Fatalf("incumbent tick after steal: %v", err)
	}
	if incumbent.IsLeader(context.Background()) {
		t.Fatalf("incumbent should have lost leadership after steal")
	}
	if ft.snapshot(t).Owner != "instance-b" {
		t.Fatalf("incumbent clobbered the new owner")
	}
}

func TestSecondInstance_NotLeader_WhileFirstAlive(t *testing.T) {
	ft := &fakeTable{}
	clock := newMockClock(t0)

	a := newTestElector(ft, "instance-a", clock)
	b := newTestElector(ft, "instance-b", clock)

	if err := a.tick(context.Background()); err != nil {
		t.Fatalf("a tick: %v", err)
	}
	clock.Advance(2 * time.Second)
	if err := b.tick(context.Background()); err != nil {
		t.Fatalf("b tick: %v", err)
	}
	if !a.IsLeader(context.Background()) {
		t.Fatalf("a should still be leader")
	}
	if b.IsLeader(context.Background()) {
		t.Fatalf("b should NOT be leader")
	}
	if ft.snapshot(t).Owner != "instance-a" {
		t.Fatalf("stored owner should remain instance-a")
	}
}

func TestReclaim_SameOwnerAfterRestart(t *testing.T) {
	ft := &fakeTable{}
	clock := newMockClock(t0)

	first := newTestElector(ft, "instance-a", clock)
	if err := first.tick(context.Background()); err != nil {
		t.Fatalf("first tick: %v", err)
	}
	// A fresh process with the same Owner has no held ETag but should
	// reclaim its own live lease.
	restarted := newTestElector(ft, "instance-a", clock)
	if err := restarted.tick(context.Background()); err != nil {
		t.Fatalf("restarted tick: %v", err)
	}
	if !restarted.IsLeader(context.Background()) {
		t.Fatalf("same-owner restart should reclaim leadership")
	}
	if v := ft.snapshot(t).Version; v != 2 {
		t.Fatalf("Version = %d, want 2", v)
	}
}

func TestSteal_LosesRace(t *testing.T) {
	ft := &fakeTable{}
	clock := newMockClock(t0)

	a := newTestElector(ft, "instance-a", clock)
	if err := a.tick(context.Background()); err != nil {
		t.Fatalf("a tick: %v", err)
	}
	clock.Advance(31 * time.Second)

	// Simulate instance-c stealing between b's GetEntity and UpdateEntity.
	var once sync.Once
	ft.beforeUpdate = func() {
		once.Do(func() {
			ft.mu.Lock()
			defer ft.mu.Unlock()
			c := newTestElector(nil, "instance-c", clock)
			body, _ := c.marshal(clock.Now().Add(30*time.Second), 9)
			ft.bump(body)
		})
	}
	b := newTestElector(ft, "instance-b", clock)
	if err := b.tick(context.Background()); err != nil {
		t.Fatalf("b tick: %v", err)
	}
	if b.IsLeader(context.Background()) {
		t.Fatalf("b should have lost the steal race")
	}
	if ft.snapshot(t).Owner != "instance-c" {
		t.Fatalf("winner should be instance-c")
	}
}

func TestCorruptEntity_IsTakenOver(t *testing.T) {
	ft := &fakeTable{}
	ft.putRaw([]byte(`{"PartitionKey":"chrono-lock","RowKey":"sched","Garbage":"x"}`))
	clock := newMockClock(t0)
	e := newTestElector(ft, "instance-a", clock)
	if err := e.tick(context.Background()); err != nil {
		t.Fatalf("tick: %v", err)
	}
	if !e.IsLeader(context.Background()) {
		t.Fatalf("corrupt entity should be treated as free")
	}
	if ft.snapshot(t).Owner != "instance-a" {
		t.Fatalf("owner should be instance-a")
	}
}

func TestEntityDeletedBetweenAddAndGet(t *testing.T) {
	// A table whose AddEntity reports a conflict while GetEntity reports
	// not-found models a delete racing between the two calls.
	ft := &racyDeleteTable{fakeTable: &fakeTable{}}
	clock := newMockClock(t0)
	e := newTestElector(ft, "instance-a", clock)
	e.leader.Store(true)
	if err := e.tick(context.Background()); err != nil {
		t.Fatalf("tick: %v", err)
	}
	if e.IsLeader(context.Background()) {
		t.Fatalf("expected not leader after vanished entity")
	}
}

type racyDeleteTable struct{ *fakeTable }

func (r *racyDeleteTable) AddEntity(context.Context, []byte, *aztables.AddEntityOptions) (aztables.AddEntityResponse, error) {
	return aztables.AddEntityResponse{}, respErr(http.StatusConflict, aztables.EntityAlreadyExists)
}

func TestTransportError_PreservesLeadership(t *testing.T) {
	ft := &fakeTable{}
	clock := newMockClock(t0)
	e := newTestElector(ft, "instance-a", clock)
	if err := e.tick(context.Background()); err != nil {
		t.Fatalf("tick: %v", err)
	}
	ft.failNext.Store(1)
	if err := e.tick(context.Background()); !errors.Is(err, errInjected) {
		t.Fatalf("tick err = %v, want injected", err)
	}
	if !e.IsLeader(context.Background()) {
		t.Fatalf("transport error must not flip leadership")
	}

	// Non-leader path: transport errors on Add / Get are surfaced too.
	other := newTestElector(ft, "instance-b", clock)
	ft.failNext.Store(1)
	if err := other.tick(context.Background()); !errors.Is(err, errInjected) {
		t.Fatalf("add err = %v, want injected", err)
	}
	// First call (Add) succeeds with conflict, second (Get) fails.
	ft2 := &failOnGet{fakeTable: ft}
	other.tc = ft2
	if err := other.tick(context.Background()); !errors.Is(err, errInjected) {
		t.Fatalf("get err = %v, want injected", err)
	}
	if other.IsLeader(context.Background()) {
		t.Fatalf("other should not be leader")
	}
}

type failOnGet struct{ *fakeTable }

func (f *failOnGet) GetEntity(context.Context, string, string, *aztables.GetEntityOptions) (aztables.GetEntityResponse, error) {
	return aztables.GetEntityResponse{}, errInjected
}

func TestStealTransportError(t *testing.T) {
	ft := &fakeTable{}
	clock := newMockClock(t0)
	a := newTestElector(ft, "instance-a", clock)
	_ = a.tick(context.Background())
	clock.Advance(31 * time.Second)
	b := newTestElector(&failOnUpdate{fakeTable: ft}, "instance-b", clock)
	if err := b.tick(context.Background()); !errors.Is(err, errInjected) {
		t.Fatalf("steal err = %v, want injected", err)
	}
	if b.IsLeader(context.Background()) {
		t.Fatalf("b should not be leader")
	}
}

type failOnUpdate struct{ *fakeTable }

func (f *failOnUpdate) UpdateEntity(context.Context, []byte, *aztables.UpdateEntityOptions) (aztables.UpdateEntityResponse, error) {
	return aztables.UpdateEntityResponse{}, errInjected
}

func TestRenew_EntityDeletedExternally_Reacquires(t *testing.T) {
	ft := &fakeTable{}
	clock := newMockClock(t0)
	e := newTestElector(ft, "instance-a", clock)
	_ = e.tick(context.Background())

	// Operator deletes the row; renew gets 404 and falls back to AddEntity.
	if _, err := ft.DeleteEntity(context.Background(), "", "", nil); err != nil {
		t.Fatalf("delete: %v", err)
	}
	if err := e.tick(context.Background()); err != nil {
		t.Fatalf("tick: %v", err)
	}
	if !e.IsLeader(context.Background()) {
		t.Fatalf("should re-acquire after external delete")
	}
	if !ft.snapshot(t).Exists {
		t.Fatalf("entity should exist again")
	}
}

func TestResign_DeletesEntity_WhenLeader(t *testing.T) {
	ft := &fakeTable{}
	clock := newMockClock(t0)
	e := newTestElector(ft, "instance-a", clock)

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
	if ft.snapshot(t).Exists {
		t.Fatalf("expected entity deleted, got %+v", ft.snapshot(t))
	}
}

func TestResign_NoOp_WhenNotLeader(t *testing.T) {
	ft := &fakeTable{}
	clock := newMockClock(t0)

	a := newTestElector(ft, "instance-a", clock)
	b := newTestElector(ft, "instance-b", clock)

	if err := a.Start(context.Background()); err != nil {
		t.Fatalf("a Start: %v", err)
	}
	if err := b.Start(context.Background()); err != nil {
		t.Fatalf("b Start: %v", err)
	}
	if !a.IsLeader(context.Background()) || b.IsLeader(context.Background()) {
		t.Fatalf("expected a leader, b not")
	}
	if err := b.Resign(context.Background()); err != nil {
		t.Fatalf("b Resign: %v", err)
	}
	if ft.snapshot(t).Owner != "instance-a" {
		t.Fatalf("b Resign clobbered a's ownership")
	}
	if err := a.Resign(context.Background()); err != nil {
		t.Fatalf("a Resign: %v", err)
	}
}

func TestResign_AfterSteal_DoesNotClobber(t *testing.T) {
	ft := &fakeTable{}
	clock := newMockClock(t0)
	a := newTestElector(ft, "instance-a", clock)
	if err := a.Start(context.Background()); err != nil {
		t.Fatalf("Start: %v", err)
	}
	// b steals behind a's back (a's local flag is still true).
	clock.Advance(31 * time.Second)
	b := newTestElector(ft, "instance-b", clock)
	if err := b.tick(context.Background()); err != nil || !b.IsLeader(context.Background()) {
		t.Fatalf("b steal failed: %v", err)
	}
	if err := a.Resign(context.Background()); err != nil {
		t.Fatalf("a Resign: %v", err)
	}
	if ft.snapshot(t).Owner != "instance-b" {
		t.Fatalf("a's Resign must not delete b's entity")
	}
}

func TestResign_BeforeStart(t *testing.T) {
	e := newTestElector(&fakeTable{}, "a", newMockClock(t0))
	if err := e.Resign(context.Background()); err != nil {
		t.Fatalf("Resign before Start: %v", err)
	}
}

func TestResign_TransportError(t *testing.T) {
	ft := &fakeTable{}
	e := newTestElector(ft, "a", newMockClock(t0))
	if err := e.Start(context.Background()); err != nil {
		t.Fatalf("Start: %v", err)
	}
	ft.failNext.Store(1)
	if err := e.Resign(context.Background()); !errors.Is(err, errInjected) {
		t.Fatalf("Resign err = %v, want injected", err)
	}
}

func TestIsLeader_ReturnsAtomicallyFast(t *testing.T) {
	bt := &blockingTable{ch: make(chan struct{})}
	defer close(bt.ch)
	e := newTestElector(bt, "instance-a", newMockClock(t0))
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

type blockingTable struct {
	fakeTable
	ch chan struct{}
}

func (b *blockingTable) AddEntity(ctx context.Context, _ []byte, _ *aztables.AddEntityOptions) (aztables.AddEntityResponse, error) {
	select {
	case <-ctx.Done():
		return aztables.AddEntityResponse{}, ctx.Err()
	case <-b.ch:
		return aztables.AddEntityResponse{}, nil
	}
}

func TestStart_IsIdempotent(t *testing.T) {
	ft := &fakeTable{}
	e := newTestElector(ft, "instance-a", newMockClock(t0))
	if err := e.Start(context.Background()); err != nil {
		t.Fatalf("Start: %v", err)
	}
	if err := e.Start(context.Background()); err != nil {
		t.Fatalf("second Start: %v", err)
	}
	_ = e.Resign(context.Background())
}

func TestRunLoop_RenewsInBackground(t *testing.T) {
	ft := &fakeTable{}
	e := newWithClient(ft, Options{Key: "k", Owner: "a", Lease: 20 * time.Millisecond})
	if err := e.Start(context.Background()); err != nil {
		t.Fatalf("Start: %v", err)
	}
	time.Sleep(80 * time.Millisecond)
	if v := ft.snapshot(t).Version; v < 2 {
		t.Fatalf("expected background renewals, Version = %d", v)
	}
	if err := e.Resign(context.Background()); err != nil {
		t.Fatalf("Resign: %v", err)
	}
}

func TestValidate_RejectsBadOptions(t *testing.T) {
	cases := map[string]Options{
		"no key":   {Owner: "y", Lease: time.Second},
		"no owner": {Key: "x", Lease: time.Second},
		"no lease": {Key: "x", Owner: "y"},
	}
	for name, opts := range cases {
		t.Run(name, func(t *testing.T) {
			e := newWithClient(&fakeTable{}, opts)
			if err := e.Start(context.Background()); err == nil {
				t.Fatalf("expected validation error for %s", name)
			}
		})
	}
}

func TestMarshal_EntityShape(t *testing.T) {
	e := newWithClient(nil, Options{PartitionKey: "pk", Key: "rk", Owner: "me", Lease: time.Second})
	b, err := e.marshal(t0, 7)
	if err != nil {
		t.Fatalf("marshal: %v", err)
	}
	var m map[string]any
	if err := json.Unmarshal(b, &m); err != nil {
		t.Fatalf("unmarshal: %v", err)
	}
	if m["PartitionKey"] != "pk" || m["RowKey"] != "rk" || m["Owner"] != "me" {
		t.Fatalf("unexpected entity: %s", b)
	}
	if m["LeaseUntil@odata.type"] != "Edm.Int64" {
		t.Fatalf("LeaseUntil must be annotated Edm.Int64: %s", b)
	}
	o, lu, v, ok := parseEntity(b)
	if !ok || o != "me" || !lu.Equal(t0) || v != 7 {
		t.Fatalf("round trip = (%q, %v, %d, %v)", o, lu, v, ok)
	}
	if _, _, _, ok := parseEntity([]byte("not json")); ok {
		t.Fatalf("garbage should not parse")
	}
}

func TestIsStatus(t *testing.T) {
	if isStatus(nil, 404) {
		t.Fatal("nil error is not a status")
	}
	if isStatus(errors.New("x"), 404) {
		t.Fatal("plain error is not a status")
	}
	wrapped := fmt.Errorf("wrap: %w", respErr(http.StatusConflict, aztables.EntityAlreadyExists))
	if !isStatus(wrapped, http.StatusConflict) {
		t.Fatal("wrapped 409 not detected")
	}
}

func TestNewFromConfig(t *testing.T) {
	cases := []struct {
		name    string
		cfg     *azcfg.Config
		table   string
		wantErr bool
	}{
		{"no table", &azcfg.Config{AccountName: "acct"}, "", true},
		{"no endpoint", &azcfg.Config{}, "locks", true},
		{"bad conn string", &azcfg.Config{ConnectionString: "garbage"}, "locks", true},
		{"conn string", &azcfg.Config{ConnectionString: "DefaultEndpointsProtocol=http;AccountName=devstoreaccount1;AccountKey=Eby8vdM02xNOcqFlqUwJPLlmEtlCDXJ1OUzFT50uSRZ6IFsuFq2UVErCz4I6tq/K1SZFPTOtr/KBHBeksoGMGw==;TableEndpoint=http://127.0.0.1:10002/devstoreaccount1;"}, "locks", false},
		{"shared key", &azcfg.Config{AccountName: "acct", AccountKey: "a2V5"}, "locks", false},
		{"bad shared key", &azcfg.Config{AccountName: "acct", AccountKey: "!!not-base64!!"}, "locks", true},
		{"token credential", &azcfg.Config{Endpoint: "https://acct.table.core.windows.net/", TokenCredential: fakeCred{}}, "locks", false},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			e, err := NewFromConfig(tc.cfg, tc.table, Options{Key: "k", Owner: "o", Lease: time.Second})
			if tc.wantErr {
				if err == nil {
					t.Fatalf("expected error")
				}
				return
			}
			if err != nil {
				t.Fatalf("NewFromConfig: %v", err)
			}
			if e == nil || e.tc == nil {
				t.Fatalf("nil elector / client")
			}
		})
	}
}

type fakeCred struct{}

func (fakeCred) GetToken(context.Context, policy.TokenRequestOptions) (azcore.AccessToken, error) {
	return azcore.AccessToken{Token: "t", ExpiresOn: time.Now().Add(time.Hour)}, nil
}
