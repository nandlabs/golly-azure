package tablelock

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"os"
	"strings"
	"testing"
	"time"

	"github.com/Azure/azure-sdk-for-go/sdk/azcore"
	"github.com/Azure/azure-sdk-for-go/sdk/data/aztables"
)

// The emulator tests run against Azurite (or a real storage account) and
// are skipped unless AZURITE_TABLES_CONNECTION_STRING is set, e.g.
//
//	docker run -p 10002:10002 mcr.microsoft.com/azure-storage/azurite azurite-table --tableHost 0.0.0.0
//	export AZURITE_TABLES_CONNECTION_STRING="DefaultEndpointsProtocol=http;AccountName=devstoreaccount1;AccountKey=Eby8vdM02xNOcqFlqUwJPLlmEtlCDXJ1OUzFT50uSRZ6IFsuFq2UVErCz4I6tq/K1SZFPTOtr/KBHBeksoGMGw==;TableEndpoint=http://127.0.0.1:10002/devstoreaccount1;"
const emulatorEnv = "AZURITE_TABLES_CONNECTION_STRING"

const emulatorTable = "gollychronolocks"

func newEmulatorClient(t *testing.T) *aztables.Client {
	t.Helper()
	cs := os.Getenv(emulatorEnv)
	if cs == "" {
		t.Skip(emulatorEnv + " not set; skipping Azurite Table Storage test")
	}
	svc, err := aztables.NewServiceClientFromConnectionString(cs, nil)
	if err != nil {
		t.Fatalf("NewServiceClientFromConnectionString: %v", err)
	}
	if _, err := svc.CreateTable(context.Background(), emulatorTable, nil); err != nil {
		var re *azcore.ResponseError
		if !errors.As(err, &re) || re.StatusCode != http.StatusConflict {
			t.Fatalf("CreateTable: %v", err)
		}
	}
	return svc.NewClient(emulatorTable)
}

func uniqueKey(t *testing.T) string {
	return fmt.Sprintf("%s-%d", strings.ReplaceAll(t.Name(), "/", "_"), time.Now().UnixNano())
}

func TestEmulator_ClaimAndRenew(t *testing.T) {
	client := newEmulatorClient(t)
	ctx := context.Background()

	e := New(client, Options{Key: uniqueKey(t), Owner: "instance-a", Lease: 500 * time.Millisecond})
	if err := e.Start(ctx); err != nil {
		t.Fatalf("Start: %v", err)
	}
	time.Sleep(750 * time.Millisecond)
	if !e.IsLeader(ctx) {
		t.Fatalf("expected leader after emulator tick")
	}
	if err := e.Resign(ctx); err != nil {
		t.Fatalf("Resign: %v", err)
	}
	if _, err := client.GetEntity(ctx, DefaultPartitionKey, e.opts.Key, nil); !isStatus(err, http.StatusNotFound) {
		t.Fatalf("entity should be deleted after Resign, got err=%v", err)
	}
}

func TestEmulator_SecondInstanceLoses(t *testing.T) {
	client := newEmulatorClient(t)
	ctx := context.Background()
	key := uniqueKey(t)

	a := New(client, Options{Key: key, Owner: "a", Lease: 500 * time.Millisecond})
	b := New(client, Options{Key: key, Owner: "b", Lease: 500 * time.Millisecond})

	if err := a.Start(ctx); err != nil {
		t.Fatalf("a.Start: %v", err)
	}
	if err := b.Start(ctx); err != nil {
		t.Fatalf("b.Start: %v", err)
	}
	time.Sleep(300 * time.Millisecond)
	if !a.IsLeader(ctx) {
		t.Fatalf("expected a to be leader")
	}
	if b.IsLeader(ctx) {
		t.Fatalf("expected b to NOT be leader")
	}
	_ = a.Resign(ctx)
	_ = b.Resign(ctx)
}

func TestEmulator_StealExpired(t *testing.T) {
	client := newEmulatorClient(t)
	ctx := context.Background()
	key := uniqueKey(t)

	clock := newMockClock(time.Now())
	a := newWithClient(client, Options{Key: key, Owner: "a", Lease: time.Second})
	a.now = clock.Now
	if err := a.tick(ctx); err != nil || !a.IsLeader(ctx) {
		t.Fatalf("a tick: leader=%v err=%v", a.IsLeader(ctx), err)
	}
	clock.Advance(4 * time.Second)
	b := newWithClient(client, Options{Key: key, Owner: "b", Lease: time.Second})
	b.now = clock.Now
	if err := b.tick(ctx); err != nil || !b.IsLeader(ctx) {
		t.Fatalf("b steal: leader=%v err=%v", b.IsLeader(ctx), err)
	}
	if err := a.tick(ctx); err != nil {
		t.Fatalf("a re-tick: %v", err)
	}
	if a.IsLeader(ctx) {
		t.Fatalf("a should have been demoted by ETag mismatch")
	}
	_, _ = client.DeleteEntity(ctx, DefaultPartitionKey, key, nil)
}
