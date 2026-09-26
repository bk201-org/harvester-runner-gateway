package gateway

import (
	"context"
	"encoding/json"
	"net/http"
	"testing"

	"github.com/bk201-org/harvester-runner-gateway/internal/auth"
	"github.com/bk201-org/harvester-runner-gateway/internal/config"
)

func TestRecoveredRetryAndHighWaterMark(t *testing.T) {
	s := testServer(10, 10)
	backend := s.backend.(*fakeBackend)
	owner := auth.Owner{RepositoryID: "123", RunID: "1001", RunAttempt: "1"}
	req := vmRequest()
	req.TTLSeconds = int(config.DefaultTTL.Seconds())
	metadata := ResourceMetadata{IdentityHash: allocationIdentity(owner, "vm", "recovered"), RequestHash: requestHash(req)}
	id := "ci-123-1001-a1-007"
	backend.allocations = []AllocationObservation{{Namespace: "ci", Owner: owner, Kind: "vm", ID: id, Metadata: metadata}}
	backend.vms[id] = fakeVM{owner: owner, item: VMStatus{ID: id, RequestHash: metadata.RequestHash, IdentityHash: metadata.IdentityHash}}
	if err := s.RecoverAllocations(context.Background()); err != nil {
		t.Fatal(err)
	}
	retry := doRequest(s, http.MethodPost, "/v1/vms", "run-one", "recovered", vmRequest())
	if retry.Code != http.StatusOK {
		t.Fatalf("recovered retry = %d %s", retry.Code, retry.Body.String())
	}
	created := doRequest(s, http.MethodPost, "/v1/vms", "run-one", "next", vmRequest())
	var item VMStatus
	if err := json.Unmarshal(created.Body.Bytes(), &item); err != nil {
		t.Fatal(err)
	}
	if created.Code != http.StatusCreated || item.ID != "ci-123-1001-a1-008" {
		t.Fatalf("next create = %d %q", created.Code, item.ID)
	}
}
