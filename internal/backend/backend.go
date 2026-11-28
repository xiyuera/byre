// Package backend defines what byre needs from the outside world and nothing more:
// how to obtain an instance, how to reach it, and how to give it back.
//
// A driver never leaks its provider's types upward. Whatever a "sandbox" is, the gateway only ever
// sees an Instance.
package backend

import (
	"context"
	"errors"
	"fmt"
	"time"
)

// Instance is one leaseable SSH target.
type Instance struct {
	// ID is stable for the lifetime of the instance and unique within its backend.
	// It ends up in logs and in annotations, so it must not contain identity material.
	ID string

	// UID is the identity of the *object*, not of its name. Names are reused: a pod called
	// byre-a1b2c3 that is deleted and recreated is a different instance with different state, and a
	// lease that remembers only the name would hand the new object to a client that expects the old
	// one. Backends that have no such identity (a static host list) leave it empty.
	UID string

	// Addr is host:port the gateway dials.
	Addr string

	// User and PrivateKey are the gateway's own credentials for that instance.
	User       string
	PrivateKey string

	// HostKeyPolicy is one of strict, accept-new, insecure.
	HostKeyPolicy string

	// Annotations are durable hints used to adopt instances after a restart.
	Annotations map[string]string

	// Created is when the object behind the instance was created, when the driver can say. It lets a
	// sweeper tell "abandoned" from "a create that is still in flight": a driver that cannot report it
	// leaves the zero value, and nothing is destroyed on a guess.
	Created time.Time
}

// Validate rejects an instance that cannot be dialled.
func (i Instance) Validate() error {
	var errs []error
	if i.ID == "" {
		errs = append(errs, errors.New("ID is empty"))
	}
	if i.Addr == "" {
		errs = append(errs, errors.New("Addr is empty"))
	}
	if i.User == "" {
		errs = append(errs, errors.New("User is empty"))
	}
	if i.PrivateKey == "" {
		errs = append(errs, errors.New("PrivateKey is empty"))
	}
	switch i.HostKeyPolicy {
	case "strict", "accept-new", "insecure":
	default:
		errs = append(errs, fmt.Errorf("unknown HostKeyPolicy %q", i.HostKeyPolicy))
	}
	return errors.Join(errs...)
}

// Backend is a driver.
//
// Acquire must return an instance that the caller may use exclusively until Release. Implementations
// that share physical capacity (a static host pool) must not hand the same instance to two callers.
type Backend interface {
	// Name is the configuration name of this backend.
	Name() string

	// Acquire returns an instance exclusively for identity, creating one if needed.
	Acquire(ctx context.Context, identity string) (Instance, error)

	// Release returns an instance to the driver. It must be idempotent.
	Release(ctx context.Context, in Instance) error

	// List returns instances this driver currently believes it owns.
	List(ctx context.Context) ([]Instance, error)

	// Adopt re-establishes bookkeeping for instances found after a restart.
	Adopt(ctx context.Context, ins []Instance) error
}

// Binder is implemented by drivers whose instances can record which identity holds them.
//
// It exists because a pooled instance is created *before* an identity asks for it: without a bind step
// the object would never carry the identity, and a restart could not tell a live lease from stale
// capacity.
type Binder interface {
	Bind(ctx context.Context, in Instance, identity string) error
}

// StaleCapacity is implemented by drivers whose List result is authoritative about identity: every
// instance they have leased out carries LabelIdentity, so a listed instance without that annotation is
// capacity left over from an earlier process rather than somebody's live lease.
//
// The command driver deliberately does not implement this. Its lister is an arbitrary program, and one
// written before the annotation contract existed returns instances with no annotations at all; treating
// those as stale is what made a gateway restart destroy instances an agent was still connected to (see
// ADR-0009). For a driver that makes no such promise an anonymous instance is kept and reported, and
// reclaiming it stays a human decision.
type StaleCapacity interface {
	// AnonymousInstancesAreStale reports whether releasing a listed instance that carries no
	// LabelIdentity annotation is safe.
	AnonymousInstancesAreStale() bool
}

// Liveness is implemented by drivers whose instances can be asked, one at a time, whether the object
// behind them still exists.
//
// It exists because a lease is remembered per identity and reused for every reconnect: when the object
// behind a lease disappears, the reuse path keeps handing out an address nobody answers on until
// ttl+grace expires. Answering "is this object still there" is a single-object read, which is why it is
// a separate method and not a filter over List: the reconnect path must not pay for the inventory.
//
// Alive returns nil when the instance still exists, an error wrapping ErrInstanceGone when it is
// definitively gone, and any other error when the answer is unknown. The distinction is load-bearing:
// "gone" means the lease must be dropped and a replacement provisioned; "unknown" means the lease must
// be kept, because a lease that is dropped is a client whose instance — and its files — are gone.
type Liveness interface {
	Alive(ctx context.Context, in Instance) error
}

// Refresher is implemented by drivers whose instance can be replaced behind a stable handle, so the
// address an instance was created with is not necessarily the address to dial now.
//
// It exists because a lease is remembered per identity and reused for every reconnect, and a claim is a
// durable name for something a controller may rebuild: deleting the object a claim selected and letting
// the controller create a replacement leaves the claim — and therefore the lease — valid, while the
// endpoint in status moves to the new object. Liveness cannot answer that question, because the claim
// never disappeared; without this, every reconnect is dialled at a name that no longer resolves, until
// the lease expires.
//
// Refresh returns the instance as it is now, with the same ID, or an error. An error wrapping
// ErrInstanceGone means the handle itself is gone and the lease must be dropped; any other error is
// "unknown", and the caller keeps the address it has, because replacing a working address with nothing
// would cut off a client that may be fine.
type Refresher interface {
	Refresh(ctx context.Context, in Instance) (Instance, error)
}

// DeletionWatcher is implemented by drivers that can report an instance disappearing as it happens.
//
// It exists because Alive() runs on specific paths only: an object deleted while a client is idle
// leaves a lease pointing at a dead address until ttl+grace expires, and every reconnect pays a
// failed dial first. A watch closes that window by pushing the event instead of waiting for a probe.
//
// A watch is not a replacement for Alive(). Watches expire, and a driver that cannot tell "the watch
// ended" from "nothing happened" would go quiet without saying so. Alive() remains the backstop for
// the paths that can afford a round trip.
type DeletionWatcher interface {
	// WatchDeletions streams the identifiers of instances that no longer exist. The channel is
	// closed when ctx is done or the underlying watch ends; a caller that wants continuous coverage
	// re-subscribes.
	WatchDeletions(ctx context.Context) (<-chan string, error)
}

// ErrInstanceGone is returned (wrapped) by a Liveness check when the object behind an instance is
// definitively absent: deleted, or replaced by a different object under the same name. It is not
// "cannot be reached" — a live instance whose sshd is wedged is a different failure, and the caller
// must not treat the two the same way.
var ErrInstanceGone = errors.New("instance no longer exists")

// ErrNoCapacity is returned by Acquire when the driver is exhausted.
var ErrNoCapacity = errors.New("no capacity")
