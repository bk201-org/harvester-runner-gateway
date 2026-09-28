package gateway

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"net/http"
	"path/filepath"
	"strings"
	"testing"

	"github.com/bk201-org/harvester-runner-gateway/internal/auth"
	"github.com/bk201-org/harvester-runner-gateway/internal/config"
)

func TestSQLiteAllocationStorePersistsGlobalHistoryAndRecoveryFloor(t *testing.T) {
	ctx := context.Background()
	path := filepath.Join(t.TempDir(), "allocations.sqlite")
	owner := auth.Owner{RepositoryID: "123", RunID: "456", RunAttempt: "1"}
	other := auth.Owner{RepositoryID: "999", RunID: "789", RunAttempt: "2"}
	prefixes := config.Config{}.IDPrefixes()
	open := func() AllocationStore {
		t.Helper()
		store, err := OpenSQLiteAllocationStore(ctx, path, prefixes)
		if err != nil {
			t.Fatal(err)
		}
		return store
	}
	store := open()
	a := newAllocator(prefixes)
	a.store = store
	if err := a.recover([]AllocationObservation{
		{Namespace: "ci", Owner: owner, Kind: "vm", ID: "ci-vm-00000013"},
	}); err != nil {
		t.Fatal(err)
	}
	id, err := a.reserveContext(ctx, "other", other, "vm")
	if err != nil || id != "ci-vm-00000014" {
		t.Fatalf("first SQLite VM reservation = %q, %v", id, err)
	}
	volume, err := a.reserveContext(ctx, "ci", owner, "volume")
	if err != nil || volume != "ci-vol-00000001" {
		t.Fatalf("independent volume reservation = %q, %v", volume, err)
	}
	if err := store.Close(); err != nil {
		t.Fatal(err)
	}
	store = open()
	defer store.Close()
	a = newAllocator(prefixes)
	a.store = store
	next, err := a.reserveContext(ctx, "ci", owner, "vm")
	if err != nil || next != "ci-vm-00000015" {
		t.Fatalf("reservation after restart = %q, %v", next, err)
	}
	var count, first int
	err = store.(*sqliteAllocationStore).db.QueryRowContext(ctx,
		"SELECT count(*), min(sequence) FROM allocation_reservations WHERE kind = 'vm'").Scan(&count, &first)
	if err != nil || count != 2 || first != 20 {
		t.Fatalf("VM history = count %d, first %d, error %v", count, first, err)
	}
}

func TestSQLiteAllocationStoreRejectsInvalidOwnerWithoutAdvancing(t *testing.T) {
	ctx := context.Background()
	store, err := OpenSQLiteAllocationStore(ctx, filepath.Join(t.TempDir(), "allocations.sqlite"), config.Config{}.IDPrefixes())
	if err != nil {
		t.Fatal(err)
	}
	defer store.Close()
	owner := auth.Owner{RepositoryID: strings.Repeat("1", 30), RunID: "bad", RunAttempt: "1"}
	if _, _, err := store.Reserve(ctx, "ci", owner, "vm", 0); !errors.Is(err, ErrInvalid) {
		t.Fatalf("invalid owner reservation error = %v", err)
	}
	var count int
	err = store.(*sqliteAllocationStore).db.QueryRowContext(ctx, "SELECT count(*) FROM allocation_counters").Scan(&count)
	if err != nil || count != 0 {
		t.Fatalf("invalid owner wrote a counter: count %d, error %v", count, err)
	}
}

func TestSQLiteAllocationStoreRequiresAbsolutePath(t *testing.T) {
	_, err := OpenSQLiteAllocationStore(context.Background(), "allocations.sqlite", config.Config{}.IDPrefixes())
	if err == nil {
		t.Fatal("relative SQLite path was accepted")
	}
}

func TestSQLiteAllocationStoreRejectsPrefixChange(t *testing.T) {
	ctx := context.Background()
	path := filepath.Join(t.TempDir(), "allocations.sqlite")
	store, err := OpenSQLiteAllocationStore(ctx, path, config.Config{}.IDPrefixes())
	if err != nil {
		t.Fatal(err)
	}
	if err := store.Close(); err != nil {
		t.Fatal(err)
	}
	_, err = OpenSQLiteAllocationStore(ctx, path, config.IDPrefixes{VM: "other-vm-", Volume: "ci-vol-"})
	if err == nil || !strings.Contains(err.Error(), "prefix mismatch") {
		t.Fatalf("prefix change error = %v", err)
	}
}

func TestSQLiteAllocationStoreRejectsLegacySchema(t *testing.T) {
	ctx := context.Background()
	path := filepath.Join(t.TempDir(), "allocations.sqlite")
	db, err := sql.Open("sqlite", path)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := db.ExecContext(ctx, "CREATE TABLE allocation_counters (namespace TEXT)"); err != nil {
		t.Fatal(err)
	}
	if err := db.Close(); err != nil {
		t.Fatal(err)
	}
	_, err = OpenSQLiteAllocationStore(ctx, path, config.Config{}.IDPrefixes())
	if err == nil || !strings.Contains(err.Error(), "legacy allocation database schema") {
		t.Fatalf("legacy schema error = %v", err)
	}
}

func TestSQLiteAllocationStoreRequiresResetAfterSeedChange(t *testing.T) {
	ctx := context.Background()
	path := filepath.Join(t.TempDir(), "allocations.sqlite")
	db, err := sql.Open("sqlite", path)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := db.ExecContext(ctx, "PRAGMA user_version = 1"); err != nil {
		t.Fatal(err)
	}
	if err := db.Close(); err != nil {
		t.Fatal(err)
	}
	_, err = OpenSQLiteAllocationStore(ctx, path, config.Config{}.IDPrefixes())
	if err == nil || !strings.Contains(err.Error(), "requires a reset") {
		t.Fatalf("previous schema error = %v", err)
	}
}

func TestGatewayCreateKeepsSequenceAfterRestartAndDeletion(t *testing.T) {
	ctx := context.Background()
	path := filepath.Join(t.TempDir(), "allocations.sqlite")
	base := testServer(10, 10)
	openServer := func() (*Server, AllocationStore) {
		t.Helper()
		store, err := OpenSQLiteAllocationStore(ctx, path, base.cfg.IDPrefixes())
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
	if first.Code != http.StatusCreated || item.ID != "ci-vm-00000001" {
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
	if second.Code != http.StatusCreated || item.ID != "ci-vm-00000002" {
		t.Fatalf("create after restart = %d %q", second.Code, item.ID)
	}
}
