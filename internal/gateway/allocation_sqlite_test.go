package gateway

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"path/filepath"
	"strings"
	"testing"

	"github.com/bk201-org/harvester-runner-gateway/internal/auth"
)

func TestSQLiteAllocationStorePersistsHistoryAndUsesRecoveryFloor(t *testing.T) {
	ctx := context.Background()
	path := filepath.Join(t.TempDir(), "allocations.sqlite")
	owner := auth.Owner{RepositoryID: "123", RunID: "456", RunAttempt: "1"}

	open := func() AllocationStore {
		t.Helper()
		store, err := OpenSQLiteAllocationStore(ctx, path)
		if err != nil {
			t.Fatal(err)
		}
		return store
	}
	store := open()
	a := newAllocator()
	a.store = store
	if err := a.recover([]AllocationObservation{
		{Namespace: "ci", Owner: owner, Kind: "vm", ID: "ci-123-456-a1-007"},
	}); err != nil {
		t.Fatal(err)
	}
	id, err := a.reserveContext(ctx, "ci", owner, "vm")
	if err != nil || id != "ci-123-456-a1-008" {
		t.Fatalf("first SQLite reservation = %q, %v", id, err)
	}
	volume, err := a.reserveContext(ctx, "ci", owner, "volume")
	if err != nil || volume != "ci-123-456-a1-001" {
		t.Fatalf("independent volume reservation = %q, %v", volume, err)
	}
	if err := store.Close(); err != nil {
		t.Fatal(err)
	}

	// The original Kubernetes observation has disappeared, but the SQLite
	// reservation still protects the high-water mark.
	store = open()
	defer store.Close()
	a = newAllocator()
	a.store = store
	next, err := a.reserveContext(ctx, "ci", owner, "vm")
	if err != nil || next != "ci-123-456-a1-009" {
		t.Fatalf("reservation after restart = %q, %v", next, err)
	}
	db := store.(*sqliteAllocationStore).db
	var count, firstSequence int
	if err := db.QueryRowContext(ctx, `SELECT count(*), min(sequence) FROM allocation_reservations
		WHERE namespace = 'ci' AND repository_id = '123' AND run_id = '456'
		AND run_attempt = '1' AND kind = 'vm'`).Scan(&count, &firstSequence); err != nil {
		t.Fatal(err)
	}
	if count != 2 || firstSequence != 8 {
		t.Fatalf("history = count %d, first sequence %d; old resource should not be imported", count, firstSequence)
	}
}

func TestSQLiteAllocationStoreRejectsInvalidNameWithoutAdvancing(t *testing.T) {
	ctx := context.Background()
	store, err := OpenSQLiteAllocationStore(ctx, filepath.Join(t.TempDir(), "allocations.sqlite"))
	if err != nil {
		t.Fatal(err)
	}
	defer store.Close()
	owner := auth.Owner{RepositoryID: strings.Repeat("1", 30), RunID: strings.Repeat("2", 30), RunAttempt: "1"}
	if _, _, err := store.Reserve(ctx, "ci", owner, "vm", 0); !errors.Is(err, ErrInvalid) {
		t.Fatalf("invalid name reservation error = %v", err)
	}
	var count int
	err = store.(*sqliteAllocationStore).db.QueryRowContext(ctx, "SELECT count(*) FROM allocation_counters").Scan(&count)
	if err != nil || count != 0 {
		t.Fatalf("invalid name wrote a counter: count %d, error %v", count, err)
	}
}

func TestSQLiteAllocationStoreRequiresAbsolutePath(t *testing.T) {
	_, err := OpenSQLiteAllocationStore(context.Background(), "allocations.sqlite")
	if err == nil {
		t.Fatal("relative SQLite path was accepted")
	}
}

func TestGatewayCreateKeepsSequenceAfterRestartAndDeletion(t *testing.T) {
	ctx := context.Background()
	path := filepath.Join(t.TempDir(), "allocations.sqlite")
	base := testServer(10, 10)
	openServer := func() (*Server, AllocationStore) {
		t.Helper()
		store, err := OpenSQLiteAllocationStore(ctx, path)
		if err != nil {
			t.Fatal(err)
		}
		server := NewServerWithAllocationStore(base.cfg, base.verifier, base.backend, nil, store)
		if err := server.RecoverAllocations(ctx); err != nil {
			_ = store.Close()
			t.Fatal(err)
		}
		return server, store
	}
	server, store := openServer()
	first := doRequest(server, http.MethodPost, "/v1/vms", "run-one", vmRequest())
	var item VMStatus
	if err := json.Unmarshal(first.Body.Bytes(), &item); err != nil {
		t.Fatal(err)
	}
	if first.Code != http.StatusCreated || item.ID != "ci-123-1001-a1-001" {
		t.Fatalf("first create = %d %q", first.Code, item.ID)
	}
	deleted := doRequest(server, http.MethodDelete, "/v1/vms/"+item.ID, "run-one", nil)
	if deleted.Code != http.StatusNoContent {
		t.Fatalf("delete = %d %s", deleted.Code, deleted.Body.String())
	}
	if err := store.Close(); err != nil {
		t.Fatal(err)
	}
	server, store = openServer()
	defer store.Close()
	second := doRequest(server, http.MethodPost, "/v1/vms", "run-one", vmRequest())
	if err := json.Unmarshal(second.Body.Bytes(), &item); err != nil {
		t.Fatal(err)
	}
	if second.Code != http.StatusCreated || item.ID != "ci-123-1001-a1-002" {
		t.Fatalf("create after restart = %d %q", second.Code, item.ID)
	}
}
