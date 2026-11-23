// Package bloblock provides a chrono.LeaderElector backed by a single
// Azure block blob. Leadership is coordinated by Blob Storage's write
// preconditions: Put Blob with If-None-Match:"*" for the first claim, and
// Put Blob with If-Match:<ETag> for renew, steal, and resign.
//
// Azure Blob Storage is strongly consistent and evaluates conditional
// headers atomically, so exactly one contender wins any contested
// transition — the losers receive HTTP 409 BlobAlreadyExists (create) or
// 412 ConditionNotMet (renew/steal) and observe the winner's state on
// their next tick.
//
// The ETag approach is used (rather than blob leases) for parity with
// golly-aws chrono/s3lock and golly-gcp chrono/gcslock: the persisted
// body is a small JSON document carrying {owner, lease_until}, so the
// lease length is not limited to Azure's 15–60s lease window and a
// crashed leader's lock can be inspected by humans.
//
// The elector runs a background renewal goroutine at Lease/2 cadence.
// IsLeader reads a local atomic — no I/O — so it is safe from hot paths.
//
// Clock skew: lease_until is a wall-clock timestamp, so instances SHOULD
// keep clock skew below Lease/2 for correctness.
package bloblock
