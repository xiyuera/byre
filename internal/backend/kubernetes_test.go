package backend

import (
	"context"
	"strings"
	"testing"
	"time"

	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/runtime/schema"
	dynamicfake "k8s.io/client-go/dynamic/fake"
	"k8s.io/client-go/kubernetes/fake"
	ktesting "k8s.io/client-go/testing"
)

var claimGVR = schema.GroupVersionResource{Group: "example.io", Version: "v1", Resource: "sandboxclaims"}

func baseKubeCfg() KubernetesConfig {
	return KubernetesConfig{
		Name:          "k8s",
		Namespace:     "instances",
		Provider:      ProviderSpec{Kind: KindPod, Image: "registry.example/sshd:1", ReadyTimeout: 2 * time.Second},
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
		{"unknown kind", func(c *KubernetesConfig) { c.Provider.Kind = "vm" }, "unknown provider.kind"},
		{"claim without template", func(c *KubernetesConfig) {
			c.Provider = ProviderSpec{Kind: KindSandboxClaim, APIGroup: "g", APIVersion: "v1", ClaimResource: "claims"}
		}, "provider.template is required"},
		{"claim without gvk", func(c *KubernetesConfig) {
			c.Provider = ProviderSpec{Kind: KindSandboxClaim, Template: "t"}
		}, "api_group, api_version and claim_resource"},
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
	if !strings.HasPrefix(in.ID, "keypod-") {
		t.Fatalf("id = %q, want the generated pod name", in.ID)
	}
	if in.Annotations[LabelIdentity] != "identity-a" {
		t.Fatalf("identity annotation missing: %v", in.Annotations)
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
	if got := pods.Items[0].Labels[LabelManaged]; got != "keypod" {
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
		Kind:          KindSandboxClaim,
		Template:      "sandbox",
		WarmPool:      "sandbox",
		ReadyTimeout:  2 * time.Second,
		APIGroup:      claimGVR.Group,
		APIVersion:    claimGVR.Version,
		ClaimResource: claimGVR.Resource,
	}
	return cfg
}

func TestClaimCreateUsesConfiguredGVKAndEndpointField(t *testing.T) {
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
	tmpl, _, _ := unstructured.NestedString(list.Items[0].Object, "spec", "templateRef", "name")
	if tmpl != "sandbox" {
		t.Fatalf("templateRef.name = %q", tmpl)
	}
	pool, _, _ := unstructured.NestedString(list.Items[0].Object, "spec", "warmPoolRef", "name")
	if pool != "sandbox" {
		t.Fatalf("warmPoolRef.name = %q", pool)
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
