# bloblock

A `chrono.LeaderElector` backed by a single Azure block blob. Ownership
is coordinated by Blob Storage's write preconditions: Put Blob with
`If-None-Match: *` for the first claim, and Put Blob / Delete Blob with
`If-Match: <ETag>` for renewals, steals, and resigns.

## Quick start

```go
import (
    "github.com/Azure/azure-sdk-for-go/sdk/azidentity"
    "github.com/Azure/azure-sdk-for-go/sdk/storage/azblob"
    "oss.nandlabs.io/golly-azure/chrono/bloblock"
    "oss.nandlabs.io/golly/chrono"
)

cred, _ := azidentity.NewDefaultAzureCredential(nil)
client, _ := azblob.NewClient("https://myaccount.blob.core.windows.net/", cred, nil)
elector := bloblock.New(client, bloblock.Options{
    Container: "locks",
    Blob:      "chrono/scheduler.lock",
    Owner:     hostnamePidRandom(),
    Lease:     15 * time.Second,
})
_ = elector.Start(ctx)
defer elector.Resign(context.Background())

sched := chrono.New(chrono.WithLeaderElector(elector))
```

`IsLeader` reads a local atomic — safe to call in hot paths.

Any `azblob.Client` works — token credential, shared key
(`azblob.NewClientWithSharedKeyCredential`), connection string
(`azblob.NewClientFromConnectionString`) or Azurite. With a container SAS
URL, build a `blockblob.Client` and use `bloblock.NewWithBlockBlobClient`.
To reuse golly-azure configuration, build the client from an
`azcfg.Config` (`cfg.Credential()`, `cfg.ClientOpts()`).

## How it works

Each tick (every `Lease/2`):

1. `Put Blob` with `If-None-Match: *` — succeeds only when no lock exists.
2. Otherwise read the blob (`{"owner": ..., "lease_until": ...}`) and its ETag.
3. If we own it (renew) or `lease_until` has passed (steal), `Put Blob`
   with `If-Match: <ETag>`; a concurrent writer makes this fail with
   412, so exactly one instance wins.
4. Otherwise someone else holds a live lease — not leader.

`Resign` stops the loop and deletes the blob with `If-Match` on the ETag
we last wrote, so it never removes a successor's lock. The persisted
expiry is `now + 3*Lease`.

ETag preconditions are used instead of Azure blob leases for parity with
`golly-aws/chrono/s3lock` and `golly-gcp/chrono/gcslock`: lease length is
not bound to Azure's 15–60 s lease window, and the lock body is human
readable.

## Requirements

- **Container must exist.** The elector does not create it.
- **Permissions**: `Storage Blob Data Contributor` (or a SAS with read,
  write and delete) on the container.
- **Clock skew**: the lease is an absolute wall-clock deadline, so
  instance clocks SHOULD stay within `Lease/2` of one another
  (NTP/chrony with the default sync interval is sufficient).

## Testing

Unit tests run offline (an in-memory backend plus the
[`blob/blobtest`](../../blob/blobtest/) fake Blob service). The Azurite
smoke tests run when `AZURITE_BLOB_ENDPOINT` is set:

```bash
docker run -p 10000:10000 mcr.microsoft.com/azure-storage/azurite azurite-blob --blobHost 0.0.0.0
AZURITE_BLOB_ENDPOINT=http://127.0.0.1:10000/devstoreaccount1 go test ./chrono/bloblock/...
```
