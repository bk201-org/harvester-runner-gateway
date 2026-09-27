package gateway

import (
	"errors"
	"testing"

	"github.com/bk201-org/harvester-runner-gateway/internal/auth"
	"github.com/bk201-org/harvester-runner-gateway/internal/config"
)

func TestAllocatorRejectsInvalidOwnerComponents(t *testing.T) {
	owner := auth.Owner{RepositoryID: "123", RunID: "not-numeric", RunAttempt: "1"}
	a := newAllocator(config.Config{}.IDPrefixes())
	if _, err := a.reserve("ci", owner, "vm"); !errors.Is(err, ErrInvalid) {
		t.Fatalf("invalid owner reserve = %v", err)
	}
	if a.high["vm"] != 0 {
		t.Fatal("invalid owner consumed a reservation")
	}
}
