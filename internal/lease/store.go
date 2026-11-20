package lease

import (
	"context"
	"errors"
	"fmt"

	"byre/internal/lease/crd"
)

// StoredBinding is the part of a lease that must survive a restart and be visible to a second
// replica. It is deliberately narrower than Entry: connection counts, the idle clock, pool state and
// metrics are process-local and stay in memory (ADR-0013).
type StoredBinding struct {
	Identity string
	// Name is the stable identity name from `identities[].name`, when configured. A fingerprint can
	// be rotated; this survives it, which is what a persistent home is named after.
	Name        string
	Backend     string
	Instance    string
	InstanceUID string

	// Phase is the persisted lifecycle state, empty for stores that have no such notion (the in-memory
	// case). Recovery reads it to tell a live binding from one being drained -- handing out an instance
	// that is already on its way out is the race ADR-0012 exists to close, and a recovered binding has
	// no in-process memory to consult.
	Phase string

	// RV is the store's optimistic-concurrency token, empty when the store has none. Recovery carries
	// it forward so a conditional write cannot resurrect a binding somebody else deleted meanwhile.
	RV string
}

// LeaseStore persists bindings. Implementations must be safe for concurrent use.
//
// The interface is this small on purpose. A store that also wants to know about connections, or
// timestamps, or phases, has drifted from what the API server is for: the hot path performs no
// writes at all, and Acquire/Release are the only callers.
type LeaseStore interface {
	// Put records a new binding. An error wrapping ErrBindingExists means another party already holds
	// this identity: the caller lost the race and must re-read rather than retry.
	Put(ctx context.Context, b StoredBinding) error
	// Delete forgets a binding. Deleting an absent one is not an error.
	Delete(ctx context.Context, identity string) error
	// List returns every binding, for startup.
	List(ctx context.Context) ([]StoredBinding, error)
}

// ErrBindingExists is returned (wrapped) by LeaseStore.Put when the identity is already bound.
var ErrBindingExists = errors.New("lease: binding exists")

// crdStore adapts crd.Store to LeaseStore: same operations, narrower types, and the "already exists"
// sentinel translated into this package's vocabulary so callers do not import the crd package just to
// tell the two apart.
type crdStore struct{ inner *crd.Store }

// NewCRDStore exposes a crd.Store as a LeaseStore.
func NewCRDStore(s *crd.Store) LeaseStore { return &crdStore{inner: s} }

func (c *crdStore) Put(ctx context.Context, b StoredBinding) error {
	// Phase is passed through rather than forced to Leased. It is written through the status
	// subresource, so this call cannot change it either way -- but pretending it can would be a lie the
	// next reader has to un-learn when part 4 starts moving leases to Draining.
	_, err := c.inner.Put(ctx, crd.Binding{
		Identity:    b.Identity,
		Name:        b.Name,
		Backend:     b.Backend,
		Instance:    b.Instance,
		InstanceUID: b.InstanceUID,
		Phase:       b.Phase,
	})
	if err == nil {
		return nil
	}
	if errors.Is(err, crd.ErrExists) {
		return fmt.Errorf("%w: %s", ErrBindingExists, b.Identity)
	}
	return err
}

func (c *crdStore) Delete(ctx context.Context, identity string) error {
	return c.inner.Delete(ctx, identity)
}

func (c *crdStore) List(ctx context.Context) ([]StoredBinding, error) {
	all, err := c.inner.List(ctx)
	if err != nil {
		return nil, err
	}
	out := make([]StoredBinding, 0, len(all))
	for _, b := range all {
		out = append(out, StoredBinding{
			Identity:    b.Identity,
			Name:        b.Name,
			Backend:     b.Backend,
			Instance:    b.Instance,
			InstanceUID: b.InstanceUID,
			Phase:       b.Phase,
			RV:          b.RV,
		})
	}
	return out, nil
}
