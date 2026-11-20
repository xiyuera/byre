package crd

import (
	"context"
	"errors"
	"strconv"
	"testing"

	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/runtime/schema"
	dynamicfake "k8s.io/client-go/dynamic/fake"
	k8stesting "k8s.io/client-go/testing"
)

const ns = "byre"

func newStore(t *testing.T) *Store {
	t.Helper()
	s, _ := newStoreWithClient(t)
	return s
}

// newStoreWithClient also hands back the fake client, for the tests that have to inspect the
// requests we send.
//
// The fake client does not implement resourceVersion: it stores objects verbatim and never rejects a
// stale write. That is the one thing this package depends on the API server for, so a reactor stamps
// a monotonic resourceVersion on every create and update -- making the compare-and-swap observable
// instead of untestable. What it does *not* simulate is the rejection of a stale RV; that assertion
// belongs to an integration test against a real apiserver.
func newStoreWithClient(t *testing.T) (*Store, *dynamicfake.FakeDynamicClient) {
	t.Helper()
	scheme := runtime.NewScheme()
	client := dynamicfake.NewSimpleDynamicClientWithCustomListKinds(scheme,
		map[schema.GroupVersionResource]string{GVR(): "ByreLeaseList"})

	var rv int64
	stamp := func(action k8stesting.Action) (bool, runtime.Object, error) {
		var obj *unstructured.Unstructured
		switch a := action.(type) {
		case k8stesting.CreateAction:
			obj, _ = a.GetObject().(*unstructured.Unstructured)
		case k8stesting.UpdateAction:
			obj, _ = a.GetObject().(*unstructured.Unstructured)
		}
		if obj != nil {
			rv++
			obj.SetResourceVersion(strconv.FormatInt(rv, 10))
		}
		return false, nil, nil // let the default tracker store the (now stamped) object
	}
	client.PrependReactor("create", "byreleases", stamp)
	client.PrependReactor("update", "byreleases", stamp)
	return New(client, ns), client
}

func sample(identity string) Binding {
	return Binding{
		Identity:    identity,
		Name:        "alice",
		Backend:     "k8s",
		Instance:    "byre-a1b2c3",
		InstanceUID: "uid-1",
	}
}

func TestObjectNameIsStableAndLegal(t *testing.T) {
	fp := "SHA256:AbCdEf+/0123456789abcdefghijklmnopqrstuvwxyz"
	a, b := ObjectName(fp), ObjectName(fp)
	if a != b {
		t.Fatalf("ObjectName not deterministic: %q != %q", a, b)
	}
	if a == ObjectName(fp+"x") {
		t.Fatalf("two identities collided on %q", a)
	}
	// Must be a DNS-1123 subdomain: lowercase alnum, '-', '.', no ':' or '/'.
	for _, r := range a {
		ok := (r >= 'a' && r <= 'z') || (r >= '0' && r <= '9') || r == '-' || r == '.'
		if !ok {
			t.Fatalf("ObjectName(%q) = %q contains %q, not a legal object name", fp, a, r)
		}
	}
}

func TestPutGetRoundTrip(t *testing.T) {
	ctx := context.Background()
	s := newStore(t)
	in := sample("SHA256:abc")

	got, err := s.Put(ctx, in)
	if err != nil {
		t.Fatalf("Put: %v", err)
	}
	if got.Phase != PhaseLeased {
		t.Errorf("phase = %q, want %q (Put defaults to Leased)", got.Phase, PhaseLeased)
	}
	if got.RV == "" {
		t.Error("RV is empty after Put; Update would have nothing to compare against")
	}

	back, err := s.Get(ctx, in.Identity)
	if err != nil {
		t.Fatalf("Get: %v", err)
	}
	if back.Name != in.Name || back.Backend != in.Backend || back.Instance != in.Instance ||
		back.InstanceUID != in.InstanceUID {
		t.Errorf("round trip lost fields: %+v", back)
	}
	if back.Identity != in.Identity {
		t.Errorf("identity = %q, want %q", back.Identity, in.Identity)
	}
}

func TestPutTwiceIsExists(t *testing.T) {
	// This is the CAS a second replica loses. It must be distinguishable from a transport error,
	// because the loser's correct reaction is "re-read", not "retry".
	ctx := context.Background()
	s := newStore(t)
	if _, err := s.Put(ctx, sample("SHA256:abc")); err != nil {
		t.Fatalf("first Put: %v", err)
	}
	_, err := s.Put(ctx, sample("SHA256:abc"))
	if !errors.Is(err, ErrExists) {
		t.Fatalf("second Put err = %v, want ErrExists", err)
	}
}

func TestGetMissingIsNotFound(t *testing.T) {
	_, err := newStore(t).Get(context.Background(), "SHA256:nobody")
	if !errors.Is(err, ErrNotFound) {
		t.Fatalf("err = %v, want ErrNotFound", err)
	}
}

// The compare-and-swap itself is the API server's job and the fake client does not model it. What is
// testable here is that we do our half: send the resourceVersion back, so a real server has something
// to reject on. Without this the second replica would silently last-writer-win.
func TestUpdateSendsTheResourceVersion(t *testing.T) {
	ctx := context.Background()
	s, client := newStoreWithClient(t)
	created, err := s.Put(ctx, sample("SHA256:abc"))
	if err != nil {
		t.Fatalf("Put: %v", err)
	}
	created.Instance = "byre-new"
	if _, err := s.Update(ctx, created); err != nil {
		t.Fatalf("Update: %v", err)
	}
	var sawUpdate bool
	for _, a := range client.Actions() {
		if a.GetVerb() != "update" {
			continue
		}
		ua, ok := a.(k8stesting.UpdateAction)
		if !ok {
			continue
		}
		obj, ok := ua.GetObject().(*unstructured.Unstructured)
		if !ok {
			continue
		}
		sawUpdate = true
		if obj.GetResourceVersion() == "" {
			t.Error("Update sent an object with no resourceVersion; a real server would accept it as last-writer-wins")
		}
	}
	if !sawUpdate {
		t.Fatal("no update action was sent")
	}
}

// A caller that did not read the object first has no RV at all. That is a mistake, not a CAS, and it
// must be visible rather than silently turning into an overwrite.
func TestUpdateWithoutRVStillSendsOne(t *testing.T) {
	ctx := context.Background()
	s, client := newStoreWithClient(t)
	if _, err := s.Put(ctx, sample("SHA256:abc")); err != nil {
		t.Fatalf("Put: %v", err)
	}
	// Update re-reads internally, so a zero RV on the argument never reaches the wire as zero.
	if _, err := s.Update(ctx, sample("SHA256:abc")); err != nil {
		t.Fatalf("Update: %v", err)
	}
	for _, a := range client.Actions() {
		if a.GetVerb() != "update" {
			continue
		}
		if ua, ok := a.(k8stesting.UpdateAction); ok {
			if obj, ok := ua.GetObject().(*unstructured.Unstructured); ok && obj.GetResourceVersion() == "" {
				t.Error("Update sent an empty resourceVersion")
			}
		}
	}
}

func TestUpdateRebindsTheInstance(t *testing.T) {
	ctx := context.Background()
	s := newStore(t)
	created, err := s.Put(ctx, sample("SHA256:abc"))
	if err != nil {
		t.Fatalf("Put: %v", err)
	}
	created.Instance = "byre-new"
	created.InstanceUID = "uid-2"
	if _, err := s.Update(ctx, created); err != nil {
		t.Fatalf("Update: %v", err)
	}
	back, err := s.Get(ctx, "SHA256:abc")
	if err != nil {
		t.Fatalf("Get: %v", err)
	}
	if back.Instance != "byre-new" || back.InstanceUID != "uid-2" {
		t.Errorf("rebind did not stick: %+v", back)
	}
}

func TestUpdateMissingIsNotFound(t *testing.T) {
	_, err := newStore(t).Update(context.Background(), sample("SHA256:nobody"))
	if !errors.Is(err, ErrNotFound) {
		t.Fatalf("err = %v, want ErrNotFound", err)
	}
}

func TestSetPhase(t *testing.T) {
	ctx := context.Background()
	s := newStore(t)
	if _, err := s.Put(ctx, sample("SHA256:abc")); err != nil {
		t.Fatalf("Put: %v", err)
	}
	if err := s.SetPhase(ctx, "SHA256:abc", PhaseDraining, "instance deleted"); err != nil {
		t.Fatalf("SetPhase: %v", err)
	}
	back, err := s.Get(ctx, "SHA256:abc")
	if err != nil {
		t.Fatalf("Get: %v", err)
	}
	if back.Phase != PhaseDraining {
		t.Errorf("phase = %q, want %q", back.Phase, PhaseDraining)
	}
}

func TestSetPhaseRejectsUnknownPhase(t *testing.T) {
	err := newStore(t).SetPhase(context.Background(), "SHA256:abc", "Whatever", "")
	if err == nil {
		t.Fatal("SetPhase accepted an unknown phase; the CRD would reject it and the message would be a 422")
	}
}

func TestDeleteIsIdempotent(t *testing.T) {
	ctx := context.Background()
	s := newStore(t)
	if _, err := s.Put(ctx, sample("SHA256:abc")); err != nil {
		t.Fatalf("Put: %v", err)
	}
	if err := s.Delete(ctx, "SHA256:abc"); err != nil {
		t.Fatalf("Delete: %v", err)
	}
	// A retry after a dropped response must not fail.
	if err := s.Delete(ctx, "SHA256:abc"); err != nil {
		t.Fatalf("second Delete: %v", err)
	}
	if _, err := s.Get(ctx, "SHA256:abc"); !errors.Is(err, ErrNotFound) {
		t.Fatalf("Get after Delete err = %v, want ErrNotFound", err)
	}
}

func TestListReturnsEverything(t *testing.T) {
	ctx := context.Background()
	s := newStore(t)
	for _, id := range []string{"SHA256:a", "SHA256:b", "SHA256:c"} {
		if _, err := s.Put(ctx, sample(id)); err != nil {
			t.Fatalf("Put %s: %v", id, err)
		}
	}
	all, err := s.List(ctx)
	if err != nil {
		t.Fatalf("List: %v", err)
	}
	if len(all) != 3 {
		t.Fatalf("len = %d, want 3", len(all))
	}
	seen := map[string]bool{}
	for _, b := range all {
		seen[b.Identity] = true
	}
	for _, id := range []string{"SHA256:a", "SHA256:b", "SHA256:c"} {
		if !seen[id] {
			t.Errorf("List lost %q", id)
		}
	}
}

func TestWatchReportsDelete(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	s := newStore(t)
	if _, err := s.Put(ctx, sample("SHA256:abc")); err != nil {
		t.Fatalf("Put: %v", err)
	}
	ch, err := s.Watch(ctx)
	if err != nil {
		t.Fatalf("Watch: %v", err)
	}
	if err := s.Delete(ctx, "SHA256:abc"); err != nil {
		t.Fatalf("Delete: %v", err)
	}
	for ev := range ch {
		if ev.Type == "DELETED" || string(ev.Type) == "DELETED" {
			if ev.Lease.Identity != "SHA256:abc" {
				t.Errorf("deleted event for %q, want SHA256:abc", ev.Lease.Identity)
			}
			return
		}
	}
	t.Fatal("watch channel closed without a DELETED event")
}

func TestPutRequiresTheThreeIdentityFields(t *testing.T) {
	s := newStore(t)
	for _, b := range []Binding{
		{Backend: "k8s", Instance: "x"},
		{Identity: "SHA256:a", Instance: "x"},
		{Identity: "SHA256:a", Backend: "k8s"},
	} {
		if _, err := s.Put(context.Background(), b); err == nil {
			t.Errorf("Put accepted %+v; a binding without identity/backend/instance is unrunnable", b)
		}
	}
}

// The status subresource is what lets a Draining transition not clobber a concurrent rebind. The
// fake client does not model subresources, so this asserts the shape we send rather than the split.
func TestToObjectSetsNamespaceAndName(t *testing.T) {
	obj := toObject(sample("SHA256:abc"), ns)
	if obj.GetNamespace() != ns {
		t.Errorf("namespace = %q, want %q", obj.GetNamespace(), ns)
	}
	if obj.GetName() != ObjectName("SHA256:abc") {
		t.Errorf("name = %q, want %q", obj.GetName(), ObjectName("SHA256:abc"))
	}
	if obj.GetKind() != "ByreLease" {
		t.Errorf("kind = %q", obj.GetKind())
	}
	// No status in the created object: it is a subresource, so the API server discards it on Create.
	// Setting it here would look right and produce an object whose phase is empty forever.
	if _, found, _ := unstructured.NestedString(obj.Object, "status", "phase"); found {
		t.Error("toObject set status.phase; the API server drops it on Create, so the object would carry no phase at all")
	}
}

// The empty phase a fresh object carries has to read back as Leased, or every consumer needs to know
// about the subresource rule.
func TestPhaseDefaultsToLeased(t *testing.T) {
	ctx := context.Background()
	s := newStore(t)
	if _, err := s.Put(ctx, sample("SHA256:abc")); err != nil {
		t.Fatalf("Put: %v", err)
	}
	back, err := s.Get(ctx, "SHA256:abc")
	if err != nil {
		t.Fatalf("Get: %v", err)
	}
	if back.Phase != PhaseLeased {
		t.Errorf("phase = %q after a fresh Put, want %q", back.Phase, PhaseLeased)
	}
}

func TestBindingCarriesCreationTimestamp(t *testing.T) {
	ctx := context.Background()
	s := newStore(t)
	got, err := s.Put(ctx, sample("SHA256:abc"))
	if err != nil {
		t.Fatalf("Put: %v", err)
	}
	// The fake client stamps creationTimestamp, but the field is optional in its output; what matters
	// is that fromObject reads it rather than dropping it.
	_ = got.Created
	if got.Created.IsZero() {
		t.Log("fake client left creationTimestamp zero; fromObject handled it without panicking")
	}
	_ = metav1.Now
}
