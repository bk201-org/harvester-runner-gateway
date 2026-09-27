package harvester

import (
	"testing"

	"github.com/bk201-org/harvester-runner-gateway/internal/auth"
)

func TestAllocationObservationRejectsMalformedNewNamesAndIgnoresOldNames(t *testing.T) {
	owner := auth.Owner{RepositoryID: "123", RunID: "456", RunAttempt: "1"}
	values := map[string]string{expiresKey: "123"}
	if _, include, err := allocationObservation("ci", "runner-gw-old", ownerLabels(owner, "vm"), values, "vm", ""); err != nil || include {
		t.Fatalf("old object = include %v, error %v", include, err)
	}
	if _, _, err := allocationObservation("ci", "ci-123-456-a1-01", ownerLabels(owner, "vm"), values, "vm", ""); err == nil {
		t.Fatal("accepted malformed new-scheme name")
	}
	if _, _, err := allocationObservation("ci", "ci-123-456-a1-001", ownerLabels(owner, "cloud-init"), values, "cloud-init", "-init"); err == nil {
		t.Fatal("accepted cloud-init Secret without dependent suffix")
	}
}
