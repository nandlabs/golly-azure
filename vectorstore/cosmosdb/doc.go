// Package cosmosdb is an Azure Cosmos DB for NoSQL vector-search backend for
// oss.nandlabs.io/golly/vectorstore, built on
// github.com/Azure/azure-sdk-for-go/sdk/data/azcosmos.
//
// Each vectorstore.Doc is stored as one JSON item:
//
//	{
//	  "id":        "<Doc.ID>",
//	  "embedding": [0.1, 0.2, ...],      // Options.VectorField
//	  "content":   "raw text",           // Options.ContentField (omitted when empty)
//	  "metadata":  {"source": "wiki"},   // Options.MetadataField (omitted when empty)
//	  "pk":        "<partition value>"   // static or metadata partitioning only
//	}
//
// Operations:
//
//   - Upsert — ContainerClient.UpsertItem per doc (replace semantics).
//
//   - Delete — ContainerClient.DeleteItem per id; 404s are ignored.
//
//   - Search — a parameterised query:
//
//     SELECT TOP @k c.id, c.content, c.metadata, VectorDistance(c.embedding, @vec) AS score
//     FROM c [WHERE <filter>] ORDER BY VectorDistance(c.embedding, @vec)
//
// Query.Filter is translated into a parameterised WHERE clause over the
// metadata object (see [BuildWhere]); values are always bound as
// @parameters, never interpolated.
//
// # Partition keys
//
// Cosmos DB requires a partition key on every point write/delete, and the
// gateway endpoint used by the Go SDK cannot execute ORDER BY / TOP across
// several physical partitions. The Store therefore supports three layouts:
//
//  1. Static partition (Options.PartitionKeyValue set): every item gets
//     "<PartitionKeyField>": PartitionKeyValue and all reads, writes and
//     vector queries target that one logical partition. Simplest; best for
//     collections up to the 20 GB logical-partition limit, and for
//     multi-tenant layouts (one Store per tenant value).
//  2. Partitioned by id (PartitionKeyField "id", the default): writes and
//     deletes use Doc.ID as the key; vector queries run cross-partition and
//     work only while the container has a single physical partition (or
//     the query is otherwise servable by the gateway).
//  3. Partitioned by a metadata attribute (PartitionKeyField = "<key>"):
//     the partition value is read from Doc.Metadata[<key>] (string, number
//     or bool) and written to the top-level property; Search scopes the
//     query to one partition when Query.Filter contains an equality on
//     <key>, and Delete first looks up each id's partition value with a
//     simple cross-partition point query.
//
// Hierarchical (multi-path) partition keys are not supported.
//
// [Store.ContainerProperties] returns container properties (partition key,
// vector embedding policy, vector index, indexing exclusions) matching the
// Store's layout, for callers who want to bootstrap the container.
//
// Target scale profile: operational / transactional workloads that keep
// vectors next to the source documents, 10^4 – 10^7 vectors per container,
// flat / quantizedFlat / DiskANN vector indexes and SQL-side metadata
// filtering.
//
// The Store is safe for concurrent use.
package cosmosdb
