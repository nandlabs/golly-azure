// Package tablelock provides a chrono.LeaderElector backed by a single
// Azure Table Storage entity (also works against the Azure Cosmos DB for
// Table API and the Azurite emulator).
//
// Ownership is coordinated with optimistic concurrency on the entity's
// ETag:
//
//   - Acquire: AddEntity (insert-if-absent). Exactly one contender's
//     insert succeeds; everybody else gets 409 Conflict.
//   - Renew: UpdateEntity (Replace) with If-Match set to the ETag this
//     instance last wrote. A 412 Precondition Failed means someone else
//     replaced the entity — leadership is lost.
//   - Steal: when the stored lease has expired (or the entity already
//     names this owner, e.g. after a process restart), UpdateEntity with
//     If-Match set to the ETag just observed via GetEntity. Concurrent
//     stealers race on the same ETag and exactly one wins.
//   - Release: DeleteEntity with If-Match set to the held ETag, so a
//     successor that already took over is never clobbered.
//
// The elector runs a background goroutine that ticks every Lease/2; the
// persisted expiry is now + 3*Lease. The public IsLeader call performs no
// I/O — it reads a local atomic updated by the renewal loop, so it is safe
// to invoke from hot paths (e.g. per-tick from the scheduler).
//
// Clock skew: the lease is an absolute wall-clock deadline (LeaseUntil),
// so instances SHOULD keep clock skew below Lease/2 for correctness.
// Chrony/NTP with a modest sync interval is sufficient.
package tablelock
