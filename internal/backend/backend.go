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
)

// Instance is one leaseable SSH target.
type Instance struct {
	// ID is stable for the lifetime of the instance and unique within its backend.
	// It ends up in logs and in annotations, so it must not contain identity material.
	ID string

	// Addr is host:port the gateway dials.
	Addr string

	// User and PrivateKey are the gateway's own credentials for that instance.
	User       string
	PrivateKey string

	// HostKeyPolicy is one of strict, accept-new, insecure.
	HostKeyPolicy string

	// Annotations are durable hints used to adopt instances after a restart.
	Annotations map[string]string
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

// ErrNoCapacity is returned by Acquire when the driver is exhausted.
var ErrNoCapacity = errors.New("no capacity")
