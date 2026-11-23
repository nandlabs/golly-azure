package cosmosdb

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"reflect"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/Azure/azure-sdk-for-go/sdk/azcore"
	"github.com/Azure/azure-sdk-for-go/sdk/azcore/policy"
	"github.com/Azure/azure-sdk-for-go/sdk/azcore/runtime"
	"github.com/Azure/azure-sdk-for-go/sdk/data/azcosmos"

	"oss.nandlabs.io/golly-azure/azcfg"
	"oss.nandlabs.io/golly/vectorstore"
)

// Compile-time check: the real container client satisfies containerAPI.
var _ containerAPI = (*azcosmos.ContainerClient)(nil)

var _ vectorstore.Store = (*Store)(nil)

// --- fakes -------------------------------------------------------------

type upsertCall struct {
	pk   azcosmos.PartitionKey
	item map[string]any
}

type deleteCall struct {
	pk azcosmos.PartitionKey
	id string
}

type queryCall struct {
	query  string
	pk     azcosmos.PartitionKey
	params []azcosmos.QueryParameter
}

type fakeContainer struct {
	mu        sync.Mutex
	upserts   []upsertCall
	deletes   []deleteCall
	queries   []queryCall
	upsertErr func(id string) error
	deleteErr func(id string) error
	// pages returns the item pages for a query (each inner slice is one page).
	pages    func(q queryCall) [][]string
	queryErr error
}

func (f *fakeContainer) UpsertItem(_ context.Context, pk azcosmos.PartitionKey, item []byte, _ *azcosmos.ItemOptions) (azcosmos.ItemResponse, error) {
	var m map[string]any
	if err := json.Unmarshal(item, &m); err != nil {
		return azcosmos.ItemResponse{}, err
	}
	f.mu.Lock()
	f.upserts = append(f.upserts, upsertCall{pk: pk, item: m})
	f.mu.Unlock()
	if f.upsertErr != nil {
		if err := f.upsertErr(m["id"].(string)); err != nil {
			return azcosmos.ItemResponse{}, err
		}
	}
	return azcosmos.ItemResponse{}, nil
}

func (f *fakeContainer) DeleteItem(_ context.Context, pk azcosmos.PartitionKey, id string, _ *azcosmos.ItemOptions) (azcosmos.ItemResponse, error) {
	f.mu.Lock()
	f.deletes = append(f.deletes, deleteCall{pk: pk, id: id})
	f.mu.Unlock()
	if f.deleteErr != nil {
		if err := f.deleteErr(id); err != nil {
			return azcosmos.ItemResponse{}, err
		}
	}
	return azcosmos.ItemResponse{}, nil
}

func (f *fakeContainer) NewQueryItemsPager(query string, pk azcosmos.PartitionKey, o *azcosmos.QueryOptions) *runtime.Pager[azcosmos.QueryItemsResponse] {
	qc := queryCall{query: query, pk: pk}
	if o != nil {
		qc.params = o.QueryParameters
	}
	f.mu.Lock()
	f.queries = append(f.queries, qc)
	f.mu.Unlock()
	var pages [][]string
	if f.pages != nil {
		pages = f.pages(qc)
	}
	idx := 0
	return runtime.NewPager(runtime.PagingHandler[azcosmos.QueryItemsResponse]{
		More: func(p azcosmos.QueryItemsResponse) bool { return p.ContinuationToken != nil },
		Fetcher: func(ctx context.Context, _ *azcosmos.QueryItemsResponse) (azcosmos.QueryItemsResponse, error) {
			if f.queryErr != nil {
				return azcosmos.QueryItemsResponse{}, f.queryErr
			}
			var resp azcosmos.QueryItemsResponse
			if idx < len(pages) {
				for _, it := range pages[idx] {
					resp.Items = append(resp.Items, []byte(it))
				}
			}
			idx++
			if idx < len(pages) {
				tok := fmt.Sprintf("tok-%d", idx)
				resp.ContinuationToken = &tok
			}
			return resp, nil
		},
	})
}

func newTestStore(t *testing.T, mutate func(*Options)) (*Store, *fakeContainer) {
	t.Helper()
	fc := &fakeContainer{}
	o := Options{}
	if mutate != nil {
		mutate(&o)
	}
	s, err := newWithContainer(o, fc, nil)
	if err != nil {
		t.Fatalf("newWithContainer: %v", err)
	}
	return s, fc
}

func param(t *testing.T, params []azcosmos.QueryParameter, name string) any {
	t.Helper()
	for _, p := range params {
		if p.Name == name {
			return p.Value
		}
	}
	t.Fatalf("parameter %s not found in %+v", name, params)
	return nil
}

func samePK(a, b azcosmos.PartitionKey) bool { return reflect.DeepEqual(a, b) }

var notFound = &azcore.ResponseError{StatusCode: 404, ErrorCode: "NotFound"}

// --- upsert ------------------------------------------------------------

func TestUpsert_PartitionByID(t *testing.T) {
	s, fc := newTestStore(t, nil)
	docs := []vectorstore.Doc{
		{ID: "a", Vector: vectorstore.Vector{1, 2, 3}, Content: "hello", Metadata: map[string]any{"source": "wiki"}},
		{ID: "b", Vector: vectorstore.Vector{4, 5, 6}},
	}
	if err := s.Upsert(context.Background(), docs...); err != nil {
		t.Fatalf("Upsert: %v", err)
	}
	if len(fc.upserts) != 2 {
		t.Fatalf("want 2 upserts, got %d", len(fc.upserts))
	}
	byID := map[string]upsertCall{}
	for _, u := range fc.upserts {
		byID[u.item["id"].(string)] = u
	}
	a := byID["a"]
	if !samePK(a.pk, azcosmos.NewPartitionKeyString("a")) {
		t.Errorf("pk(a) = %+v", a.pk)
	}
	if a.item["content"] != "hello" || a.item["metadata"].(map[string]any)["source"] != "wiki" {
		t.Errorf("item(a) = %v", a.item)
	}
	if v := a.item["embedding"].([]any); len(v) != 3 {
		t.Errorf("embedding = %v", v)
	}
	b := byID["b"]
	if _, ok := b.item["content"]; ok {
		t.Errorf("empty content should be omitted: %v", b.item)
	}
	if _, ok := b.item["metadata"]; ok {
		t.Errorf("empty metadata should be omitted: %v", b.item)
	}
}

func TestUpsert_StaticPartition(t *testing.T) {
	s, fc := newTestStore(t, func(o *Options) {
		o.PartitionKeyField = "tenant"
		o.PartitionKeyValue = "acme"
		o.VectorField = "vec"
		o.ContentField = "text"
		o.MetadataField = "attrs"
	})
	if err := s.Upsert(context.Background(), vectorstore.Doc{ID: "a", Vector: vectorstore.Vector{1}, Content: "c", Metadata: map[string]any{"x": 1}}); err != nil {
		t.Fatal(err)
	}
	u := fc.upserts[0]
	if !samePK(u.pk, azcosmos.NewPartitionKeyString("acme")) {
		t.Errorf("pk = %+v", u.pk)
	}
	if u.item["tenant"] != "acme" || u.item["vec"] == nil || u.item["text"] != "c" || u.item["attrs"] == nil {
		t.Errorf("item = %v", u.item)
	}
}

func TestUpsert_MetadataPartition(t *testing.T) {
	s, fc := newTestStore(t, func(o *Options) { o.PartitionKeyField = "category" })
	ctx := context.Background()
	cases := []struct {
		val  any
		want azcosmos.PartitionKey
	}{
		{"books", azcosmos.NewPartitionKeyString("books")},
		{3, azcosmos.NewPartitionKeyNumber(3)},
		{true, azcosmos.NewPartitionKeyBool(true)},
		{nil, azcosmos.NullPartitionKey},
	}
	for i, tc := range cases {
		id := fmt.Sprint("d", i)
		if err := s.Upsert(ctx, vectorstore.Doc{ID: id, Vector: vectorstore.Vector{1}, Metadata: map[string]any{"category": tc.val}}); err != nil {
			t.Fatalf("Upsert %v: %v", tc.val, err)
		}
		u := fc.upserts[len(fc.upserts)-1]
		if !samePK(u.pk, tc.want) {
			t.Errorf("pk for %v = %+v", tc.val, u.pk)
		}
		if v, ok := u.item["category"]; !ok || (tc.val != nil && fmt.Sprint(v) != fmt.Sprint(tc.val)) {
			t.Errorf("top-level partition property = %v", u.item)
		}
	}
	err := s.Upsert(ctx, vectorstore.Doc{ID: "x", Vector: vectorstore.Vector{1}})
	if !errors.Is(err, ErrMissingPartitionKey) {
		t.Errorf("want ErrMissingPartitionKey, got %v", err)
	}
	err = s.Upsert(ctx, vectorstore.Doc{ID: "x", Vector: vectorstore.Vector{1}, Metadata: map[string]any{"category": []string{"a"}}})
	if !errors.Is(err, ErrMissingPartitionKey) {
		t.Errorf("want ErrMissingPartitionKey for slice, got %v", err)
	}
}

func TestUpsert_Validation(t *testing.T) {
	s, fc := newTestStore(t, func(o *Options) { o.Dimension = 2 })
	ctx := context.Background()
	if err := s.Upsert(ctx); err != nil {
		t.Errorf("empty upsert: %v", err)
	}
	if err := s.Upsert(ctx, vectorstore.Doc{Vector: vectorstore.Vector{1, 2}}); !errors.Is(err, vectorstore.ErrEmptyID) {
		t.Errorf("want ErrEmptyID, got %v", err)
	}
	if err := s.Upsert(ctx, vectorstore.Doc{ID: "a"}); !errors.Is(err, vectorstore.ErrEmptyVector) {
		t.Errorf("want ErrEmptyVector, got %v", err)
	}
	if err := s.Upsert(ctx, vectorstore.Doc{ID: "a", Vector: vectorstore.Vector{1}}); !errors.Is(err, vectorstore.ErrDimMismatch) {
		t.Errorf("want ErrDimMismatch, got %v", err)
	}
	if err := s.Upsert(ctx, vectorstore.Doc{ID: "a", Vector: vectorstore.Vector{1, 2}, Metadata: map[string]any{"bad": make(chan int)}}); err == nil {
		t.Error("want marshal error")
	}
	if len(fc.upserts) != 0 {
		t.Errorf("no writes expected, got %d", len(fc.upserts))
	}
}

func TestUpsert_ErrorStopsAndConcurrency(t *testing.T) {
	boom := errors.New("boom")
	s, fc := newTestStore(t, func(o *Options) { o.MaxConcurrency = 1 })
	fc.upsertErr = func(id string) error {
		if id == "d2" {
			return boom
		}
		return nil
	}
	docs := make([]vectorstore.Doc, 10)
	for i := range docs {
		docs[i] = vectorstore.Doc{ID: fmt.Sprint("d", i), Vector: vectorstore.Vector{1}}
	}
	err := s.Upsert(context.Background(), docs...)
	if !errors.Is(err, boom) || !strings.Contains(err.Error(), `"d2"`) {
		t.Fatalf("want boom for d2, got %v", err)
	}
	if n := len(fc.upserts); n >= 10 {
		t.Errorf("expected remaining writes to be cancelled, got %d calls", n)
	}
}

func TestUpsert_ParallelAll(t *testing.T) {
	s, fc := newTestStore(t, func(o *Options) { o.MaxConcurrency = 8 })
	docs := make([]vectorstore.Doc, 50)
	for i := range docs {
		docs[i] = vectorstore.Doc{ID: fmt.Sprint("d", i), Vector: vectorstore.Vector{1}}
	}
	if err := s.Upsert(context.Background(), docs...); err != nil {
		t.Fatal(err)
	}
	if len(fc.upserts) != 50 {
		t.Errorf("want 50 writes, got %d", len(fc.upserts))
	}
}

func TestUpsert_CancelledContext(t *testing.T) {
	s, fc := newTestStore(t, nil)
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	err := s.Upsert(ctx, vectorstore.Doc{ID: "a", Vector: vectorstore.Vector{1}})
	if !errors.Is(err, context.Canceled) {
		t.Errorf("want context.Canceled, got %v", err)
	}
	if len(fc.upserts) != 0 {
		t.Errorf("no writes expected, got %d", len(fc.upserts))
	}
}

// --- search ------------------------------------------------------------

func TestSearch_QueryShape(t *testing.T) {
	s, fc := newTestStore(t, nil)
	fc.pages = func(queryCall) [][]string {
		return [][]string{
			{`{"id":"a","content":"hello","metadata":{"source":"wiki"},"score":0.93}`},
			{},
			{`{"id":"b","score":0.71}`},
		}
	}
	hits, err := s.Search(context.Background(), vectorstore.Query{Vector: vectorstore.Vector{0.1, 0.2}, TopK: 5})
	if err != nil {
		t.Fatalf("Search: %v", err)
	}
	if len(hits) != 2 {
		t.Fatalf("want 2 hits across pages, got %d", len(hits))
	}
	if hits[0].ID != "a" || hits[0].Content != "hello" || hits[0].Metadata["source"] != "wiki" || hits[0].Score != 0.93 {
		t.Errorf("hit0 = %+v", hits[0])
	}
	if hits[1].ID != "b" || hits[1].Metadata != nil {
		t.Errorf("hit1 = %+v", hits[1])
	}
	q := fc.queries[0]
	want := "SELECT TOP @k c.id, c.content AS content, c.metadata AS metadata, VectorDistance(c.embedding, @vec) AS score FROM c ORDER BY VectorDistance(c.embedding, @vec)"
	if q.query != want {
		t.Errorf("query =\n%s\nwant\n%s", q.query, want)
	}
	if !samePK(q.pk, azcosmos.NewPartitionKey()) {
		t.Errorf("expected cross-partition query, pk = %+v", q.pk)
	}
	if param(t, q.params, "@k") != 5 {
		t.Errorf("@k = %v", param(t, q.params, "@k"))
	}
	if v := param(t, q.params, "@vec").([]float32); len(v) != 2 {
		t.Errorf("@vec = %v", v)
	}
}

func TestSearch_FilterAndPartitionScoping(t *testing.T) {
	s, fc := newTestStore(t, func(o *Options) { o.PartitionKeyField = "category"; o.MetadataField = "meta-data" })
	_, err := s.Search(context.Background(), vectorstore.Query{
		Vector: vectorstore.Vector{1},
		Filter: map[string]any{"category": "books", "rank": map[string]any{"$gt": 2}},
	})
	if err != nil {
		t.Fatal(err)
	}
	q := fc.queries[0]
	if !strings.Contains(q.query, `WHERE c["meta-data"].category = @f0 AND c["meta-data"].rank > @f1 ORDER BY`) {
		t.Errorf("query = %s", q.query)
	}
	if !strings.Contains(q.query, `c["meta-data"] AS metadata`) {
		t.Errorf("metadata projection missing: %s", q.query)
	}
	if !samePK(q.pk, azcosmos.NewPartitionKeyString("books")) {
		t.Errorf("expected single-partition query, pk = %+v", q.pk)
	}
	if param(t, q.params, "@f0") != "books" || param(t, q.params, "@f1") != 2 {
		t.Errorf("params = %+v", q.params)
	}
	// Operator form on the partition key does not scope.
	_, _ = s.Search(context.Background(), vectorstore.Query{Vector: vectorstore.Vector{1}, Filter: map[string]any{"category": map[string]any{"$ne": "x"}}})
	if !samePK(fc.queries[1].pk, azcosmos.NewPartitionKey()) {
		t.Errorf("expected cross-partition for operator filter")
	}
}

func TestSearch_StaticPartition(t *testing.T) {
	s, fc := newTestStore(t, func(o *Options) { o.PartitionKeyField = "pk"; o.PartitionKeyValue = "p1" })
	if _, err := s.Search(context.Background(), vectorstore.Query{Vector: vectorstore.Vector{1}}); err != nil {
		t.Fatal(err)
	}
	if !samePK(fc.queries[0].pk, azcosmos.NewPartitionKeyString("p1")) {
		t.Errorf("pk = %+v", fc.queries[0].pk)
	}
	if param(t, fc.queries[0].params, "@k") != 10 {
		t.Errorf("default TopK not applied")
	}
}

func TestSearch_TopKTruncatesAndMinScore(t *testing.T) {
	rows := [][]string{{
		`{"id":"a","score":0.9}`, `{"id":"b","score":0.4}`, `{"id":"c","score":0.8}`, `{"id":"d","score":0.7}`,
	}}
	s, fc := newTestStore(t, nil)
	fc.pages = func(queryCall) [][]string { return rows }
	hits, err := s.Search(context.Background(), vectorstore.Query{Vector: vectorstore.Vector{1}, TopK: 2, MinScore: 0.5})
	if err != nil {
		t.Fatal(err)
	}
	if len(hits) != 2 || hits[0].ID != "a" || hits[1].ID != "c" {
		t.Errorf("hits = %+v", hits)
	}

	// Euclidean: lower is better, MinScore acts as a max distance.
	e, fe := newTestStore(t, func(o *Options) { o.DistanceFunction = azcosmos.VectorDistanceFunctionEuclidean })
	fe.pages = func(queryCall) [][]string {
		return [][]string{{`{"id":"near","score":0.2}`, `{"id":"far","score":3.5}`}}
	}
	hits, err = e.Search(context.Background(), vectorstore.Query{Vector: vectorstore.Vector{1}, MinScore: 1})
	if err != nil {
		t.Fatal(err)
	}
	if len(hits) != 1 || hits[0].ID != "near" {
		t.Errorf("euclidean hits = %+v", hits)
	}
}

func TestSearch_Errors(t *testing.T) {
	s, fc := newTestStore(t, func(o *Options) { o.Dimension = 1 })
	ctx := context.Background()
	if _, err := s.Search(ctx, vectorstore.Query{}); !errors.Is(err, vectorstore.ErrEmptyVector) {
		t.Errorf("want ErrEmptyVector, got %v", err)
	}
	if _, err := s.Search(ctx, vectorstore.Query{Vector: vectorstore.Vector{1, 2}}); !errors.Is(err, vectorstore.ErrDimMismatch) {
		t.Errorf("want ErrDimMismatch, got %v", err)
	}
	if _, err := s.Search(ctx, vectorstore.Query{Vector: vectorstore.Vector{1}, Filter: map[string]any{"a": map[string]any{"$foo": 1}}}); !errors.Is(err, ErrInvalidFilter) {
		t.Errorf("want ErrInvalidFilter, got %v", err)
	}
	fc.pages = func(queryCall) [][]string { return [][]string{{`not json`}} }
	if _, err := s.Search(ctx, vectorstore.Query{Vector: vectorstore.Vector{1}}); err == nil {
		t.Error("want decode error")
	}
	fc.queryErr = errors.New("gateway says no")
	if _, err := s.Search(ctx, vectorstore.Query{Vector: vectorstore.Vector{1}}); err == nil || !strings.Contains(err.Error(), "gateway says no") {
		t.Errorf("want query error, got %v", err)
	}
}

// --- delete ------------------------------------------------------------

func TestDelete_ByIDAndStatic(t *testing.T) {
	s, fc := newTestStore(t, nil)
	fc.deleteErr = func(id string) error {
		if id == "missing" {
			return notFound
		}
		return nil
	}
	if err := s.Delete(context.Background(), "a", "missing"); err != nil {
		t.Fatalf("Delete: %v", err)
	}
	if len(fc.deletes) != 2 {
		t.Fatalf("want 2 deletes, got %d", len(fc.deletes))
	}
	for _, d := range fc.deletes {
		if !samePK(d.pk, azcosmos.NewPartitionKeyString(d.id)) {
			t.Errorf("pk for %s = %+v", d.id, d.pk)
		}
	}

	st, fs := newTestStore(t, func(o *Options) { o.PartitionKeyField = "pk"; o.PartitionKeyValue = "p1" })
	if err := st.Delete(context.Background(), "a"); err != nil {
		t.Fatal(err)
	}
	if !samePK(fs.deletes[0].pk, azcosmos.NewPartitionKeyString("p1")) {
		t.Errorf("static pk = %+v", fs.deletes[0].pk)
	}
}

func TestDelete_Errors(t *testing.T) {
	s, fc := newTestStore(t, nil)
	ctx := context.Background()
	if err := s.Delete(ctx); err != nil {
		t.Errorf("empty delete: %v", err)
	}
	if err := s.Delete(ctx, "a", ""); !errors.Is(err, vectorstore.ErrEmptyID) {
		t.Errorf("want ErrEmptyID, got %v", err)
	}
	fc.deleteErr = func(string) error { return &azcore.ResponseError{StatusCode: 500} }
	if err := s.Delete(ctx, "a"); err == nil {
		t.Error("want error on 500")
	}
}

func TestDelete_MetadataPartitionLookup(t *testing.T) {
	s, fc := newTestStore(t, func(o *Options) { o.PartitionKeyField = "category" })
	fc.pages = func(q queryCall) [][]string {
		return [][]string{{`{"id":"a","pk":"books"}`}, {`{"id":"b","pk":7}`, `{"id":"c"}`}}
	}
	if err := s.Delete(context.Background(), "a", "b", "c", "gone"); err != nil {
		t.Fatalf("Delete: %v", err)
	}
	q := fc.queries[0]
	if q.query != "SELECT c.id, c.category AS pk FROM c WHERE ARRAY_CONTAINS(@ids, c.id)" {
		t.Errorf("lookup query = %s", q.query)
	}
	if !samePK(q.pk, azcosmos.NewPartitionKey()) {
		t.Errorf("lookup must be cross-partition")
	}
	if ids := param(t, q.params, "@ids").([]string); len(ids) != 4 {
		t.Errorf("@ids = %v", ids)
	}
	if len(fc.deletes) != 3 {
		t.Fatalf("want 3 deletes (unknown id skipped), got %d", len(fc.deletes))
	}
	got := map[string]azcosmos.PartitionKey{}
	for _, d := range fc.deletes {
		got[d.id] = d.pk
	}
	if !samePK(got["a"], azcosmos.NewPartitionKeyString("books")) || !samePK(got["b"], azcosmos.NewPartitionKeyNumber(7)) || !samePK(got["c"], azcosmos.NullPartitionKey) {
		t.Errorf("delete pks = %+v", got)
	}
}

func TestDelete_LookupChunkingAndErrors(t *testing.T) {
	s, fc := newTestStore(t, func(o *Options) { o.PartitionKeyField = "category" })
	ids := make([]string, lookupChunk+10)
	for i := range ids {
		ids[i] = fmt.Sprint("id", i)
	}
	if err := s.Delete(context.Background(), ids...); err != nil {
		t.Fatal(err)
	}
	if len(fc.queries) != 2 {
		t.Errorf("want 2 lookup chunks, got %d", len(fc.queries))
	}
	fc.pages = func(queryCall) [][]string { return [][]string{{`{"id":"a","pk":{"x":1}}`}} }
	if err := s.Delete(context.Background(), "a"); err == nil {
		t.Error("want unsupported pk type error")
	}
	fc.pages = func(queryCall) [][]string { return [][]string{{`garbage`}} }
	if err := s.Delete(context.Background(), "a"); err == nil {
		t.Error("want decode error")
	}
	fc.queryErr = errors.New("nope")
	if err := s.Delete(context.Background(), "a"); err == nil {
		t.Error("want lookup error")
	}
}

// --- lifecycle / construction ------------------------------------------

func TestClose(t *testing.T) {
	s, fc := newTestStore(t, nil)
	if err := s.Close(); err != nil {
		t.Fatal(err)
	}
	if err := s.Close(); err != nil {
		t.Fatal(err)
	}
	ctx := context.Background()
	if err := s.Upsert(ctx, vectorstore.Doc{ID: "a", Vector: vectorstore.Vector{1}}); err == nil {
		t.Error("Upsert after Close should fail")
	}
	if _, err := s.Search(ctx, vectorstore.Query{Vector: vectorstore.Vector{1}}); err == nil {
		t.Error("Search after Close should fail")
	}
	if err := s.Delete(ctx, "a"); err == nil {
		t.Error("Delete after Close should fail")
	}
	if len(fc.upserts)+len(fc.deletes)+len(fc.queries) != 0 {
		t.Error("no calls expected after Close")
	}
}

func TestNewWithContainer_Validation(t *testing.T) {
	cases := map[string]Options{
		"static on id":     {PartitionKeyValue: "x"},
		"bad distance":     {DistanceFunction: "manhattan"},
		"vector is id":     {VectorField: "id"},
		"system prop":      {ContentField: "_ts"},
		"dup fields":       {ContentField: "metadata"},
		"pk dup vector":    {PartitionKeyField: "embedding"},
		"nested field":     {MetadataField: "a/b"},
		"nested pk":        {PartitionKeyField: "a/b"},
		"static dup field": {PartitionKeyField: "content", PartitionKeyValue: "x"},
	}
	for name, o := range cases {
		t.Run(name, func(t *testing.T) {
			if _, err := newWithContainer(o, &fakeContainer{}, nil); err == nil {
				t.Error("expected validation error")
			}
		})
	}
}

// emulatorKey is the well-known, public Cosmos DB emulator key.
const emulatorKey = "C2y6yDjf5/R+ob0N8A7Cgv30VRDJIWEHLM+4QDU5DE2nQ9nDuVTqobD4b8mGGyPMbIZnqyMsEcaGQy67XIw/Jw=="

func TestNew_BuildsClients(t *testing.T) {
	keyCfg := azcfg.NewConfig()
	keyCfg.AccountKey = emulatorKey
	keyCfg.SetEndpoint("https://localhost:8081/")

	connCfg := azcfg.NewConfig()
	connCfg.SetConnectionString("AccountEndpoint=https://localhost:8081/;AccountKey=" + emulatorKey + ";")

	tokCfg := azcfg.NewConfig()
	tokCfg.SetCredential(fakeCred{})
	tokCfg.SetAccountName("myacct")

	for name, cfg := range map[string]*azcfg.Config{"key": keyCfg, "connstr": connCfg, "token": tokCfg} {
		t.Run(name, func(t *testing.T) {
			s, err := New(Options{Config: cfg, Database: "db", Container: "c"})
			if err != nil {
				t.Fatalf("New: %v", err)
			}
			if s.owned == nil {
				t.Error("expected owned client")
			}
			if err := s.Close(); err != nil {
				t.Fatal(err)
			}
		})
	}
}

func TestNew_Errors(t *testing.T) {
	cases := map[string]Options{
		"no db":        {Container: "c", Endpoint: "https://x"},
		"no endpoint":  {Database: "d", Container: "c", Config: azcfg.NewConfig()},
		"bad key":      {Database: "d", Container: "c", Endpoint: "https://x", Config: &azcfg.Config{AccountKey: "!!not-base64"}},
		"bad connstr":  {Database: "d", Container: "c", Config: &azcfg.Config{ConnectionString: "garbage"}},
		"bad creds":    {Database: "d", Container: "c", Endpoint: "https://x", Config: &azcfg.Config{ClientSecret: "s"}},
		"bad distance": {Database: "d", Container: "c", Endpoint: "https://x", Config: &azcfg.Config{TokenCredential: fakeCred{}}, DistanceFunction: "x"},
	}
	for name, o := range cases {
		t.Run(name, func(t *testing.T) {
			if _, err := New(o); err == nil {
				t.Error("expected error")
			}
		})
	}
}

func TestNew_CallerOwnedClient(t *testing.T) {
	key, _ := azcosmos.NewKeyCredential(emulatorKey)
	c, err := azcosmos.NewClientWithKey("https://localhost:8081/", key, nil)
	if err != nil {
		t.Fatal(err)
	}
	cc, err := c.NewContainer("db", "c")
	if err != nil {
		t.Fatal(err)
	}
	s, err := New(Options{Client: cc})
	if err != nil {
		t.Fatal(err)
	}
	if s.owned != nil {
		t.Error("caller-owned client must not be owned by the Store")
	}
	_ = s.Close()
}

type fakeCred struct{}

func (fakeCred) GetToken(context.Context, policy.TokenRequestOptions) (azcore.AccessToken, error) {
	return azcore.AccessToken{Token: "t", ExpiresOn: time.Now().Add(time.Hour)}, nil
}

func TestContainerProperties(t *testing.T) {
	s, _ := newTestStore(t, func(o *Options) {
		o.Dimension = 768
		o.PartitionKeyField = "tenant"
		o.PartitionKeyValue = "acme"
		o.DistanceFunction = azcosmos.VectorDistanceFunctionDotProduct
		o.IndexType = azcosmos.VectorIndexTypeDiskANN
	})
	p, err := s.ContainerProperties("vectors")
	if err != nil {
		t.Fatal(err)
	}
	if p.ID != "vectors" || p.PartitionKeyDefinition.Paths[0] != "/tenant" {
		t.Errorf("props = %+v", p)
	}
	ve := p.VectorEmbeddingPolicy.VectorEmbeddings[0]
	if ve.Path != "/embedding" || ve.Dimensions != 768 || ve.DistanceFunction != azcosmos.VectorDistanceFunctionDotProduct || ve.DataType != azcosmos.VectorDataTypeFloat32 {
		t.Errorf("embedding policy = %+v", ve)
	}
	vi := p.IndexingPolicy.VectorIndexes[0]
	if vi.Path != "/embedding" || vi.Type != azcosmos.VectorIndexTypeDiskANN {
		t.Errorf("vector index = %+v", vi)
	}
	found := false
	for _, ex := range p.IndexingPolicy.ExcludedPaths {
		if ex.Path == "/embedding/*" {
			found = true
		}
	}
	if !found {
		t.Errorf("vector path should be excluded from range indexing: %+v", p.IndexingPolicy.ExcludedPaths)
	}
	if _, err := json.Marshal(p); err != nil {
		t.Errorf("marshal: %v", err)
	}

	d, _ := newTestStore(t, nil)
	if p, _ := (&Store{dimension: 3, vectorField: "v", pkField: "id", distance: "cosine", indexType: azcosmos.VectorIndexTypeFlat}).ContainerProperties("x"); p.IndexingPolicy.VectorIndexes[0].Type != azcosmos.VectorIndexTypeFlat {
		t.Errorf("flat index type not applied")
	}
	if _, err := d.ContainerProperties("x"); err == nil {
		t.Error("want Dimension error")
	}
	if _, err := s.ContainerProperties(""); err == nil {
		t.Error("want id error")
	}
	if d.indexType != azcosmos.VectorIndexTypeQuantizedFlat || d.distance != azcosmos.VectorDistanceFunctionCosine {
		t.Errorf("defaults = %s %s", d.indexType, d.distance)
	}
}

func TestPartitionKeyOf(t *testing.T) {
	ok := []any{"s", true, 1.5, float32(2), 3, int32(4), int64(5), json.Number("6"), nil}
	for _, v := range ok {
		if _, err := partitionKeyOf(v); err != nil {
			t.Errorf("partitionKeyOf(%v): %v", v, err)
		}
	}
	for _, v := range []any{json.Number("x"), []string{"a"}, map[string]any{}} {
		if _, err := partitionKeyOf(v); err == nil {
			t.Errorf("partitionKeyOf(%v) should fail", v)
		}
	}
}
