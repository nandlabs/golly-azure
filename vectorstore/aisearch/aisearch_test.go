package aisearch

import (
	"context"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/Azure/azure-sdk-for-go/sdk/azcore"
	"github.com/Azure/azure-sdk-for-go/sdk/azcore/policy"

	"oss.nandlabs.io/golly-azure/azcfg"
	"oss.nandlabs.io/golly/vectorstore"
)

// --- test harness ------------------------------------------------------

type capturedReq struct {
	Method string
	Path   string
	Query  string
	Header http.Header
	Body   []byte
}

type fakeServer struct {
	*httptest.Server
	mu        sync.Mutex
	requests  []capturedReq
	responder func(r capturedReq) (int, string)
}

func newFakeServer(t *testing.T, responder func(r capturedReq) (int, string)) *fakeServer {
	t.Helper()
	if responder == nil {
		responder = okResponder
	}
	fs := &fakeServer{responder: responder}
	fs.Server = httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		body, _ := io.ReadAll(r.Body)
		c := capturedReq{Method: r.Method, Path: r.URL.Path, Query: r.URL.RawQuery, Header: r.Header.Clone(), Body: body}
		fs.mu.Lock()
		fs.requests = append(fs.requests, c)
		fs.mu.Unlock()
		status, resp := fs.responder(c)
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(status)
		_, _ = w.Write([]byte(resp))
	}))
	t.Cleanup(fs.Close)
	return fs
}

func (fs *fakeServer) captured() []capturedReq {
	fs.mu.Lock()
	defer fs.mu.Unlock()
	return append([]capturedReq(nil), fs.requests...)
}

// okResponder echoes every indexing action back as successful.
func okResponder(r capturedReq) (int, string) {
	if strings.HasSuffix(r.Path, "/docs/index") {
		var in struct {
			Value []map[string]any `json:"value"`
		}
		_ = json.Unmarshal(r.Body, &in)
		out := make([]map[string]any, 0, len(in.Value))
		for _, a := range in.Value {
			out = append(out, map[string]any{"key": a["id"], "status": true, "statusCode": 200})
		}
		b, _ := json.Marshal(map[string]any{"value": out})
		return 200, string(b)
	}
	if strings.HasSuffix(r.Path, "/docs/search") {
		return 200, `{"value":[]}`
	}
	return 200, `{}`
}

func (fs *fakeServer) clientOptions() *policy.ClientOptions {
	return &policy.ClientOptions{
		Transport: fs.Client(),
		Retry:     policy.RetryOptions{MaxRetries: -1},
	}
}

func newTestStore(t *testing.T, responder func(r capturedReq) (int, string), mutate func(*Options)) (*Store, *fakeServer) {
	t.Helper()
	fs := newFakeServer(t, responder)
	o := Options{
		Endpoint:      fs.URL,
		Index:         "docs",
		APIKey:        "secret-key",
		ClientOptions: fs.clientOptions(),
	}
	if mutate != nil {
		mutate(&o)
	}
	s, err := New(o)
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	return s, fs
}

func decodeBody(t *testing.T, b []byte) map[string]any {
	t.Helper()
	var m map[string]any
	if err := json.Unmarshal(b, &m); err != nil {
		t.Fatalf("decode body %s: %v", b, err)
	}
	return m
}

type fakeCred struct{ scopes []string }

func (f *fakeCred) GetToken(_ context.Context, o policy.TokenRequestOptions) (azcore.AccessToken, error) {
	f.scopes = o.Scopes
	return azcore.AccessToken{Token: "tok-123", ExpiresOn: time.Now().Add(time.Hour)}, nil
}

// --- tests -------------------------------------------------------------

func TestUpsert_MergeOrUpload(t *testing.T) {
	s, fs := newTestStore(t, nil, nil)
	docs := []vectorstore.Doc{
		{ID: "a", Vector: vectorstore.Vector{1, 2, 3}, Metadata: map[string]any{"source": "wiki"}, Content: "hello"},
		{ID: "b", Vector: vectorstore.Vector{4, 5, 6}},
	}
	if err := s.Upsert(context.Background(), docs...); err != nil {
		t.Fatalf("Upsert: %v", err)
	}
	reqs := fs.captured()
	if len(reqs) != 1 {
		t.Fatalf("want 1 request, got %d", len(reqs))
	}
	r := reqs[0]
	if r.Method != http.MethodPost || r.Path != "/indexes/docs/docs/index" {
		t.Errorf("unexpected request %s %s", r.Method, r.Path)
	}
	if !strings.Contains(r.Query, "api-version="+DefaultAPIVersion) {
		t.Errorf("missing api-version: %q", r.Query)
	}
	if got := r.Header.Get("api-key"); got != "secret-key" {
		t.Errorf("api-key header = %q", got)
	}
	body := decodeBody(t, r.Body)
	vals := body["value"].([]any)
	if len(vals) != 2 {
		t.Fatalf("want 2 actions, got %d", len(vals))
	}
	a0 := vals[0].(map[string]any)
	if a0["@search.action"] != "mergeOrUpload" || a0["id"] != "a" || a0["content"] != "hello" {
		t.Errorf("action0 = %v", a0)
	}
	if md := a0["metadata"].(map[string]any); md["source"] != "wiki" {
		t.Errorf("metadata = %v", md)
	}
	if v := a0["embedding"].([]any); len(v) != 3 || v[2] != float64(3) {
		t.Errorf("embedding = %v", v)
	}
	a1 := vals[1].(map[string]any)
	if _, ok := a1["content"]; ok {
		t.Errorf("empty content must be omitted under mergeOrUpload: %v", a1)
	}
	if _, ok := a1["metadata"]; ok {
		t.Errorf("empty metadata must be omitted: %v", a1)
	}
}

func TestUpsert_UploadAndCustomFields(t *testing.T) {
	s, fs := newTestStore(t, nil, func(o *Options) {
		o.Action = ActionUpload
		o.IDField = "key"
		o.VectorField = "vec"
		o.ContentField = "text"
		o.MetadataField = "meta"
		o.APIVersion = "2025-09-01"
	})
	if err := s.Upsert(context.Background(), vectorstore.Doc{ID: "a", Vector: vectorstore.Vector{1}}); err != nil {
		t.Fatalf("Upsert: %v", err)
	}
	r := fs.captured()[0]
	if !strings.Contains(r.Query, "api-version=2025-09-01") {
		t.Errorf("api-version = %q", r.Query)
	}
	a := decodeBody(t, r.Body)["value"].([]any)[0].(map[string]any)
	if a["@search.action"] != "upload" || a["key"] != "a" || a["vec"] == nil {
		t.Errorf("action = %v", a)
	}
	if c, ok := a["text"]; !ok || c != "" {
		t.Errorf("upload must always send content, got %v", a)
	}
}

func TestUpsert_FlattenMetadata(t *testing.T) {
	s, fs := newTestStore(t, nil, func(o *Options) { o.FlattenMetadata = true })
	err := s.Upsert(context.Background(), vectorstore.Doc{ID: "a", Vector: vectorstore.Vector{1}, Metadata: map[string]any{"lang": "en"}})
	if err != nil {
		t.Fatalf("Upsert: %v", err)
	}
	a := decodeBody(t, fs.captured()[0].Body)["value"].([]any)[0].(map[string]any)
	if a["lang"] != "en" {
		t.Errorf("flattened metadata missing: %v", a)
	}
	if _, ok := a["metadata"]; ok {
		t.Errorf("metadata field should not be written when flattening: %v", a)
	}
	err = s.Upsert(context.Background(), vectorstore.Doc{ID: "a", Vector: vectorstore.Vector{1}, Metadata: map[string]any{"content": "x"}})
	if err == nil {
		t.Error("expected collision error for reserved metadata key")
	}
}

func TestUpsert_Batching(t *testing.T) {
	s, fs := newTestStore(t, nil, func(o *Options) { o.BatchSize = 2 })
	docs := make([]vectorstore.Doc, 5)
	for i := range docs {
		docs[i] = vectorstore.Doc{ID: fmt.Sprintf("d%d", i), Vector: vectorstore.Vector{float32(i)}}
	}
	if err := s.Upsert(context.Background(), docs...); err != nil {
		t.Fatalf("Upsert: %v", err)
	}
	reqs := fs.captured()
	if len(reqs) != 3 {
		t.Fatalf("want 3 batches, got %d", len(reqs))
	}
	for i, want := range []int{2, 2, 1} {
		if got := len(decodeBody(t, reqs[i].Body)["value"].([]any)); got != want {
			t.Errorf("batch %d: want %d, got %d", i, want, got)
		}
	}
}

func TestUpsert_Validation(t *testing.T) {
	s, fs := newTestStore(t, nil, func(o *Options) { o.Dimension = 3 })
	ctx := context.Background()
	if err := s.Upsert(ctx); err != nil {
		t.Errorf("empty upsert: %v", err)
	}
	if err := s.Upsert(ctx, vectorstore.Doc{Vector: vectorstore.Vector{1, 2, 3}}); !errors.Is(err, vectorstore.ErrEmptyID) {
		t.Errorf("want ErrEmptyID, got %v", err)
	}
	if err := s.Upsert(ctx, vectorstore.Doc{ID: "x"}); !errors.Is(err, vectorstore.ErrEmptyVector) {
		t.Errorf("want ErrEmptyVector, got %v", err)
	}
	if err := s.Upsert(ctx, vectorstore.Doc{ID: "x", Vector: vectorstore.Vector{1}}); !errors.Is(err, vectorstore.ErrDimMismatch) {
		t.Errorf("want ErrDimMismatch, got %v", err)
	}
	if n := len(fs.captured()); n != 0 {
		t.Errorf("no requests expected, got %d", n)
	}
}

func TestUpsert_PartialFailure(t *testing.T) {
	s, _ := newTestStore(t, func(r capturedReq) (int, string) {
		return 207, `{"value":[{"key":"a","status":true,"statusCode":200},{"key":"b","status":false,"statusCode":400,"errorMessage":"bad field"}]}`
	}, nil)
	err := s.Upsert(context.Background(),
		vectorstore.Doc{ID: "a", Vector: vectorstore.Vector{1}},
		vectorstore.Doc{ID: "b", Vector: vectorstore.Vector{1}})
	var be *BatchError
	if !errors.As(err, &be) {
		t.Fatalf("want *BatchError, got %v", err)
	}
	if len(be.Failed) != 1 || be.Failed[0].Key != "b" || be.Failed[0].StatusCode != 400 || be.Failed[0].Message != "bad field" {
		t.Errorf("failed = %+v", be.Failed)
	}
	if !strings.Contains(be.Error(), "bad field") {
		t.Errorf("error text = %q", be.Error())
	}
}

func TestBatchError_Truncates(t *testing.T) {
	be := &BatchError{Op: "upsert", Index: "i"}
	for i := 0; i < 5; i++ {
		be.Failed = append(be.Failed, ItemError{Key: fmt.Sprint(i), StatusCode: 400})
	}
	if !strings.Contains(be.Error(), "2 more") {
		t.Errorf("error = %q", be.Error())
	}
}

func TestUpsert_HTTPError(t *testing.T) {
	s, _ := newTestStore(t, func(r capturedReq) (int, string) {
		return 403, `{"error":{"code":"Forbidden","message":"nope"}}`
	}, nil)
	err := s.Upsert(context.Background(), vectorstore.Doc{ID: "a", Vector: vectorstore.Vector{1}})
	var re *azcore.ResponseError
	if !errors.As(err, &re) || re.StatusCode != 403 {
		t.Fatalf("want 403 ResponseError, got %v", err)
	}
}

func TestEncodeKeys_RoundTrip(t *testing.T) {
	id := "s3://bucket/path with spaces.txt"
	enc := base64.RawURLEncoding.EncodeToString([]byte(id))
	s, fs := newTestStore(t, func(r capturedReq) (int, string) {
		if strings.HasSuffix(r.Path, "/docs/search") {
			return 200, `{"value":[{"@search.score":0.5,"id":"` + enc + `"},{"@search.score":0.4,"id":"not*base64"}]}`
		}
		return okResponder(r)
	}, func(o *Options) { o.EncodeKeys = true })
	ctx := context.Background()
	if err := s.Upsert(ctx, vectorstore.Doc{ID: id, Vector: vectorstore.Vector{1}}); err != nil {
		t.Fatal(err)
	}
	if err := s.Delete(ctx, id); err != nil {
		t.Fatal(err)
	}
	reqs := fs.captured()
	for _, r := range reqs {
		a := decodeBody(t, r.Body)["value"].([]any)[0].(map[string]any)
		if a["id"] != enc {
			t.Errorf("key not encoded: %v", a["id"])
		}
	}
	hits, err := s.Search(ctx, vectorstore.Query{Vector: vectorstore.Vector{1}})
	if err != nil {
		t.Fatal(err)
	}
	if hits[0].ID != id || hits[1].ID != "not*base64" {
		t.Errorf("decoded ids = %q, %q", hits[0].ID, hits[1].ID)
	}
}

func TestSearch_VectorQuery(t *testing.T) {
	resp := `{"value":[` +
		`{"@search.score":0.91,"id":"a","content":"hello","metadata":{"source":"wiki","rank":1}},` +
		`{"@search.score":0.52,"id":"b","content":null,"metadata":null}]}`
	s, fs := newTestStore(t, func(r capturedReq) (int, string) { return 200, resp }, nil)
	hits, err := s.Search(context.Background(), vectorstore.Query{Vector: vectorstore.Vector{0.1, 0.2}, TopK: 5})
	if err != nil {
		t.Fatalf("Search: %v", err)
	}
	if len(hits) != 2 {
		t.Fatalf("want 2 hits, got %d", len(hits))
	}
	if hits[0].ID != "a" || hits[0].Score != 0.91 || hits[0].Content != "hello" || hits[0].Metadata["source"] != "wiki" {
		t.Errorf("hit0 = %+v", hits[0])
	}
	if hits[1].ID != "b" || hits[1].Content != "" || hits[1].Metadata != nil {
		t.Errorf("hit1 = %+v", hits[1])
	}
	r := fs.captured()[0]
	if r.Path != "/indexes/docs/docs/search" || r.Method != http.MethodPost {
		t.Errorf("request = %s %s", r.Method, r.Path)
	}
	body := decodeBody(t, r.Body)
	if body["top"] != float64(5) || body["select"] != "id,content,metadata" {
		t.Errorf("body = %v", body)
	}
	if _, ok := body["search"]; ok {
		t.Errorf("pure vector query must not send search text: %v", body)
	}
	if _, ok := body["filter"]; ok {
		t.Errorf("no filter expected: %v", body)
	}
	vq := body["vectorQueries"].([]any)[0].(map[string]any)
	if vq["kind"] != "vector" || vq["fields"] != "embedding" || vq["k"] != float64(5) {
		t.Errorf("vectorQuery = %v", vq)
	}
	if _, ok := vq["exhaustive"]; ok {
		t.Errorf("exhaustive should be omitted by default")
	}
}

func TestSearch_HybridAndFilter(t *testing.T) {
	s, fs := newTestStore(t, nil, func(o *Options) { o.Exhaustive = true })
	_, err := s.Search(context.Background(), vectorstore.Query{
		Vector: vectorstore.Vector{1},
		Filter: map[string]any{
			QueryTextKey: "what is fnord?",
			"source":     "wiki",
			"rank":       map[string]any{"$gte": 2},
			RawFilterKey: "search.ismatch('x')",
		},
	})
	if err != nil {
		t.Fatalf("Search: %v", err)
	}
	body := decodeBody(t, fs.captured()[0].Body)
	if body["search"] != "what is fnord?" {
		t.Errorf("search = %v", body["search"])
	}
	want := "(search.ismatch('x')) and metadata/rank ge 2 and metadata/source eq 'wiki'"
	if body["filter"] != want {
		t.Errorf("filter = %q, want %q", body["filter"], want)
	}
	if body["vectorFilterMode"] != "preFilter" {
		t.Errorf("vectorFilterMode = %v", body["vectorFilterMode"])
	}
	if body["top"] != float64(10) {
		t.Errorf("default top = %v", body["top"])
	}
	vq := body["vectorQueries"].([]any)[0].(map[string]any)
	if vq["exhaustive"] != true {
		t.Errorf("exhaustive = %v", vq["exhaustive"])
	}
}

func TestSearch_FlattenedHits(t *testing.T) {
	resp := `{"value":[{"@search.score":1.5,"@search.rerankerScore":2,"id":"a","content":"c","embedding":[1,2],"lang":"en","tags":["x"]}]}`
	s, fs := newTestStore(t, func(r capturedReq) (int, string) { return 200, resp }, func(o *Options) { o.FlattenMetadata = true })
	hits, err := s.Search(context.Background(), vectorstore.Query{Vector: vectorstore.Vector{1}, Filter: map[string]any{"lang": "en"}})
	if err != nil {
		t.Fatal(err)
	}
	h := hits[0]
	if len(h.Metadata) != 2 || h.Metadata["lang"] != "en" {
		t.Errorf("metadata = %v", h.Metadata)
	}
	body := decodeBody(t, fs.captured()[0].Body)
	if body["filter"] != "lang eq 'en'" {
		t.Errorf("filter = %v", body["filter"])
	}
	if _, ok := body["select"]; ok {
		t.Errorf("select should be omitted when flattening")
	}
}

func TestSearch_MinScoreAndErrors(t *testing.T) {
	resp := `{"value":[{"@search.score":0.9,"id":"a"},{"@search.score":0.1,"id":"b"}]}`
	s, _ := newTestStore(t, func(r capturedReq) (int, string) { return 200, resp }, func(o *Options) { o.Dimension = 1 })
	ctx := context.Background()
	hits, err := s.Search(ctx, vectorstore.Query{Vector: vectorstore.Vector{1}, MinScore: 0.5})
	if err != nil || len(hits) != 1 || hits[0].ID != "a" {
		t.Fatalf("hits=%v err=%v", hits, err)
	}
	if _, err := s.Search(ctx, vectorstore.Query{}); !errors.Is(err, vectorstore.ErrEmptyVector) {
		t.Errorf("want ErrEmptyVector, got %v", err)
	}
	if _, err := s.Search(ctx, vectorstore.Query{Vector: vectorstore.Vector{1, 2}}); !errors.Is(err, vectorstore.ErrDimMismatch) {
		t.Errorf("want ErrDimMismatch, got %v", err)
	}
	if _, err := s.Search(ctx, vectorstore.Query{Vector: vectorstore.Vector{1}, Filter: map[string]any{QueryTextKey: 1}}); !errors.Is(err, ErrInvalidFilter) {
		t.Errorf("want ErrInvalidFilter for non-string text, got %v", err)
	}
	if _, err := s.Search(ctx, vectorstore.Query{Vector: vectorstore.Vector{1}, Filter: map[string]any{"bad key": 1}}); !errors.Is(err, ErrInvalidFilter) {
		t.Errorf("want ErrInvalidFilter for bad key, got %v", err)
	}
}

func TestSearch_BadResponses(t *testing.T) {
	cases := map[string]string{
		"score":    `{"value":[{"@search.score":"x","id":"a"}]}`,
		"id":       `{"value":[{"@search.score":1,"id":5}]}`,
		"content":  `{"value":[{"id":"a","content":5}]}`,
		"metadata": `{"value":[{"id":"a","metadata":"str"}]}`,
		"json":     `not json`,
	}
	for name, resp := range cases {
		t.Run(name, func(t *testing.T) {
			s, _ := newTestStore(t, func(r capturedReq) (int, string) { return 200, resp }, nil)
			if _, err := s.Search(context.Background(), vectorstore.Query{Vector: vectorstore.Vector{1}}); err == nil {
				t.Error("expected decode error")
			}
		})
	}
}

func TestSearch_NotFound(t *testing.T) {
	s, _ := newTestStore(t, func(r capturedReq) (int, string) {
		return 404, `{"error":{"code":"ResourceNotFound","message":"index missing"}}`
	}, nil)
	_, err := s.Search(context.Background(), vectorstore.Query{Vector: vectorstore.Vector{1}})
	if !IsNotFound(err) {
		t.Errorf("want IsNotFound, got %v", err)
	}
	if IsNotFound(errors.New("x")) {
		t.Error("IsNotFound(plain error) should be false")
	}
}

func TestDelete(t *testing.T) {
	s, fs := newTestStore(t, func(r capturedReq) (int, string) {
		return 207, `{"value":[{"key":"a","status":true,"statusCode":200},{"key":"zz","status":false,"statusCode":404}]}`
	}, nil)
	if err := s.Delete(context.Background(), "a", "zz"); err != nil {
		t.Fatalf("Delete: %v", err)
	}
	vals := decodeBody(t, fs.captured()[0].Body)["value"].([]any)
	if len(vals) != 2 {
		t.Fatalf("want 2 actions, got %d", len(vals))
	}
	a := vals[0].(map[string]any)
	if a["@search.action"] != "delete" || a["id"] != "a" || len(a) != 2 {
		t.Errorf("delete action = %v", a)
	}
	if err := s.Delete(context.Background()); err != nil {
		t.Errorf("empty delete: %v", err)
	}
	if err := s.Delete(context.Background(), ""); !errors.Is(err, vectorstore.ErrEmptyID) {
		t.Errorf("want ErrEmptyID, got %v", err)
	}
	if n := len(fs.captured()); n != 1 {
		t.Errorf("want 1 request total, got %d", n)
	}
}

func TestClose(t *testing.T) {
	s, fs := newTestStore(t, nil, nil)
	if err := s.Close(); err != nil {
		t.Fatal(err)
	}
	if err := s.Close(); err != nil {
		t.Fatal(err)
	}
	ctx := context.Background()
	if err := s.Upsert(ctx, vectorstore.Doc{ID: "x", Vector: vectorstore.Vector{1}}); err == nil {
		t.Error("Upsert after Close should fail")
	}
	if _, err := s.Search(ctx, vectorstore.Query{Vector: vectorstore.Vector{1}}); err == nil {
		t.Error("Search after Close should fail")
	}
	if err := s.Delete(ctx, "x"); err == nil {
		t.Error("Delete after Close should fail")
	}
	if err := s.CreateIndex(ctx); err == nil {
		t.Error("CreateIndex after Close should fail")
	}
	if err := s.DeleteIndex(ctx); err == nil {
		t.Error("DeleteIndex after Close should fail")
	}
	if n := len(fs.captured()); n != 0 {
		t.Errorf("no requests expected after close, got %d", n)
	}
}

func TestBearerTokenAuth(t *testing.T) {
	fs := newFakeServer(t, nil)
	cred := &fakeCred{}
	cfg := azcfg.NewConfig()
	cfg.SetCredential(cred)
	cfg.SetEndpoint(fs.URL)
	s, err := New(Options{Index: "docs", Config: cfg, ClientOptions: fs.clientOptions()})
	if err != nil {
		t.Fatal(err)
	}
	if err := s.Delete(context.Background(), "a"); err != nil {
		t.Fatalf("Delete: %v", err)
	}
	r := fs.captured()[0]
	if got := r.Header.Get("Authorization"); got != "Bearer tok-123" {
		t.Errorf("Authorization = %q", got)
	}
	if r.Header.Get("api-key") != "" {
		t.Error("api-key must not be sent with bearer auth")
	}
	if len(cred.scopes) != 1 || cred.scopes[0] != DefaultScope {
		t.Errorf("scopes = %v", cred.scopes)
	}
}

func TestAccountKeyAndNameFallback(t *testing.T) {
	cfg := azcfg.NewConfig()
	cfg.SetAccountName("mysvc")
	cfg.AccountKey = "k"
	s, err := New(Options{Index: "docs", Config: cfg})
	if err != nil {
		t.Fatal(err)
	}
	if s.endpoint != "https://mysvc.search.windows.net" {
		t.Errorf("endpoint = %q", s.endpoint)
	}
}

func TestNew_Validation(t *testing.T) {
	cases := map[string]Options{
		"no index":      {Endpoint: "https://x.search.windows.net", APIKey: "k"},
		"no endpoint":   {Index: "i", APIKey: "k", Config: azcfg.NewConfig()},
		"bad endpoint":  {Index: "i", APIKey: "k", Endpoint: "::nope"},
		"bad action":    {Index: "i", APIKey: "k", Endpoint: "https://x", Action: "merge"},
		"bad metric":    {Index: "i", APIKey: "k", Endpoint: "https://x", Metric: "hamming2"},
		"bad field":     {Index: "i", APIKey: "k", Endpoint: "https://x", VectorField: "a b"},
		"nested field":  {Index: "i", APIKey: "k", Endpoint: "https://x", IDField: "a/b"},
		"dup field":     {Index: "i", APIKey: "k", Endpoint: "https://x", VectorField: "content"},
		"partial creds": {Index: "i", Endpoint: "https://x", Config: &azcfg.Config{ClientSecret: "s"}},
	}
	for name, o := range cases {
		t.Run(name, func(t *testing.T) {
			if _, err := New(o); err == nil {
				t.Error("expected error")
			}
		})
	}
	// FlattenMetadata ignores MetadataField collisions.
	if _, err := New(Options{Index: "i", APIKey: "k", Endpoint: "https://x", FlattenMetadata: true, MetadataField: "content"}); err != nil {
		t.Errorf("flatten: %v", err)
	}
}

func TestCreateIndex(t *testing.T) {
	var body map[string]any
	s, fs := newTestStore(t, func(r capturedReq) (int, string) {
		_ = json.Unmarshal(r.Body, &body)
		return 201, `{}`
	}, func(o *Options) { o.Dimension = 3; o.Metric = MetricDotProduct })
	err := s.CreateIndex(context.Background(),
		MetadataField{Name: "source"},
		MetadataField{Name: "rank", Type: EdmInt64, Sortable: true, Facetable: true, Searchable: true})
	if err != nil {
		t.Fatalf("CreateIndex: %v", err)
	}
	r := fs.captured()[0]
	if r.Method != http.MethodPut || r.Path != "/indexes/docs" {
		t.Errorf("request = %s %s", r.Method, r.Path)
	}
	fields := body["fields"].([]any)
	if len(fields) != 4 {
		t.Fatalf("want 4 fields, got %d: %v", len(fields), fields)
	}
	if f := fields[0].(map[string]any); f["name"] != "id" || f["key"] != true {
		t.Errorf("key field = %v", f)
	}
	if f := fields[2].(map[string]any); f["dimensions"] != float64(3) || f["vectorSearchProfile"] != vectorProfileName {
		t.Errorf("vector field = %v", f)
	}
	md := fields[3].(map[string]any)
	if md["type"] != "Edm.ComplexType" || len(md["fields"].([]any)) != 2 {
		t.Errorf("metadata field = %v", md)
	}
	sub := md["fields"].([]any)
	if f := sub[0].(map[string]any); f["type"] != EdmString || f["filterable"] != true {
		t.Errorf("default sub-field = %v", f)
	}
	if f := sub[1].(map[string]any); f["filterable"] != false || f["sortable"] != true || f["facetable"] != true || f["searchable"] != true {
		t.Errorf("rank sub-field = %v", f)
	}
	alg := body["vectorSearch"].(map[string]any)["algorithms"].([]any)[0].(map[string]any)
	if alg["kind"] != "hnsw" || alg["hnswParameters"].(map[string]any)["metric"] != "dotProduct" {
		t.Errorf("algorithm = %v", alg)
	}
}

func TestCreateIndex_FlattenAndErrors(t *testing.T) {
	var body map[string]any
	s, _ := newTestStore(t, func(r capturedReq) (int, string) {
		_ = json.Unmarshal(r.Body, &body)
		return 200, `{}`
	}, func(o *Options) { o.Dimension = 2; o.FlattenMetadata = true })
	ctx := context.Background()
	if err := s.CreateIndex(ctx, MetadataField{Name: "lang"}); err != nil {
		t.Fatal(err)
	}
	if fields := body["fields"].([]any); len(fields) != 4 || fields[3].(map[string]any)["name"] != "lang" {
		t.Errorf("fields = %v", fields)
	}
	if err := s.CreateIndex(ctx, MetadataField{Name: "content"}); err == nil {
		t.Error("expected reserved collision")
	}
	if err := s.CreateIndex(ctx, MetadataField{Name: "a-b"}); err == nil {
		t.Error("expected invalid name")
	}

	// No metadata sub-fields -> no complex field; no dimension -> error.
	s2, _ := newTestStore(t, func(r capturedReq) (int, string) {
		_ = json.Unmarshal(r.Body, &body)
		return 204, ``
	}, func(o *Options) { o.Dimension = 2 })
	if err := s2.CreateIndex(ctx); err != nil {
		t.Fatal(err)
	}
	if fields := body["fields"].([]any); len(fields) != 3 {
		t.Errorf("fields = %v", fields)
	}
	s3, _ := newTestStore(t, nil, nil)
	if err := s3.CreateIndex(ctx); err == nil {
		t.Error("expected Dimension error")
	}
	s4, _ := newTestStore(t, func(r capturedReq) (int, string) { return 400, `{"error":{"message":"bad"}}` }, func(o *Options) { o.Dimension = 2 })
	if err := s4.CreateIndex(ctx); err == nil {
		t.Error("expected HTTP error")
	}
}

func TestDeleteIndex(t *testing.T) {
	status := 204
	s, fs := newTestStore(t, func(r capturedReq) (int, string) { return status, `` }, nil)
	ctx := context.Background()
	if err := s.DeleteIndex(ctx); err != nil {
		t.Fatal(err)
	}
	if r := fs.captured()[0]; r.Method != http.MethodDelete || r.Path != "/indexes/docs" {
		t.Errorf("request = %s %s", r.Method, r.Path)
	}
	status = 404
	if err := s.DeleteIndex(ctx); err != nil {
		t.Errorf("404 should be tolerated: %v", err)
	}
	status = 500
	if err := s.DeleteIndex(ctx); err == nil {
		t.Error("expected error on 500")
	}
}

func TestContextCancelled(t *testing.T) {
	s, _ := newTestStore(t, nil, nil)
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	if _, err := s.Search(ctx, vectorstore.Query{Vector: vectorstore.Vector{1}}); err == nil {
		t.Error("expected context error")
	}
}

var _ vectorstore.Store = (*Store)(nil)
