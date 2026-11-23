package aisearch

import (
	"context"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"net/url"
	"strings"
	"sync/atomic"

	"github.com/Azure/azure-sdk-for-go/sdk/azcore"
	"github.com/Azure/azure-sdk-for-go/sdk/azcore/policy"
	"github.com/Azure/azure-sdk-for-go/sdk/azcore/runtime"

	"oss.nandlabs.io/golly-azure/azcfg"
	"oss.nandlabs.io/golly/vectorstore"
)

const (
	// DefaultAPIVersion is the Azure AI Search data-plane REST api-version
	// used when Options.APIVersion is empty. It is a GA version that supports
	// vectorQueries, hybrid search and vector filter modes.
	DefaultAPIVersion = "2024-07-01"
	// DefaultScope is the Entra ID token scope for the Azure AI Search data
	// plane in the public cloud. Sovereign clouds use a different audience
	// (e.g. https://search.azure.us/.default) — set Options.Scope.
	DefaultScope = "https://search.azure.com/.default"
	// APIKeyHeader is the request header that carries an admin/query key.
	APIKeyHeader = "api-key"

	// defaultBatchSize is the per-request document cap. Azure AI Search
	// accepts at most 1000 actions (and 16 MB) per indexing batch.
	defaultBatchSize = 1000
	// defaultTopK mirrors the vectorstore default.
	defaultTopK = 10

	moduleName    = "golly-azure/vectorstore/aisearch"
	moduleVersion = "v0.1.0"
)

// Default field names (match the schema written by CreateIndex).
const (
	DefaultIDField       = "id"
	DefaultVectorField   = "embedding"
	DefaultContentField  = "content"
	DefaultMetadataField = "metadata"
)

// Action is an Azure AI Search indexing action used by Upsert.
type Action string

const (
	// ActionMergeOrUpload merges the supplied fields into an existing
	// document, or uploads it when absent. Fields omitted from the Doc (e.g.
	// empty Content) keep their previous values. This is the default.
	ActionMergeOrUpload Action = "mergeOrUpload"
	// ActionUpload replaces the whole document (strict upsert semantics).
	ActionUpload Action = "upload"
)

// Metric is the HNSW similarity metric written by CreateIndex.
type Metric string

const (
	MetricCosine     Metric = "cosine"
	MetricEuclidean  Metric = "euclidean"
	MetricDotProduct Metric = "dotProduct"
)

// Options configures a Store.
type Options struct {
	// Endpoint is the search service URL, e.g.
	// "https://my-service.search.windows.net". When empty it falls back to
	// Config.Endpoint, then to "https://<Config.AccountName>.search.windows.net".
	Endpoint string
	// Index is the target index name. Required.
	Index string

	// APIKey is an admin key (required for writes) or query key. When set,
	// it is sent in the "api-key" header and Entra ID auth is not used.
	// When empty, Config.AccountKey is used as the key if present.
	APIKey string
	// Config supplies the Entra ID credential (cfg.Credential()), endpoint
	// fallbacks and azcore client options. Nil resolves the azcfg config
	// registered as azcfg.DefaultConfigName, or a fresh DefaultAzureCredential
	// config.
	Config *azcfg.Config
	// Scope overrides the token scope (default DefaultScope).
	Scope string
	// APIVersion overrides the REST api-version (default DefaultAPIVersion).
	APIVersion string
	// ClientOptions, when non-nil, overrides Config.ClientOpts() for the
	// HTTP pipeline (transport, retry, logging, ...).
	ClientOptions *policy.ClientOptions

	// IDField is the index key field (default "id").
	IDField string
	// VectorField is the Collection(Edm.Single) vector field (default "embedding").
	VectorField string
	// ContentField is the searchable text field (default "content").
	ContentField string
	// MetadataField is the Edm.ComplexType field holding Doc.Metadata
	// (default "metadata"). Ignored when FlattenMetadata is set.
	MetadataField string
	// FlattenMetadata stores every Doc.Metadata key as a top-level index
	// field instead of nesting it under MetadataField. Filters then target
	// "<key>" rather than "metadata/<key>", and on read every non-reserved
	// field is returned as metadata.
	FlattenMetadata bool
	// EncodeKeys base64url-encodes document ids before writing them. Azure
	// AI Search keys may only contain letters, digits, '_', '-' and '='; turn
	// this on when ids can contain other characters (URIs, paths, ...).
	// Hits are decoded back transparently.
	EncodeKeys bool

	// Action is the indexing action used by Upsert (default ActionMergeOrUpload).
	Action Action
	// BatchSize caps the number of actions per indexing request (default
	// and maximum 1000).
	BatchSize int
	// Dimension is the embedding dimension. When > 0, Upsert and Search
	// validate vector length (vectorstore.ErrDimMismatch); CreateIndex
	// requires it.
	Dimension int
	// Metric is the HNSW metric used by CreateIndex (default MetricCosine).
	Metric Metric
	// Exhaustive forces exhaustive (brute-force) KNN on every vector query
	// instead of the HNSW graph — useful for ground-truth comparisons.
	Exhaustive bool
}

// Store implements vectorstore.Store on top of an Azure AI Search index.
type Store struct {
	pl         runtime.Pipeline
	endpoint   string
	index      string
	apiVersion string

	idField       string
	vectorField   string
	contentField  string
	metadataField string
	flatten       bool
	encodeKeys    bool

	action     Action
	batchSize  int
	dimension  int
	metric     Metric
	exhaustive bool

	closed atomic.Bool
}

// New validates options, resolves credentials and returns a ready Store. It
// performs no network calls.
func New(opts Options) (*Store, error) {
	if opts.Index == "" {
		return nil, errors.New("vectorstore/aisearch: Options.Index is required")
	}
	cfg := opts.Config
	if cfg == nil {
		cfg = azcfg.GetConfigOrDefault(nil, azcfg.DefaultConfigName)
	}
	endpoint := opts.Endpoint
	if endpoint == "" {
		endpoint = cfg.Endpoint
	}
	if endpoint == "" && cfg.AccountName != "" {
		endpoint = "https://" + cfg.AccountName + ".search.windows.net"
	}
	if endpoint == "" {
		return nil, errors.New("vectorstore/aisearch: Options.Endpoint (or Config.Endpoint / Config.AccountName) is required")
	}
	u, err := url.Parse(endpoint)
	if err != nil || u.Scheme == "" || u.Host == "" {
		return nil, fmt.Errorf("vectorstore/aisearch: invalid endpoint %q", endpoint)
	}

	s := &Store{
		endpoint:      strings.TrimRight(endpoint, "/"),
		index:         opts.Index,
		apiVersion:    orDefault(opts.APIVersion, DefaultAPIVersion),
		idField:       orDefault(opts.IDField, DefaultIDField),
		vectorField:   orDefault(opts.VectorField, DefaultVectorField),
		contentField:  orDefault(opts.ContentField, DefaultContentField),
		metadataField: orDefault(opts.MetadataField, DefaultMetadataField),
		flatten:       opts.FlattenMetadata,
		encodeKeys:    opts.EncodeKeys,
		action:        opts.Action,
		batchSize:     opts.BatchSize,
		dimension:     opts.Dimension,
		metric:        opts.Metric,
		exhaustive:    opts.Exhaustive,
	}
	if s.action == "" {
		s.action = ActionMergeOrUpload
	}
	switch s.action {
	case ActionMergeOrUpload, ActionUpload:
	default:
		return nil, fmt.Errorf("vectorstore/aisearch: unsupported Action %q", s.action)
	}
	if s.batchSize <= 0 || s.batchSize > defaultBatchSize {
		s.batchSize = defaultBatchSize
	}
	if s.metric == "" {
		s.metric = MetricCosine
	}
	switch s.metric {
	case MetricCosine, MetricEuclidean, MetricDotProduct:
	default:
		return nil, fmt.Errorf("vectorstore/aisearch: unknown Metric %q", s.metric)
	}
	if err := s.validateFieldNames(); err != nil {
		return nil, err
	}

	var authPolicy policy.Policy
	key := opts.APIKey
	if key == "" {
		key = cfg.AccountKey
	}
	if key != "" {
		authPolicy = runtime.NewKeyCredentialPolicy(azcore.NewKeyCredential(key), APIKeyHeader, nil)
	} else {
		cred, err := cfg.Credential()
		if err != nil {
			return nil, fmt.Errorf("vectorstore/aisearch: resolve credential: %w", err)
		}
		authPolicy = runtime.NewBearerTokenPolicy(cred, []string{orDefault(opts.Scope, DefaultScope)}, nil)
	}
	var clientOpts policy.ClientOptions
	if opts.ClientOptions != nil {
		clientOpts = *opts.ClientOptions
	} else {
		clientOpts = cfg.ClientOpts()
	}
	s.pl = runtime.NewPipeline(moduleName, moduleVersion, runtime.PipelineOptions{
		PerRetry: []policy.Policy{authPolicy},
	}, &clientOpts)
	return s, nil
}

func (s *Store) validateFieldNames() error {
	names := map[string]string{
		"IDField":      s.idField,
		"VectorField":  s.vectorField,
		"ContentField": s.contentField,
	}
	if !s.flatten {
		names["MetadataField"] = s.metadataField
	}
	seen := map[string]string{}
	for opt, n := range names {
		if !fieldPathRE.MatchString(n) || strings.Contains(n, "/") {
			return fmt.Errorf("vectorstore/aisearch: Options.%s %q is not a valid field name", opt, n)
		}
		if other, dup := seen[n]; dup {
			return fmt.Errorf("vectorstore/aisearch: Options.%s and Options.%s both use field %q", opt, other, n)
		}
		seen[n] = opt
	}
	return nil
}

// Upsert writes docs with the configured indexing action (mergeOrUpload by
// default). Batches larger than BatchSize are split into multiple requests;
// a failure on any batch aborts the sequence. Per-document failures reported
// by the service (HTTP 207) are returned as a *BatchError.
func (s *Store) Upsert(ctx context.Context, docs ...vectorstore.Doc) error {
	if s.closed.Load() {
		return errClosed
	}
	if len(docs) == 0 {
		return nil
	}
	actions := make([]map[string]any, 0, len(docs))
	for _, d := range docs {
		if err := vectorstore.ValidateDoc(d); err != nil {
			return err
		}
		if s.dimension > 0 && len(d.Vector) != s.dimension {
			return fmt.Errorf("%w: index dim=%d, doc=%q dim=%d", vectorstore.ErrDimMismatch, s.dimension, d.ID, len(d.Vector))
		}
		a, err := s.docAction(d)
		if err != nil {
			return err
		}
		actions = append(actions, a)
	}
	return s.indexBatches(ctx, actions, "upsert")
}

// docAction renders one indexing action for d.
func (s *Store) docAction(d vectorstore.Doc) (map[string]any, error) {
	a := map[string]any{
		"@search.action": string(s.action),
		s.idField:        s.encodeKey(d.ID),
		s.vectorField:    []float32(d.Vector),
	}
	if d.Content != "" || s.action == ActionUpload {
		a[s.contentField] = d.Content
	}
	if s.flatten {
		for k, v := range d.Metadata {
			if k == s.idField || k == s.vectorField || k == s.contentField || strings.HasPrefix(k, "@") {
				return nil, fmt.Errorf("vectorstore/aisearch: doc %q: metadata key %q collides with a reserved field", d.ID, k)
			}
			a[k] = v
		}
	} else if len(d.Metadata) > 0 {
		a[s.metadataField] = d.Metadata
	}
	return a, nil
}

// Search runs a vector (or hybrid) query and returns hits ordered by
// descending @search.score.
//
// Query.Filter is translated to an OData $filter (see BuildFilter) applied
// in preFilter mode; the reserved key QueryTextKey ("_text") supplies
// optional hybrid search text and RawFilterKey ("_odata") a raw OData
// expression.
func (s *Store) Search(ctx context.Context, q vectorstore.Query) ([]vectorstore.Hit, error) {
	if s.closed.Load() {
		return nil, errClosed
	}
	if len(q.Vector) == 0 {
		return nil, vectorstore.ErrEmptyVector
	}
	if s.dimension > 0 && len(q.Vector) != s.dimension {
		return nil, fmt.Errorf("%w: query dim=%d, index dim=%d", vectorstore.ErrDimMismatch, len(q.Vector), s.dimension)
	}
	topK := q.TopK
	if topK <= 0 {
		topK = defaultTopK
	}
	vq := map[string]any{
		"kind":   "vector",
		"vector": []float32(q.Vector),
		"fields": s.vectorField,
		"k":      topK,
	}
	if s.exhaustive {
		vq["exhaustive"] = true
	}
	body := map[string]any{
		"top":           topK,
		"vectorQueries": []map[string]any{vq},
	}
	if !s.flatten {
		body["select"] = strings.Join([]string{s.idField, s.contentField, s.metadataField}, ",")
	}
	if text, ok := q.Filter[QueryTextKey]; ok {
		str, isStr := text.(string)
		if !isStr {
			return nil, fmt.Errorf("%w: %q must be a string, got %T", ErrInvalidFilter, QueryTextKey, text)
		}
		if str != "" {
			body["search"] = str
		}
	}
	prefix := s.metadataField
	if s.flatten {
		prefix = ""
	}
	filter, err := BuildFilter(prefix, q.Filter)
	if err != nil {
		return nil, err
	}
	if filter != "" {
		body["filter"] = filter
		body["vectorFilterMode"] = "preFilter"
	}

	var out struct {
		Value []map[string]json.RawMessage `json:"value"`
	}
	if err := s.do(ctx, http.MethodPost, s.indexPath()+"/docs/search", body, &out, http.StatusOK); err != nil {
		return nil, fmt.Errorf("vectorstore/aisearch: search %q: %w", s.index, err)
	}
	hits := make([]vectorstore.Hit, 0, len(out.Value))
	for _, raw := range out.Value {
		h, err := s.decodeHit(raw)
		if err != nil {
			return nil, err
		}
		if q.MinScore != 0 && h.Score < q.MinScore {
			continue
		}
		hits = append(hits, h)
	}
	return hits, nil
}

// decodeHit maps one search result document onto a vectorstore.Hit.
func (s *Store) decodeHit(raw map[string]json.RawMessage) (vectorstore.Hit, error) {
	var h vectorstore.Hit
	if v, ok := raw["@search.score"]; ok {
		var score float64
		if err := json.Unmarshal(v, &score); err != nil {
			return h, fmt.Errorf("vectorstore/aisearch: parse @search.score: %w", err)
		}
		h.Score = float32(score)
	}
	if v, ok := raw[s.idField]; ok {
		var id string
		if err := json.Unmarshal(v, &id); err != nil {
			return h, fmt.Errorf("vectorstore/aisearch: parse %s: %w", s.idField, err)
		}
		h.ID = s.decodeKey(id)
	}
	if v, ok := raw[s.contentField]; ok && string(v) != "null" {
		if err := json.Unmarshal(v, &h.Content); err != nil {
			return h, fmt.Errorf("vectorstore/aisearch: parse %s for %q: %w", s.contentField, h.ID, err)
		}
	}
	if s.flatten {
		for k, v := range raw {
			if k == s.idField || k == s.contentField || k == s.vectorField || strings.HasPrefix(k, "@") {
				continue
			}
			var val any
			if err := json.Unmarshal(v, &val); err != nil {
				return h, fmt.Errorf("vectorstore/aisearch: parse field %q for %q: %w", k, h.ID, err)
			}
			if h.Metadata == nil {
				h.Metadata = make(map[string]any)
			}
			h.Metadata[k] = val
		}
	} else if v, ok := raw[s.metadataField]; ok && string(v) != "null" {
		if err := json.Unmarshal(v, &h.Metadata); err != nil {
			return h, fmt.Errorf("vectorstore/aisearch: parse %s for %q: %w", s.metadataField, h.ID, err)
		}
	}
	return h, nil
}

// Delete removes docs by id using "delete" indexing actions. Unknown ids are
// silently ignored by the service.
func (s *Store) Delete(ctx context.Context, ids ...string) error {
	if s.closed.Load() {
		return errClosed
	}
	if len(ids) == 0 {
		return nil
	}
	actions := make([]map[string]any, 0, len(ids))
	for _, id := range ids {
		if id == "" {
			return vectorstore.ErrEmptyID
		}
		actions = append(actions, map[string]any{
			"@search.action": "delete",
			s.idField:        s.encodeKey(id),
		})
	}
	return s.indexBatches(ctx, actions, "delete")
}

// Close marks the Store closed. It holds no connections of its own beyond
// the shared HTTP transport, so this is idempotent and never fails.
func (s *Store) Close() error {
	s.closed.Store(true)
	return nil
}

// --- internals --------------------------------------------------------

// ItemError is one per-document failure reported by an indexing batch.
type ItemError struct {
	Key        string
	StatusCode int
	Message    string
}

// BatchError is returned when the service accepted an indexing batch but
// rejected one or more of its documents (HTTP 207 Multi-Status).
type BatchError struct {
	Op     string
	Index  string
	Failed []ItemError
}

// Error implements error.
func (e *BatchError) Error() string {
	var b strings.Builder
	fmt.Fprintf(&b, "vectorstore/aisearch: %s %q: %d document(s) failed", e.Op, e.Index, len(e.Failed))
	for i, f := range e.Failed {
		if i == 3 {
			fmt.Fprintf(&b, "; ... (%d more)", len(e.Failed)-i)
			break
		}
		fmt.Fprintf(&b, "; key=%q status=%d: %s", f.Key, f.StatusCode, f.Message)
	}
	return b.String()
}

type indexResult struct {
	Key          string  `json:"key"`
	Status       bool    `json:"status"`
	ErrorMessage *string `json:"errorMessage"`
	StatusCode   int     `json:"statusCode"`
}

func (s *Store) indexBatches(ctx context.Context, actions []map[string]any, op string) error {
	for start := 0; start < len(actions); start += s.batchSize {
		end := min(start+s.batchSize, len(actions))
		var out struct {
			Value []indexResult `json:"value"`
		}
		body := map[string]any{"value": actions[start:end]}
		if err := s.do(ctx, http.MethodPost, s.indexPath()+"/docs/index", body, &out, http.StatusOK, http.StatusMultiStatus); err != nil {
			return fmt.Errorf("vectorstore/aisearch: %s %q [%d:%d]: %w", op, s.index, start, end, err)
		}
		var failed []ItemError
		for _, r := range out.Value {
			if r.Status || (op == "delete" && r.StatusCode == http.StatusNotFound) {
				continue
			}
			msg := ""
			if r.ErrorMessage != nil {
				msg = *r.ErrorMessage
			}
			failed = append(failed, ItemError{Key: s.decodeKey(r.Key), StatusCode: r.StatusCode, Message: msg})
		}
		if len(failed) > 0 {
			return &BatchError{Op: op, Index: s.index, Failed: failed}
		}
	}
	return nil
}

// do sends a JSON request through the pipeline and decodes the response.
func (s *Store) do(ctx context.Context, method, path string, body, out any, okStatus ...int) error {
	req, err := runtime.NewRequest(ctx, method, s.endpoint+path)
	if err != nil {
		return err
	}
	qp := req.Raw().URL.Query()
	qp.Set("api-version", s.apiVersion)
	req.Raw().URL.RawQuery = qp.Encode()
	req.Raw().Header.Set("Accept", "application/json")
	if body != nil {
		if err := runtime.MarshalAsJSON(req, body); err != nil {
			return err
		}
	}
	resp, err := s.pl.Do(req)
	if err != nil {
		return err
	}
	if !runtime.HasStatusCode(resp, okStatus...) {
		return runtime.NewResponseError(resp)
	}
	if out == nil || resp.StatusCode == http.StatusNoContent {
		_, _ = runtime.Payload(resp)
		return nil
	}
	if err := runtime.UnmarshalAsJSON(resp, out); err != nil {
		return fmt.Errorf("decode response: %w", err)
	}
	return nil
}

func (s *Store) indexPath() string {
	return "/indexes/" + url.PathEscape(s.index)
}

func (s *Store) encodeKey(id string) string {
	if !s.encodeKeys {
		return id
	}
	return base64.RawURLEncoding.EncodeToString([]byte(id))
}

func (s *Store) decodeKey(key string) string {
	if !s.encodeKeys {
		return key
	}
	b, err := base64.RawURLEncoding.DecodeString(key)
	if err != nil {
		// Not one of ours (written by another client) — surface verbatim.
		return key
	}
	return string(b)
}

func orDefault(v, def string) string {
	if v == "" {
		return def
	}
	return v
}

// IsNotFound reports whether err is an Azure AI Search 404 response (e.g. the
// index does not exist).
func IsNotFound(err error) bool {
	var re *azcore.ResponseError
	return errors.As(err, &re) && re.StatusCode == http.StatusNotFound
}

// errClosed is returned when any method is called after Close.
var errClosed = errors.New("vectorstore/aisearch: store is closed")
