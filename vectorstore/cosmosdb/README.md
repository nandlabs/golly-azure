# vectorstore/cosmosdb

Azure Cosmos DB for NoSQL vector-search backend for the
[`oss.nandlabs.io/golly/vectorstore`](https://pkg.go.dev/oss.nandlabs.io/golly/vectorstore)
`Store` interface, built on
[`azcosmos`](https://pkg.go.dev/github.com/Azure/azure-sdk-for-go/sdk/data/azcosmos).

## Features

- **Upsert** through `UpsertItem` (replace semantics) with bounded concurrency.
- **Delete** through `DeleteItem`. 404s are ignored, so the operation is idempotent.
- **Vector search** with
  `SELECT TOP @k c.id, …, VectorDistance(c.embedding, @vec) AS score FROM c [WHERE …] ORDER BY VectorDistance(c.embedding, @vec)`.
- **Metadata filters** translated to **parameterised** SQL. Values are never
  interpolated into the query.
- **Partition-key aware**: static partition, partition by id, or partition
  by a metadata attribute (see below).
- Credentials from `azcfg` (connection string, account key or Entra ID), or
  a caller-owned `*azcosmos.ContainerClient`.
- `ContainerProperties` helper that builds a matching vector embedding policy
  and vector index (flat / quantizedFlat / diskANN).

## Install

```bash
go get oss.nandlabs.io/golly-azure/vectorstore/cosmosdb
```

## Quick start

```go
import (
    "github.com/Azure/azure-sdk-for-go/sdk/data/azcosmos"
    "oss.nandlabs.io/golly-azure/azcfg"
    "oss.nandlabs.io/golly-azure/vectorstore/cosmosdb"
    "oss.nandlabs.io/golly/vectorstore"
)

cfg := azcfg.NewConfig()                       // DefaultAzureCredential
store, err := cosmosdb.New(cosmosdb.Options{
    Endpoint:          "https://my-acct.documents.azure.com:443/",
    Config:            cfg,
    Database:          "rag",
    Container:         "vectors",
    PartitionKeyField: "tenant",                // container partition key path: /tenant
    PartitionKeyValue: "acme",                  // every doc lives in partition "acme"
    Dimension:         1536,
})
if err != nil { return err }
defer store.Close()                              // closes the client New built

_ = store.Upsert(ctx, vectorstore.Doc{
    ID: "d1", Vector: emb, Content: "raw text",
    Metadata: map[string]any{"source": "wiki"},
})
hits, _ := store.Search(ctx, vectorstore.Query{
    Vector: qEmb, TopK: 5,
    Filter: map[string]any{"source": "wiki"},
})
```

Pass a caller-owned client instead with `Options{Client: containerClient, ...}`.
`Close` then leaves the client alone.

### Bootstrapping the container

```go
props, _ := store.ContainerProperties("vectors")
db, _ := cosmosClient.NewDatabase("rag")
_, err := db.CreateContainer(ctx, props, nil)
```

`ContainerProperties` writes:

- the partition key path `/<PartitionKeyField>`
- a vector embedding policy for `/<VectorField>` (float32, `Dimension`, `DistanceFunction`)
- a vector index (`IndexType`)
- an exclusion of `/<VectorField>/*` from range indexing

Vector policies cannot be changed after creation.

## Item layout

```json
{
  "id": "d1",
  "embedding": [0.1, 0.2],
  "content": "raw text",
  "metadata": {"source": "wiki"},
  "tenant": "acme"
}
```

`embedding`, `content` and `metadata` are configurable with `VectorField`,
`ContentField` and `MetadataField`. The partition property (`tenant` above)
is only written for static and metadata partitioning.

## Partition keys

Cosmos DB needs the partition key for every point write and delete. The
gateway used by the Go SDK cannot run `TOP` / `ORDER BY` queries that span
several physical partitions. Pick the layout that fits:

| Layout | Options | Writes / deletes | Vector search |
|---|---|---|---|
| **Static** | `PartitionKeyField: "pk", PartitionKeyValue: "tenant-a"` | partition `"tenant-a"` | single-partition: always works |
| **By id** (default) | `PartitionKeyField: "id"` | partition = `Doc.ID` | cross-partition: works only while the container has one physical partition |
| **By metadata** | `PartitionKeyField: "category"` | partition = `Doc.Metadata["category"]` (string / number / bool). Delete first looks up each id's partition with a cross-partition point query | single-partition when `Query.Filter["category"]` is a scalar, otherwise cross-partition |

The static layout is the recommended default for RAG collections. A logical
partition holds up to 20 GB. For multi-tenant isolation, use one Store per
tenant value. Hierarchical (multi-path) partition keys are not supported.

## Configuration

| Option | Default | Notes |
|---|---|---|
| `Client` | — | Caller-owned container client. When set, the connection options are ignored |
| `Endpoint` | `Config.Endpoint`, then `https://<Config.AccountName>.documents.azure.com:443/` | |
| `Config` | azcfg `default` config | Auth order: `ConnectionString`, then `AccountKey`, then `Credential()` |
| `ClientOptions` | from `Config.ClientOpts()` | `*azcosmos.ClientOptions` |
| `Database` / `Container` | — | Required without `Client` |
| `VectorField` / `ContentField` / `MetadataField` | `embedding` / `content` / `metadata` | Top-level property names |
| `PartitionKeyField` / `PartitionKeyValue` | `id` / `""` | See above |
| `Dimension` | `0` | When > 0, vector lengths are validated. Required by `ContainerProperties` |
| `DistanceFunction` | `cosine` | `cosine`, `dotproduct` or `euclidean`. Sets the policy and the `MinScore` direction |
| `IndexType` | `quantizedFlat` | `flat`, `quantizedFlat` or `diskANN` |
| `MaxConcurrency` | `4` | In-flight point operations for Upsert / Delete |

## Filter dialect

Keys address properties of the metadata object (`c.metadata.<key>`, or
bracket notation for keywords and special characters). Entries are AND-ed,
and every value is bound as a `@fN` parameter:

| Filter | SQL |
|---|---|
| `"k": v` | `c.metadata.k = @f0` |
| `"k": nil` | `IS_NULL(c.metadata.k)` |
| `"k": []any{a, b}` / `{"$in": [...]}` | `ARRAY_CONTAINS(@f0, c.metadata.k)` |
| `{"$nin": [...]}` | `NOT ARRAY_CONTAINS(@f0, c.metadata.k)` |
| `{"$ne"/"$gt"/"$gte"/"$lt"/"$lte": v}` | `!=` / `>` / `>=` / `<` / `<=` |
| `{"$any": v}` (array-valued fields) | `ARRAY_CONTAINS(c.metadata.k, @f0)` |
| `{"$exists": true}` | `IS_DEFINED(c.metadata.k)` |
| `"_sql": "<predicate>"` | raw predicate AND-ed in (trusted input only) |

`cosmosdb.BuildWhere` is exported if you need the translation on its own.

## Notes

- **Score.** `Hit.Score` is the raw `VectorDistance` value. For cosine and
  dotproduct it is a similarity, so higher is better. For euclidean it is a
  distance, so lower is better. `MinScore` drops hits that are *less similar*
  than the threshold: `score < MinScore`, or `score > MinScore` for euclidean.
  Results always come back most-similar first.
- **Non-atomic batches.** `Upsert` / `Delete` issue one point operation per
  item. The first failure cancels the remaining operations, and items already
  written stay written.
- **Emulator tests.** `cosmosdb_emulator_test.go` runs against a Cosmos DB
  emulator with vector search enabled. Set
  `COSMOS_EMULATOR_ENDPOINT=https://localhost:8081/` and optionally
  `COSMOS_EMULATOR_KEY`. The test is skipped by default.
