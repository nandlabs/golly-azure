# vectorstore/aisearch

Azure AI Search backend for the
[`oss.nandlabs.io/golly/vectorstore`](https://pkg.go.dev/oss.nandlabs.io/golly/vectorstore)
`Store` interface.

Azure AI Search has no stable Go data-plane SDK, so this package calls the
documented [REST API] over an `azcore` pipeline (retries, logging,
telemetry and tracing come from azcore). Authentication is either an
`api-key` or a Microsoft Entra ID token from an `azcfg.Config` credential.

[REST API]: https://learn.microsoft.com/rest/api/searchservice/

## Features

- **Upsert** through `POST /indexes/{index}/docs/index` with `mergeOrUpload`
  actions (or `upload` for strict replace), batched up to 1000 docs per request.
- **Delete** with `delete` actions. Unknown keys are ignored.
- **Vector search** with `vectorQueries` (`kind: vector`, `k = TopK`), and
  optional **hybrid** search (BM25 + vector, RRF-fused) via the `_text` filter key.
- **Metadata filters** translated to OData `$filter` (pre-filter mode).
- **Configurable field names** (key, vector, content, metadata), a nested
  complex-type metadata field or flattened top-level fields, and an optional
  base64url key encoding for ids that are not valid search keys.
- `api-version` option (default `2024-07-01`) and a configurable token scope
  for sovereign clouds.
- `CreateIndex` / `DeleteIndex` bootstrap helpers (HNSW profile with
  cosine, euclidean or dotProduct).
- Per-document failures (HTTP 207) are returned as `*aisearch.BatchError`.

## Install

```bash
go get oss.nandlabs.io/golly-azure/vectorstore/aisearch
```

## Quick start

```go
import (
    "oss.nandlabs.io/golly-azure/azcfg"
    "oss.nandlabs.io/golly-azure/vectorstore/aisearch"
    "oss.nandlabs.io/golly/vectorstore"
)

// Entra ID auth (DefaultAzureCredential, managed identity, SP, ...).
// The identity needs "Search Index Data Contributor" (writes) or
// "Search Index Data Reader" (queries).
store, err := aisearch.New(aisearch.Options{
    Endpoint:  "https://my-search.search.windows.net",
    Index:     "docs",
    Config:    azcfg.NewConfig(),       // nil = azcfg "default" config
    Dimension: 1536,                    // validates vectors; needed by CreateIndex
})
// ...or API-key auth:
// store, err := aisearch.New(aisearch.Options{Endpoint: ep, Index: "docs", APIKey: adminKey})
if err != nil { return err }
defer store.Close()

// Optional: bootstrap an index with an HNSW vector profile.
_ = store.CreateIndex(ctx,
    aisearch.MetadataField{Name: "source"},
    aisearch.MetadataField{Name: "year", Type: aisearch.EdmInt32},
)

_ = store.Upsert(ctx, vectorstore.Doc{
    ID: "d1", Vector: emb, Content: "raw text",
    Metadata: map[string]any{"source": "wiki", "year": 2024},
})

hits, _ := store.Search(ctx, vectorstore.Query{
    Vector: qEmb,
    TopK:   10,
    Filter: map[string]any{
        aisearch.QueryTextKey: "what is fnord?",        // optional: hybrid search
        "source":              "wiki",
        "year":                map[string]any{"$gte": 2020},
    },
})
```

## Configuration

| Option | Default | Notes |
|---|---|---|
| `Endpoint` | `Config.Endpoint`, then `https://<Config.AccountName>.search.windows.net` | Search service URL |
| `Index` | — (required) | Target index |
| `APIKey` | `Config.AccountKey` | Sent as `api-key`; when empty, Entra ID auth is used |
| `Config` | azcfg `default` config | Credential + client options |
| `Scope` | `https://search.azure.com/.default` | e.g. `https://search.azure.us/.default` for Azure Government |
| `APIVersion` | `2024-07-01` | Any GA/preview version that supports `vectorQueries` |
| `ClientOptions` | `Config.ClientOpts()` | Transport, retry, logging |
| `IDField` / `VectorField` / `ContentField` / `MetadataField` | `id` / `embedding` / `content` / `metadata` | Must match the index schema |
| `FlattenMetadata` | `false` | Metadata keys become top-level fields |
| `EncodeKeys` | `false` | base64url-encode ids (keys only allow `[A-Za-z0-9_\-=]`) |
| `Action` | `mergeOrUpload` | `upload` for whole-document replace |
| `BatchSize` | `1000` | Max actions per indexing request |
| `Dimension` | `0` | When > 0, vector lengths are validated |
| `Metric` | `cosine` | HNSW metric for `CreateIndex` |
| `Exhaustive` | `false` | Brute-force KNN instead of HNSW |

## Filter dialect

`Query.Filter` keys address `metadata/<key>` (or `<key>` with
`FlattenMetadata`); `a/b` paths reach nested complex sub-fields. Entries
are AND-ed:

| Filter | OData |
|---|---|
| `"k": "v"` | `metadata/k eq 'v'` |
| `"k": nil` | `metadata/k eq null` |
| `"k": []any{"a","b"}` | `(metadata/k eq 'a' or metadata/k eq 'b')` |
| `"k": {"$ne"/"$gt"/"$gte"/"$lt"/"$lte": v}` | `ne` / `gt` / `ge` / `lt` / `le` |
| `"k": {"$in": [...]}` / `{"$nin": [...]}` | `(… or …)` / `not (… or …)` |
| `"k": {"$any": "x"}` | `metadata/k/any(t: t eq 'x')` (collection fields) |
| `"k": {"$exists": true}` | `metadata/k ne null` |
| `"_odata": "<expr>"` | raw expression AND-ed in |
| `"_text": "<query>"` | hybrid search text (not a filter) |

String literals are escaped (`'` becomes `''`). Field paths are validated
against the OData identifier grammar. `time.Time` values render as
`DateTimeOffset` literals. `aisearch.BuildFilter` is exported if you need
the translation on its own.

## Notes

- **Fixed schema.** Azure AI Search rejects fields missing from the index.
  Declare every metadata key (`CreateIndex(ctx, MetadataField{...}...)` or
  your own schema). Filters need `filterable` fields.
- **mergeOrUpload semantics.** Fields left out of a `Doc` (empty `Content`,
  nil `Metadata`) keep their stored values. Use `Action: aisearch.ActionUpload`
  for strict replace.
- **Score.** `Hit.Score` is `@search.score`. Pure vector queries return a
  similarity in which higher is better. Hybrid queries return RRF-fused
  scores, which are much smaller (for example 0.03), so set `MinScore` to match.
- **Hit.Content / Metadata** come from the content and metadata fields. The
  vector itself is never returned.
- **Close.** No connections are owned. `Close` only marks the Store closed.
