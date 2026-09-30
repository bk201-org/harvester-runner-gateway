package gateway

import (
	"context"
	"encoding/json"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/bk201-org/harvester-runner-gateway/internal/auth"
	"github.com/bk201-org/harvester-runner-gateway/internal/config"
)

func TestDeveloperIsolationAndSharedQuota(t *testing.T) {
	base := testServer(4, 4)
	cfg := base.cfg
	tokens := []string{strings.Repeat("ab", 32), strings.Repeat("cd", 32), strings.Repeat("ef", 32), "run-one"}
	for i, id := range []string{"alice", "bob", "smoke"} {
		path := filepath.Join(t.TempDir(), "token")
		if err := os.WriteFile(path, []byte(tokens[i]), 0600); err != nil {
			t.Fatal(err)
		}
		if id == "smoke" {
			cfg.LocalSmoke = config.LocalSmokeConfig{RepositoryID: "123", TokenFile: path}
		} else {
			cfg.Developers = append(cfg.Developers, config.DeveloperConfig{ID: id, RepositoryID: "123", TokenFile: path})
		}
	}
	verifier, err := auth.NewLocalSmokeVerifier(fakeVerifier{}, cfg)
	if err != nil {
		t.Fatal(err)
	}
	verifier, err = auth.NewDeveloperVerifier(verifier, cfg)
	if err != nil {
		t.Fatal(err)
	}
	server := NewServer(cfg, verifier, newFakeBackend())
	for _, kind := range []string{"vms", "volumes"} {
		var body any = vmRequest()
		if kind == "volumes" {
			body = VolumeRequest{Size: "1Gi"}
		}
		for _, token := range tokens {
			created := doRequest(server, http.MethodPost, "/v1/"+kind, token, body)
			var item struct{ ID string }
			if err := json.Unmarshal(created.Body.Bytes(), &item); err != nil || created.Code != 201 {
				t.Fatalf("create %s: %d %s", kind, created.Code, created.Body.String())
			}
			for _, caller := range tokens {
				want := http.StatusOK
				if caller != token {
					want = http.StatusNotFound
				}
				if got := doRequest(server, http.MethodGet, "/v1/"+kind+"/"+item.ID, caller, nil); got.Code != want {
					t.Fatalf("cross-owner %s: %d want %d", kind, got.Code, want)
				}
				if caller != token {
					if got := doRequest(server, http.MethodDelete, "/v1/"+kind+"/"+item.ID, caller, nil); got.Code != 404 {
						t.Fatalf("cross-owner delete: %d", got.Code)
					}
				}
			}
		}
		if got := doRequest(server, http.MethodPost, "/v1/"+kind, tokens[0], body); got.Code != 409 {
			t.Fatalf("shared quota: %d %s", got.Code, got.Body.String())
		}
		for _, token := range tokens {
			list := doRequest(server, http.MethodGet, "/v1/"+kind, token, nil)
			var items []json.RawMessage
			if err := json.Unmarshal(list.Body.Bytes(), &items); err != nil || len(items) != 1 {
				t.Fatalf("owner list: %s", list.Body.String())
			}
		}
	}
}

func TestDeveloperAllocationPersistenceAndRecovery(t *testing.T) {
	ctx := context.Background()
	path := filepath.Join(t.TempDir(), "allocations.sqlite")
	prefixes := config.Config{}.IDPrefixes()
	owner := auth.Owner{RepositoryID: "123", RunID: "dev-alice", RunAttempt: "1"}
	store, err := OpenSQLiteAllocationStore(ctx, path, prefixes)
	if err != nil {
		t.Fatal(err)
	}
	id, _, err := store.Reserve(ctx, "ci", owner, "vm", 0)
	if err != nil {
		t.Fatal(err)
	}
	if err := store.Close(); err != nil {
		t.Fatal(err)
	}
	store, err = OpenSQLiteAllocationStore(ctx, path, prefixes)
	if err != nil {
		t.Fatal(err)
	}
	defer store.Close()
	next, _, err := store.Reserve(ctx, "ci", owner, "vm", 0)
	if err != nil || next == id {
		t.Fatalf("restart allocation: %s %v", next, err)
	}
	allocator := newAllocator(prefixes)
	if err := allocator.recover([]AllocationObservation{{Namespace: "ci", Owner: owner, Kind: "vm", ID: next}}); err != nil {
		t.Fatal(err)
	}
	// Recovery must work even after the developer credential has been revoked.
	if nextID, err := allocator.reserve("ci", owner, "vm"); err != nil || nextID == next {
		t.Fatalf("recovery: %s %v", nextID, err)
	}
	for _, invalid := range []string{"dev-", "dev-Alice", "dev-alice,bob"} {
		owner.RunID = invalid
		if validOwner(owner) {
			t.Fatalf("accepted %q", invalid)
		}
	}
}
