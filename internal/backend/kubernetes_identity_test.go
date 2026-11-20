package backend

import (
	"context"
	"strings"
	"testing"
	"time"

	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/runtime/schema"
	"k8s.io/apimachinery/pkg/types"
	dynamicfake "k8s.io/client-go/dynamic/fake"
	"k8s.io/client-go/kubernetes/fake"
)

// claimDyn builds a driver whose dynamic client holds claims in the test namespace, plus the client so a
// test can create objects and read them back.
func claimDyn(t *testing.T, cfg *KubernetesConfig) (*Kubernetes, *dynamicfake.FakeDynamicClient) {
	t.Helper()
	cfg.Client = fake.NewSimpleClientset()
	dyn := dynamicfake.NewSimpleDynamicClientWithCustomListKinds(runtime.NewScheme(),
		map[schema.GroupVersionResource]string{claimGVR: "SandboxClaimList"})
	cfg.Dynamic = dyn
	k, err := NewKubernetes(*cfg)
	if err != nil {
		t.Fatalf("NewKubernetes: %v", err)
	}
	return k, dyn
}

func putClaim(t *testing.T, dyn *dynamicfake.FakeDynamicClient, name, uid string, labels, ann map[string]string,
	status map[string]any, created time.Time) {
	t.Helper()
	obj := &unstructured.Unstructured{Object: map[string]any{
		"apiVersion": claimGVR.Group + "/" + claimGVR.Version,
		"kind":       "SandboxClaim",
		"metadata": map[string]any{
			"name":      name,
			"namespace": "instances",
		},
	}}
	obj.SetUID(types.UID(uid))
	obj.SetLabels(labels)
	obj.SetAnnotations(ann)
	if status != nil {
		obj.Object["status"] = status
	}
	if !created.IsZero() {
		obj.SetCreationTimestamp(metav1.NewTime(created))
	}
	if _, err := dyn.Resource(claimGVR).Namespace("instances").Create(context.Background(), obj, metav1.CreateOptions{}); err != nil {
		t.Fatalf("create claim %s: %v", name, err)
	}
}

// managedLabels are what createClaim puts on the object; a claim without them is not byre's and is never
// listed, which is why the leak below is about the address check and not about selection.
func managedLabels() map[string]string {
	return map[string]string{LabelManaged: "byre"}
}

// TestListReturnsAmbiguousObjectsWithoutAnAddress is the regression test for the leak: an object that
// carries the managed-by label but no address used to be dropped from List, so Adopt could neither
// destroy it nor reuse it, and it ran until somebody looked. On the live cluster two claims in exactly
// that shape stayed Running for half an hour across a restart.
func TestListReturnsAmbiguousObjectsWithoutAnAddress(t *testing.T) {
	cfg := claimConfig()
	k, dyn := claimDyn(t, &cfg)

	created := time.Date(2026, 11, 20, 6, 31, 18, 0, time.UTC)
	putClaim(t, dyn, "byre-b11f7e8c", "uid-1", managedLabels(), nil, nil, created)

	ins, err := k.List(context.Background())
	if err != nil {
		t.Fatalf("list: %v", err)
	}
	if len(ins) != 1 {
		t.Fatalf("listed %d instances, want 1: an object the driver cannot address must still be "+
			"reported, or adoption and the reaper never learn it exists", len(ins))
	}
	if ins[0].ID != "byre-b11f7e8c" {
		t.Fatalf("id = %q, want the claim", ins[0].ID)
	}
	if ins[0].Addr != "" {
		t.Fatalf("addr = %q, want empty (the object advertises none)", ins[0].Addr)
	}
	if !ins[0].Created.Equal(created) {
		t.Fatalf("created = %v, want %v (the sweep's age guard is the only thing that makes destroying "+
			"an anonymous object safe)", ins[0].Created, created)
	}
}

// TestListUsesPodIPsWhenTheServiceNameIsMissing pins the second half of the same defect: readiness accepts
// the pod IPs the controller publishes, while the old List read only the configured endpoint field. An
// object that answered on its pod IP was therefore invisible.
func TestListUsesPodIPsWhenTheServiceNameIsMissing(t *testing.T) {
	cfg := claimConfig()
	k, dyn := claimDyn(t, &cfg)

	putClaim(t, dyn, "byre-131237e2", "uid-2", managedLabels(), nil,
		map[string]any{"sandbox": map[string]any{"podIPs": []any{"10.0.0.5"}}}, time.Time{})

	ins, err := k.List(context.Background())
	if err != nil {
		t.Fatalf("list: %v", err)
	}
	if len(ins) != 1 {
		t.Fatalf("listed %d, want 1", len(ins))
	}
	if ins[0].Addr != "10.0.0.5:22" {
		t.Fatalf("addr = %q, want the pod IP with the default port", ins[0].Addr)
	}
}

// TestBindWritesLabelsAsWellAsAnnotations is defect B in code: the annotation alone is unselectable and
// invisible on anything that only inherits labels (a pod from a template). The label exists so the
// operator's own key — hermes.profile here — travels with the object and can be propagated.
func TestBindWritesLabelsAsWellAsAnnotations(t *testing.T) {
	cfg := claimConfig()
	cfg.IdentityLabels = func(string) map[string]string {
		return map[string]string{"hermes.profile": "eng"}
	}
	k, dyn := claimDyn(t, &cfg)

	putClaim(t, dyn, "byre-db05fc0d", "uid-3", managedLabels(), nil,
		map[string]any{"sandbox": map[string]any{"serviceFQDN": "claim.instances.svc"}}, time.Time{})

	in := Instance{ID: "byre-db05fc0d", UID: "uid-3"}
	if err := k.Bind(context.Background(), in, "WcHWo47zy7eNKb8CgeTkYX4rjr4CoNcp+xtNo63NYRE"); err != nil {
		t.Fatalf("bind: %v", err)
	}

	stored, err := dyn.Resource(claimGVR).Namespace("instances").Get(context.Background(), in.ID, metav1.GetOptions{})
	if err != nil {
		t.Fatalf("get: %v", err)
	}
	ann := stored.GetAnnotations()
	if ann[LabelIdentity] != "WcHWo47zy7eNKb8CgeTkYX4rjr4CoNcp+xtNo63NYRE" {
		t.Fatalf("identity annotation = %q, want the exact fingerprint", ann[LabelIdentity])
	}
	labels := stored.GetLabels()
	if got := labels["hermes.profile"]; got != "eng" {
		t.Fatalf("hermes.profile = %q, want eng (operator-declared labels must reach the object)", got)
	}
	if got := labels[LabelIdentityRef]; got != "WcHWo47zy7eNKb8CgeTkYX4rjr4CoNcp-xtNo63NYRE" {
		t.Fatalf("%s = %q, want the transliterated fingerprint", LabelIdentityRef, got)
	}
	if strings.ContainsAny(labels[LabelIdentityRef], "+/=") {
		t.Fatalf("%s = %q: a label value may not contain '+', '/' or '='", LabelIdentityRef, labels[LabelIdentityRef])
	}
}

// TestLabelValueIsUsableAsALabelValue covers the characters a fingerprint actually contains, plus the
// two boundaries Kubernetes rejects: an empty value and one longer than 63 characters.
func TestLabelValueIsUsableAsALabelValue(t *testing.T) {
	tests := []struct{ in, want string }{
		{"IFXHiuBwm3X5C/3uft36pvoUOr9r6lz+Fbke8rrNK1Y", "IFXHiuBwm3X5C_3uft36pvoUOr9r6lz-Fbke8rrNK1Y"},
		{"plain-eng", "plain-eng"},
		{"", "unknown"},
		{"====", "unknown"},
		{strings.Repeat("a", 80), strings.Repeat("a", 63)},
	}
	for _, tc := range tests {
		got := labelValue(tc.in)
		if got != tc.want {
			t.Fatalf("labelValue(%q) = %q, want %q", tc.in, got, tc.want)
		}
		if got == "" || len(got) > 63 {
			t.Fatalf("labelValue(%q) = %q: length must be 1-63", tc.in, got)
		}
	}
}

// TestListDoesNotDropOtherPeoplesObjects guards the other direction: widening what List reports must not
// widen what it selects. An object without the managed-by label belongs to something else.
func TestListDoesNotDropOtherPeoplesObjects(t *testing.T) {
	cfg := claimConfig()
	k, dyn := claimDyn(t, &cfg)

	putClaim(t, dyn, "someone-elses", "uid-4", map[string]string{"app": "unrelated"}, nil,
		map[string]any{"sandbox": map[string]any{"serviceFQDN": "x.instances.svc"}}, time.Time{})

	ins, err := k.List(context.Background())
	if err != nil {
		t.Fatalf("list: %v", err)
	}
	if len(ins) != 0 {
		t.Fatalf("listed %d foreign objects: %+v", len(ins), ins)
	}
}
