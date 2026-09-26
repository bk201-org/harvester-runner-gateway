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
	firstIdentity := allocationIdentity(owner, "vm", "first")
	first, err := a.reserve("ci", owner, "vm", firstIdentity)
	if err != nil || first != "ci-123-456-a2-001" {
		t.Fatalf("first = %q, %v", first, err)
	}
	if retry, _ := a.reserve("ci", owner, "vm", firstIdentity); retry != first {
		t.Fatalf("retry = %q, want %q", retry, first)
	}
	second, _ := a.reserve("ci", owner, "vm", allocationIdentity(owner, "vm", "second"))
	if second != "ci-123-456-a2-002" {
		t.Fatalf("second = %q", second)
	}
	volume, _ := a.reserve("ci", owner, "volume", allocationIdentity(owner, "volume", "first"))
	if volume != first {
		t.Fatalf("independent volume sequence = %q, want %q", volume, first)
	}

	scope := scopeFor("ci", owner, "vm")
	a.high[scope] = 998
	got999, _ := a.reserve("ci", owner, "vm", allocationIdentity(owner, "vm", "999"))
	got1000, _ := a.reserve("ci", owner, "vm", allocationIdentity(owner, "vm", "1000"))
	if got999 != "ci-123-456-a2-999" || got1000 != "ci-123-456-a2-1000" {
		t.Fatalf("width transition = %q, %q", got999, got1000)
	}

	local := auth.Owner{RepositoryID: "123", RunID: "local-smoke", RunAttempt: "1"}
	localID, err := a.reserve("ci", local, "vm", allocationIdentity(local, "vm", "smoke"))
	if err != nil || localID != "ci-123-local-smoke-a1-001" {
		t.Fatalf("local smoke = %q, %v", localID, err)
	}
}

func TestAllocatorRejectsOversizedNameBeforeReservation(t *testing.T) {
	owner := auth.Owner{RepositoryID: strings.Repeat("1", 30), RunID: strings.Repeat("2", 30), RunAttempt: "1"}
	a := newAllocator()
	if _, err := a.reserve("ci", owner, "vm", allocationIdentity(owner, "vm", "key")); !errors.Is(err, ErrInvalid) {
		t.Fatalf("oversized reserve = %v", err)
	}
	if got := a.high[scopeFor("ci", owner, "vm")]; got != 0 || len(a.identity) != 0 {
		t.Fatalf("failed reservation advanced state: high=%d identities=%d", got, len(a.identity))
	}
}

func TestAllocatorRecoveryAndConflicts(t *testing.T) {
	owner := auth.Owner{RepositoryID: "123", RunID: "456", RunAttempt: "1"}
	identityOne := allocationIdentity(owner, "vm", "one")
	identityThree := allocationIdentity(owner, "vm", "three")
	requestOne := requestHash(VMRequest{CPU: 1})
	requestThree := requestHash(VMRequest{CPU: 3})
	a := newAllocator()
	observations := []AllocationObservation{
		{Namespace: "ci", Owner: owner, Kind: "vm", ID: "ci-123-456-a1-001", Metadata: ResourceMetadata{IdentityHash: identityOne, RequestHash: requestOne}},
		{Namespace: "ci", Owner: owner, Kind: "vm", ID: "ci-123-456-a1-003", Metadata: ResourceMetadata{IdentityHash: identityThree, RequestHash: requestThree}},
		// A dependent object repeats the same parent allocation.
		{Namespace: "ci", Owner: owner, Kind: "vm", ID: "ci-123-456-a1-003", Metadata: ResourceMetadata{IdentityHash: identityThree, RequestHash: requestThree}},
	}
	if err := a.recover(observations); err != nil {
		t.Fatal(err)
	}
	if id, ok := a.lookup("ci", owner, "vm", identityThree); !ok || id != "ci-123-456-a1-003" {
		t.Fatalf("recovered mapping = %q, %v", id, ok)
	}
	next, err := a.reserve("ci", owner, "vm", allocationIdentity(owner, "vm", "next"))
	if err != nil || next != "ci-123-456-a1-004" {
		t.Fatalf("next = %q, %v", next, err)
	}
	// A rescan containing fewer resources cannot lower the live high-water mark.
	if err := a.recover(observations[:1]); err != nil {
		t.Fatal(err)
	}
	afterRescan, _ := a.reserve("ci", owner, "vm", allocationIdentity(owner, "vm", "after"))
	if afterRescan != "ci-123-456-a1-005" {
		t.Fatalf("after rescan = %q", afterRescan)
	}

	conflict := append([]AllocationObservation{}, observations[0])
	conflict = append(conflict, AllocationObservation{Namespace: "ci", Owner: owner, Kind: "vm", ID: "ci-123-456-a1-002",
		Metadata: ResourceMetadata{IdentityHash: identityOne, RequestHash: requestOne}})
	if err := newAllocator().recover(conflict); err == nil {
		t.Fatal("accepted one identity mapped to multiple IDs")
	}
}
