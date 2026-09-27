package gateway

import (
	"errors"
	"strings"
	"testing"

	"github.com/bk201-org/harvester-runner-gateway/internal/auth"
)

func TestAllocatorSequencesScopesKindsAndWidth(t *testing.T) {
	owner := auth.Owner{RepositoryID: "123", RunID: "456", RunAttempt: "2"}
	a := newAllocator()
	first, err := a.reserve("ci", owner, "vm")
	if err != nil || first != "ci-123-456-a2-001" {
		t.Fatalf("first = %q, %v", first, err)
	}
	second, _ := a.reserve("ci", owner, "vm")
	if second != "ci-123-456-a2-002" {
		t.Fatalf("second = %q", second)
	}
	volume, _ := a.reserve("ci", owner, "volume")
	if volume != first {
		t.Fatalf("independent volume sequence = %q, want %q", volume, first)
	}
	scope := scopeFor("ci", owner, "vm")
	a.high[scope] = 998
	got999, _ := a.reserve("ci", owner, "vm")
	got1000, _ := a.reserve("ci", owner, "vm")
	if got999 != "ci-123-456-a2-999" || got1000 != "ci-123-456-a2-1000" {
		t.Fatalf("width transition = %q, %q", got999, got1000)
	}
	local := auth.Owner{RepositoryID: "123", RunID: "local-smoke", RunAttempt: "1"}
	localID, err := a.reserve("ci", local, "vm")
	if err != nil || localID != "ci-123-local-smoke-a1-001" {
		t.Fatalf("local smoke = %q, %v", localID, err)
	}
}

func TestAllocatorRejectsOversizedNameBeforeReservation(t *testing.T) {
	owner := auth.Owner{RepositoryID: strings.Repeat("1", 30), RunID: strings.Repeat("2", 30), RunAttempt: "1"}
	a := newAllocator()
	if _, err := a.reserve("ci", owner, "vm"); !errors.Is(err, ErrInvalid) {
		t.Fatalf("oversized reserve = %v", err)
	}
	if got := a.high[scopeFor("ci", owner, "vm")]; got != 0 {
		t.Fatalf("failed reservation advanced state: high=%d", got)
	}
}

func TestAllocatorRecoversHighWaterMarks(t *testing.T) {
	owner := auth.Owner{RepositoryID: "123", RunID: "456", RunAttempt: "1"}
	a := newAllocator()
	observations := []AllocationObservation{
		{Namespace: "ci", Owner: owner, Kind: "vm", ID: "ci-123-456-a1-001"},
		{Namespace: "ci", Owner: owner, Kind: "vm", ID: "ci-123-456-a1-003"},
		// A dependent object repeats the same parent allocation.
		{Namespace: "ci", Owner: owner, Kind: "vm", ID: "ci-123-456-a1-003"},
	}
	if err := a.recover(observations); err != nil {
		t.Fatal(err)
	}
	next, err := a.reserve("ci", owner, "vm")
	if err != nil || next != "ci-123-456-a1-004" {
		t.Fatalf("next = %q, %v", next, err)
	}
	if err := a.recover(observations[:1]); err != nil {
		t.Fatal(err)
	}
	afterRescan, _ := a.reserve("ci", owner, "vm")
	if afterRescan != "ci-123-456-a1-005" {
		t.Fatalf("after rescan = %q", afterRescan)
	}
	invalid := append([]AllocationObservation{}, observations...)
	invalid = append(invalid, AllocationObservation{Namespace: "ci", Owner: owner, Kind: "vm", ID: "ci-999-456-a1-010"})
	if err := a.recover(invalid); err == nil {
		t.Fatal("accepted an allocation with a mismatched owner")
	}
	afterFailure, _ := a.reserve("ci", owner, "vm")
	if afterFailure != "ci-123-456-a1-006" {
		t.Fatalf("failed recovery advanced high-water mark: %q", afterFailure)
	}
}
