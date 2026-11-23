# tablelock

A `chrono.LeaderElector` backed by a single **Azure Table Storage** entity
(also works with Azure Cosmos DB for Table and the Azurite emulator).
Ownership is coordinated with ETag optimistic concurrency:

| Transition | Operation                                                      |
| ---------- | -------------------------------------------------------------- |
| Acquire    | `AddEntity` (insert-if-absent; 409 if someone holds it)        |
| Renew      | `UpdateEntity` Replace with `If-Match: <held ETag>`            |
| Steal      | `GetEntity`, then `UpdateEntity` with `If-Match: <seen ETag>` once the lease has expired |
| Release    | `DeleteEntity` with `If-Match: <held ETag>`                    |

Table Storage evaluates `If-Match` atomically per entity, so exactly one
contender wins any contested transition; a 412 means you lost.

## Quick start

```go
import (
    "github.com/Azure/azure-sdk-for-go/sdk/data/aztables"
    "oss.nandlabs.io/golly-azure/chrono/tablelock"
    "oss.nandlabs.io/golly/chrono"
)

svc, _ := aztables.NewServiceClient("https://myaccount.table.core.windows.net", cred, nil)
elector := tablelock.New(svc.NewClient("chronolocks"), tablelock.Options{
    Key:   "scheduler",
    Owner: hostnamePidRandom(),
    Lease: 15 * time.Second,
})
_ = elector.Start(ctx)
defer elector.Resign(context.Background())

sched := chrono.NewScheduler(chrono.WithLeaderElector(elector))
```

Or build the table client from an `azcfg.Config` (connection string,
account name + key, or Entra ID credential, in that order):

```go
cfg := azcfg.NewConfig()
cfg.SetAccountName("myaccount") // Entra ID via DefaultAzureCredential
elector, err := tablelock.NewFromConfig(cfg, "chronolocks", tablelock.Options{
    Key: "scheduler", Owner: owner, Lease: 15 * time.Second,
})
```

`IsLeader` reads a local atomic — safe to call in hot paths.

## Table schema

The table must exist (the elector does not create it). Entities:

| Property       | Type               | Notes                                         |
| -------------- | ------------------ | --------------------------------------------- |
| `PartitionKey` | String             | `Options.PartitionKey` (default `chrono-lock`) |
| `RowKey`       | String             | `Options.Key`                                 |
| `Owner`        | String             | Current holder's identity                     |
| `LeaseUntil`   | Int64 (unix ms)    | Absolute lease deadline (now + 3×Lease)       |
| `Version`      | Int64              | Monotonic write counter; diagnostics only     |

With Entra ID auth the identity needs the **Storage Table Data
Contributor** role.

## Timing

Renewal runs every `Lease/2`; the persisted deadline is `now + 3*Lease`.
Transport errors never flip local leadership — the persisted lease is the
source of truth for other contenders.

**Clock skew:** the lease is an absolute wall-clock deadline, so instance
clocks SHOULD stay within `Lease/2` of one another (NTP/chrony with the
default sync interval is sufficient).

## Testing

Offline unit tests use an in-memory fake with Table Storage ETag
semantics. Azurite tests are skipped unless
`AZURITE_TABLES_CONNECTION_STRING` is set:

```sh
docker run -p 10002:10002 mcr.microsoft.com/azure-storage/azurite azurite-table --tableHost 0.0.0.0
export AZURITE_TABLES_CONNECTION_STRING="DefaultEndpointsProtocol=http;AccountName=devstoreaccount1;AccountKey=Eby8vdM02xNOcqFlqUwJPLlmEtlCDXJ1OUzFT50uSRZ6IFsuFq2UVErCz4I6tq/K1SZFPTOtr/KBHBeksoGMGw==;TableEndpoint=http://127.0.0.1:10002/devstoreaccount1;"
GOWORK=off go test -race ./chrono/tablelock/...
```
