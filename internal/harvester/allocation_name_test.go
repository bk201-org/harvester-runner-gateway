package harvester

import (
	"testing"

	"github.com/bk201/harvester-runner-gateway/internal/auth"
	"github.com/bk201/harvester-runner-gateway/internal/config"
)

func TestAllocationObservationRejectsMalformedNewNamesAndIgnoresOldNames(t *testing.T) {
	owner := auth.Owner{RepositoryID: "123", RunID: "456", RunAttempt: "1"}
	values := map[string]string{expiresKey: "123"}
	if _, include, err := allocationObservation("ci", "runner-gw-old", ownerLabels(owner, "vm"), values, "vm", "", config.Config{}.IDPrefixes()); err != nil || include {
		t.Fatalf("old object = include %v, error %v", include, err)
	}
	if _, _, err := allocationObservation("ci", "ci-vm-0000000", ownerLabels(owner, "vm"), values, "vm", "", config.Config{}.IDPrefixes()); err == nil {
		t.Fatal("accepted malformed new-scheme name")
	}
	if _, _, err := allocationObservation("ci", "ci-vm-00000001", ownerLabels(owner, "cloud-init"), values, "cloud-init", "-init", config.Config{}.IDPrefixes()); err == nil {
		t.Fatal("accepted cloud-init Secret without dependent suffix")
	}
}

func TestAllocationObservationUsesConfiguredPrefix(t *testing.T) {
	owner := auth.Owner{RepositoryID: "123", RunID: "456", RunAttempt: "1"}
	labels := ownerLabels(owner, "vm")
	values := map[string]string{expiresKey: "123"}
	prefixes := config.IDPrefixes{VM: "build-vm-", Volume: "build-vol-"}
	item, include, err := allocationObservation("ci", "build-vm-00000001", labels, values, "vm", "", prefixes)
	if err != nil || !include || item.ID != "build-vm-00000001" {
		t.Fatalf("custom prefix observation = %+v, include %t, error %v", item, include, err)
	}
	_, include, err = allocationObservation("ci", "ci-vm-00000001", labels, values, "vm", "", prefixes)
	if err != nil || include {
		t.Fatalf("prior prefix observation: include %t, error %v", include, err)
	}
}
