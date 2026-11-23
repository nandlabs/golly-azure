package bloblock

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"sync"
	"sync/atomic"
	"time"

	"github.com/Azure/azure-sdk-for-go/sdk/azcore"
	"github.com/Azure/azure-sdk-for-go/sdk/azcore/streaming"
	"github.com/Azure/azure-sdk-for-go/sdk/azcore/to"
	"github.com/Azure/azure-sdk-for-go/sdk/storage/azblob"
	"github.com/Azure/azure-sdk-for-go/sdk/storage/azblob/blob"
	"github.com/Azure/azure-sdk-for-go/sdk/storage/azblob/bloberror"
	"github.com/Azure/azure-sdk-for-go/sdk/storage/azblob/blockblob"
	"oss.nandlabs.io/golly/chrono"
)

// Compile-time check that Elector satisfies chrono.LeaderElector.
var _ chrono.LeaderElector = (*Elector)(nil)

// Options configures an Azure-blob-backed leader elector.
type Options struct {
	// Container is the blob container that holds the lock blob. The
	// container must exist prior to Start; the elector does not create
	// containers.
	Container string
	// Blob is the blob name within the container (for example
	// "locks/scheduler.lock"). All contenders for the same leadership
	// role MUST agree on Container + Blob.
	Blob string
	// Owner uniquely identifies THIS instance. Hostname+pid+random is
	// a reasonable default.
	Owner string
	// Lease is the renewal cadence. Renewal fires every Lease/2; the
	// persisted expiry is now + 3*Lease. Choose Lease larger than
	// expected clock skew and GC pauses.
	Lease time.Duration
}

// lockDoc is the JSON body persisted in the lock blob.
type lockDoc struct {
	Owner      string    `json:"owner"`
	LeaseUntil time.Time `json:"lease_until"`
}

// backend abstracts the blob operations the elector needs so tests can
// swap in an in-memory fake with matching precondition semantics.
type backend interface {
	// read returns the blob body and ETag, or errNotFound.
	read(ctx context.Context) ([]byte, azcore.ETag, error)
	// create writes body only if the blob does not exist (If-None-Match: *).
	create(ctx context.Context, body []byte) (azcore.ETag, error)
	// replace overwrites body only if the current ETag matches (If-Match).
	replace(ctx context.Context, body []byte, etag azcore.ETag) (azcore.ETag, error)
	// remove deletes the blob only if the current ETag matches (If-Match).
	remove(ctx context.Context, etag azcore.ETag) error
}

var (
	// errPreconditionFailed is the canonical sentinel returned by backend
	// methods when an If-Match / If-None-Match condition fails.
	errPreconditionFailed = errors.New("bloblock: precondition failed")
	// errNotFound is returned by backend methods when the blob is absent.
	errNotFound = errors.New("bloblock: lock blob not found")
)

// Elector implements chrono.LeaderElector on top of an Azure block blob
// guarded by ETag preconditions.
type Elector struct {
	be   backend
	opts Options
	now  func() time.Time

	leader atomic.Bool
	// heldETag is the ETag returned by the last successful write. It is
	// used for Resign (If-Match). Only accessed from the renewal
	// goroutine and from Resign after the goroutine has exited.
	heldETag azcore.ETag

	startOnce sync.Once
	started   bool
	cancel    context.CancelFunc
	done      chan struct{}
}

// New constructs a blob-backed Elector using the provided client. The
// elector does not own the client — the caller manages its lifecycle.
func New(client *azblob.Client, opts Options) *Elector {
	bb := client.ServiceClient().NewContainerClient(opts.Container).NewBlockBlobClient(opts.Blob)
	return NewWithBlockBlobClient(bb, opts)
}

// NewWithBlockBlobClient constructs an Elector around an existing block
// blob client (for example one built from a container SAS URL). The
// Container and Blob fields of opts are informational only in this case
// but must still be non-empty.
func NewWithBlockBlobClient(client *blockblob.Client, opts Options) *Elector {
	return newWithBackend(&blobBackend{c: client}, opts)
}

// newWithBackend is the internal constructor used by New and tests.
func newWithBackend(be backend, opts Options) *Elector {
	return &Elector{
		be:   be,
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

// Resign stops the renewal loop and, if this instance owns the lock,
// deletes the blob with an If-Match precondition — that way a successor
// which has already stolen the lease keeps its blob.
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
	if !e.leader.Load() || e.heldETag == "" {
		return nil
	}
	err := e.be.remove(ctx, e.heldETag)
	e.leader.Store(false)
	e.heldETag = ""
	// Someone else already stole/rewrote the lock, or it is already gone:
	// both are fine on resign.
	if errors.Is(err, errPreconditionFailed) || errors.Is(err, errNotFound) {
		return nil
	}
	return err
}

func (e *Elector) validate() error {
	if e.opts.Container == "" {
		return errors.New("bloblock: Container required")
	}
	if e.opts.Blob == "" {
		return errors.New("bloblock: Blob required")
	}
	if e.opts.Owner == "" {
		return errors.New("bloblock: Owner required")
	}
	if e.opts.Lease <= 0 {
		return errors.New("bloblock: Lease must be > 0")
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
//  1. Try Put Blob with If-None-Match:* to claim when the blob is absent.
//  2. On precondition failure, read the current blob and inspect
//     owner + lease_until.
//  3. If we own it (renew) or it's expired (steal), Put Blob with
//     If-Match:<currentETag> — atomically overwriting the version we
//     just read.
//  4. Otherwise mark not-leader.
func (e *Elector) tick(ctx context.Context) error {
	now := e.now()
	body, err := marshalDoc(lockDoc{Owner: e.opts.Owner, LeaseUntil: now.Add(3 * e.opts.Lease)})
	if err != nil {
		return err
	}

	// (1) First-claim path.
	etag, err := e.be.create(ctx, body)
	if err == nil {
		e.heldETag = etag
		e.leader.Store(true)
		return nil
	}
	if !errors.Is(err, errPreconditionFailed) {
		// Transport-level failure: preserve prior atomic value.
		return err
	}

	// (2) Blob exists — read to inspect owner + lease.
	raw, curETag, err := e.be.read(ctx)
	if errors.Is(err, errNotFound) {
		// Rare race: someone deleted between our create attempt and the
		// read. Next tick will retry the create path.
		e.leader.Store(false)
		return nil
	}
	if err != nil {
		return err
	}
	doc := parseDoc(raw)

	// (3) Renew (we already hold it) or steal (expired).
	if doc.Owner == e.opts.Owner || !doc.LeaseUntil.After(now) {
		newETag, werr := e.be.replace(ctx, body, curETag)
		if werr != nil {
			if errors.Is(werr, errPreconditionFailed) || errors.Is(werr, errNotFound) {
				// Another instance beat us to the same write.
				e.leader.Store(false)
				return nil
			}
			return werr
		}
		e.heldETag = newETag
		e.leader.Store(true)
		return nil
	}
	// (4) Someone else holds a live lease.
	e.leader.Store(false)
	return nil
}

// marshalDoc serialises lockDoc to JSON.
func marshalDoc(d lockDoc) ([]byte, error) {
	b, err := json.Marshal(d)
	if err != nil {
		return nil, fmt.Errorf("bloblock: marshal lockDoc: %w", err)
	}
	return b, nil
}

// parseDoc decodes a stored body into a lockDoc. Corrupt bodies are
// treated as an expired lease so the elector can steal them cleanly.
func parseDoc(body []byte) lockDoc {
	var d lockDoc
	if err := json.Unmarshal(body, &d); err != nil {
		return lockDoc{}
	}
	return d
}

// isPrecondition reports whether err is a failed write precondition.
// Azure answers If-None-Match:* on an existing blob with 409
// BlobAlreadyExists and a stale If-Match with 412 ConditionNotMet.
func isPrecondition(err error) bool {
	if err == nil {
		return false
	}
	if bloberror.HasCode(err, bloberror.ConditionNotMet, bloberror.BlobAlreadyExists) {
		return true
	}
	var re *azcore.ResponseError
	return errors.As(err, &re) && re.StatusCode == http.StatusPreconditionFailed
}

// isNotFound reports whether err indicates the lock blob is absent.
func isNotFound(err error) bool {
	if err == nil {
		return false
	}
	if bloberror.HasCode(err, bloberror.BlobNotFound) {
		return true
	}
	var re *azcore.ResponseError
	return errors.As(err, &re) && re.StatusCode == http.StatusNotFound &&
		!bloberror.HasCode(err, bloberror.ContainerNotFound)
}

// normalize maps SDK errors onto the backend sentinels.
func normalize(err error) error {
	switch {
	case err == nil:
		return nil
	case isPrecondition(err):
		return fmt.Errorf("%w: %w", errPreconditionFailed, err)
	case isNotFound(err):
		return fmt.Errorf("%w: %w", errNotFound, err)
	default:
		return err
	}
}

// blobBackend is the production backend that dispatches to the Azure
// block blob client and normalises precondition / not-found errors.
type blobBackend struct{ c *blockblob.Client }

func (b *blobBackend) read(ctx context.Context) ([]byte, azcore.ETag, error) {
	resp, err := b.c.DownloadStream(ctx, nil)
	if err != nil {
		return nil, "", normalize(err)
	}
	defer func() { _ = resp.Body.Close() }()
	body, err := io.ReadAll(resp.Body)
	if err != nil {
		return nil, "", err
	}
	var etag azcore.ETag
	if resp.ETag != nil {
		etag = *resp.ETag
	}
	return body, etag, nil
}

func (b *blobBackend) put(ctx context.Context, body []byte, cond *blob.ModifiedAccessConditions) (azcore.ETag, error) {
	resp, err := b.c.Upload(ctx, streaming.NopCloser(bytes.NewReader(body)), &blockblob.UploadOptions{
		HTTPHeaders:      &blob.HTTPHeaders{BlobContentType: to.Ptr("application/json")},
		AccessConditions: &blob.AccessConditions{ModifiedAccessConditions: cond},
	})
	if err != nil {
		return "", normalize(err)
	}
	var etag azcore.ETag
	if resp.ETag != nil {
		etag = *resp.ETag
	}
	return etag, nil
}

func (b *blobBackend) create(ctx context.Context, body []byte) (azcore.ETag, error) {
	return b.put(ctx, body, &blob.ModifiedAccessConditions{IfNoneMatch: to.Ptr(azcore.ETagAny)})
}

func (b *blobBackend) replace(ctx context.Context, body []byte, etag azcore.ETag) (azcore.ETag, error) {
	return b.put(ctx, body, &blob.ModifiedAccessConditions{IfMatch: to.Ptr(etag)})
}

func (b *blobBackend) remove(ctx context.Context, etag azcore.ETag) error {
	_, err := b.c.Delete(ctx, &blob.DeleteOptions{
		AccessConditions: &blob.AccessConditions{
			ModifiedAccessConditions: &blob.ModifiedAccessConditions{IfMatch: to.Ptr(etag)},
		},
	})
	return normalize(err)
}
