// Package aisearch is an Azure AI Search (formerly Azure Cognitive Search)
// backend for oss.nandlabs.io/golly/vectorstore.
//
// Azure AI Search has no stable Go data-plane SDK, so the Store talks to the
// documented REST API directly over an azcore pipeline (retries, logging,
// telemetry and tracing come from azcore):
//
//   - Upsert — POST /indexes/{index}/docs/index with "mergeOrUpload" actions
//     (configurable: "upload" gives strict replace semantics).
//   - Delete — the same batch endpoint with "delete" actions.
//   - Search — POST /indexes/{index}/docs/search with a vectorQueries entry
//     (kind=vector, k=TopK). A natural-language string supplied under the
//     reserved filter key [QueryTextKey] ("_text") turns the request into a
//     hybrid (BM25 + vector, RRF-fused) query.
//
// Authentication is either an admin/query API key (the "api-key" header) or
// a Microsoft Entra ID token for scope https://search.azure.com/.default
// obtained from an azcfg.Config credential (DefaultAzureCredential, managed
// identity, service principal, ...).
//
// Query.Filter is translated to an OData $filter expression — see
// [BuildFilter] and the README for the supported dialect (equality, $in,
// comparison operators, collection any(), raw OData passthrough).
//
// Field names (key, vector, content, metadata) are configurable to match an
// existing index; [Store.CreateIndex] bootstraps a matching schema with an
// HNSW vector profile for callers who do not manage the index out of band.
//
// Target scale profile: managed search services with 10^5 – 10^8 vectors
// per index, HNSW or exhaustive KNN, hybrid keyword + vector retrieval and
// rich OData filtering.
//
// The Store is safe for concurrent use.
package aisearch
