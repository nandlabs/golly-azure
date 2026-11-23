package cosmosdb

import (
	"context"
	"crypto/tls"
	"errors"
	"fmt"
	"net/http"
	"os"
	"testing"
	"time"

	"github.com/Azure/azure-sdk-for-go/sdk/azcore"
	"github.com/Azure/azure-sdk-for-go/sdk/azcore/policy"
	"github.com/Azure/azure-sdk-for-go/sdk/data/azcosmos"

	"oss.nandlabs.io/golly/vectorstore"
)

// newEmulatorContainer connects to a Cosmos DB emulator (or a real account)
// at COSMOS_EMULATOR_ENDPOINT, e.g. "https://localhost:8081/". The test is
// skipped unless that variable is set. COSMOS_EMULATOR_KEY overrides the
// well-known emulator key. The emulator's self-signed certificate is
// accepted. The emulator must have vector search enabled (Windows emulator:
// /EnablePreview; Linux vNext emulator: vector search is on by default).
func newEmulatorContainer(t *testing.T, props func(id string) (azcosmos.ContainerProperties, error)) *azcosmos.ContainerClient {
	t.Helper()
	endpoint := os.Getenv("COSMOS_EMULATOR_ENDPOINT")
	if endpoint == "" {
		t.Skip("COSMOS_EMULATOR_ENDPOINT not set; skipping Cosmos DB emulator test")
	}
	key := os.Getenv("COSMOS_EMULATOR_KEY")
	if key == "" {
		key = emulatorKey
	}
	cred, err := azcosmos.NewKeyCredential(key)
	if err != nil {
		t.Fatal(err)
	}
	httpClient := &http.Client{Transport: &http.Transport{
		TLSClientConfig: &tls.Config{InsecureSkipVerify: true}, //nolint:gosec // emulator uses a self-signed cert.
	}}
	client, err := azcosmos.NewClientWithKey(endpoint, cred, &azcosmos.ClientOptions{
		ClientOptions: policy.ClientOptions{Transport: httpClient},
	})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(client.Close)

	ctx := context.Background()
	dbName := "golly_vectorstore_test"
	if _, err := client.CreateDatabase(ctx, azcosmos.DatabaseProperties{ID: dbName}, nil); err != nil && !isConflict(err) {
		t.Fatalf("CreateDatabase: %v", err)
	}
	db, err := client.NewDatabase(dbName)
	if err != nil {
		t.Fatal(err)
	}
	containerID := fmt.Sprintf("vs_%d", time.Now().UnixNano())
	p, err := props(containerID)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := db.CreateContainer(ctx, p, nil); err != nil {
		t.Fatalf("CreateContainer: %v", err)
	}
	cc, err := db.NewContainer(containerID)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _, _ = cc.Delete(context.Background(), nil) })
	return cc
}

func isConflict(err error) bool {
	var re *azcore.ResponseError
	return errors.As(err, &re) && re.StatusCode == http.StatusConflict
}

func TestEmulator_StaticPartitionRoundTrip(t *testing.T) {
	opts := Options{Dimension: 3, PartitionKeyField: "pk", PartitionKeyValue: "tenant-a"}
	// Build a throwaway store to derive matching container properties.
	tmpl, err := newWithContainer(opts, &fakeContainer{}, nil)
	if err != nil {
		t.Fatal(err)
	}
	cc := newEmulatorContainer(t, tmpl.ContainerProperties)
	opts.Client = cc
	s, err := New(opts)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = s.Close() }()

	ctx := context.Background()
	docs := []vectorstore.Doc{
		{ID: "x", Vector: vectorstore.Vector{1, 0, 0}, Content: "x axis", Metadata: map[string]any{"axis": "x"}},
		{ID: "y", Vector: vectorstore.Vector{0, 1, 0}, Content: "y axis", Metadata: map[string]any{"axis": "y"}},
		{ID: "xy", Vector: vectorstore.Vector{0.7, 0.7, 0}, Metadata: map[string]any{"axis": "xy"}},
	}
	if err := s.Upsert(ctx, docs...); err != nil {
		t.Fatalf("Upsert: %v", err)
	}
	hits, err := s.Search(ctx, vectorstore.Query{Vector: vectorstore.Vector{1, 0.1, 0}, TopK: 2})
	if err != nil {
		t.Fatalf("Search: %v", err)
	}
	if len(hits) != 2 || hits[0].ID != "x" || hits[0].Content != "x axis" || hits[0].Metadata["axis"] != "x" {
		t.Fatalf("hits = %+v", hits)
	}
	hits, err = s.Search(ctx, vectorstore.Query{Vector: vectorstore.Vector{1, 0, 0}, TopK: 3, Filter: map[string]any{"axis": "y"}})
	if err != nil {
		t.Fatalf("filtered Search: %v", err)
	}
	if len(hits) != 1 || hits[0].ID != "y" {
		t.Fatalf("filtered hits = %+v", hits)
	}
	if err := s.Delete(ctx, "x", "does-not-exist"); err != nil {
		t.Fatalf("Delete: %v", err)
	}
	hits, err = s.Search(ctx, vectorstore.Query{Vector: vectorstore.Vector{1, 0, 0}, TopK: 3})
	if err != nil {
		t.Fatal(err)
	}
	for _, h := range hits {
		if h.ID == "x" {
			t.Fatalf("deleted doc still returned: %+v", hits)
		}
	}
}

func TestEmulator_MetadataPartitionDelete(t *testing.T) {
	opts := Options{Dimension: 2, PartitionKeyField: "category"}
	tmpl, err := newWithContainer(opts, &fakeContainer{}, nil)
	if err != nil {
		t.Fatal(err)
	}
	cc := newEmulatorContainer(t, tmpl.ContainerProperties)
	opts.Client = cc
	s, err := New(opts)
	if err != nil {
		t.Fatal(err)
	}
	ctx := context.Background()
	if err := s.Upsert(ctx,
		vectorstore.Doc{ID: "a", Vector: vectorstore.Vector{1, 0}, Metadata: map[string]any{"category": "books"}},
		vectorstore.Doc{ID: "b", Vector: vectorstore.Vector{0, 1}, Metadata: map[string]any{"category": "films"}},
	); err != nil {
		t.Fatalf("Upsert: %v", err)
	}
	hits, err := s.Search(ctx, vectorstore.Query{Vector: vectorstore.Vector{1, 0}, Filter: map[string]any{"category": "films"}})
	if err != nil {
		t.Fatalf("Search: %v", err)
	}
	if len(hits) != 1 || hits[0].ID != "b" {
		t.Fatalf("hits = %+v", hits)
	}
	if err := s.Delete(ctx, "a", "b", "zzz"); err != nil {
		t.Fatalf("Delete: %v", err)
	}
	hits, err = s.Search(ctx, vectorstore.Query{Vector: vectorstore.Vector{1, 0}, Filter: map[string]any{"category": "books"}})
	if err != nil {
		t.Fatal(err)
	}
	if len(hits) != 0 {
		t.Fatalf("expected no hits after delete, got %+v", hits)
	}
}
