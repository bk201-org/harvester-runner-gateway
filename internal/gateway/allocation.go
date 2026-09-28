package gateway

import (
	"context"
	"fmt"
	"math"
	"strconv"
	"strings"
	"sync"

	"k8s.io/apimachinery/pkg/util/validation"

	"github.com/bk201-org/harvester-runner-gateway/internal/auth"
	"github.com/bk201-org/harvester-runner-gateway/internal/config"
)

const firstSequence uint64 = 1
const maxPublicIDLength = 63

// AllocationObservation describes one surviving Kubernetes object. Several
// observations can describe the same VM allocation (VM, VMI, root and Secret).
type AllocationObservation struct {
	Namespace string
	Owner     auth.Owner
	Kind      string
	ID        string
}

type allocator struct {
	mu       sync.Mutex
	high     map[string]uint64
	prefixes config.IDPrefixes
	store    AllocationStore
}

// AllocationStore persists reservations. Other database implementations can
// replace SQLite without changing the allocator or Kubernetes backend.
type AllocationStore interface {
	Reserve(context.Context, string, auth.Owner, string, uint64) (string, uint64, error)
	Close() error
}

func newAllocator(prefixes config.IDPrefixes) *allocator {
	return &allocator{high: map[string]uint64{}, prefixes: prefixes}
}

func validOwner(owner auth.Owner) bool {
	return decimal(owner.RepositoryID) && decimal(owner.RunAttempt) &&
		(owner.RunID == "local-smoke" || decimal(owner.RunID))
}

func decimal(s string) bool {
	if s == "" {
		return false
	}
	for _, c := range s {
		if c < '0' || c > '9' {
			return false
		}
	}
	return true
}

func (a *allocator) reserve(namespace string, owner auth.Owner, kind string) (string, error) {
	return a.reserveContext(context.Background(), namespace, owner, kind)
}

func (a *allocator) reserveContext(ctx context.Context, namespace string, owner auth.Owner, kind string) (string, error) {
	if namespace == "" || !validOwner(owner) || a.prefixes.ForKind(kind) == "" {
		return "", fmt.Errorf("%w: invalid allocation metadata", ErrInvalid)
	}
	a.mu.Lock()
	defer a.mu.Unlock()
	if a.store != nil {
		id, sequence, err := a.store.Reserve(ctx, namespace, owner, kind, a.high[kind])
		if err != nil {
			return "", err
		}
		a.high[kind] = sequence
		return id, nil
	}
	next := max(a.high[kind], firstSequence-1) + 1
	if next > math.MaxInt64 {
		return "", fmt.Errorf("%w: resource sequence exhausted", ErrInvalid)
	}
	id, err := formatPublicID(a.prefixes.ForKind(kind), next)
	if err != nil {
		return "", err
	}
	a.high[kind] = next
	return id, nil
}

func formatPublicID(prefix string, sequence uint64) (string, error) {
	if prefix == "" || sequence < firstSequence || sequence > math.MaxInt64 {
		return "", fmt.Errorf("%w: invalid resource sequence", ErrInvalid)
	}
	id := fmt.Sprintf("%s%08x", prefix, sequence)
	if len(id) > maxPublicIDLength || len(validation.IsDNS1123Label(id)) != 0 {
		return "", fmt.Errorf("%w: generated resource name is too long or invalid", ErrInvalid)
	}
	return id, nil
}

func ParseResourceID(prefix, id string) (uint64, bool) {
	if prefix == "" || !strings.HasPrefix(id, prefix) || len(id) > maxPublicIDLength ||
		len(validation.IsDNS1123Label(id)) != 0 {
		return 0, false
	}
	sequence, err := strconv.ParseUint(strings.TrimPrefix(id, prefix), 16, 64)
	if err != nil || sequence < firstSequence || sequence > math.MaxInt64 {
		return 0, false
	}
	canonical, err := formatPublicID(prefix, sequence)
	return sequence, err == nil && id == canonical
}

func (a *allocator) recover(observations []AllocationObservation) error {
	high := map[string]uint64{}
	for _, observation := range observations {
		prefix := a.prefixes.ForKind(observation.Kind)
		if observation.Namespace == "" || !validOwner(observation.Owner) || prefix == "" {
			return fmt.Errorf("invalid allocation metadata for %q", observation.ID)
		}
		sequence, ok := ParseResourceID(prefix, observation.ID)
		if !ok {
			return fmt.Errorf("invalid allocation name for %q", observation.ID)
		}
		if sequence > high[observation.Kind] {
			high[observation.Kind] = sequence
		}
	}
	a.mu.Lock()
	defer a.mu.Unlock()
	for kind, sequence := range high {
		if sequence > a.high[kind] {
			a.high[kind] = sequence
		}
	}
	return nil
}
