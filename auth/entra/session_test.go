package entra

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"net/url"
	"regexp"
	"sort"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/Azure/azure-sdk-for-go/sdk/azcore"
	"github.com/Azure/azure-sdk-for-go/sdk/azcore/policy"
	"github.com/Azure/azure-sdk-for-go/sdk/data/aztables"
	"oss.nandlabs.io/golly-azure/azcfg"
	"oss.nandlabs.io/golly/auth"
)

var errBoom = errors.New("boom")

// memTable is an in-memory tableAPI that stores the raw entity JSON
// produced by encodeSession, and evaluates the two filter shapes the
// store issues.
type memTable struct {
	mu      sync.Mutex
	rows    map[string][]byte // pk|rk -> entity json
	failOn  string
	queries []string
	deletes int
}

func newMemTable() *memTable { return &memTable{rows: map[string][]byte{}} }

func (m *memTable) fail(op string) error {
	if m.failOn == op {
		return errBoom
	}
	return nil
}

func (m *memTable) getEntity(_ context.Context, pk, rk string) ([]byte, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	if err := m.fail("get"); err != nil {
		return nil, err
	}
	b, ok := m.rows[pk+"|"+rk]
	if !ok {
		return nil, errEntityNotFound
	}
	return append([]byte(nil), b...), nil
}

func (m *memTable) upsertEntity(_ context.Context, entity []byte) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	if err := m.fail("upsert"); err != nil {
		return err
	}
	var keys struct{ PartitionKey, RowKey string }
	if err := json.Unmarshal(entity, &keys); err != nil {
		return err
	}
	m.rows[keys.PartitionKey+"|"+keys.RowKey] = append([]byte(nil), entity...)
	return nil
}

func (m *memTable) deleteEntity(_ context.Context, pk, rk string) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	if err := m.fail("delete"); err != nil {
		return err
	}
	m.deletes++
	delete(m.rows, pk+"|"+rk)
	return nil
}

var (
	userFilterRe = regexp.MustCompile(`^UserID eq '((?:[^']|'')*)' and RowKey eq 'session'$`)
	expFilterRe  = regexp.MustCompile(`^ExpiresAt lt datetime'([^']+)' and RowKey eq 'session'$`)
)

func (m *memTable) queryEntities(_ context.Context, filter string) ([][]byte, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.queries = append(m.queries, filter)
	if err := m.fail("query"); err != nil {
		return nil, err
	}
	var match func(map[string]any) bool
	if g := userFilterRe.FindStringSubmatch(filter); g != nil {
		uid := strings.ReplaceAll(g[1], "''", "'")
		match = func(e map[string]any) bool { return e["UserID"] == uid }
	} else if g := expFilterRe.FindStringSubmatch(filter); g != nil {
		cutoff, err := time.Parse(time.RFC3339Nano, g[1])
		if err != nil {
			return nil, err
		}
		match = func(e map[string]any) bool {
			s, ok := e["ExpiresAt"].(string)
			if !ok {
				return false
			}
			t, err := time.Parse(time.RFC3339Nano, s)
			return err == nil && t.Before(cutoff)
		}
	} else {
		return nil, errors.New("unexpected filter: " + filter)
	}
	keys := make([]string, 0, len(m.rows))
	for k := range m.rows {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	var out [][]byte
	for _, k := range keys {
		var e map[string]any
		_ = json.Unmarshal(m.rows[k], &e)
		if match(e) {
			out = append(out, m.rows[k])
		}
	}
	return out, nil
}

func (m *memTable) createTable(context.Context) error { return m.fail("create") }

func newSession(id, sub string, exp time.Time) *auth.Session {
	now := time.Now().UTC().Truncate(time.Second)
	return &auth.Session{
		ID: id, Subject: sub, CreatedAt: now, LastSeen: now, ExpiresAt: exp,
		Data: map[string]any{"k": "v", "n": float64(3)},
	}
}

func TestTableSessionStore_PutGetDelete(t *testing.T) {
	mt := newMemTable()
	s := newSessionStoreWithBackend(mt).WithTimeout(time.Second)
	exp := time.Now().Add(time.Hour).UTC().Truncate(time.Second)
	in := newSession("sid-1", "user-1", exp)
	if err := s.Put(in); err != nil {
		t.Fatalf("Put: %v", err)
	}
	got, ok := s.Get("sid-1")
	if !ok {
		t.Fatal("Get: not found")
	}
	if got.Subject != "user-1" || !got.ExpiresAt.Equal(exp) || got.Data["k"] != "v" {
		t.Fatalf("round trip mismatch: %+v", got)
	}
	// Mirrored properties carry EDM annotations.
	var raw map[string]any
	_ = json.Unmarshal(mt.rows["sid-1|session"], &raw)
	if raw["UserID"] != "user-1" || raw["ExpiresAt@odata.type"] != "Edm.DateTime" || raw["CreatedAt@odata.type"] != "Edm.DateTime" {
		t.Fatalf("entity shape: %v", raw)
	}
	if err := s.Delete("sid-1"); err != nil {
		t.Fatal(err)
	}
	if _, ok := s.Get("sid-1"); ok {
		t.Fatal("Get after Delete: found")
	}
	if err := s.Delete("sid-1"); err != nil {
		t.Fatalf("idempotent Delete: %v", err)
	}
}

func TestTableSessionStore_WorksWithSessionManager(t *testing.T) {
	s := newSessionStoreWithBackend(newMemTable())
	sm := auth.NewSessionManager(s, auth.SessionConfig{Absolute: time.Hour, Sliding: 10 * time.Minute})
	sess, err := sm.New("user-9", map[string]any{"role": "admin"})
	if err != nil {
		t.Fatal(err)
	}
	if _, ok := sm.Validate(sess.ID); !ok {
		t.Fatal("Validate")
	}
	if err := sm.Touch(sess.ID); err != nil {
		t.Fatal(err)
	}
	if err := sm.Revoke(sess.ID); err != nil {
		t.Fatal(err)
	}
	if _, ok := sm.Validate(sess.ID); ok {
		t.Fatal("revoked session still valid")
	}
}

func TestTableSessionStore_TTL(t *testing.T) {
	mt := newMemTable()
	now := time.Now()
	s := newSessionStoreWithBackend(mt).WithClock(func() time.Time { return now })
	_ = s.Put(newSession("live", "u", now.Add(time.Hour)))
	_ = s.Put(newSession("dead", "u", now.Add(-time.Minute)))
	_ = s.Put(newSession("forever", "u", time.Time{}))

	if _, ok := s.Get("dead"); ok {
		t.Fatal("expired session returned")
	}
	if _, ok := mt.rows["dead|session"]; ok {
		t.Fatal("expired session should be deleted on Get")
	}
	if _, ok := s.Get("forever"); !ok {
		t.Fatal("session without expiry should be returned")
	}

	_ = s.Put(newSession("dead2", "u", now.Add(-time.Second)))
	_ = s.Put(newSession("dead3", "v", now.Add(-time.Hour)))
	list, err := s.ListByUser(context.Background(), "u")
	if err != nil {
		t.Fatal(err)
	}
	ids := []string{}
	for _, x := range list {
		ids = append(ids, x.ID)
	}
	sort.Strings(ids)
	if strings.Join(ids, ",") != "forever,live" {
		t.Fatalf("ListByUser = %v", ids)
	}

	n, err := s.PurgeExpired(context.Background())
	if err != nil || n != 2 {
		t.Fatalf("PurgeExpired = %d, %v", n, err)
	}
	if len(mt.rows) != 2 {
		t.Fatalf("rows left = %d", len(mt.rows))
	}
}

func TestTableSessionStore_ListByUser_EscapesQuotes(t *testing.T) {
	mt := newMemTable()
	s := newSessionStoreWithBackend(mt)
	_ = s.Put(newSession("a", "o'brien", time.Now().Add(time.Hour)))
	_ = s.Put(newSession("b", "other", time.Now().Add(time.Hour)))
	list, err := s.ListByUser(context.Background(), "o'brien")
	if err != nil {
		t.Fatal(err)
	}
	if len(list) != 1 || list[0].ID != "a" {
		t.Fatalf("ListByUser = %v", list)
	}
	if !strings.Contains(mt.queries[len(mt.queries)-1], "'o''brien'") {
		t.Fatalf("filter not escaped: %s", mt.queries[len(mt.queries)-1])
	}
	if _, err := s.ListByUser(context.Background(), ""); err == nil {
		t.Fatal("empty uid should error")
	}
}

func TestTableSessionStore_Errors(t *testing.T) {
	mt := newMemTable()
	s := newSessionStoreWithBackend(mt)
	if err := s.Put(nil); !errors.Is(err, ErrInvalidSessionID) {
		t.Errorf("nil: %v", err)
	}
	for _, id := range []string{"", "a/b", `a\b`, "a#b", "a?b", "a'b", "a\x01b", strings.Repeat("x", 1025)} {
		if err := s.Put(newSession(id, "u", time.Time{})); !errors.Is(err, ErrInvalidSessionID) {
			t.Errorf("Put(%q): %v", id, err)
		}
		if err := s.Delete(id); !errors.Is(err, ErrInvalidSessionID) {
			t.Errorf("Delete(%q): %v", id, err)
		}
		if _, ok := s.Get(id); ok {
			t.Errorf("Get(%q) ok", id)
		}
	}
	big := newSession("big", "u", time.Time{})
	big.Data = map[string]any{"blob": strings.Repeat("x", 40*1024)}
	if err := s.Put(big); !errors.Is(err, ErrSessionTooLarge) {
		t.Errorf("big: %v", err)
	}
	bad := newSession("bad", "u", time.Time{})
	bad.Data = map[string]any{"ch": make(chan int)}
	if err := s.Put(bad); err == nil {
		t.Error("unmarshalable data should error")
	}

	_ = s.Put(newSession("ok", "u", time.Time{}))
	mt.failOn = "get"
	if _, ok := s.Get("ok"); ok {
		t.Error("get failure should report not found")
	}
	mt.failOn = "upsert"
	if err := s.Put(newSession("x", "u", time.Time{})); !errors.Is(err, errBoom) {
		t.Errorf("upsert: %v", err)
	}
	mt.failOn = "delete"
	if err := s.Delete("ok"); !errors.Is(err, errBoom) {
		t.Errorf("delete: %v", err)
	}
	mt.failOn = "query"
	if _, err := s.ListByUser(context.Background(), "u"); !errors.Is(err, errBoom) {
		t.Errorf("query: %v", err)
	}
	if _, err := s.PurgeExpired(context.Background()); !errors.Is(err, errBoom) {
		t.Errorf("purge: %v", err)
	}
	mt.failOn = "create"
	if err := s.CreateTableIfNotExists(context.Background()); !errors.Is(err, errBoom) {
		t.Errorf("create: %v", err)
	}
	mt.failOn = ""

	// Corrupt rows.
	mt.rows["c1|session"] = []byte(`{"PartitionKey":"c1","RowKey":"session","UserID":"z"}`)
	mt.rows["c2|session"] = []byte(`{"PartitionKey":"c2","RowKey":"session","UserID":"y","Payload":5}`)
	mt.rows["c3|session"] = []byte(`{"PartitionKey":"c3","RowKey":"session","UserID":"w","Payload":"{"}`)
	mt.rows["c4|session"] = []byte(`not json`)
	for _, id := range []string{"c1", "c2", "c3", "c4"} {
		if _, ok := s.Get(id); ok {
			t.Errorf("corrupt %s returned", id)
		}
	}
	if _, err := s.ListByUser(context.Background(), "z"); err == nil {
		t.Error("corrupt row in ListByUser should error")
	}
	mt.failOn = "delete"
	mt.rows["e|session"] = mustEncode(t, newSession("e", "u", time.Now().Add(-time.Hour)))
	if _, err := s.PurgeExpired(context.Background()); !errors.Is(err, errBoom) {
		t.Errorf("purge delete failure: %v", err)
	}
}

func mustEncode(t *testing.T, s *auth.Session) []byte {
	t.Helper()
	b, err := encodeSession(s)
	if err != nil {
		t.Fatal(err)
	}
	return b
}

func TestNewTableSessionStore_Panics(t *testing.T) {
	defer func() {
		if recover() == nil {
			t.Fatal("expected panic")
		}
	}()
	NewTableSessionStore(nil)
}

// fakeTableService is a tiny Table Storage REST emulator sufficient to
// exercise the azTable adapter end-to-end through the real aztables
// client.
type fakeTableService struct {
	mu       sync.Mutex
	entities map[string]map[string]any // "pk|rk" -> entity
	tables   map[string]bool
}

var entityPathRe = regexp.MustCompile(`^/([A-Za-z0-9]+)\(PartitionKey='((?:[^']|'')*)',RowKey='((?:[^']|'')*)'\)$`)

func (f *fakeTableService) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	f.mu.Lock()
	defer f.mu.Unlock()
	path, _ := url.PathUnescape(r.URL.EscapedPath())
	w.Header().Set("Content-Type", "application/json;odata=minimalmetadata")
	writeErr := func(status int, code string) {
		w.Header().Set("x-ms-error-code", code)
		w.WriteHeader(status)
		_, _ = w.Write([]byte(`{"odata.error":{"code":"` + code + `","message":{"value":"x"}}}`))
	}
	switch {
	case path == "/Tables" && r.Method == http.MethodPost:
		var body struct{ TableName string }
		_ = json.NewDecoder(r.Body).Decode(&body)
		if f.tables[body.TableName] {
			writeErr(http.StatusConflict, "TableAlreadyExists")
			return
		}
		f.tables[body.TableName] = true
		w.WriteHeader(http.StatusNoContent)
	case strings.HasSuffix(path, "()") && r.Method == http.MethodGet:
		filter := r.URL.Query().Get("$filter")
		var vals []map[string]any
		keys := make([]string, 0, len(f.entities))
		for k := range f.entities {
			keys = append(keys, k)
		}
		sort.Strings(keys)
		for _, k := range keys {
			e := f.entities[k]
			if g := userFilterRe.FindStringSubmatch(filter); g != nil && e["UserID"] != strings.ReplaceAll(g[1], "''", "'") {
				continue
			}
			vals = append(vals, e)
		}
		_ = json.NewEncoder(w).Encode(map[string]any{"value": vals})
	default:
		m := entityPathRe.FindStringSubmatch(path)
		if m == nil {
			writeErr(http.StatusBadRequest, "InvalidInput")
			return
		}
		key := strings.ReplaceAll(m[2], "''", "'") + "|" + strings.ReplaceAll(m[3], "''", "'")
		switch r.Method {
		case http.MethodGet:
			e, ok := f.entities[key]
			if !ok {
				writeErr(http.StatusNotFound, "ResourceNotFound")
				return
			}
			_ = json.NewEncoder(w).Encode(e)
		case http.MethodPut, "MERGE", http.MethodPatch:
			b, _ := io.ReadAll(r.Body)
			var e map[string]any
			if err := json.Unmarshal(b, &e); err != nil {
				writeErr(http.StatusBadRequest, "InvalidInput")
				return
			}
			f.entities[key] = e
			w.Header().Set("ETag", `W/"1"`)
			w.WriteHeader(http.StatusNoContent)
		case http.MethodDelete:
			if _, ok := f.entities[key]; !ok {
				writeErr(http.StatusNotFound, "ResourceNotFound")
				return
			}
			delete(f.entities, key)
			w.WriteHeader(http.StatusNoContent)
		default:
			writeErr(http.StatusMethodNotAllowed, "x")
		}
	}
}

func newFakeTableClient(t *testing.T) (*aztables.Client, *fakeTableService) {
	t.Helper()
	f := &fakeTableService{entities: map[string]map[string]any{}, tables: map[string]bool{}}
	srv := httptest.NewServer(f)
	t.Cleanup(srv.Close)
	c, err := aztables.NewClientWithNoCredential(srv.URL+"/sessions", &aztables.ClientOptions{
		ClientOptions: azcore.ClientOptions{Retry: policy.RetryOptions{MaxRetries: -1}},
	})
	if err != nil {
		t.Fatal(err)
	}
	return c, f
}

func TestTableSessionStore_AzTablesAdapter(t *testing.T) {
	c, _ := newFakeTableClient(t)
	s := NewTableSessionStore(c)
	ctx := context.Background()
	if err := s.CreateTableIfNotExists(ctx); err != nil {
		t.Fatalf("create: %v", err)
	}
	if err := s.CreateTableIfNotExists(ctx); err != nil {
		t.Fatalf("create (exists): %v", err)
	}
	exp := time.Now().Add(time.Hour).UTC().Truncate(time.Second)
	if err := s.Put(newSession("sid-a", "alice", exp)); err != nil {
		t.Fatalf("Put: %v", err)
	}
	_ = s.Put(newSession("sid-b", "bob", exp))
	got, ok := s.Get("sid-a")
	if !ok || got.Subject != "alice" || !got.ExpiresAt.Equal(exp) {
		t.Fatalf("Get = %+v, %v", got, ok)
	}
	if _, ok := s.Get("missing"); ok {
		t.Fatal("missing found")
	}
	list, err := s.ListByUser(ctx, "alice")
	if err != nil || len(list) != 1 || list[0].ID != "sid-a" {
		t.Fatalf("ListByUser = %v, %v", list, err)
	}
	if err := s.Delete("sid-a"); err != nil {
		t.Fatal(err)
	}
	if err := s.Delete("sid-a"); err != nil {
		t.Fatalf("delete missing: %v", err)
	}
}

func TestTableSessionStore_AzTablesAdapter_Errors(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("x-ms-error-code", "InternalError")
		w.WriteHeader(http.StatusInternalServerError)
	}))
	defer srv.Close()
	c, err := aztables.NewClientWithNoCredential(srv.URL+"/sessions", &aztables.ClientOptions{
		ClientOptions: azcore.ClientOptions{Retry: policy.RetryOptions{MaxRetries: -1}},
	})
	if err != nil {
		t.Fatal(err)
	}
	a := &azTable{c: c}
	ctx := context.Background()
	if _, err := a.getEntity(ctx, "a", "b"); err == nil || errors.Is(err, errEntityNotFound) {
		t.Errorf("get: %v", err)
	}
	if err := a.deleteEntity(ctx, "a", "b"); err == nil {
		t.Error("delete should fail")
	}
	if _, err := a.queryEntities(ctx, "x"); err == nil {
		t.Error("query should fail")
	}
	if err := a.createTable(ctx); err == nil {
		t.Error("create should fail")
	}
}

func TestNewTableSessionStoreFromConfig(t *testing.T) {
	if _, err := NewTableSessionStoreFromConfig(nil, "t"); err == nil {
		t.Error("nil cfg")
	}
	if _, err := NewTableSessionStoreFromConfig(azcfg.NewConfig(), ""); err == nil {
		t.Error("empty table")
	}
	if _, err := NewTableSessionStoreFromConfig(azcfg.NewConfig(), "t"); err == nil {
		t.Error("no account info")
	}

	cs := azcfg.NewConfig()
	cs.SetConnectionString("DefaultEndpointsProtocol=http;AccountName=devstoreaccount1;AccountKey=Eby8vdM02xNOcqFlqUwJPLlmEtlCDXJ1OUzFT50uSRZ6IFsuFq2UVErCz4I6tq/K1SZFPTOtr/KBHBeksoGMGw==;TableEndpoint=http://127.0.0.1:10002/devstoreaccount1;")
	if s, err := NewTableSessionStoreFromConfig(cs, "sessions"); err != nil || s == nil {
		t.Errorf("connection string: %v", err)
	}
	badCS := azcfg.NewConfig()
	badCS.SetConnectionString("garbage")
	if _, err := NewTableSessionStoreFromConfig(badCS, "sessions"); err == nil {
		t.Error("bad connection string should fail")
	}

	sk := azcfg.NewConfig()
	sk.SetSharedKey("acct", "Eby8vdM02xNOcqFlqUwJPLlmEtlCDXJ1OUzFT50uSRZ6IFsuFq2UVErCz4I6tq/K1SZFPTOtr/KBHBeksoGMGw==")
	if s, err := NewTableSessionStoreFromConfig(sk, "sessions"); err != nil || s == nil {
		t.Errorf("shared key: %v", err)
	}
	badKey := azcfg.NewConfig()
	badKey.SetSharedKey("acct", "%%%not-base64")
	if _, err := NewTableSessionStoreFromConfig(badKey, "sessions"); err == nil {
		t.Error("bad shared key should fail")
	}

	tc := azcfg.NewConfig()
	tc.SetEndpoint("https://acct.table.core.windows.net/")
	tc.SetCredential(staticCred{})
	if s, err := NewTableSessionStoreFromConfig(tc, "sessions"); err != nil || s == nil {
		t.Errorf("token credential: %v", err)
	}
}

type staticCred struct{}

func (staticCred) GetToken(context.Context, policy.TokenRequestOptions) (azcore.AccessToken, error) {
	return azcore.AccessToken{Token: "t", ExpiresOn: time.Now().Add(time.Hour)}, nil
}

var _ auth.SessionStore = (*TableSessionStore)(nil)
