package cosmosdb

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"strings"
	"sync"
	"sync/atomic"

	"github.com/Azure/azure-sdk-for-go/sdk/azcore"
	"github.com/Azure/azure-sdk-for-go/sdk/azcore/runtime"
	"github.com/Azure/azure-sdk-for-go/sdk/data/azcosmos"

	"oss.nandlabs.io/golly-azure/azcfg"
	"oss.nandlabs.io/golly/vectorstore"
)

// Default property names.
const (
	DefaultVectorField       = "embedding"
	DefaultContentField      = "content"
	DefaultMetadataField     = "metadata"
	DefaultPartitionKeyField = "id"

	defaultTopK        = 10
	defaultConcurrency = 4
	lookupChunk        = 256
)

// ErrMissingPartitionKey is returned by Upsert when the Store partitions by
// a metadata attribute and a Doc does not carry a usable value for it.
var ErrMissingPartitionKey = errors.New("vectorstore/cosmosdb: document has no usable partition key value")

// containerAPI is the minimal surface of *azcosmos.ContainerClient the Store
// exercises. Extracted as an interface so tests can inject fakes.
type containerAPI interface {
	UpsertItem(ctx context.Context, pk azcosmos.PartitionKey, item []byte, o *azcosmos.ItemOptions) (azcosmos.ItemResponse, error)
	DeleteItem(ctx context.Context, pk azcosmos.PartitionKey, itemID string, o *azcosmos.ItemOptions) (azcosmos.ItemResponse, error)
	NewQueryItemsPager(query string, pk azcosmos.PartitionKey, o *azcosmos.QueryOptions) *runtime.Pager[azcosmos.QueryItemsResponse]
}

// Options configures a Store.
type Options struct {
	// Client is a caller-owned container client. When set, Endpoint,
	// Config, Database and Container are ignored and Store.Close leaves the
	// client alone.
	Client *azcosmos.ContainerClient

	// Endpoint is the account endpoint ("https://<account>.documents.azure.com:443/").
	// Falls back to Config.Endpoint, then to the URL derived from
	// Config.AccountName. Ignored when Config.ConnectionString is set.
	Endpoint string
	// Config supplies credentials: ConnectionString, else AccountKey (shared
	// key), else the Entra ID credential from cfg.Credential(). Nil resolves
	// the azcfg config registered as azcfg.DefaultConfigName.
	Config *azcfg.Config
	// ClientOptions overrides the azcosmos client options built from
	// Config.ClientOpts().
	ClientOptions *azcosmos.ClientOptions
	// Database and Container name the target container (required when
	// Client is nil).
	Database  string
	Container string

	// VectorField is the item property holding the embedding (default "embedding").
	// It must match a path in the container's vector embedding policy.
	VectorField string
	// ContentField holds Doc.Content (default "content").
	ContentField string
	// MetadataField holds Doc.Metadata as a nested object (default "metadata").
	MetadataField string

	// PartitionKeyField is the top-level property the container is
	// partitioned on, i.e. its partition key path without the leading '/'
	// (default "id"). See the package documentation for the three layouts.
	PartitionKeyField string
	// PartitionKeyValue pins every item to one logical partition: the value
	// is written to PartitionKeyField and used for all operations. Requires
	// PartitionKeyField != "id".
	PartitionKeyValue string

	// Dimension is the embedding dimension. When > 0, Upsert and Search
	// validate vector lengths (vectorstore.ErrDimMismatch);
	// ContainerProperties requires it.
	Dimension int
	// DistanceFunction is the vector distance function (default cosine).
	// It is written by ContainerProperties and decides how MinScore is
	// interpreted (see Search). It does not override the container policy
	// at query time.
	DistanceFunction azcosmos.VectorDistanceFunction
	// IndexType is the vector index written by ContainerProperties
	// (default quantizedFlat).
	IndexType azcosmos.VectorIndexType
	// MaxConcurrency bounds the number of in-flight point operations during
	// Upsert / Delete (default 4).
	MaxConcurrency int
}

type pkMode int

const (
	pkByID pkMode = iota
	pkStatic
	pkByMetadata
)

// Store implements vectorstore.Store on top of an Azure Cosmos DB for NoSQL
// container with a vector embedding policy.
type Store struct {
	container containerAPI
	owned     *azcosmos.Client

	vectorField   string
	contentField  string
	metadataField string
	pkField       string
	pkValue       string
	mode          pkMode

	dimension   int
	distance    azcosmos.VectorDistanceFunction
	indexType   azcosmos.VectorIndexType
	concurrency int

	closed  atomic.Bool
	closeMu sync.Mutex
}

// New validates options and returns a ready Store. When Options.Client is
// nil it builds an azcosmos client from Config / Endpoint (no network calls
// are made until the first operation).
func New(opts Options) (*Store, error) {
	if opts.Client != nil {
		return newWithContainer(opts, opts.Client, nil)
	}
	if opts.Database == "" || opts.Container == "" {
		return nil, errors.New("vectorstore/cosmosdb: Options.Database and Options.Container are required when Options.Client is nil")
	}
	client, err := newClient(opts)
	if err != nil {
		return nil, err
	}
	cc, err := client.NewContainer(opts.Database, opts.Container)
	if err != nil {
		client.Close()
		return nil, fmt.Errorf("vectorstore/cosmosdb: new container client: %w", err)
	}
	s, err := newWithContainer(opts, cc, client)
	if err != nil {
		client.Close()
		return nil, err
	}
	return s, nil
}

// newClient builds an azcosmos.Client from the azcfg configuration.
func newClient(opts Options) (*azcosmos.Client, error) {
	cfg := opts.Config
	if cfg == nil {
		cfg = azcfg.GetConfigOrDefault(nil, azcfg.DefaultConfigName)
	}
	co := opts.ClientOptions
	if co == nil {
		co = &azcosmos.ClientOptions{ClientOptions: cfg.ClientOpts()}
	}
	if cfg.ConnectionString != "" {
		c, err := azcosmos.NewClientFromConnectionString(cfg.ConnectionString, co)
		if err != nil {
			return nil, fmt.Errorf("vectorstore/cosmosdb: client from connection string: %w", err)
		}
		return c, nil
	}
	endpoint := opts.Endpoint
	if endpoint == "" {
		endpoint = cfg.Endpoint
	}
	if endpoint == "" && cfg.AccountName != "" {
		endpoint = "https://" + cfg.AccountName + ".documents.azure.com:443/"
	}
	if endpoint == "" {
		return nil, errors.New("vectorstore/cosmosdb: Options.Endpoint (or Config.Endpoint / Config.AccountName / Config.ConnectionString) is required")
	}
	if cfg.AccountKey != "" {
		key, err := azcosmos.NewKeyCredential(cfg.AccountKey)
		if err != nil {
			return nil, fmt.Errorf("vectorstore/cosmosdb: account key: %w", err)
		}
		c, err := azcosmos.NewClientWithKey(endpoint, key, co)
		if err != nil {
			return nil, fmt.Errorf("vectorstore/cosmosdb: client with key: %w", err)
		}
		return c, nil
	}
	cred, err := cfg.Credential()
	if err != nil {
		return nil, fmt.Errorf("vectorstore/cosmosdb: resolve credential: %w", err)
	}
	c, err := azcosmos.NewClient(endpoint, cred, co)
	if err != nil {
		return nil, fmt.Errorf("vectorstore/cosmosdb: client: %w", err)
	}
	return c, nil
}

// newWithContainer is the internal constructor used by New and by tests.
func newWithContainer(opts Options, c containerAPI, owned *azcosmos.Client) (*Store, error) {
	s := &Store{
		container:     c,
		owned:         owned,
		vectorField:   orDefault(opts.VectorField, DefaultVectorField),
		contentField:  orDefault(opts.ContentField, DefaultContentField),
		metadataField: orDefault(opts.MetadataField, DefaultMetadataField),
		pkField:       orDefault(opts.PartitionKeyField, DefaultPartitionKeyField),
		pkValue:       opts.PartitionKeyValue,
		dimension:     opts.Dimension,
		distance:      opts.DistanceFunction,
		indexType:     opts.IndexType,
		concurrency:   opts.MaxConcurrency,
	}
	if s.distance == "" {
		s.distance = azcosmos.VectorDistanceFunctionCosine
	}
	switch s.distance {
	case azcosmos.VectorDistanceFunctionCosine, azcosmos.VectorDistanceFunctionDotProduct, azcosmos.VectorDistanceFunctionEuclidean:
	default:
		return nil, fmt.Errorf("vectorstore/cosmosdb: unknown DistanceFunction %q", s.distance)
	}
	if s.indexType == "" {
		s.indexType = azcosmos.VectorIndexTypeQuantizedFlat
	}
	if s.concurrency <= 0 {
		s.concurrency = defaultConcurrency
	}
	switch {
	case s.pkValue != "":
		if s.pkField == "id" {
			return nil, errors.New("vectorstore/cosmosdb: Options.PartitionKeyValue requires a PartitionKeyField other than \"id\"")
		}
		s.mode = pkStatic
	case s.pkField == "id":
		s.mode = pkByID
	default:
		s.mode = pkByMetadata
	}
	seen := map[string]string{}
	for _, f := range [][2]string{
		{"VectorField", s.vectorField},
		{"ContentField", s.contentField},
		{"MetadataField", s.metadataField},
		{"PartitionKeyField", s.pkField},
	} {
		if strings.Contains(f[1], "/") {
			return nil, fmt.Errorf("vectorstore/cosmosdb: Options.%s %q must be a top-level property name", f[0], f[1])
		}
		if f[0] != "PartitionKeyField" && (f[1] == "id" || strings.HasPrefix(f[1], "_")) {
			return nil, fmt.Errorf("vectorstore/cosmosdb: Options.%s %q collides with a system property", f[0], f[1])
		}
		if other, dup := seen[f[1]]; dup {
			return nil, fmt.Errorf("vectorstore/cosmosdb: Options.%s and Options.%s both use property %q", f[0], other, f[1])
		}
		seen[f[1]] = f[0]
	}
	return s, nil
}

// Upsert inserts or replaces docs by ID via UpsertItem. Docs are validated
// up front; writes then run with bounded concurrency and the first failure
// cancels the remaining writes (already-written docs are not rolled back).
func (s *Store) Upsert(ctx context.Context, docs ...vectorstore.Doc) error {
	if s.closed.Load() {
		return errClosed
	}
	if len(docs) == 0 {
		return nil
	}
	type prepared struct {
		id   string
		pk   azcosmos.PartitionKey
		body []byte
	}
	items := make([]prepared, 0, len(docs))
	for _, d := range docs {
		if err := vectorstore.ValidateDoc(d); err != nil {
			return err
		}
		if s.dimension > 0 && len(d.Vector) != s.dimension {
			return fmt.Errorf("%w: container dim=%d, doc=%q dim=%d", vectorstore.ErrDimMismatch, s.dimension, d.ID, len(d.Vector))
		}
		pk, body, err := s.buildItem(d)
		if err != nil {
			return err
		}
		items = append(items, prepared{id: d.ID, pk: pk, body: body})
	}
	return s.forEach(ctx, len(items), func(ctx context.Context, i int) error {
		if _, err := s.container.UpsertItem(ctx, items[i].pk, items[i].body, nil); err != nil {
			return fmt.Errorf("vectorstore/cosmosdb: upsert %q: %w", items[i].id, err)
		}
		return nil
	})
}

// buildItem renders the stored JSON item and partition key for d.
func (s *Store) buildItem(d vectorstore.Doc) (azcosmos.PartitionKey, []byte, error) {
	item := map[string]any{
		"id":          d.ID,
		s.vectorField: []float32(d.Vector),
	}
	if d.Content != "" {
		item[s.contentField] = d.Content
	}
	if len(d.Metadata) > 0 {
		item[s.metadataField] = d.Metadata
	}
	var pk azcosmos.PartitionKey
	switch s.mode {
	case pkStatic:
		item[s.pkField] = s.pkValue
		pk = azcosmos.NewPartitionKeyString(s.pkValue)
	case pkByID:
		pk = azcosmos.NewPartitionKeyString(d.ID)
	case pkByMetadata:
		v, ok := d.Metadata[s.pkField]
		if !ok {
			return pk, nil, fmt.Errorf("%w: doc %q lacks metadata %q", ErrMissingPartitionKey, d.ID, s.pkField)
		}
		var err error
		if pk, err = partitionKeyOf(v); err != nil {
			return pk, nil, fmt.Errorf("%w: doc %q metadata %q: %v", ErrMissingPartitionKey, d.ID, s.pkField, err)
		}
		item[s.pkField] = v
	}
	body, err := json.Marshal(item)
	if err != nil {
		return pk, nil, fmt.Errorf("vectorstore/cosmosdb: marshal doc %q: %w", d.ID, err)
	}
	return pk, body, nil
}

// Search runs a VectorDistance query and returns up to TopK hits, most
// similar first (Cosmos DB orders ORDER BY VectorDistance by similarity for
// every distance function).
//
// Hit.Score is the raw VectorDistance value: a similarity for cosine /
// dotproduct (higher is better) and a distance for euclidean (lower is
// better). MinScore follows the same orientation: hits less similar than
// the threshold are dropped, i.e. score < MinScore for cosine / dotproduct
// and score > MinScore for euclidean.
//
// The query is scoped to a single partition when the Store uses a static
// partition key, or when Query.Filter contains a scalar equality on the
// metadata partition key attribute; otherwise it runs cross-partition (see
// the package documentation for gateway limitations).
func (s *Store) Search(ctx context.Context, q vectorstore.Query) ([]vectorstore.Hit, error) {
	if s.closed.Load() {
		return nil, errClosed
	}
	if len(q.Vector) == 0 {
		return nil, vectorstore.ErrEmptyVector
	}
	if s.dimension > 0 && len(q.Vector) != s.dimension {
		return nil, fmt.Errorf("%w: query dim=%d, container dim=%d", vectorstore.ErrDimMismatch, len(q.Vector), s.dimension)
	}
	topK := q.TopK
	if topK <= 0 {
		topK = defaultTopK
	}
	query, params, err := s.buildSearchQuery(q.Filter)
	if err != nil {
		return nil, err
	}
	params = append(params,
		azcosmos.QueryParameter{Name: "@k", Value: topK},
		azcosmos.QueryParameter{Name: "@vec", Value: []float32(q.Vector)},
	)

	pager := s.container.NewQueryItemsPager(query, s.searchPartition(q.Filter), &azcosmos.QueryOptions{QueryParameters: params})
	hits := make([]vectorstore.Hit, 0, topK)
	for pager.More() && len(hits) < topK {
		page, err := pager.NextPage(ctx)
		if err != nil {
			return nil, fmt.Errorf("vectorstore/cosmosdb: search: %w", err)
		}
		for _, raw := range page.Items {
			var row struct {
				ID       string         `json:"id"`
				Content  string         `json:"content"`
				Metadata map[string]any `json:"metadata"`
				Score    float64        `json:"score"`
			}
			if err := json.Unmarshal(raw, &row); err != nil {
				return nil, fmt.Errorf("vectorstore/cosmosdb: decode search row: %w", err)
			}
			score := float32(row.Score)
			if q.MinScore != 0 && !s.meetsMinScore(score, q.MinScore) {
				continue
			}
			hits = append(hits, vectorstore.Hit{ID: row.ID, Score: score, Content: row.Content, Metadata: row.Metadata})
			if len(hits) == topK {
				break
			}
		}
	}
	return hits, nil
}

// buildSearchQuery renders the vector query text and filter parameters.
func (s *Store) buildSearchQuery(filter map[string]any) (string, []azcosmos.QueryParameter, error) {
	where, params, err := BuildWhere(prop("c", s.metadataField), filter)
	if err != nil {
		return "", nil, err
	}
	dist := "VectorDistance(" + prop("c", s.vectorField) + ", @vec)"
	var b strings.Builder
	b.WriteString("SELECT TOP @k c.id, ")
	b.WriteString(prop("c", s.contentField))
	b.WriteString(" AS content, ")
	b.WriteString(prop("c", s.metadataField))
	b.WriteString(" AS metadata, ")
	b.WriteString(dist)
	b.WriteString(" AS score FROM c")
	if where != "" {
		b.WriteString(" WHERE ")
		b.WriteString(where)
	}
	b.WriteString(" ORDER BY ")
	b.WriteString(dist)
	return b.String(), params, nil
}

// searchPartition picks the partition a vector query runs against.
func (s *Store) searchPartition(filter map[string]any) azcosmos.PartitionKey {
	switch s.mode {
	case pkStatic:
		return azcosmos.NewPartitionKeyString(s.pkValue)
	case pkByMetadata:
		if v, ok := filter[s.pkField]; ok {
			if pk, err := partitionKeyOf(v); err == nil {
				return pk
			}
		}
	}
	return azcosmos.NewPartitionKey()
}

func (s *Store) meetsMinScore(score, min float32) bool {
	if s.distance == azcosmos.VectorDistanceFunctionEuclidean {
		return score <= min
	}
	return score >= min
}

// Delete removes items by id. Unknown ids (HTTP 404) are silently skipped.
// When the Store partitions by a metadata attribute, the partition value of
// each id is first resolved with a cross-partition lookup query; ids that
// are not found are skipped.
func (s *Store) Delete(ctx context.Context, ids ...string) error {
	if s.closed.Load() {
		return errClosed
	}
	if len(ids) == 0 {
		return nil
	}
	for _, id := range ids {
		if id == "" {
			return vectorstore.ErrEmptyID
		}
	}
	var targets []idPartition
	switch s.mode {
	case pkStatic:
		pk := azcosmos.NewPartitionKeyString(s.pkValue)
		for _, id := range ids {
			targets = append(targets, idPartition{id, pk})
		}
	case pkByID:
		for _, id := range ids {
			targets = append(targets, idPartition{id, azcosmos.NewPartitionKeyString(id)})
		}
	case pkByMetadata:
		found, err := s.lookupPartitions(ctx, ids)
		if err != nil {
			return err
		}
		targets = found
	}
	return s.forEach(ctx, len(targets), func(ctx context.Context, i int) error {
		_, err := s.container.DeleteItem(ctx, targets[i].pk, targets[i].id, nil)
		if err != nil && !isNotFound(err) {
			return fmt.Errorf("vectorstore/cosmosdb: delete %q: %w", targets[i].id, err)
		}
		return nil
	})
}

type idPartition struct {
	id string
	pk azcosmos.PartitionKey
}

// lookupPartitions resolves the partition key values of ids with a simple
// cross-partition filter query (servable by the gateway).
func (s *Store) lookupPartitions(ctx context.Context, ids []string) ([]idPartition, error) {
	query := "SELECT c.id, " + prop("c", s.pkField) + " AS pk FROM c WHERE ARRAY_CONTAINS(@ids, c.id)"
	var out []idPartition
	for start := 0; start < len(ids); start += lookupChunk {
		end := min(start+lookupChunk, len(ids))
		pager := s.container.NewQueryItemsPager(query, azcosmos.NewPartitionKey(), &azcosmos.QueryOptions{
			QueryParameters: []azcosmos.QueryParameter{{Name: "@ids", Value: ids[start:end]}},
		})
		for pager.More() {
			page, err := pager.NextPage(ctx)
			if err != nil {
				return nil, fmt.Errorf("vectorstore/cosmosdb: resolve partition keys: %w", err)
			}
			for _, raw := range page.Items {
				var row struct {
					ID string `json:"id"`
					PK any    `json:"pk"`
				}
				if err := json.Unmarshal(raw, &row); err != nil {
					return nil, fmt.Errorf("vectorstore/cosmosdb: decode partition lookup: %w", err)
				}
				pk, err := partitionKeyOf(row.PK)
				if err != nil {
					return nil, fmt.Errorf("vectorstore/cosmosdb: item %q partition key: %w", row.ID, err)
				}
				out = append(out, idPartition{id: row.ID, pk: pk})
			}
		}
	}
	return out, nil
}

// Close marks the Store closed. A client built by New is closed; a
// caller-supplied Options.Client is left untouched. Safe to call multiple
// times.
func (s *Store) Close() error {
	s.closeMu.Lock()
	defer s.closeMu.Unlock()
	if s.closed.Swap(true) {
		return nil
	}
	if s.owned != nil {
		s.owned.Close()
	}
	return nil
}

// ContainerProperties returns container properties matching the Store's
// layout — partition key path, a vector embedding policy for VectorField
// (Dimension, DistanceFunction, float32) and an indexing policy that
// excludes the raw vector from range indexing and adds a vector index
// (IndexType). Pass it to azcosmos.DatabaseClient.CreateContainer to
// bootstrap a container. Requires Options.Dimension > 0.
func (s *Store) ContainerProperties(containerID string) (azcosmos.ContainerProperties, error) {
	if containerID == "" {
		return azcosmos.ContainerProperties{}, errors.New("vectorstore/cosmosdb: container id is required")
	}
	if s.dimension <= 0 {
		return azcosmos.ContainerProperties{}, errors.New("vectorstore/cosmosdb: ContainerProperties requires Options.Dimension > 0")
	}
	vecPath := "/" + s.vectorField
	return azcosmos.ContainerProperties{
		ID: containerID,
		PartitionKeyDefinition: azcosmos.PartitionKeyDefinition{
			Kind:    azcosmos.PartitionKeyKindHash,
			Paths:   []string{"/" + s.pkField},
			Version: 2,
		},
		VectorEmbeddingPolicy: &azcosmos.VectorEmbeddingPolicy{
			VectorEmbeddings: []azcosmos.VectorEmbedding{{
				Path:             vecPath,
				DataType:         azcosmos.VectorDataTypeFloat32,
				DistanceFunction: s.distance,
				Dimensions:       int32(s.dimension), //nolint:gosec // embedding dims are far below int32 max.
			}},
		},
		IndexingPolicy: &azcosmos.IndexingPolicy{
			Automatic:     true,
			IndexingMode:  azcosmos.IndexingModeConsistent,
			IncludedPaths: []azcosmos.IncludedPath{{Path: "/*"}},
			ExcludedPaths: []azcosmos.ExcludedPath{{Path: "/\"_etag\"/?"}, {Path: vecPath + "/*"}},
			VectorIndexes: []azcosmos.VectorIndex{{Path: vecPath, Type: s.indexType}},
		},
	}, nil
}

// --- helpers ----------------------------------------------------------

// forEach runs fn for indexes [0,n) with at most s.concurrency in flight.
// The first error cancels the shared context and is returned.
func (s *Store) forEach(ctx context.Context, n int, fn func(ctx context.Context, i int) error) error {
	if n == 0 {
		return nil
	}
	ctx, cancel := context.WithCancel(ctx)
	defer cancel()
	var (
		wg       sync.WaitGroup
		once     sync.Once
		firstErr error
		sem      = make(chan struct{}, s.concurrency)
	)
	for i := 0; i < n; i++ {
		select {
		case sem <- struct{}{}:
		case <-ctx.Done():
		}
		if ctx.Err() != nil {
			break
		}
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			defer func() { <-sem }()
			if err := fn(ctx, i); err != nil {
				once.Do(func() { firstErr = err; cancel() })
			}
		}(i)
	}
	wg.Wait()
	if firstErr != nil {
		return firstErr
	}
	return ctx.Err()
}

// partitionKeyOf converts a JSON-ish scalar into an azcosmos.PartitionKey.
func partitionKeyOf(v any) (azcosmos.PartitionKey, error) {
	switch x := v.(type) {
	case nil:
		return azcosmos.NullPartitionKey, nil
	case string:
		return azcosmos.NewPartitionKeyString(x), nil
	case bool:
		return azcosmos.NewPartitionKeyBool(x), nil
	case float64:
		return azcosmos.NewPartitionKeyNumber(x), nil
	case float32:
		return azcosmos.NewPartitionKeyNumber(float64(x)), nil
	case int:
		return azcosmos.NewPartitionKeyNumber(float64(x)), nil
	case int32:
		return azcosmos.NewPartitionKeyNumber(float64(x)), nil
	case int64:
		return azcosmos.NewPartitionKeyNumber(float64(x)), nil
	case json.Number:
		f, err := x.Float64()
		if err != nil {
			return azcosmos.PartitionKey{}, err
		}
		return azcosmos.NewPartitionKeyNumber(f), nil
	default:
		return azcosmos.PartitionKey{}, fmt.Errorf("unsupported partition key type %T", v)
	}
}

func isNotFound(err error) bool {
	var re *azcore.ResponseError
	return errors.As(err, &re) && re.StatusCode == http.StatusNotFound
}

func orDefault(v, def string) string {
	if v == "" {
		return def
	}
	return v
}

// errClosed is returned when any method is called after Close.
var errClosed = errors.New("vectorstore/cosmosdb: store is closed")
