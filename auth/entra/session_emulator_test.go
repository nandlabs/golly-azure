package entra

import (
	"context"
	"fmt"
	"os"
	"testing"
	"time"

	"oss.nandlabs.io/golly-azure/azcfg"
)

// newEmulatorStore connects to Azurite (or a real storage account) via
// the connection string in AZURITE_TABLES_CONNECTION_STRING. The test is
// skipped unless that variable is set. Example for Azurite:
//
//	AZURITE_TABLES_CONNECTION_STRING="DefaultEndpointsProtocol=http;AccountName=devstoreaccount1;AccountKey=<azurite-key>;TableEndpoint=http://127.0.0.1:10002/devstoreaccount1;" \
//	  GOWORK=off go test ./auth/entra/...
func newEmulatorStore(t *testing.T) *TableSessionStore {
	t.Helper()
	cs := os.Getenv("AZURITE_TABLES_CONNECTION_STRING")
	if cs == "" {
		t.Skip("AZURITE_TABLES_CONNECTION_STRING not set; skipping Table Storage smoke test")
	}
	cfg := azcfg.NewConfig()
	cfg.SetConnectionString(cs)
	table := fmt.Sprintf("entrasess%d", time.Now().UnixNano()%1_000_000_000)
	s, err := NewTableSessionStoreFromConfig(cfg, table)
	if err != nil {
		t.Fatalf("NewTableSessionStoreFromConfig: %v", err)
	}
	if err := s.WithTimeout(10 * time.Second).CreateTableIfNotExists(context.Background()); err != nil {
		t.Fatalf("CreateTableIfNotExists: %v", err)
	}
	return s
}

func TestSmoke_TableSessionStore(t *testing.T) {
	s := newEmulatorStore(t)
	ctx := context.Background()
	live := newSession("smoke-live", "smoke-user", time.Now().Add(time.Hour))
	dead := newSession("smoke-dead", "smoke-user", time.Now().Add(-time.Hour))
	if err := s.Put(live); err != nil {
		t.Fatalf("Put: %v", err)
	}
	if err := s.Put(dead); err != nil {
		t.Fatalf("Put: %v", err)
	}
	if _, ok := s.Get(live.ID); !ok {
		t.Fatal("Get live: not found")
	}
	list, err := s.ListByUser(ctx, "smoke-user")
	if err != nil || len(list) != 1 {
		t.Fatalf("ListByUser = %d, %v", len(list), err)
	}
	if _, err := s.PurgeExpired(ctx); err != nil {
		t.Fatalf("PurgeExpired: %v", err)
	}
	if err := s.Delete(live.ID); err != nil {
		t.Fatalf("Delete: %v", err)
	}
}
