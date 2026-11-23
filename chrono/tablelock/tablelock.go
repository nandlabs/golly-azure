package tablelock

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"github.com/Azure/azure-sdk-for-go/sdk/azcore"
	"github.com/Azure/azure-sdk-for-go/sdk/data/aztables"
	"oss.nandlabs.io/golly-azure/azcfg"
	"oss.nandlabs.io/golly/chrono"
)

// DefaultPartitionKey is the PartitionKey used when Options.PartitionKey
// is empty.
const DefaultPartitionKey = "chrono-lock"

// Entity property names — kept as constants so the schema is documented
// in one place and easy to keep in sync with README.md.
const (
	propOwner      = "Owner"
	propLeaseUntil = "LeaseUntil"
	propVersion    = "Version"
)

// ErrInvalidConfig is returned by NewFromConfig when the azcfg.Config
// cannot be turned into a table client.
var ErrInvalidConfig = errors.New("tablelock: invalid config")

// Options configures a Table-Storage-backed leader elector.
type Options struct {
	// PartitionKey is the entity PartitionKey. Defaults to
	// DefaultPartitionKey ("chrono-lock"). Several locks may share a
	// partition; they are distinguished by Key (the RowKey).
	PartitionKey string
	// Key is the RowKey that identifies THIS lock. All contenders for the
	// same leadership role MUST agree on table + PartitionKey + Key.
	Key string
	// Owner is the identity of THIS instance. It should be globally
	// unique across contenders (hostname+pid+random is a good default).
	Owner string
	// Lease is the renewal cadence. The elector attempts renewal every
	// Lease/2, and the persisted expiry is now + 3*Lease. Choose Lease
	// larger than expected clock skew and GC pauses.
	Lease time.Duration
}

// tableClient is the subset of *aztables.Client the elector uses. Tests
// substitute an in-memory fake with matching ETag semantics.
type tableClient interface {
	AddEntity(ctx context.Context, entity []byte, options *aztables.AddEntityOptions) (aztables.AddEntityResponse, error)
	GetEntity(ctx context.Context, partitionKey string, rowKey string, options *aztables.GetEntityOptions) (aztables.GetEntityResponse, error)
	UpdateEntity(ctx context.Context, entity []byte, options *aztables.UpdateEntityOptions) (aztables.UpdateEntityResponse, error)
	DeleteEntity(ctx context.Context, partitionKey string, rowKey string, options *aztables.DeleteEntityOptions) (aztables.DeleteEntityResponse, error)
}

// Compile-time checks.
var (
	_ tableClient          = (*aztables.Client)(nil)
	_ chrono.LeaderElector = (*Elector)(nil)
)

// Elector implements chrono.LeaderElector on top of a single Azure Table
// entity guarded by ETag-conditional writes.
type Elector struct {
	tc   tableClient
	opts Options
	now  func() time.Time

	leader atomic.Bool

	// heldETag / heldVersion describe the entity version we last wrote
	// successfully. They are touched only by the renewal goroutine (and
	// by Start's synchronous tick before it launches, and by Resign after
	// it has exited), so mu exists purely to keep the race detector and
	// external callers of ETag() honest.
	mu          sync.Mutex
	heldETag    azcore.ETag
	heldVersion int64

	startOnce sync.Once
	started   bool
	cancel    context.CancelFunc
	done      chan struct{}
}

// New constructs an Elector on the given table client. The client must be
// scoped to an existing table (e.g. ServiceClient.NewClient("chronolocks"));
// the elector does not create tables and does not own the client.
func New(client *aztables.Client, opts Options) *Elector {
	return newWithClient(client, opts)
}

// NewFromConfig builds an *aztables.Client for table from an azcfg.Config
// and returns an Elector on it. Credential resolution order:
//
//  1. cfg.ConnectionString (account key or SAS; works with Azurite's
//     "UseDevelopmentStorage=true"-style explicit connection strings).
//  2. cfg.AccountName + cfg.AccountKey (shared key).
//  3. cfg.Credential() (Microsoft Entra ID; requires the "Storage Table
//     Data Contributor" role on the table or account).
//
// The service URL is cfg.Endpoint when set, otherwise
// https://<AccountName>.table.core.windows.net. A nil cfg resolves to
// azcfg.GetConfigOrDefault(nil, azcfg.DefaultConfigName).
func NewFromConfig(cfg *azcfg.Config, table string, opts Options) (*Elector, error) {
	client, err := newTableClient(cfg, table)
	if err != nil {
		return nil, err
	}
	return New(client, opts), nil
}

func newTableClient(cfg *azcfg.Config, table string) (*aztables.Client, error) {
	if table == "" {
		return nil, fmt.Errorf("%w: table name required", ErrInvalidConfig)
	}
	if cfg == nil {
		cfg = azcfg.GetConfigOrDefault(nil, azcfg.DefaultConfigName)
	}
	clientOpts := &aztables.ClientOptions{ClientOptions: cfg.ClientOpts()}

	if cfg.ConnectionString != "" {
		svc, err := aztables.NewServiceClientFromConnectionString(cfg.ConnectionString, clientOpts)
		if err != nil {
			return nil, fmt.Errorf("%w: connection string: %v", ErrInvalidConfig, err)
		}
		return svc.NewClient(table), nil
	}

	endpoint := strings.TrimRight(cfg.Endpoint, "/")
	if endpoint == "" {
		if cfg.AccountName == "" {
			return nil, fmt.Errorf("%w: one of ConnectionString, Endpoint or AccountName required", ErrInvalidConfig)
		}
		endpoint = "https://" + cfg.AccountName + ".table.core.windows.net"
	}
	tableURL := endpoint + "/" + table

	if cfg.AccountName != "" && cfg.AccountKey != "" {
		cred, err := aztables.NewSharedKeyCredential(cfg.AccountName, cfg.AccountKey)
		if err != nil {
			return nil, fmt.Errorf("%w: shared key: %v", ErrInvalidConfig, err)
		}
		return aztables.NewClientWithSharedKey(tableURL, cred, clientOpts)
	}

	cred, err := cfg.Credential()
	if err != nil {
		return nil, fmt.Errorf("tablelock: credential: %w", err)
	}
	return aztables.NewClient(tableURL, cred, clientOpts)
}

// newWithClient is the internal constructor used by New and tests.
func newWithClient(tc tableClient, opts Options) *Elector {
	if opts.PartitionKey == "" {
		opts.PartitionKey = DefaultPartitionKey
	}
	return &Elector{
		tc:   tc,
		opts: opts,
		now:  time.Now,
	}
}

// Start validates configuration and launches the background renewal
// goroutine. Idempotent — repeated calls are no-ops. Start performs one
// synchronous tick before returning so callers who wait briefly can
// observe leadership without racing the ticker.
func (e *Elector) Start(ctx context.Context) error {
	if err := e.validate(); err != nil {
		return err
	}
	e.startOnce.Do(func() {
		loopCtx, cancel := context.WithCancel(context.Background())
		e.cancel = cancel
		e.done = make(chan struct{})
		e.started = true
		_ = e.tick(ctx)
		go e.run(loopCtx)
	})
	return nil
}

// IsLeader reads the atomic leadership flag maintained by the renewal
// loop. It does NOT perform I/O and is safe from hot paths.
func (e *Elector) IsLeader(_ context.Context) bool { return e.leader.Load() }

// Resign stops the renewal loop and, if this instance currently owns the
// lock, deletes the entity with If-Match set to the held ETag so a
// successor that has already stolen the lease is not clobbered.
func (e *Elector) Resign(ctx context.Context) error {
	if !e.started {
		return nil
	}
	if e.cancel != nil {
		e.cancel()
	}
	if e.done != nil {
		<-e.done
	}
	if !e.leader.Load() {
		return nil
	}
	e.mu.Lock()
	etag := e.heldETag
	e.heldETag = ""
	e.heldVersion = 0
	e.mu.Unlock()
	e.leader.Store(false)
	if etag == "" {
		return nil
	}
	_, err := e.tc.DeleteEntity(ctx, e.opts.PartitionKey, e.opts.Key, &aztables.DeleteEntityOptions{IfMatch: &etag})
	if isStatus(err, http.StatusPreconditionFailed) || isStatus(err, http.StatusNotFound) {
		// Someone else already stole the lease (or the entity is gone) —
		// resign is a no-op.
		return nil
	}
	return err
}

func (e *Elector) validate() error {
	if e.opts.Key == "" {
		return errors.New("tablelock: Key required")
	}
	if e.opts.Owner == "" {
		return errors.New("tablelock: Owner required")
	}
	if e.opts.Lease <= 0 {
		return errors.New("tablelock: Lease must be > 0")
	}
	return nil
}

func (e *Elector) run(ctx context.Context) {
	defer close(e.done)
	interval := e.opts.Lease / 2
	if interval <= 0 {
		interval = e.opts.Lease
	}
	t := time.NewTicker(interval)
	defer t.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-t.C:
			_ = e.tick(ctx)
		}
	}
}

// tick runs one renewal attempt.
//
//  1. If we hold an ETag, renew with UpdateEntity(If-Match: held ETag).
//     412 / 404 mean the entity changed hands (or vanished); drop the
//     ETag and fall through to the acquire path.
//  2. Acquire with AddEntity. 409 means the entity exists.
//  3. On 409, GetEntity. If the stored lease is expired or already names
//     us, steal / reclaim with UpdateEntity(If-Match: observed ETag).
//     Otherwise a foreign live lease is present: not leader.
//  4. Transport failures do NOT flip leadership state — the persisted
//     LeaseUntil is the source of truth for other observers.
func (e *Elector) tick(ctx context.Context) error {
	now := e.now()
	expiry := now.Add(3 * e.opts.Lease)

	e.mu.Lock()
	held, version := e.heldETag, e.heldVersion
	e.mu.Unlock()

	// 1. Renew.
	if held != "" {
		etag, err := e.replace(ctx, held, expiry, version+1)
		switch {
		case err == nil:
			e.setHeld(etag, version+1)
			return nil
		case isStatus(err, http.StatusPreconditionFailed), isStatus(err, http.StatusNotFound):
			e.setHeld("", 0)
			e.leader.Store(false)
		default:
			return err
		}
	}

	// 2. Acquire (insert-if-absent).
	body, err := e.marshal(expiry, 1)
	if err != nil {
		return err
	}
	resp, err := e.tc.AddEntity(ctx, body, nil)
	if err == nil {
		e.setHeld(resp.ETag, 1)
		e.leader.Store(true)
		return nil
	}
	if !isStatus(err, http.StatusConflict) {
		return err
	}

	// 3. Inspect the existing entity.
	got, err := e.tc.GetEntity(ctx, e.opts.PartitionKey, e.opts.Key, nil)
	if err != nil {
		if isStatus(err, http.StatusNotFound) {
			// Deleted between our insert and read — retry next tick.
			e.leader.Store(false)
			return nil
		}
		return err
	}
	owner, leaseUntil, curVersion, ok := parseEntity(got.Value)
	if ok && owner != e.opts.Owner && leaseUntil.After(now) {
		// Foreign live lease.
		e.leader.Store(false)
		return nil
	}
	// Expired, ours (e.g. restarted with the same Owner), or corrupt:
	// take it over conditionally on the ETag we just observed.
	etag, err := e.replace(ctx, got.ETag, expiry, curVersion+1)
	switch {
	case err == nil:
		e.setHeld(etag, curVersion+1)
		e.leader.Store(true)
		return nil
	case isStatus(err, http.StatusPreconditionFailed), isStatus(err, http.StatusNotFound):
		// Lost the race to another stealer.
		e.leader.Store(false)
		return nil
	default:
		return err
	}
}

func (e *Elector) setHeld(etag azcore.ETag, version int64) {
	e.mu.Lock()
	e.heldETag, e.heldVersion = etag, version
	e.mu.Unlock()
}

// replace writes the lock entity with UpdateEntity(Replace, If-Match: etag).
func (e *Elector) replace(ctx context.Context, etag azcore.ETag, expiry time.Time, version int64) (azcore.ETag, error) {
	body, err := e.marshal(expiry, version)
	if err != nil {
		return "", err
	}
	resp, err := e.tc.UpdateEntity(ctx, body, &aztables.UpdateEntityOptions{
		IfMatch:    &etag,
		UpdateMode: aztables.UpdateModeReplace,
	})
	if err != nil {
		return "", err
	}
	return resp.ETag, nil
}

func (e *Elector) marshal(expiry time.Time, version int64) ([]byte, error) {
	ent := aztables.EDMEntity{
		Entity: aztables.Entity{
			PartitionKey: e.opts.PartitionKey,
			RowKey:       e.opts.Key,
		},
		Properties: map[string]any{
			propOwner:      e.opts.Owner,
			propLeaseUntil: aztables.EDMInt64(expiry.UnixMilli()),
			propVersion:    aztables.EDMInt64(version),
		},
	}
	return json.Marshal(ent)
}

// parseEntity extracts Owner, LeaseUntil and Version from an entity JSON
// payload. ok=false means the entity is unreadable / missing required
// fields and should be treated as "no lease held".
func parseEntity(b []byte) (owner string, leaseUntil time.Time, version int64, ok bool) {
	var ent aztables.EDMEntity
	if err := json.Unmarshal(b, &ent); err != nil {
		return "", time.Time{}, 0, false
	}
	o, ok1 := ent.Properties[propOwner].(string)
	lu, ok2 := ent.Properties[propLeaseUntil].(aztables.EDMInt64)
	if !ok1 || !ok2 {
		return "", time.Time{}, 0, false
	}
	if v, okv := ent.Properties[propVersion].(aztables.EDMInt64); okv {
		version = int64(v)
	}
	return o, time.UnixMilli(int64(lu)), version, true
}

// isStatus reports whether err is an *azcore.ResponseError with the given
// HTTP status code.
func isStatus(err error, code int) bool {
	if err == nil {
		return false
	}
	var re *azcore.ResponseError
	return errors.As(err, &re) && re.StatusCode == code
}
