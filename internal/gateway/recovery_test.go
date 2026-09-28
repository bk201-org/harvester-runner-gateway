package gateway

import (
	"context"
	"encoding/json"
	"net/http"
	"testing"

	"github.com/bk201/harvester-runner-gateway/internal/auth"
)

func TestRecoveredHighWaterMark(t *testing.T) {
	s := testServer(10, 10)
	backend := s.backend.(*fakeBackend)
	owner := auth.Owner{RepositoryID: "123", RunID: "1001", RunAttempt: "1"}
	id := "ci-vm-00000013"
	backend.allocations = []AllocationObservation{{Namespace: "ci", Owner: owner, Kind: "vm", ID: id}}
	backend.vms[id] = fakeVM{owner: owner, item: VMStatus{ID: id}}
	if err := s.RecoverAllocations(context.Background()); err != nil {
		t.Fatal(err)
	}
	created := doRequest(s, http.MethodPost, "/v1/vms", "run-one", vmRequest())
	var item VMStatus
	if err := json.Unmarshal(created.Body.Bytes(), &item); err != nil {
		t.Fatal(err)
	}
	if created.Code != http.StatusCreated || item.ID != "ci-vm-00000014" {
		t.Fatalf("next create = %d %q", created.Code, item.ID)
	}
}
