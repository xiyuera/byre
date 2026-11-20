// Package crd persists the identity -> instance binding as a ByreLease custom resource.
//
// This is the layer ADR-0013 asks for: the binding is the one fact every reader must agree on, so it
// lives in the API server instead of in one process's memory. Everything else in the lease manager
// (connection counts, the idle clock, pool state) stays in memory, which is why this package has no
// idea what a connection is.
//
// The design constraint that shapes every method: the hot path must not write. `Touch`-style updates
// do not exist here on purpose. Writes happen when a lease is created, rebound or released, and a
// caller that finds itself wanting to persist something per keystroke has found a bug.
package crd

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"time"

	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/runtime/schema"
	"k8s.io/apimachinery/pkg/watch"
	"k8s.io/client-go/dynamic"
)

// Phase values. Leased and Draining are the only two states a persisted binding can be in;
// "pending" (an instance being created) is deliberately not persisted, because it describes work in
// one process that no other replica could act on.
const (
	PhaseLeased   = "Leased"
	PhaseDraining = "Draining"
)

// Errors callers branch on. They are deliberately distinct: ErrNotFound means "rebind from
// scratch", ErrExists means "someone else won the race, re-read", ErrConflict means "the object
// changed under us, re-read and retry".
var (
	ErrNotFound = errors.New("lease: no binding for identity")
	ErrExists   = errors.New("lease: binding already exists")
	ErrConflict = errors.New("lease: binding changed under us")
)

// Binding is one row of the lease table.
type Binding struct {
	Identity    string
	Name        string
	Backend     string
	Instance    string
	InstanceUID string
	Phase       string

	// RV is the resourceVersion this Binding was read at. Update and SetPhase send it back, which is
	// what makes them compare-and-swap rather than last-writer-wins.
	RV string
	// Created is the object's creationTimestamp; zero for a Binding that has not been written yet.
	Created time.Time
}

// Event is one watch notification about a lease.
type Event struct {
	Type  watch.EventType
	Lease Binding
}

// GVR is the resource this package talks to. Fixed rather than configured: the CRD ships with byre,
// and a deployment that needs a different group is a fork, not a setting.
func GVR() schema.GroupVersionResource {
	return schema.GroupVersionResource{Group: "byre.io", Version: "v1alpha1", Resource: "byreleases"}
}

// ObjectName is the deterministic metadata.name for an identity.
//
// A fingerprint is "SHA256:base64url" — it contains ':' and '/' and cannot be an object name. Hashing
// it keeps the name stable across restarts and short enough for the 253-character limit, and makes
// Get an O(1) call instead of a List. The fingerprint itself is in spec.identity, so nothing is lost.
func ObjectName(identity string) string {
	sum := sha256.Sum256([]byte(identity))
	return "byre-" + hex.EncodeToString(sum[:8])
}

// Store reads and writes bindings.
type Store struct {
	dyn dynamic.Interface
	ns  string
	gvr schema.GroupVersionResource
}

// New builds a store over a namespace.
func New(dyn dynamic.Interface, namespace string) *Store {
	return &Store{dyn: dyn, ns: namespace, gvr: GVR()}
}

// Namespace reports the namespace the store writes to.
func (s *Store) Namespace() string { return s.ns }

// Get returns the binding for an identity.
func (s *Store) Get(ctx context.Context, identity string) (Binding, error) {
	obj, err := s.dyn.Resource(s.gvr).Namespace(s.ns).Get(ctx, ObjectName(identity), metav1.GetOptions{})
	if apierrors.IsNotFound(err) {
		return Binding{}, ErrNotFound
	}
	if err != nil {
		return Binding{}, fmt.Errorf("lease: get %s: %w", ObjectName(identity), err)
	}
	return fromObject(obj), nil
}

// Put writes a new binding. It returns ErrExists if one is already there, which is the CAS a second
// replica loses: the loser re-reads rather than overwriting the winner.
func (s *Store) Put(ctx context.Context, b Binding) (Binding, error) {
	if b.Identity == "" || b.Backend == "" || b.Instance == "" {
		return Binding{}, fmt.Errorf("lease: binding needs identity, backend and instance")
	}
	if b.Phase == "" {
		b.Phase = PhaseLeased
	}
	obj := toObject(b, s.ns)
	created, err := s.dyn.Resource(s.gvr).Namespace(s.ns).Create(ctx, obj, metav1.CreateOptions{})
	if apierrors.IsAlreadyExists(err) {
		return Binding{}, ErrExists
	}
	if err != nil {
		return Binding{}, fmt.Errorf("lease: create %s: %w", obj.GetName(), err)
	}
	return fromObject(created), nil
}

// Update rewrites the spec of an existing binding, compare-and-swapping on b.RV.
//
// A caller that did not read the object first has no RV and gets ErrConflict from the server, which
// is the correct outcome: it means "re-read before deciding".
func (s *Store) Update(ctx context.Context, b Binding) (Binding, error) {
	cur, err := s.Get(ctx, b.Identity)
	if err != nil {
		return Binding{}, err
	}
	if b.RV != "" && cur.RV != b.RV {
		return Binding{}, ErrConflict
	}
	obj := toObject(b, s.ns)
	obj.SetResourceVersion(cur.RV)
	updated, err := s.dyn.Resource(s.gvr).Namespace(s.ns).Update(ctx, obj, metav1.UpdateOptions{})
	if apierrors.IsConflict(err) {
		return Binding{}, ErrConflict
	}
	if err != nil {
		return Binding{}, fmt.Errorf("lease: update %s: %w", ObjectName(b.Identity), err)
	}
	return fromObject(updated), nil
}

// SetPhase writes status.phase (and an optional human-readable message) through the status
// subresource, so a Draining transition cannot clobber a concurrent spec change.
func (s *Store) SetPhase(ctx context.Context, identity, phase, message string) error {
	switch phase {
	case PhaseLeased, PhaseDraining:
	default:
		return fmt.Errorf("lease: unknown phase %q", phase)
	}
	cur, err := s.Get(ctx, identity)
	if err != nil {
		return err
	}
	obj := toObject(cur, s.ns)
	status := map[string]any{"phase": phase}
	if message != "" {
		status["message"] = message
	}
	status["lastTransitionTime"] = time.Now().UTC().Format(time.RFC3339)
	obj.Object["status"] = status
	obj.SetResourceVersion(cur.RV)
	_, err = s.dyn.Resource(s.gvr).Namespace(s.ns).UpdateStatus(ctx, obj, metav1.UpdateOptions{})
	if apierrors.IsConflict(err) {
		return ErrConflict
	}
	if apierrors.IsNotFound(err) {
		return ErrNotFound
	}
	if err != nil {
		return fmt.Errorf("lease: set phase %s: %w", ObjectName(identity), err)
	}
	return nil
}

// Delete removes a binding. Deleting one that is already gone is not an error: the caller's intent
// ("this identity holds nothing") is satisfied either way, and a retry after a dropped response must
// not fail.
func (s *Store) Delete(ctx context.Context, identity string) error {
	err := s.dyn.Resource(s.gvr).Namespace(s.ns).Delete(ctx, ObjectName(identity), metav1.DeleteOptions{})
	if apierrors.IsNotFound(err) {
		return nil
	}
	if err != nil {
		return fmt.Errorf("lease: delete %s: %w", ObjectName(identity), err)
	}
	return nil
}

// List returns every binding in the namespace. This is what startup reads; there is no "rebuild the
// table from annotations" step once the CRD is the truth.
func (s *Store) List(ctx context.Context) ([]Binding, error) {
	list, err := s.dyn.Resource(s.gvr).Namespace(s.ns).List(ctx, metav1.ListOptions{})
	if err != nil {
		return nil, fmt.Errorf("lease: list: %w", err)
	}
	out := make([]Binding, 0, len(list.Items))
	for i := range list.Items {
		out = append(out, fromObject(&list.Items[i]))
	}
	return out, nil
}

// Watch streams lease events until ctx is done. The caller owns closing behaviour: the channel is
// closed when the watch ends and is not restarted, because a silent reconnect hides exactly the
// window this mechanism exists to close. A caller that wants to survive an expired watch wraps this
// in its own relist loop.
func (s *Store) Watch(ctx context.Context) (<-chan Event, error) {
	w, err := s.dyn.Resource(s.gvr).Namespace(s.ns).Watch(ctx, metav1.ListOptions{})
	if err != nil {
		return nil, fmt.Errorf("lease: watch: %w", err)
	}
	out := make(chan Event, 32)
	go func() {
		defer close(out)
		defer w.Stop()
		for {
			select {
			case <-ctx.Done():
				return
			case ev, ok := <-w.ResultChan():
				if !ok {
					return
				}
				obj, ok := ev.Object.(*unstructured.Unstructured)
				if !ok {
					continue
				}
				select {
				case out <- Event{Type: ev.Type, Lease: fromObject(obj)}:
				case <-ctx.Done():
					return
				}
			}
		}
	}()
	return out, nil
}

// Ready reports whether the CRD is installed, so startup can fail with a sentence that names the
// missing object instead of a bare 404 on the first acquire.
func (s *Store) Ready(ctx context.Context) error {
	_, err := s.dyn.Resource(s.gvr).Namespace(s.ns).List(ctx, metav1.ListOptions{Limit: 1})
	switch {
	case apierrors.IsNotFound(err):
		return fmt.Errorf("lease: the ByreLease CRD is not installed in this cluster (apply deploy/crd/byreleases.yaml)")
	case apierrors.IsForbidden(err):
		return fmt.Errorf("lease: not allowed to list byreleases in namespace %q: %w", s.ns, err)
	case err != nil:
		return fmt.Errorf("lease: cannot reach byreleases: %w", err)
	}
	return nil
}

func toObject(b Binding, namespace string) *unstructured.Unstructured {
	spec := map[string]any{
		"identity": b.Identity,
		"backend":  b.Backend,
		"instance": b.Instance,
	}
	if b.Name != "" {
		spec["name"] = b.Name
	}
	if b.InstanceUID != "" {
		spec["instanceUID"] = b.InstanceUID
	}
	phase := b.Phase
	if phase == "" {
		phase = PhaseLeased
	}
	return &unstructured.Unstructured{Object: map[string]any{
		"apiVersion": "byre.io/v1alpha1",
		"kind":       "ByreLease",
		"metadata":   map[string]any{"name": ObjectName(b.Identity), "namespace": namespace},
		"spec":       spec,
		"status":     map[string]any{"phase": phase},
	}}
}

func fromObject(obj *unstructured.Unstructured) Binding {
	get := func(fields ...string) string {
		v, _, _ := unstructured.NestedString(obj.Object, fields...)
		return v
	}
	b := Binding{
		Identity:    get("spec", "identity"),
		Name:        get("spec", "name"),
		Backend:     get("spec", "backend"),
		Instance:    get("spec", "instance"),
		InstanceUID: get("spec", "instanceUID"),
		Phase:       get("status", "phase"),
		RV:          obj.GetResourceVersion(),
	}
	if ts := obj.GetCreationTimestamp(); !ts.IsZero() {
		b.Created = ts.Time
	}
	return b
}
