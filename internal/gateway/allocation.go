package gateway

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"math"
	"regexp"
	"strconv"
	"sync"

	"k8s.io/apimachinery/pkg/util/validation"

	"github.com/bk201-org/harvester-runner-gateway/internal/auth"
)

const maxPublicIDLength = 58

var decimalIDPattern = regexp.MustCompile(`^[0-9]+$`)

var publicIDPattern = regexp.MustCompile(`^ci-([0-9]+)-([0-9]+|local-smoke)-a([0-9]+)-([0-9]{3,})$`)

// ResourceMetadata is persisted on every object which can recover an
// allocation after a gateway restart.
type ResourceMetadata struct {
	IdentityHash string
	RequestHash  string
}

// AllocationObservation describes one surviving Kubernetes object. Several
// observations can describe the same VM allocation (VM, VMI, root and Secret).
type AllocationObservation struct {
	Namespace string
	Owner     auth.Owner
	Kind      string
	ID        string
	Metadata  ResourceMetadata
}

type allocationScope struct {
	Namespace string
	Owner     auth.Owner
	Kind      string
}

type allocationKey struct {
	Scope    allocationScope
	Identity string
}

type allocator struct {
	mu       sync.Mutex
	high     map[allocationScope]uint64
	identity map[allocationKey]string
}

func newAllocator() *allocator {
	return &allocator{high: map[allocationScope]uint64{}, identity: map[allocationKey]string{}}
}

func allocationIdentity(owner auth.Owner, kind, key string) string {
	// JSON gives this tuple an unambiguous, stable encoding without retaining
	// the raw idempotency key in Kubernetes.
	encoded, _ := json.Marshal(struct {
		RepositoryID string `json:"repositoryID"`
		RunID        string `json:"runID"`
		RunAttempt   string `json:"runAttempt"`
		Kind         string `json:"kind"`
		Key          string `json:"key"`
	}{owner.RepositoryID, owner.RunID, owner.RunAttempt, kind, key})
	digest := sha256.Sum256(encoded)
	return hex.EncodeToString(digest[:])
}

func requestHash(request any) string {
	data, _ := json.Marshal(request)
	digest := sha256.Sum256(data)
	return hex.EncodeToString(digest[:])
}

func scopeFor(namespace string, owner auth.Owner, kind string) allocationScope {
	return allocationScope{Namespace: namespace, Owner: owner, Kind: kind}
}

func (a *allocator) lookup(namespace string, owner auth.Owner, kind, identity string) (string, bool) {
	a.mu.Lock()
	defer a.mu.Unlock()
	id, ok := a.identity[allocationKey{Scope: scopeFor(namespace, owner, kind), Identity: identity}]
	return id, ok
}

func (a *allocator) reserve(namespace string, owner auth.Owner, kind, identity string) (string, error) {
	a.mu.Lock()
	defer a.mu.Unlock()
	scope := scopeFor(namespace, owner, kind)
	key := allocationKey{Scope: scope, Identity: identity}
	if id, ok := a.identity[key]; ok {
		return id, nil
	}
	if a.high[scope] == math.MaxUint64 {
		return "", fmt.Errorf("%w: resource sequence exhausted", ErrInvalid)
	}
	next := a.high[scope] + 1
	id, err := formatPublicID(owner, next)
	if err != nil {
		return "", err
	}
	a.high[scope] = next
	a.identity[key] = id
	return id, nil
}

func formatPublicID(owner auth.Owner, sequence uint64) (string, error) {
	if !decimalIDPattern.MatchString(owner.RepositoryID) || !decimalIDPattern.MatchString(owner.RunAttempt) ||
		(owner.RunID != "local-smoke" && !decimalIDPattern.MatchString(owner.RunID)) {
		return "", fmt.Errorf("%w: invalid resource owner", ErrInvalid)
	}
	if sequence == 0 {
		return "", fmt.Errorf("%w: resource sequence must be positive", ErrInvalid)
	}
	id := fmt.Sprintf("ci-%s-%s-a%s-%03d", owner.RepositoryID, owner.RunID, owner.RunAttempt, sequence)
	if len(id) > maxPublicIDLength || len(validation.IsDNS1123Label(id)) != 0 {
		return "", fmt.Errorf("%w: generated resource name is too long or invalid", ErrInvalid)
	}
	return id, nil
}

func ParseResourceID(id string) (auth.Owner, uint64, bool) {
	if len(id) > maxPublicIDLength || len(validation.IsDNS1123Label(id)) != 0 {
		return auth.Owner{}, 0, false
	}
	parts := publicIDPattern.FindStringSubmatch(id)
	if parts == nil {
		return auth.Owner{}, 0, false
	}
	sequence, err := strconv.ParseUint(parts[4], 10, 64)
	if err != nil || sequence == 0 || fmt.Sprintf("%03d", sequence) != parts[4] {
		return auth.Owner{}, 0, false
	}
	return auth.Owner{RepositoryID: parts[1], RunID: parts[2], RunAttempt: parts[3]}, sequence, true
}

func validIdentityHash(value string) bool {
	decoded, err := hex.DecodeString(value)
	return err == nil && len(decoded) == sha256.Size && len(value) == sha256.Size*2
}

func (a *allocator) recover(observations []AllocationObservation) error {
	type recoveredIdentity struct {
		ID          string
		RequestHash string
	}
	high := map[allocationScope]uint64{}
	identities := map[allocationKey]recoveredIdentity{}
	ids := map[allocationScope]map[string]ResourceMetadata{}
	for _, observation := range observations {
		if observation.Namespace == "" || (observation.Kind != "vm" && observation.Kind != "volume") ||
			!validIdentityHash(observation.Metadata.IdentityHash) || !validIdentityHash(observation.Metadata.RequestHash) {
			return fmt.Errorf("invalid allocation metadata for %q", observation.ID)
		}
		nameOwner, sequence, ok := ParseResourceID(observation.ID)
		if !ok || nameOwner != observation.Owner {
			return fmt.Errorf("invalid allocation name or owner for %q", observation.ID)
		}
		scope := scopeFor(observation.Namespace, observation.Owner, observation.Kind)
		key := allocationKey{Scope: scope, Identity: observation.Metadata.IdentityHash}
		if previous, exists := identities[key]; exists &&
			(previous.ID != observation.ID || previous.RequestHash != observation.Metadata.RequestHash) {
			return fmt.Errorf("allocation identity has conflicting IDs or request hashes")
		}
		if ids[scope] == nil {
			ids[scope] = map[string]ResourceMetadata{}
		}
		if previous, exists := ids[scope][observation.ID]; exists && previous != observation.Metadata {
			return fmt.Errorf("allocation ID %q has conflicting metadata", observation.ID)
		}
		identities[key] = recoveredIdentity{ID: observation.ID, RequestHash: observation.Metadata.RequestHash}
		ids[scope][observation.ID] = observation.Metadata
		if sequence > high[scope] {
			high[scope] = sequence
		}
	}

	a.mu.Lock()
	defer a.mu.Unlock()
	for key, recovered := range identities {
		if previous, ok := a.identity[key]; ok && previous != recovered.ID {
			return fmt.Errorf("recovered identity conflicts with in-memory allocation %q", previous)
		}
	}
	// Publish only after the complete scan validates, and merge so a rescan can
	// never lower a live process's high-water marks or discard reservations.
	for scope, sequence := range high {
		if sequence > a.high[scope] {
			a.high[scope] = sequence
		}
	}
	for key, recovered := range identities {
		a.identity[key] = recovered.ID
	}
	return nil
}
