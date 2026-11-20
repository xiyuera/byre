package backend

import (
	"context"
	"testing"

	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/runtime/schema"
	dynamicfake "k8s.io/client-go/dynamic/fake"
	"k8s.io/client-go/kubernetes/fake"
)

// homeDriver builds a workloads driver whose instances carry a per-identity claim. The fake dynamic
// client is not used by this path, but NewKubernetes insists on both clients before it skips the
// in-cluster rest config.
func homeDriver(t *testing.T, cs *fake.Clientset, namer func(string) (string, error)) *Kubernetes {
	t.Helper()
	withProbe(t, func(context.Context, string) error { return nil })
	cfg := baseKubeCfg()
	cfg.Provider.Home = &PersistentHome{
		MountPath:    "/work",
		ClaimName:    "home-{{.Name}}",
		StorageClass: "fast",
		Size:         "1Gi",
	}
	cfg.IdentityName = namer
	readyPodReactor(cs, "192.0.2.7")
	cfg.Client = cs
	cfg.Dynamic = dynamicfake.NewSimpleDynamicClientWithCustomListKinds(runtime.NewScheme(),
		map[schema.GroupVersionResource]string{claimGVR: "SandboxClaimList"})
	k, err := NewKubernetes(cfg)
	if err != nil {
		t.Fatalf("NewKubernetes: %v", err)
	}
	return k
}

func TestAcquireWithHomeCreatesClaimAndMountsIt(t *testing.T) {
	cs := fake.NewSimpleClientset()
	k := homeDriver(t, cs, func(string) (string, error) { return "alice", nil })
	ctx := context.Background()

	in, err := k.Acquire(ctx, "SHA256:abc")
	if err != nil {
		t.Fatalf("Acquire: %v", err)
	}

	pvc, err := cs.CoreV1().PersistentVolumeClaims("instances").Get(ctx, "home-alice", metav1.GetOptions{})
	if err != nil {
		t.Fatalf("claim was not created: %v", err)
	}
	if len(pvc.Spec.AccessModes) != 1 || pvc.Spec.AccessModes[0] != corev1.ReadWriteOnce {
		t.Errorf("accessModes = %v, want [ReadWriteOnce]", pvc.Spec.AccessModes)
	}
	if pvc.Spec.StorageClassName == nil || *pvc.Spec.StorageClassName != "fast" {
		t.Errorf("storageClassName = %v, want fast", pvc.Spec.StorageClassName)
	}
	if got := pvc.Labels[LabelHome]; got != "true" {
		t.Errorf("label %s = %q, want true", LabelHome, got)
	}
	if got := pvc.Labels[LabelManaged]; got != "byre" {
		t.Errorf("label %s = %q, want byre", LabelManaged, got)
	}

	pod, err := cs.CoreV1().Pods("instances").Get(ctx, in.ID, metav1.GetOptions{})
	if err != nil {
		t.Fatalf("pod: %v", err)
	}
	if len(pod.Spec.Volumes) != 1 || pod.Spec.Volumes[0].PersistentVolumeClaim == nil {
		t.Fatalf("pod has no claim volume: %+v", pod.Spec.Volumes)
	}
	if got := pod.Spec.Volumes[0].PersistentVolumeClaim.ClaimName; got != "home-alice" {
		t.Errorf("volume claimName = %q, want home-alice", got)
	}
	if len(pod.Spec.Containers[0].VolumeMounts) != 1 {
		t.Fatalf("volumeMounts = %+v", pod.Spec.Containers[0].VolumeMounts)
	}
	if got := pod.Spec.Containers[0].VolumeMounts[0].MountPath; got != "/work" {
		t.Errorf("mountPath = %q, want /work", got)
	}
}

func TestAcquireWithHomeReusesExistingClaim(t *testing.T) {
	cs := fake.NewSimpleClientset()
	k := homeDriver(t, cs, func(string) (string, error) { return "alice", nil })
	ctx := context.Background()

	first, err := k.Acquire(ctx, "SHA256:abc")
	if err != nil {
		t.Fatalf("first Acquire: %v", err)
	}
	if err := k.Release(ctx, first); err != nil {
		t.Fatalf("Release: %v", err)
	}

	if _, err := cs.CoreV1().PersistentVolumeClaims("instances").Get(ctx, "home-alice", metav1.GetOptions{}); err != nil {
		t.Fatalf("claim disappeared on release: %v", err)
	}

	second, err := k.Acquire(ctx, "SHA256:abc")
	if err != nil {
		t.Fatalf("second Acquire: %v", err)
	}
	pod, err := cs.CoreV1().Pods("instances").Get(ctx, second.ID, metav1.GetOptions{})
	if err != nil {
		t.Fatalf("pod: %v", err)
	}
	if got := pod.Spec.Volumes[0].PersistentVolumeClaim.ClaimName; got != "home-alice" {
		t.Errorf("second pod claimName = %q, want home-alice", got)
	}
	claims, err := cs.CoreV1().PersistentVolumeClaims("instances").List(ctx, metav1.ListOptions{})
	if err != nil {
		t.Fatal(err)
	}
	if len(claims.Items) != 1 {
		t.Errorf("claims = %d, want 1 (reused, not duplicated)", len(claims.Items))
	}
}

func TestAcquireWithHomeFailsWhenNameIsUnknown(t *testing.T) {
	cs := fake.NewSimpleClientset()
	k := homeDriver(t, cs, func(string) (string, error) {
		return "", context.Canceled
	})

	if _, err := k.Acquire(context.Background(), "SHA256:abc"); err == nil {
		t.Fatal("Acquire succeeded with no identity name; it must fail rather than invent one")
	}
	pods, _ := cs.CoreV1().Pods("instances").List(context.Background(), metav1.ListOptions{})
	claims, _ := cs.CoreV1().PersistentVolumeClaims("instances").List(context.Background(), metav1.ListOptions{})
	if len(pods.Items) != 0 || len(claims.Items) != 0 {
		t.Errorf("created %d pods and %d claims on failure, want 0 and 0", len(pods.Items), len(claims.Items))
	}
}

func TestHomeRequiresWorkloadsKind(t *testing.T) {
	_, err := NewKubernetes(KubernetesConfig{
		Name:      "sandbox",
		Namespace: "ns",
		Provider: ProviderSpec{
			Kind:       KindClaims,
			Template:   "tpl",
			APIGroup:   "g",
			APIVersion: "v1",
			Resource:   "r",
			ObjectKind: "K",
			Home: &PersistentHome{
				MountPath: "/work", ClaimName: "home-{{.Name}}", StorageClass: "fast", Size: "1Gi",
			},
		},
		SSHUser: "root", SSHPrivateKey: "/key",
		IdentityName: func(string) (string, error) { return "alice", nil },
		DryRun:       true,
	})
	if err == nil {
		t.Fatal("a claim backend accepted provider.home; it cannot reference an existing PVC")
	}
}

func TestHomeRequiresIdentityNamer(t *testing.T) {
	_, err := NewKubernetes(KubernetesConfig{
		Name:      "sandbox",
		Namespace: "ns",
		Provider: ProviderSpec{
			Kind:  KindWorkloads,
			Image: "img:1",
			Home: &PersistentHome{
				MountPath: "/work", ClaimName: "home-{{.Name}}", StorageClass: "fast", Size: "1Gi",
			},
		},
		SSHUser: "root", SSHPrivateKey: "/key",
		DryRun: true,
	})
	if err == nil {
		t.Fatal("provider.home was accepted without an identity namer")
	}
}

func TestHomeClaimNamesOnlyListsClaimsThisDriverMade(t *testing.T) {
	cs := fake.NewSimpleClientset()
	k := homeDriver(t, cs, func(string) (string, error) { return "alice", nil })
	ctx := context.Background()

	mk := func(name string, labels map[string]string) {
		_, err := cs.CoreV1().PersistentVolumeClaims("instances").Create(ctx, &corev1.PersistentVolumeClaim{
			ObjectMeta: metav1.ObjectMeta{Name: name, Namespace: "instances", Labels: labels},
		}, metav1.CreateOptions{})
		if err != nil {
			t.Fatalf("seed %s: %v", name, err)
		}
	}
	mk("home-alice", map[string]string{LabelManaged: "byre", LabelHome: "true"})
	mk("home-bob", map[string]string{LabelManaged: "byre", LabelHome: "true"})
	// A claim an operator made by hand is theirs. Reporting it as a leak of ours would be wrong, and
	// worse, it would invite an automated cleanup to touch data this process never created.
	mk("manual-scratch", map[string]string{LabelManaged: "byre"})
	mk("someone-elses", map[string]string{LabelManaged: "other", LabelHome: "true"})

	got, err := k.HomeClaimNames(ctx)
	if err != nil {
		t.Fatalf("HomeClaimNames: %v", err)
	}
	if len(got) != 2 {
		t.Fatalf("names = %v, want exactly the two byre home claims", got)
	}
	seen := map[string]bool{got[0]: true, got[1]: true}
	for _, want := range []string{"home-alice", "home-bob"} {
		if !seen[want] {
			t.Errorf("%s missing from %v", want, got)
		}
	}
}

func TestHomeClaimNamesIsEmptyWithoutAHome(t *testing.T) {
	cs := fake.NewSimpleClientset()
	withProbe(t, func(context.Context, string) error { return nil })
	cfg := baseKubeCfg() // no Provider.Home
	cfg.Client = cs
	cfg.Dynamic = dynamicfake.NewSimpleDynamicClientWithCustomListKinds(runtime.NewScheme(),
		map[schema.GroupVersionResource]string{claimGVR: "SandboxClaimList"})
	k, err := NewKubernetes(cfg)
	if err != nil {
		t.Fatal(err)
	}
	got, err := k.HomeClaimNames(context.Background())
	if err != nil || len(got) != 0 {
		t.Fatalf("HomeClaimNames on a backend without a home = %v, %v; want empty", got, err)
	}
}
