package entra

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"strings"
	"time"
	"unicode/utf16"

	"github.com/Azure/azure-sdk-for-go/sdk/azcore"
	"github.com/Azure/azure-sdk-for-go/sdk/data/aztables"
	"oss.nandlabs.io/golly-azure/azcfg"
	"oss.nandlabs.io/golly/auth"
)

// Property names used in the Azure Table Storage entity schema. The
// PartitionKey is the session id and the RowKey is the constant
// SessionRowKey, so every Get/Put/Delete is a single-entity point
// operation. The remaining properties mirror auth.Session fields at the
// top level so operators can query them without hydrating the JSON
// payload.
const (
	userIDProp    = "UserID"
	payloadProp   = "Payload"
	expiresAtProp = "ExpiresAt"
	createdAtProp = "CreatedAt"

	// SessionRowKey is the RowKey written for every session entity.
	SessionRowKey = "session"

	// maxPayloadUTF16 is the Table Storage limit for a string property
	// (64 KiB, i.e. 32K UTF-16 code units).
	maxPayloadUTF16 = 32 * 1024
)

// ErrInvalidSessionID is returned by Put/Delete when the session id
// cannot be used as a Table Storage PartitionKey (empty, longer than
// 1 KiB, or containing '/', '\\', '#', '?', a single quote or a control
// character).
var ErrInvalidSessionID = errors.New("auth/entra: invalid session id")

// ErrSessionTooLarge is returned by Put when the JSON-encoded session
// exceeds the 64 KiB Table Storage string-property limit.
var ErrSessionTooLarge = errors.New("auth/entra: session payload exceeds 64 KiB table property limit")

// errEntityNotFound is the internal not-found signal of tableAPI.
var errEntityNotFound = errors.New("auth/entra: entity not found")

// tableAPI is the subset of the aztables client used by the session
// store. Extracted so tests can substitute an in-memory fake without
// needing Azurite.
type tableAPI interface {
	getEntity(ctx context.Context, pk, rk string) ([]byte, error)
	upsertEntity(ctx context.Context, entity []byte) error
	deleteEntity(ctx context.Context, pk, rk string) error
	queryEntities(ctx context.Context, filter string) ([][]byte, error)
	createTable(ctx context.Context) error
}

// TableSessionStore persists auth.Session values in an Azure Table
// Storage (or Cosmos DB Table API) table. Each session is serialized
// via json.Marshal into the Payload property of an entity whose schema
// is:
//
//	PartitionKey : session id
//	RowKey       : "session" (SessionRowKey)
//	UserID       : string   — mirror of Session.Subject
//	Payload      : string   — JSON-encoded Session
//	ExpiresAt    : DateTime — mirror of Session.ExpiresAt (omitted when zero)
//	CreatedAt    : DateTime — mirror of Session.CreatedAt (omitted when zero)
//
// Table Storage has no native TTL, so the store emulates it: Get treats
// a session whose ExpiresAt has passed as absent (and deletes it best-
// effort), ListByUser skips expired sessions, and PurgeExpired deletes
// every expired entity in one sweep (run it periodically, e.g. from a
// golly chrono job).
type TableSessionStore struct {
	api     tableAPI
	timeout time.Duration
	now     func() time.Time
}

// NewTableSessionStore constructs a session store backed by the given
// aztables table client. The returned type satisfies auth.SessionStore
// and additionally exposes ListByUser and PurgeExpired.
func NewTableSessionStore(client *aztables.Client) *TableSessionStore {
	if client == nil {
		panic("auth/entra: nil aztables.Client")
	}
	return &TableSessionStore{api: &azTable{c: client}, now: time.Now}
}

// NewTableSessionStoreFromConfig builds the aztables client from an
// azcfg.Config and returns a store for the named table. Resolution:
//   - cfg.ConnectionString, if set (works with Azurite);
//   - otherwise a shared key (cfg.AccountName + cfg.AccountKey);
//   - otherwise cfg.Credential() (Entra ID / managed identity).
//
// The service URL is cfg.Endpoint when set, else
// https://<AccountName>.table.core.windows.net.
func NewTableSessionStoreFromConfig(cfg *azcfg.Config, table string) (*TableSessionStore, error) {
	if cfg == nil {
		return nil, errors.New("auth/entra: nil azcfg.Config")
	}
	if table == "" {
		return nil, errors.New("auth/entra: empty table name")
	}
	opts := &aztables.ClientOptions{ClientOptions: cfg.ClientOpts()}
	if cfg.ConnectionString != "" {
		svc, err := aztables.NewServiceClientFromConnectionString(cfg.ConnectionString, opts)
		if err != nil {
			return nil, fmt.Errorf("auth/entra: table client from connection string: %w", err)
		}
		return NewTableSessionStore(svc.NewClient(table)), nil
	}
	serviceURL := strings.TrimRight(cfg.Endpoint, "/")
	if serviceURL == "" {
		if cfg.AccountName == "" {
			return nil, errors.New("auth/entra: azcfg.Config needs ConnectionString, Endpoint or AccountName")
		}
		serviceURL = "https://" + cfg.AccountName + ".table.core.windows.net"
	}
	tableURL := serviceURL + "/" + table
	if cfg.AccountName != "" && cfg.AccountKey != "" {
		cred, err := aztables.NewSharedKeyCredential(cfg.AccountName, cfg.AccountKey)
		if err != nil {
			return nil, fmt.Errorf("auth/entra: shared key credential: %w", err)
		}
		c, err := aztables.NewClientWithSharedKey(tableURL, cred, opts)
		if err != nil {
			return nil, err
		}
		return NewTableSessionStore(c), nil
	}
	cred, err := cfg.Credential()
	if err != nil {
		return nil, fmt.Errorf("auth/entra: resolve credential: %w", err)
	}
	c, err := aztables.NewClient(tableURL, cred, opts)
	if err != nil {
		return nil, err
	}
	return NewTableSessionStore(c), nil
}

// newSessionStoreWithBackend is the unit-test constructor.
func newSessionStoreWithBackend(api tableAPI) *TableSessionStore {
	return &TableSessionStore{api: api, now: time.Now}
}

// WithTimeout sets a per-call context deadline applied to every
// request issued by the auth.SessionStore methods (and as an upper bound
// for ListByUser/PurgeExpired). A non-positive value clears it.
func (s *TableSessionStore) WithTimeout(d time.Duration) *TableSessionStore {
	s.timeout = d
	return s
}

// WithClock overrides the time source used for TTL evaluation.
// Primarily used by tests.
func (s *TableSessionStore) WithClock(now func() time.Time) *TableSessionStore {
	if now != nil {
		s.now = now
	}
	return s
}

// CreateTableIfNotExists creates the backing table, treating an
// already-existing table as success.
func (s *TableSessionStore) CreateTableIfNotExists(ctx context.Context) error {
	ctx, cancel := s.withTimeout(ctx)
	defer cancel()
	return s.api.createTable(ctx)
}

// Get implements auth.SessionStore. It returns (nil, false) when the
// id is unknown, the session has expired (the entity is then deleted
// best-effort), or on any transport/decoding error — the interface has
// no error channel.
func (s *TableSessionStore) Get(id string) (*auth.Session, bool) {
	if validateKey(id) != nil {
		return nil, false
	}
	ctx, cancel := s.withTimeout(context.Background())
	defer cancel()
	raw, err := s.api.getEntity(ctx, id, SessionRowKey)
	if err != nil {
		return nil, false
	}
	sess, err := decodeSession(raw)
	if err != nil {
		return nil, false
	}
	if s.expired(sess) {
		_ = s.api.deleteEntity(ctx, id, SessionRowKey)
		return nil, false
	}
	return sess, true
}

// Put implements auth.SessionStore. It upserts (replace mode) the
// entity for sess.ID.
func (s *TableSessionStore) Put(sess *auth.Session) error {
	if sess == nil {
		return fmt.Errorf("%w: nil session", ErrInvalidSessionID)
	}
	if err := validateKey(sess.ID); err != nil {
		return err
	}
	entity, err := encodeSession(sess)
	if err != nil {
		return err
	}
	ctx, cancel := s.withTimeout(context.Background())
	defer cancel()
	return s.api.upsertEntity(ctx, entity)
}

// Delete implements auth.SessionStore. Missing entities are treated as
// success so revocation is idempotent.
func (s *TableSessionStore) Delete(id string) error {
	if err := validateKey(id); err != nil {
		return err
	}
	ctx, cancel := s.withTimeout(context.Background())
	defer cancel()
	return s.api.deleteEntity(ctx, id, SessionRowKey)
}

// ListByUser returns every non-expired session whose Subject equals
// uid. Table Storage has no secondary indexes, so this is a filtered
// table scan ("UserID eq '<uid>'") evaluated server-side; it is fine
// for "sign out everywhere" style operations but should not sit on a
// hot request path for very large tables.
func (s *TableSessionStore) ListByUser(ctx context.Context, uid string) ([]*auth.Session, error) {
	if uid == "" {
		return nil, errors.New("auth/entra: empty user id")
	}
	ctx, cancel := s.withTimeout(ctx)
	defer cancel()
	filter := fmt.Sprintf("%s eq '%s' and RowKey eq '%s'", userIDProp, odataEscape(uid), SessionRowKey)
	rows, err := s.api.queryEntities(ctx, filter)
	if err != nil {
		return nil, fmt.Errorf("auth/entra: list sessions for %q: %w", uid, err)
	}
	out := make([]*auth.Session, 0, len(rows))
	for _, row := range rows {
		sess, err := decodeSession(row)
		if err != nil {
			return nil, fmt.Errorf("auth/entra: decode session for uid %q: %w", uid, err)
		}
		if s.expired(sess) {
			continue
		}
		out = append(out, sess)
	}
	return out, nil
}

// PurgeExpired deletes every session entity whose ExpiresAt is in the
// past and returns how many were removed. It is the Table Storage
// stand-in for a native TTL policy; schedule it periodically.
func (s *TableSessionStore) PurgeExpired(ctx context.Context) (int, error) {
	ctx, cancel := s.withTimeout(ctx)
	defer cancel()
	cutoff := s.now().UTC().Format(time.RFC3339Nano)
	filter := fmt.Sprintf("%s lt datetime'%s' and RowKey eq '%s'", expiresAtProp, cutoff, SessionRowKey)
	rows, err := s.api.queryEntities(ctx, filter)
	if err != nil {
		return 0, fmt.Errorf("auth/entra: query expired sessions: %w", err)
	}
	n := 0
	for _, row := range rows {
		var keys struct {
			PartitionKey string
			RowKey       string
		}
		if err := json.Unmarshal(row, &keys); err != nil || keys.PartitionKey == "" {
			continue
		}
		if err := s.api.deleteEntity(ctx, keys.PartitionKey, keys.RowKey); err != nil {
			return n, fmt.Errorf("auth/entra: delete expired session: %w", err)
		}
		n++
	}
	return n, nil
}

func (s *TableSessionStore) expired(sess *auth.Session) bool {
	return !sess.ExpiresAt.IsZero() && !s.now().Before(sess.ExpiresAt)
}

func (s *TableSessionStore) withTimeout(ctx context.Context) (context.Context, context.CancelFunc) {
	if ctx == nil {
		ctx = context.Background()
	}
	if s.timeout <= 0 {
		return ctx, func() {}
	}
	return context.WithTimeout(ctx, s.timeout)
}

// validateKey enforces the Table Storage key-character rules on a
// session id.
func validateKey(id string) error {
	if id == "" {
		return fmt.Errorf("%w: empty", ErrInvalidSessionID)
	}
	if len(id) > 1024 {
		return fmt.Errorf("%w: longer than 1 KiB", ErrInvalidSessionID)
	}
	for _, r := range id {
		switch {
		case r == '/', r == '\\', r == '#', r == '?', r == '\'':
			return fmt.Errorf("%w: contains %q", ErrInvalidSessionID, r)
		case r < 0x20, r >= 0x7f && r <= 0x9f:
			return fmt.Errorf("%w: contains a control character", ErrInvalidSessionID)
		}
	}
	return nil
}

// odataEscape escapes a string literal for an OData $filter.
func odataEscape(s string) string { return strings.ReplaceAll(s, "'", "''") }

// encodeSession marshals a session into an aztables entity.
func encodeSession(sess *auth.Session) ([]byte, error) {
	b, err := json.Marshal(sess)
	if err != nil {
		return nil, fmt.Errorf("auth/entra: marshal session: %w", err)
	}
	payload := string(b)
	if len(payload) > maxPayloadUTF16 && len(utf16.Encode([]rune(payload))) > maxPayloadUTF16 {
		return nil, ErrSessionTooLarge
	}
	props := map[string]any{
		userIDProp:  sess.Subject,
		payloadProp: payload,
	}
	if !sess.ExpiresAt.IsZero() {
		props[expiresAtProp] = aztables.EDMDateTime(sess.ExpiresAt.UTC())
	}
	if !sess.CreatedAt.IsZero() {
		props[createdAtProp] = aztables.EDMDateTime(sess.CreatedAt.UTC())
	}
	return json.Marshal(aztables.EDMEntity{
		Entity:     aztables.Entity{PartitionKey: sess.ID, RowKey: SessionRowKey},
		Properties: props,
	})
}

// decodeSession recovers a session from its entity form. The Payload
// property is the round-trip source of truth; mirrored properties are
// ignored on read.
func decodeSession(raw []byte) (*auth.Session, error) {
	var row map[string]json.RawMessage
	if err := json.Unmarshal(raw, &row); err != nil {
		return nil, fmt.Errorf("auth/entra: decode entity: %w", err)
	}
	p, ok := row[payloadProp]
	if !ok {
		return nil, fmt.Errorf("auth/entra: entity missing %q property", payloadProp)
	}
	var payload string
	if err := json.Unmarshal(p, &payload); err != nil {
		return nil, fmt.Errorf("auth/entra: %q property is not a string: %w", payloadProp, err)
	}
	var sess auth.Session
	if err := json.Unmarshal([]byte(payload), &sess); err != nil {
		return nil, fmt.Errorf("auth/entra: unmarshal session: %w", err)
	}
	return &sess, nil
}

// azTable adapts *aztables.Client to tableAPI.
type azTable struct {
	c *aztables.Client
}

func (a *azTable) getEntity(ctx context.Context, pk, rk string) ([]byte, error) {
	resp, err := a.c.GetEntity(ctx, pk, rk, nil)
	if err != nil {
		if isStatus(err, http.StatusNotFound) {
			return nil, errEntityNotFound
		}
		return nil, err
	}
	return resp.Value, nil
}

func (a *azTable) upsertEntity(ctx context.Context, entity []byte) error {
	_, err := a.c.UpsertEntity(ctx, entity, &aztables.UpsertEntityOptions{UpdateMode: aztables.UpdateModeReplace})
	return err
}

func (a *azTable) deleteEntity(ctx context.Context, pk, rk string) error {
	_, err := a.c.DeleteEntity(ctx, pk, rk, nil)
	if err != nil && isStatus(err, http.StatusNotFound) {
		return nil
	}
	return err
}

func (a *azTable) queryEntities(ctx context.Context, filter string) ([][]byte, error) {
	pager := a.c.NewListEntitiesPager(&aztables.ListEntitiesOptions{Filter: &filter})
	var out [][]byte
	for pager.More() {
		page, err := pager.NextPage(ctx)
		if err != nil {
			return nil, err
		}
		out = append(out, page.Entities...)
	}
	return out, nil
}

func (a *azTable) createTable(ctx context.Context) error {
	_, err := a.c.CreateTable(ctx, nil)
	if err != nil {
		var re *azcore.ResponseError
		if errors.As(err, &re) && (re.StatusCode == http.StatusConflict || re.ErrorCode == string(aztables.TableAlreadyExists)) {
			return nil
		}
	}
	return err
}

func isStatus(err error, code int) bool {
	var re *azcore.ResponseError
	return errors.As(err, &re) && re.StatusCode == code
}
