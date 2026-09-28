package gateway

import (
	"testing"

	"github.com/bk201/harvester-runner-gateway/internal/auth"
	"github.com/bk201/harvester-runner-gateway/internal/config"
)

func TestAllocatorUsesGlobalPerKindHexSequences(t *testing.T) {
	owner := auth.Owner{RepositoryID: "123", RunID: "456", RunAttempt: "2"}
	other := auth.Owner{RepositoryID: "999", RunID: "789", RunAttempt: "1"}
	a := newAllocator(config.Config{}.IDPrefixes())
	for _, tc := range []struct {
		namespace  string
		owner      auth.Owner
		kind, want string
	}{
		{"ci", owner, "vm", "ci-vm-00000001"},
		{"ci", owner, "vm", "ci-vm-00000002"},
		{"other", other, "vm", "ci-vm-00000003"},
		{"ci", owner, "volume", "ci-vol-00000001"},
		{"other", other, "volume", "ci-vol-00000002"},
	} {
		id, err := a.reserve(tc.namespace, tc.owner, tc.kind)
		if err != nil || id != tc.want {
			t.Fatalf("reserve %s = %q, %v; want %q", tc.kind, id, err, tc.want)
		}
	}
	a.high["vm"] = 0xfffffffe
	id, err := a.reserve("ci", owner, "vm")
	if err != nil || id != "ci-vm-ffffffff" {
		t.Fatalf("last eight-digit ID = %q, %v", id, err)
	}
	id, err = a.reserve("ci", owner, "vm")
	if err != nil || id != "ci-vm-100000000" {
		t.Fatalf("width growth = %q, %v", id, err)
	}
}

func TestAllocatorCustomPrefixesAndCanonicalParsing(t *testing.T) {
	prefixes := config.IDPrefixes{VM: "build-vm-", Volume: "build-disk-"}
	owner := auth.Owner{RepositoryID: "123", RunID: "456", RunAttempt: "1"}
	a := newAllocator(prefixes)
	id, err := a.reserve("ci", owner, "vm")
	if err != nil || id != "build-vm-00000001" {
		t.Fatalf("custom prefix = %q, %v", id, err)
	}
	if n, ok := ParseResourceID(prefixes.VM, id); !ok || n != firstSequence {
		t.Fatalf("parse %q = %d, %t", id, n, ok)
	}
	for _, invalid := range []string{"ci-vm-00000001", "build-vm-0000000A", "build-vm-0000000001", "build-vm-00000000", "build-vm-0000000g", "ci-123-456-a1-001"} {
		if _, ok := ParseResourceID(prefixes.VM, invalid); ok {
			t.Errorf("accepted %q", invalid)
		}
	}
}

func TestAllocatorRecoversGlobalHighWaterMarks(t *testing.T) {
	owner := auth.Owner{RepositoryID: "123", RunID: "456", RunAttempt: "1"}
	other := auth.Owner{RepositoryID: "999", RunID: "789", RunAttempt: "2"}
	a := newAllocator(config.Config{}.IDPrefixes())
	observations := []AllocationObservation{
		{Namespace: "ci", Owner: owner, Kind: "vm", ID: "ci-vm-00000011"},
		{Namespace: "other", Owner: other, Kind: "vm", ID: "ci-vm-00000013"},
		{Namespace: "other", Owner: other, Kind: "vm", ID: "ci-vm-00000013"},
		{Namespace: "ci", Owner: owner, Kind: "volume", ID: "ci-vol-00000020"},
	}
	if err := a.recover(observations); err != nil {
		t.Fatal(err)
	}
	vm, err := a.reserve("ci", owner, "vm")
	if err != nil || vm != "ci-vm-00000014" {
		t.Fatalf("recovered VM = %q, %v", vm, err)
	}
	volume, err := a.reserve("other", other, "volume")
	if err != nil || volume != "ci-vol-00000021" {
		t.Fatalf("recovered volume = %q, %v", volume, err)
	}
	if err := a.recover(observations[:1]); err != nil {
		t.Fatal(err)
	}
	next, err := a.reserve("ci", owner, "vm")
	if err != nil || next != "ci-vm-00000015" {
		t.Fatalf("rescan lowered counter: %q, %v", next, err)
	}
	invalid := append([]AllocationObservation{}, observations...)
	invalid = append(invalid, AllocationObservation{Namespace: "ci", Owner: owner, Kind: "vm", ID: "ci-vol-00000030"})
	if err := a.recover(invalid); err == nil {
		t.Fatal("accepted a wrong-kind allocation")
	}
	next, _ = a.reserve("ci", owner, "vm")
	if next != "ci-vm-00000016" {
		t.Fatalf("failed recovery advanced counter: %q", next)
	}
}
