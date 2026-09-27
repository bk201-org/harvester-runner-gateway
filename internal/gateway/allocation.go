package gateway

import (
	"context"
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

// AllocationObservation describes one surviving Kubernetes object. Several
// observations can describe the same VM allocation (VM, VMI, root and Secret).
type AllocationObservation struct {
	Namespace string
	Owner     auth.Owner
	Kind      string
	ID        string
}

type allocationScope struct {
	Namespace string
	Owner     auth.Owner
	Kind      string
}

type allocator struct {
	mu    sync.Mutex
	high  map[allocationScope]uint64
	store AllocationStore
}

// AllocationStore persists reservations. Other database implementations can
// replace SQLite without changing the allocator or Kubernetes backend.
type AllocationStore interface {
	Reserve(context.Context, string, auth.Owner, string, uint64) (string, uint64, error)
	Close() error
}

func newAllocator() *allocator {
	return &allocator{high: map[allocationScope]uint64{}}
}

func scopeFor(namespace string, owner auth.Owner, kind string) allocationScope {
	return allocationScope{Namespace: namespace, Owner: owner, Kind: kind}
}

func (a *allocator) reserve(namespace string, owner auth.Owner, kind string) (string, error) {
	return a.reserveContext(context.Background(), namespace, owner, kind)
}

func (a *allocator) reserveContext(ctx context.Context, namespace string, owner auth.Owner, kind string) (string, error) {
	a.mu.Lock()
	defer a.mu.Unlock()
	scope := scopeFor(namespace, owner, kind)
	if a.store != nil {
		id, sequence, err := a.store.Reserve(ctx, namespace, owner, kind, a.high[scope])
		if err != nil {
			return "", err
		}
		a.high[scope] = sequence
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

func (a *allocator) recover(observations []AllocationObservation) error {
	high := map[allocationScope]uint64{}
	for _, observation := range observations {
		if observation.Namespace == "" || (observation.Kind != "vm" && observation.Kind != "volume") {
			return fmt.Errorf("invalid allocation metadata for %q", observation.ID)
		}
		nameOwner, sequence, ok := ParseResourceID(observation.ID)
		if !ok || nameOwner != observation.Owner {
			return fmt.Errorf("invalid allocation name or owner for %q", observation.ID)
		}
		scope := scopeFor(observation.Namespace, observation.Owner, observation.Kind)
		if sequence > high[scope] {
			high[scope] = sequence
		}
	}

	a.mu.Lock()
	defer a.mu.Unlock()
	// A rescan cannot lower a live process's high-water marks.
	for scope, sequence := range high {
		if sequence > a.high[scope] {
			a.high[scope] = sequence
		}
	}
	return nil
}
