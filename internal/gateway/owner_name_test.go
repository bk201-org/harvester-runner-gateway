package gateway

import (
	"errors"
	"testing"

	"github.com/bk201-org/harvester-runner-gateway/internal/auth"
)

func TestAllocatorRejectsInvalidOwnerComponents(t *testing.T) {
	owner := auth.Owner{RepositoryID: "123", RunID: "not-numeric", RunAttempt: "1"}
	a := newAllocator()
	if _, err := a.reserve("ci", owner, "vm", allocationIdentity(owner, "vm", "key")); !errors.Is(err, ErrInvalid) {
		t.Fatalf("invalid owner reserve = %v", err)
	}
	if len(a.identity) != 0 || a.high[scopeFor("ci", owner, "vm")] != 0 {
		t.Fatal("invalid owner consumed a reservation")
	}
}
