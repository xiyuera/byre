package backend

import (
	"context"
	"errors"
	"net"
	"strings"
	"sync"
	"testing"
	"time"

	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/runtime/schema"
	"k8s.io/apimachinery/pkg/types"
	dynamicfake "k8s.io/client-go/dynamic/fake"
	"k8s.io/client-go/kubernetes/fake"
	ktesting "k8s.io/client-go/testing"

	"byre/internal/metrics"
)

var claimGVR = schema.GroupVersionResource{Group: "example.io", Version: "v1", Resource: "sandboxclaims"}

func baseKubeCfg() KubernetesConfig {
	return KubernetesConfig{
		Name:          "k8s",
		Namespace:     "instances",
		Provider:      ProviderSpec{Kind: KindWorkloads, Image: "registry.example/sshd:1", ReadyTimeout: 2 * time.Second},
		SSHUser:       "sandbox",
		SSHPrivateKey: "/keys/pool",
		HostKeyPolicy: "accept-new",
	}
}

func TestNewKubernetesValidation(t *testing.T) {
	tests := []struct {
		name   string
		mutate func(*KubernetesConfig)
		want   string
	}{
		{"no name", func(c *KubernetesConfig) { c.Name = "" }, "name is required"},
		{"no namespace", func(c *KubernetesConfig) { c.Namespace = "" }, "namespace is required"},
		{"no user", func(c *KubernetesConfig) { c.SSHUser = "" }, "ssh user and private key"},
		{"pod without image", func(c *KubernetesConfig) { c.Provider.Image = "" }, "provider.image is required"},
		{"unknown kind", func(c *KubernetesConfig) { c.Provider.Kind = "vm" }, "unknown provision"},
		{"claim without selector", func(c *KubernetesConfig) {
			c.Provider = ProviderSpec{Kind: KindClaims, APIGroup: "g", APIVersion: "v1", Resource: "claims"}
		}, "provider.template or provider.warm_pool is required"},
		{"claim without gvk", func(c *KubernetesConfig) {
			c.Provider = ProviderSpec{Kind: KindClaims, Template: "t"}
		}, "api_group, api_version, resource and object_kind"},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			cfg := baseKubeCfg()
			tc.mutate(&cfg)
			_, err := NewKubernetes(cfg)
			if err == nil || !strings.Contains(err.Error(), tc.want) {
				t.Fatalf("err = %v, want %q", err, tc.want)
			}
		})
	}
}

// TestNewKubernetesDryRun is the path `-check-config` takes: validate, then stop. It has to work on a
// workstation with no cluster credentials, and it still has to reject an incomplete provider block.
func TestNewKubernetesDryRun(t *testing.T) {
	cfg := claimConfig()
	cfg.DryRun = true
	drv, err := NewKubernetes(cfg)
	if err != nil {
		t.Fatalf("NewKubernetes(dry run): %v", err)
	}
	if drv.client != nil || drv.dyn != nil {
		t.Fatalf("dry run built cluster clients: client=%v dynamic=%v", drv.client, drv.dyn)
	}

	cfg = claimConfig()
	cfg.DryRun = true
	cfg.Provider.APIGroup = ""
	if _, err := NewKubernetes(cfg); err == nil || !strings.Contains(err.Error(), "api_group") {
		t.Fatalf("dry run accepted a claim without api_group: err = %v", err)
	}
}

// readyPodReactor makes the fake API behave like a scheduler: the pod becomes Ready with an IP during
// creation, so the driver's wait loop terminates immediately.
func readyPodReactor(cs *fake.Clientset, ip string) {
	cs.PrependReactor("create", "pods", func(action ktesting.Action) (bool, runtime.Object, error) {
		pod := action.(ktesting.CreateAction).GetObject().(*corev1.Pod)
		pod.Status.Phase = corev1.PodRunning
		pod.Status.PodIP = ip
		pod.Status.Conditions = []corev1.PodCondition{{Type: corev1.PodReady, Status: corev1.ConditionTrue}}
		return false, nil, nil
	})
}

func TestPodCreateListRelease(t *testing.T) {
	cfg := baseKubeCfg()
	cs := fake.NewSimpleClientset()
	readyPodReactor(cs, "192.0.2.7")
	cfg.Client = cs
	dyn := dynamicfake.NewSimpleDynamicClientWithCustomListKinds(runtime.NewScheme(),
		map[schema.GroupVersionResource]string{claimGVR: "SandboxClaimList"})
	cfg.Dynamic = dyn

	k, err := NewKubernetes(cfg)
	if err != nil {
		t.Fatalf("NewKubernetes: %v", err)
	}
	ctx := context.Background()

	in, err := k.Acquire(ctx, "identity-a")
	if err != nil {
		t.Fatalf("acquire: %v", err)
	}
	if in.Addr != "192.0.2.7:22" {
		t.Fatalf("addr = %q, want 192.0.2.7:22", in.Addr)
	}
	if !strings.HasPrefix(in.ID, "byre-") {
		t.Fatalf("id = %q, want the generated pod name", in.ID)
	}
	if in.Annotations[LabelIdentity] != "identity-a" {
		t.Fatalf("identity annotation missing: %v", in.Annotations)
	}
	// The binding must be on the object, or a restart cannot adopt the instance.
	stored, err := cs.CoreV1().Pods("instances").Get(ctx, in.ID, metav1.GetOptions{})
	if err != nil {
		t.Fatalf("get pod: %v", err)
	}
	if stored.Annotations[LabelIdentity] != "identity-a" {
		t.Fatalf("object annotations = %v, want identity recorded on the pod", stored.Annotations)
	}
	if err := in.Validate(); err != nil {
		t.Fatalf("instance invalid: %v", err)
	}

	pods, err := cs.CoreV1().Pods("instances").List(ctx, metav1.ListOptions{})
	if err != nil {
		t.Fatalf("list: %v", err)
	}
	if len(pods.Items) != 1 {
		t.Fatalf("pods = %d, want 1", len(pods.Items))
	}
	if got := pods.Items[0].Labels[LabelManaged]; got != "byre" {
		t.Fatalf("managed-by label = %q", got)
	}
	if len(pods.Items[0].Spec.Containers) != 1 || pods.Items[0].Spec.Containers[0].Image != "registry.example/sshd:1" {
		t.Fatalf("container spec not applied: %+v", pods.Items[0].Spec.Containers)
	}

	listed, err := k.List(ctx)
	if err != nil {
		t.Fatalf("driver list: %v", err)
	}
	if len(listed) != 1 || listed[0].Addr != "192.0.2.7:22" {
		t.Fatalf("driver list = %+v", listed)
	}

	if err := k.Release(ctx, in); err != nil {
		t.Fatalf("release: %v", err)
	}
	pods, _ = cs.CoreV1().Pods("instances").List(ctx, metav1.ListOptions{})
	if len(pods.Items) != 0 {
		t.Fatalf("pod not deleted: %d left", len(pods.Items))
	}
	// Releasing twice must not fail.
	if err := k.Release(ctx, in); err != nil {
		t.Fatalf("second release: %v", err)
	}
}

func TestPodWaitsForReady(t *testing.T) {
	cfg := baseKubeCfg()
	cfg.Provider.ReadyTimeout = 300 * time.Millisecond
	cs := fake.NewSimpleClientset() // never becomes ready
	cfg.Client = cs
	cfg.Dynamic = dynamicfake.NewSimpleDynamicClientWithCustomListKinds(runtime.NewScheme(),
		map[schema.GroupVersionResource]string{claimGVR: "SandboxClaimList"})

	k, err := NewKubernetes(cfg)
	if err != nil {
		t.Fatalf("NewKubernetes: %v", err)
	}
	start := time.Now()
	if _, err := k.Create(context.Background()); err == nil {
		t.Fatal("expected a readiness timeout")
	}
	if elapsed := time.Since(start); elapsed > 3*time.Second {
		t.Fatalf("timeout took %s, want ~300ms", elapsed)
	}
	// A failed acquisition must not leave the object behind.
	pods, _ := cs.CoreV1().Pods("instances").List(context.Background(), metav1.ListOptions{})
	if len(pods.Items) != 0 {
		t.Fatalf("failed create left %d pods behind", len(pods.Items))
	}
}

func claimConfig() KubernetesConfig {
	cfg := baseKubeCfg()
	cfg.Provider = ProviderSpec{
		Kind:         KindClaims,
		Template:     "sandbox",
		WarmPool:     "sandbox",
		ReadyTimeout: 2 * time.Second,
		APIGroup:     claimGVR.Group,
		APIVersion:   claimGVR.Version,
		Resource:     claimGVR.Resource,
		ObjectKind:   "SandboxClaim",
	}
	return cfg
}

func TestClaimCreateUsesConfiguredGVKAndEndpointField(t *testing.T) {
	withProbe(t, func(context.Context, string) error { return nil })
	cfg := claimConfig()
	cfg.Client = fake.NewSimpleClientset()
	dyn := dynamicfake.NewSimpleDynamicClientWithCustomListKinds(runtime.NewScheme(),
		map[schema.GroupVersionResource]string{claimGVR: "SandboxClaimList"})
	dyn.PrependReactor("create", claimGVR.Resource, func(action ktesting.Action) (bool, runtime.Object, error) {
		obj := action.(ktesting.CreateAction).GetObject().(*unstructured.Unstructured)
		obj.Object["status"] = map[string]any{
			"sandbox": map[string]any{"serviceFQDN": "claim-1.instances.svc:22"},
		}
		return false, nil, nil
	})
	cfg.Dynamic = dyn

	k, err := NewKubernetes(cfg)
	if err != nil {
		t.Fatalf("NewKubernetes: %v", err)
	}

	in, err := k.Create(context.Background())
	if err != nil {
		t.Fatalf("create claim: %v", err)
	}
	if in.Addr != "claim-1.instances.svc:22" {
		t.Fatalf("addr = %q", in.Addr)
	}

	list, err := dyn.Resource(claimGVR).Namespace("instances").List(context.Background(), metav1.ListOptions{})
	if err != nil {
		t.Fatalf("list claims: %v", err)
	}
	if len(list.Items) != 1 {
		t.Fatalf("claims = %d, want 1", len(list.Items))
	}
	pool, _, _ := unstructured.NestedString(list.Items[0].Object, "spec", "warmPoolRef", "name")
	if pool != "sandbox" {
		t.Fatalf("warmPoolRef.name = %q", pool)
	}
	tmpl, _, _ := unstructured.NestedString(list.Items[0].Object, "spec", "templateRef", "name")
	if tmpl != "sandbox" {
		t.Fatalf("templateRef.name = %q", tmpl)
	}

	if err := k.Release(context.Background(), in); err != nil {
		t.Fatalf("release: %v", err)
	}
	list, _ = dyn.Resource(claimGVR).Namespace("instances").List(context.Background(), metav1.ListOptions{})
	if len(list.Items) != 0 {
		t.Fatalf("claim not deleted")
	}
}

func TestClaimTimeoutCleansUp(t *testing.T) {
	withProbe(t, func(context.Context, string) error { return errors.New("connection refused") })
	cfg := claimConfig()
	cfg.Provider.ReadyTimeout = 300 * time.Millisecond
	cfg.Client = fake.NewSimpleClientset()
	dyn := dynamicfake.NewSimpleDynamicClientWithCustomListKinds(runtime.NewScheme(),
		map[schema.GroupVersionResource]string{claimGVR: "SandboxClaimList"})
	cfg.Dynamic = dyn

	k, err := NewKubernetes(cfg)
	if err != nil {
		t.Fatalf("NewKubernetes: %v", err)
	}
	if _, err := k.Create(context.Background()); err == nil {
		t.Fatal("expected a timeout when no endpoint appears")
	}
	list, _ := dyn.Resource(claimGVR).Namespace("instances").List(context.Background(), metav1.ListOptions{})
	if len(list.Items) != 0 {
		t.Fatalf("timeout left %d claims behind", len(list.Items))
	}
}

func TestEndpointFieldNormalisesPort(t *testing.T) {
	k := &Kubernetes{cfg: KubernetesConfig{Provider: ProviderSpec{EndpointField: []string{"status", "addr"}}}}
	obj := &unstructured.Unstructured{Object: map[string]any{
		"status": map[string]any{"addr": "instance.internal"},
	}}
	if got := k.endpointFromObject(obj); got != "instance.internal:22" {
		t.Fatalf("endpoint = %q, want instance.internal:22", got)
	}
	obj.Object["status"] = map[string]any{"addr": "instance.internal:2222"}
	if got := k.endpointFromObject(obj); got != "instance.internal:2222" {
		t.Fatalf("endpoint = %q, want the explicit port kept", got)
	}
	if got := k.endpointFromObject(&unstructured.Unstructured{Object: map[string]any{}}); got != "" {
		t.Fatalf("missing field returned %q", got)
	}
}

// withProbe replaces the readiness probe for the duration of one test.
func withProbe(t *testing.T, fn func(ctx context.Context, addr string) error) {
	t.Helper()
	orig := probeAddr
	probeAddr = fn
	t.Cleanup(func() { probeAddr = orig })
}

func TestClaimIsNotReadyUntilEndpointResolves(t *testing.T) {
	cfg := claimConfig()
	cfg.Provider.ReadyTimeout = 400 * time.Millisecond
	cfg.Client = fake.NewSimpleClientset()
	dyn := dynamicfake.NewSimpleDynamicClientWithCustomListKinds(runtime.NewScheme(),
		map[schema.GroupVersionResource]string{claimGVR: "SandboxClaimList"})
	dyn.PrependReactor("create", claimGVR.Resource, func(action ktesting.Action) (bool, runtime.Object, error) {
		obj := action.(ktesting.CreateAction).GetObject().(*unstructured.Unstructured)
		obj.Object["status"] = map[string]any{
			"sandbox": map[string]any{"serviceFQDN": "byre-1.instances.svc.cluster.local"},
		}
		return false, nil, nil
	})
	cfg.Dynamic = dyn

	// The controller publishes the field before DNS answers: this is the real-world race.
	withProbe(t, func(context.Context, string) error { return errors.New("connection refused") })

	k, err := NewKubernetes(cfg)
	if err != nil {
		t.Fatalf("NewKubernetes: %v", err)
	}
	if _, err := k.Create(context.Background()); err == nil {
		t.Fatal("expected a timeout while the endpoint stays unresolvable")
	}
	list, _ := dyn.Resource(claimGVR).Namespace("instances").List(context.Background(), metav1.ListOptions{})
	if len(list.Items) != 0 {
		t.Fatalf("unresolvable claim was left behind (%d)", len(list.Items))
	}
}

func TestClaimSucceedsOnceDNSAnswers(t *testing.T) {
	cfg := claimConfig()
	cfg.Client = fake.NewSimpleClientset()
	dyn := dynamicfake.NewSimpleDynamicClientWithCustomListKinds(runtime.NewScheme(),
		map[schema.GroupVersionResource]string{claimGVR: "SandboxClaimList"})
	dyn.PrependReactor("create", claimGVR.Resource, func(action ktesting.Action) (bool, runtime.Object, error) {
		obj := action.(ktesting.CreateAction).GetObject().(*unstructured.Unstructured)
		obj.Object["status"] = map[string]any{
			"sandbox": map[string]any{"serviceFQDN": "byre-1.instances.svc.cluster.local"},
		}
		return false, nil, nil
	})
	cfg.Dynamic = dyn

	var calls int
	withProbe(t, func(context.Context, string) error {
		calls++
		if calls < 3 {
			return errors.New("connection refused")
		}
		return nil
	})

	k, err := NewKubernetes(cfg)
	if err != nil {
		t.Fatalf("NewKubernetes: %v", err)
	}
	in, err := k.Create(context.Background())
	if err != nil {
		t.Fatalf("create: %v", err)
	}
	if in.Addr != "byre-1.instances.svc.cluster.local:22" {
		t.Fatalf("addr = %q", in.Addr)
	}
	if calls < 3 {
		t.Fatalf("resolved after %d calls, want >= 3", calls)
	}
}

func TestClaimFallsBackToPodIPWhenServiceNameDoesNotAnswer(t *testing.T) {
	cfg := claimConfig()
	cfg.Client = fake.NewSimpleClientset()
	dyn := dynamicfake.NewSimpleDynamicClientWithCustomListKinds(runtime.NewScheme(),
		map[schema.GroupVersionResource]string{claimGVR: "SandboxClaimList"})
	dyn.PrependReactor("create", claimGVR.Resource, func(action ktesting.Action) (bool, runtime.Object, error) {
		obj := action.(ktesting.CreateAction).GetObject().(*unstructured.Unstructured)
		obj.Object["status"] = map[string]any{
			"sandbox": map[string]any{
				"serviceFQDN": "byre-1.instances.svc.cluster.local",
				"podIPs":      []any{"192.0.2.9"},
			},
		}
		return false, nil, nil
	})
	cfg.Dynamic = dyn

	// The service name is not answering yet; the pod IP is.
	withProbe(t, func(_ context.Context, addr string) error {
		if strings.HasPrefix(addr, "byre-1.") {
			return errors.New("no such host")
		}
		return nil
	})

	k, err := NewKubernetes(cfg)
	if err != nil {
		t.Fatalf("NewKubernetes: %v", err)
	}
	in, err := k.Create(context.Background())
	if err != nil {
		t.Fatalf("create: %v", err)
	}
	if in.Addr != "192.0.2.9:22" {
		t.Fatalf("addr = %q, want the pod IP fallback", in.Addr)
	}
}

// ---------------------------------------------------------------- instance identity (v0.1.3)

// fakeSSHDPeer listens on loopback and answers every connection with banner ("" = accept and stay
// silent). It exists so the readiness probe can be tested against a real socket instead of a stub: the
// failure being fixed is "TCP accepted, SSH never spoke", and only a real socket can produce it.
func fakeSSHDPeer(t *testing.T, banner string) string {
	t.Helper()
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatalf("listen: %v", err)
	}
	var mu sync.Mutex
	var conns []net.Conn
	t.Cleanup(func() {
		_ = ln.Close()
		mu.Lock()
		defer mu.Unlock()
		for _, c := range conns {
			_ = c.Close()
		}
	})
	go func() {
		for {
			c, err := ln.Accept()
			if err != nil {
				return
			}
			mu.Lock()
			conns = append(conns, c)
			mu.Unlock()
			if banner != "" {
				_, _ = c.Write([]byte(banner))
			}
		}
	}()
	return ln.Addr().String()
}

// fakeSSHDPeerThenClose writes payload and closes the connection straight away. It is how a
// half-started sshd behaves: a few banner bytes and then nothing at all.
func fakeSSHDPeerThenClose(t *testing.T, payload string) string {
	t.Helper()
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatalf("listen: %v", err)
	}
	t.Cleanup(func() { _ = ln.Close() })
	go func() {
		c, err := ln.Accept()
		if err != nil {
			return
		}
		_, _ = c.Write([]byte(payload))
		_ = c.Close()
	}()
	return ln.Addr().String()
}

func testPod(name, uid, identity, annotatedUID string) *corev1.Pod {
	ann := map[string]string{LabelInstance: name}
	if identity != "" {
		ann[LabelIdentity] = identity
	}
	if annotatedUID != "" {
		ann[LabelUID] = annotatedUID
	}
	return &corev1.Pod{ObjectMeta: metav1.ObjectMeta{
		Name:        name,
		Namespace:   "instances",
		UID:         types.UID(uid),
		Labels:      map[string]string{LabelManaged: "byre"},
		Annotations: ann,
	}}
}

// TestListDropsIdentityWhenObjectWasReplaced pins the whole point of the UID annotation: a name is
// reusable, an instance is not. The same name with a different UID must come back unbound, so adoption
// destroys it instead of handing a stranger's object to the client that held the old lease.
func TestListDropsIdentityWhenObjectWasReplaced(t *testing.T) {
	cfg := baseKubeCfg()
	cs := fake.NewSimpleClientset()
	cfg.Client = cs
	cfg.Dynamic = dynamicfake.NewSimpleDynamicClientWithCustomListKinds(runtime.NewScheme(),
		map[schema.GroupVersionResource]string{claimGVR: "SandboxClaimList"})
	k, err := NewKubernetes(cfg)
	if err != nil {
		t.Fatalf("NewKubernetes: %v", err)
	}
	ctx := context.Background()

	original := testPod("byre-x", "uid-old", "alice", "uid-old")
	if _, err := cs.CoreV1().Pods("instances").Create(ctx, original, metav1.CreateOptions{}); err != nil {
		t.Fatalf("create: %v", err)
	}
	listed, err := k.List(ctx)
	if err != nil {
		t.Fatalf("list: %v", err)
	}
	if len(listed) != 1 || listed[0].Annotations[LabelIdentity] != "alice" || listed[0].UID != "uid-old" {
		t.Fatalf("bound instance not reported as bound: %+v", listed)
	}

	// Delete, then create something else under the same name — the object the lease pointed at is gone.
	if err := cs.CoreV1().Pods("instances").Delete(ctx, "byre-x", metav1.DeleteOptions{}); err != nil {
		t.Fatalf("delete: %v", err)
	}
	replacement := testPod("byre-x", "uid-new", "alice", "uid-old")
	if _, err := cs.CoreV1().Pods("instances").Create(ctx, replacement, metav1.CreateOptions{}); err != nil {
		t.Fatalf("recreate: %v", err)
	}

	listed, err = k.List(ctx)
	if err != nil {
		t.Fatalf("list after replace: %v", err)
	}
	if len(listed) != 1 {
		t.Fatalf("instances = %d, want 1", len(listed))
	}
	if listed[0].UID != "uid-new" {
		t.Fatalf("uid = %q, want the replacement's uid", listed[0].UID)
	}
	if id, ok := listed[0].Annotations[LabelIdentity]; ok {
		t.Fatalf("identity %q was inherited by a different object with the same name: %v", id, listed[0].Annotations)
	}
	// The object itself is untouched; only the adopted view is unbound.
	stored, err := cs.CoreV1().Pods("instances").Get(ctx, "byre-x", metav1.GetOptions{})
	if err != nil {
		t.Fatalf("get: %v", err)
	}
	if stored.Annotations[LabelIdentity] != "alice" {
		t.Fatalf("List must not mutate the object: %v", stored.Annotations)
	}
}

// TestAdoptBackfillsUIDOnLegacyBinding covers the upgrade path: objects bound by v0.1.2 carry no UID
// annotation, and treating them as stale would destroy every live lease on the first restart after the
// upgrade. They are adopted, and the annotation is written back so the next restart is strict.
func TestAdoptBackfillsUIDOnLegacyBinding(t *testing.T) {
	cfg := baseKubeCfg()
	cs := fake.NewSimpleClientset()
	cfg.Client = cs
	cfg.Dynamic = dynamicfake.NewSimpleDynamicClientWithCustomListKinds(runtime.NewScheme(),
		map[schema.GroupVersionResource]string{claimGVR: "SandboxClaimList"})
	k, err := NewKubernetes(cfg)
	if err != nil {
		t.Fatalf("NewKubernetes: %v", err)
	}
	ctx := context.Background()

	if _, err := cs.CoreV1().Pods("instances").Create(ctx, testPod("byre-legacy", "uid-1", "bob", ""), metav1.CreateOptions{}); err != nil {
		t.Fatalf("create: %v", err)
	}
	ins, err := k.List(ctx)
	if err != nil {
		t.Fatalf("list: %v", err)
	}
	if len(ins) != 1 || ins[0].Annotations[LabelIdentity] != "bob" {
		t.Fatalf("legacy binding was dropped instead of adopted: %+v", ins)
	}
	if ins[0].Annotations[LabelUID] != "" {
		t.Fatalf("annotation appeared before Adopt ran: %v", ins[0].Annotations)
	}
	if err := k.Adopt(ctx, ins); err != nil {
		t.Fatalf("adopt: %v", err)
	}
	stored, err := cs.CoreV1().Pods("instances").Get(ctx, "byre-legacy", metav1.GetOptions{})
	if err != nil {
		t.Fatalf("get: %v", err)
	}
	if got := stored.Annotations[LabelUID]; got != "uid-1" {
		t.Fatalf("uid annotation = %q, want the object's uid backfilled", got)
	}
}

// TestBindingWritesObjectUID: the annotation is only useful if it is written when an instance is bound.
func TestBindingWritesObjectUID(t *testing.T) {
	cfg := baseKubeCfg()
	cs := fake.NewSimpleClientset()
	readyPodReactor(cs, "192.0.2.7")
	cfg.Client = cs
	cfg.Dynamic = dynamicfake.NewSimpleDynamicClientWithCustomListKinds(runtime.NewScheme(),
		map[schema.GroupVersionResource]string{claimGVR: "SandboxClaimList"})
	// Give the created pod a UID, as the API server would.
	cs.PrependReactor("create", "pods", func(action ktesting.Action) (bool, runtime.Object, error) {
		pod := action.(ktesting.CreateAction).GetObject().(*corev1.Pod)
		pod.UID = "uid-created"
		return false, nil, nil
	})
	k, err := NewKubernetes(cfg)
	if err != nil {
		t.Fatalf("NewKubernetes: %v", err)
	}
	ctx := context.Background()
	in, err := k.Acquire(ctx, "carol")
	if err != nil {
		t.Fatalf("acquire: %v", err)
	}
	if in.UID != "uid-created" {
		t.Fatalf("instance uid = %q, want the object's uid", in.UID)
	}
	stored, err := cs.CoreV1().Pods("instances").Get(ctx, in.ID, metav1.GetOptions{})
	if err != nil {
		t.Fatalf("get: %v", err)
	}
	if got := stored.Annotations[LabelUID]; got != "uid-created" {
		t.Fatalf("annotations = %v, want %s=uid-created", stored.Annotations, LabelUID)
	}
}

// ---------------------------------------------------------------- readiness is "SSH answers"

func TestSSHBannerProbe(t *testing.T) {
	tests := []struct {
		name          string
		banner        string
		wantErr       bool
		wantHandshake bool
	}{
		{"ssh 2.0", "SSH-2.0-OpenSSH_9.6\r\n", false, false},
		{"ssh 1.99", "SSH-1.99-OpenSSH_3.9\r\n", true, true},
		{"http on port 22", "HTTP/1.1 400 Bad Request\r\n", true, true},
		{"silence", "", true, true},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			addr := fakeSSHDPeer(t, tc.banner)
			before := metrics.HandshakeFailTotal()
			// A short context keeps the "silent peer" case from waiting out the full banner timeout.
			ctx, cancel := context.WithTimeout(context.Background(), 400*time.Millisecond)
			defer cancel()
			err := sshBannerProbe(ctx, addr)
			if tc.wantErr != (err != nil) {
				t.Fatalf("probe err = %v, wantErr %v", err, tc.wantErr)
			}
			counted := metrics.HandshakeFailTotal() - before
			wantCount := int64(0)
			if tc.wantHandshake {
				wantCount = 1
			}
			if counted != wantCount {
				t.Fatalf("handshake failures counted = %d, want %d (a dial error and a broken peer are different failures)", counted, wantCount)
			}
		})
	}
}

// TestClaimIsNotReadyWhenOnlyTCPAnswers is the regression this whole change exists for: a peer that
// accepts the connection and never sends a banner passes a bare dial, so the old probe reported "ready"
// for an instance every real session would fail to use.
func TestClaimIsNotReadyWhenOnlyTCPAnswers(t *testing.T) {
	addr := fakeSSHDPeer(t, "") // accepts, says nothing

	cfg := claimConfig()
	cfg.Provider.ReadyTimeout = 600 * time.Millisecond
	cfg.Client = fake.NewSimpleClientset()
	dyn := dynamicfake.NewSimpleDynamicClientWithCustomListKinds(runtime.NewScheme(),
		map[schema.GroupVersionResource]string{claimGVR: "SandboxClaimList"})
	dyn.PrependReactor("create", claimGVR.Resource, func(action ktesting.Action) (bool, runtime.Object, error) {
		obj := action.(ktesting.CreateAction).GetObject().(*unstructured.Unstructured)
		obj.Object["status"] = map[string]any{"sandbox": map[string]any{"serviceFQDN": addr}}
		return false, nil, nil
	})
	cfg.Dynamic = dyn

	// No probe injection: the production probe is the thing under test.
	k, err := NewKubernetes(cfg)
	if err != nil {
		t.Fatalf("NewKubernetes: %v", err)
	}
	if _, err := k.Create(context.Background()); err == nil {
		t.Fatal("an instance that accepts TCP and never speaks SSH was judged ready")
	}
	list, _ := dyn.Resource(claimGVR).Namespace("instances").List(context.Background(), metav1.ListOptions{})
	if len(list.Items) != 0 {
		t.Fatalf("unready claim was left behind (%d)", len(list.Items))
	}
}

// TestClaimIsReadyWhenSSHBannerArrives is the positive control for the test above.
func TestClaimIsReadyWhenSSHBannerArrives(t *testing.T) {
	addr := fakeSSHDPeer(t, "SSH-2.0-byre-test\r\n")

	cfg := claimConfig()
	cfg.Client = fake.NewSimpleClientset()
	dyn := dynamicfake.NewSimpleDynamicClientWithCustomListKinds(runtime.NewScheme(),
		map[schema.GroupVersionResource]string{claimGVR: "SandboxClaimList"})
	dyn.PrependReactor("create", claimGVR.Resource, func(action ktesting.Action) (bool, runtime.Object, error) {
		obj := action.(ktesting.CreateAction).GetObject().(*unstructured.Unstructured)
		obj.Object["status"] = map[string]any{"sandbox": map[string]any{"serviceFQDN": addr}}
		return false, nil, nil
	})
	cfg.Dynamic = dyn

	k, err := NewKubernetes(cfg)
	if err != nil {
		t.Fatalf("NewKubernetes: %v", err)
	}
	in, err := k.Create(context.Background())
	if err != nil {
		t.Fatalf("create: %v", err)
	}
	if in.Addr != addr {
		t.Fatalf("addr = %q, want %q", in.Addr, addr)
	}
}

// A missing instance and a broken one are different failures. Only the second is a handshake
// failure: if an unreachable address bumped the counter, the number would measure "the pool is
// cold" rather than "an instance is answering without speaking SSH", and the two need different
// responses.
func TestProbeDialFailureIsNotAHandshakeFailure(t *testing.T) {
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatalf("listen: %v", err)
	}
	addr := ln.Addr().String()
	_ = ln.Close() // nothing listens on this port any more, so the dial is refused

	before := metrics.HandshakeFailTotal()
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
	defer cancel()
	if err := sshBannerProbe(ctx, addr); err == nil {
		t.Fatal("a probe of a closed port reported ready")
	}
	if got := metrics.HandshakeFailTotal() - before; got != 0 {
		t.Fatalf("dial failure counted as %d handshake failures; an unreachable address is not a broken peer", got)
	}
}

// A peer that sends part of a banner and then stops is the shape of a hung sshd: the version prefix
// can even be right. Accepting any bytes would report it ready, so the probe must insist on the CRLF
// that terminates the line.
func TestProbeRejectsTruncatedBanner(t *testing.T) {
	addr := fakeSSHDPeerThenClose(t, "SSH-2.0-byre-test") // no CRLF, then EOF

	before := metrics.HandshakeFailTotal()
	err := sshBannerProbe(context.Background(), addr)
	if err == nil {
		t.Fatal("a peer that sent a truncated banner (no CRLF) was judged ready")
	}
	if got := metrics.HandshakeFailTotal() - before; got != 1 {
		t.Fatalf("handshake failures counted = %d, want 1", got)
	}
}

// TestPodInstanceGetsConservativeSecurityContext covers the pod provider's defaults: an instance used to
// run with no resources and no securityContext at all (root, default capabilities).
func TestPodInstanceGetsConservativeSecurityContext(t *testing.T) {
	cfg := baseKubeCfg()
	cs := fake.NewSimpleClientset()
	readyPodReactor(cs, "192.0.2.7")
	cfg.Client = cs
	cfg.Dynamic = dynamicfake.NewSimpleDynamicClientWithCustomListKinds(runtime.NewScheme(),
		map[schema.GroupVersionResource]string{claimGVR: "SandboxClaimList"})
	k, err := NewKubernetes(cfg)
	if err != nil {
		t.Fatalf("NewKubernetes: %v", err)
	}
	if _, err := k.Create(context.Background()); err != nil {
		t.Fatalf("create: %v", err)
	}
	pods, err := cs.CoreV1().Pods("instances").List(context.Background(), metav1.ListOptions{})
	if err != nil || len(pods.Items) != 1 {
		t.Fatalf("pods: %v %v", pods, err)
	}
	spec := pods.Items[0].Spec
	if spec.SecurityContext == nil || spec.SecurityContext.RunAsNonRoot == nil || !*spec.SecurityContext.RunAsNonRoot {
		t.Fatalf("pod securityContext does not require a non-root user: %+v", spec.SecurityContext)
	}
	if spec.SecurityContext.SeccompProfile == nil || spec.SecurityContext.SeccompProfile.Type != corev1.SeccompProfileTypeRuntimeDefault {
		t.Fatalf("seccomp profile not set: %+v", spec.SecurityContext)
	}
	c := spec.Containers[0]
	if c.SecurityContext == nil || c.SecurityContext.AllowPrivilegeEscalation == nil || *c.SecurityContext.AllowPrivilegeEscalation {
		t.Fatalf("privilege escalation not disabled: %+v", c.SecurityContext)
	}
	if c.SecurityContext.Capabilities == nil || len(c.SecurityContext.Capabilities.Drop) != 1 || c.SecurityContext.Capabilities.Drop[0] != "ALL" {
		t.Fatalf("capabilities not dropped: %+v", c.SecurityContext.Capabilities)
	}
	if c.Resources.Limits.Memory().IsZero() || c.Resources.Requests.Memory().IsZero() {
		t.Fatalf("no resources requested: %+v", c.Resources)
	}
}
