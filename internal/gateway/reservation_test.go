package gateway

import (
	"encoding/json"
	"fmt"
	"net/http"
	"testing"
)

func TestValidationAndQuotaFailuresDoNotReserveSequence(t *testing.T) {
	s := testServer(0, 1)
	if got := doRequest(s, http.MethodPost, "/v1/vms", "run-one", vmRequest()); got.Code != http.StatusConflict {
		t.Fatalf("quota rejection = %d %s", got.Code, got.Body.String())
	}
	s.cfg.Repositories[0].Quota.MaxActiveVMs = 2
	backend := s.backend.(*fakeBackend)
	backend.validateErr = fmt.Errorf("%w: rejected before reservation", ErrInvalid)
	if got := doRequest(s, http.MethodPost, "/v1/vms", "run-one", vmRequest()); got.Code != http.StatusUnprocessableEntity {
		t.Fatalf("validation rejection = %d %s", got.Code, got.Body.String())
	}
	backend.validateErr = nil
	created := doRequest(s, http.MethodPost, "/v1/vms", "run-one", vmRequest())
	var item VMStatus
	if err := json.Unmarshal(created.Body.Bytes(), &item); err != nil {
		t.Fatal(err)
	}
	if created.Code != http.StatusCreated || item.ID != "ci-123-1001-a1-001" {
		t.Fatalf("first accepted create = %d %q", created.Code, item.ID)
	}
}
