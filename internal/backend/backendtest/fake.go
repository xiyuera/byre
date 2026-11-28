// Package backendtest holds the fakes that tests of the packages above share.
//
// It is a normal package rather than a _test.go file because three packages need the same fake: the
// driver's own tests, the lease tests and the ssh front end tests. The last two have to observe what
// happens when an instance disappears underneath a lease, and a private copy in each package would
// drift — the drift would show up as a test that passes for the wrong reason.
package backendtest

import (
	"context"
	"encoding/json"
	"fmt"
	"sync/atomic"
	"testing"
	"time"

	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/runtime/schema"
	"k8s.io/apimachinery/pkg/types"
	"k8s.io/client-go/dynamic"
	dynamicfake "k8s.io/client-go/dynamic/fake"
	"k8s.io/client-go/kubernetes/fake"
	ktesting "k8s.io/client-go/testing"

	"byre/internal/backend"
)

// NewWorkloadDriver returns a pod-backed driver over a fake clientset, plus the clientset itself, so a
// test can delete a pod the way the cluster would.
//
// Every pod created through it becomes Running and Ready with its own address (192.0.2.2, 192.0.2.3, …).
// That matters more than it looks: a fake that handed out one address would make "the client was moved to
// a replacement instance" indistinguishable from "the client was given the dead one again", which is
// precisely the assertion the tests built on this fake must make.
func NewWorkloadDriver(t *testing.T, name, namespace string) (*backend.Kubernetes, *fake.Clientset) {
	t.Helper()

	// The fake hands out 192.0.2.x addresses that nothing answers on, and readiness is now "the address
	// answers and speaks SSH". Stub the probe: these tests are about the lease table and the front end.
	restoreProbe := backend.SetReadyProbeForTest(func(context.Context, string) error { return nil })
	t.Cleanup(restoreProbe)

	cs := fake.NewSimpleClientset()
	var seq int64
	cs.PrependReactor("create", "pods", func(action ktesting.Action) (bool, runtime.Object, error) {
		pod := action.(ktesting.CreateAction).GetObject().(*corev1.Pod)
		n := atomic.AddInt64(&seq, 1)
		pod.UID = types.UID(fmt.Sprintf("uid-%04d", n))
		pod.Status.Phase = corev1.PodRunning
		pod.Status.PodIP = fmt.Sprintf("192.0.2.%d", n+1)
		pod.Status.Conditions = []corev1.PodCondition{{Type: corev1.PodReady, Status: corev1.ConditionTrue}}
		return false, nil, nil
	})

	drv, err := backend.NewKubernetes(backend.KubernetesConfig{
		Name:      name,
		Namespace: namespace,
		Provider: backend.ProviderSpec{
			Kind:         backend.KindWorkloads,
			Image:        "registry.example/sshd:1",
			ReadyTimeout: 5 * time.Second,
		},
		SSHUser:       "sandbox",
		SSHPrivateKey: "/keys/pool",
		HostKeyPolicy: "accept-new",
		Client:        cs,
		Dynamic: dynamicfake.NewSimpleDynamicClientWithCustomListKinds(
			runtime.NewScheme(), map[schema.GroupVersionResource]string{}),
	})
	if err != nil {
		t.Fatalf("kubernetes backend: %v", err)
	}
	return drv, cs
}

// DeletePod removes an instance's object and fails the test if it is still there afterwards: a test that
// forgot to delete would otherwise assert the behaviour of a live instance while claiming to assert the
// behaviour of a dead one.
func DeletePod(t *testing.T, cs *fake.Clientset, namespace, name string) {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	if err := cs.CoreV1().Pods(namespace).Delete(ctx, name, metav1.DeleteOptions{}); err != nil {
		t.Fatalf("delete pod %s: %v", name, err)
	}
	if _, err := cs.CoreV1().Pods(namespace).Get(ctx, name, metav1.GetOptions{}); err == nil {
		t.Fatalf("pod %s is still present after the delete", name)
	}
}

// claimGVR is the coordinates the claim-backed fake serves. They are deliberately not a real API group:
// the driver takes all four from configuration, and a test that hard-coded a vendor's names would stop
// testing that.
var claimGVR = schema.GroupVersionResource{Group: "sandbox.example", Version: "v1alpha1", Resource: "sandboxclaims"}

// NewClaimDriver returns a claim-backed driver over a fake dynamic client, plus the client itself, so a
// test can move the claim's selected sandbox the way the controller does.
//
// Each claim created through it is answered with a status naming its own sandbox (sandbox-1, sandbox-2,
// …) and an FQDN under the namespace, which is what the driver dials. Without the create and patch
// reactors this path cannot be exercised at all: the fake dynamic client has no default patch support,
// and the driver annotates every claim it creates.
func NewClaimDriver(t *testing.T, name, namespace string) (*backend.Kubernetes, dynamic.Interface) {
	t.Helper()

	restoreProbe := backend.SetReadyProbeForTest(func(context.Context, string) error { return nil })
	t.Cleanup(restoreProbe)

	fdyn := dynamicfake.NewSimpleDynamicClientWithCustomListKinds(runtime.NewScheme(),
		map[schema.GroupVersionResource]string{claimGVR: "SandboxClaimList"})
	dyn := dynamic.Interface(fdyn)

	var seq int64
	fdyn.PrependReactor("create", claimGVR.Resource, func(action ktesting.Action) (bool, runtime.Object, error) {
		obj := action.(ktesting.CreateAction).GetObject().(*unstructured.Unstructured)
		n := atomic.AddInt64(&seq, 1)
		obj.SetUID(types.UID(fmt.Sprintf("claim-uid-%04d", n)))
		sandbox := fmt.Sprintf("sandbox-%d", n)
		obj.Object["status"] = map[string]any{
			"sandbox": map[string]any{
				"name":        sandbox,
				"serviceFQDN": fmt.Sprintf("%s.%s.svc:22", sandbox, namespace),
			},
		}
		return false, nil, nil
	})

	// A metadata merge patch, applied to what the tracker holds. That is all the driver sends (the
	// binding annotations and labels), and the fake dynamic client has no default reactor for it.
	//
	// The tracker is used directly rather than going back through the client: reactors run with the
	// fake's lock held, so a nested Get there deadlocks the whole test binary (observed as a 10-minute
	// go-test timeout with every goroutine parked in Fake.Invokes).
	fdyn.PrependReactor("patch", claimGVR.Resource, func(action ktesting.Action) (bool, runtime.Object, error) {
		pa := action.(ktesting.PatchAction)
		stored, err := fdyn.Tracker().Get(claimGVR, namespace, pa.GetName())
		if err != nil {
			return true, nil, err
		}
		obj, ok := stored.(*unstructured.Unstructured)
		if !ok {
			return true, nil, fmt.Errorf("claim %s is a %T, not an unstructured object", pa.GetName(), stored)
		}
		var patch struct {
			Metadata struct {
				Annotations map[string]string `json:"annotations"`
				Labels      map[string]string `json:"labels"`
			} `json:"metadata"`
		}
		if err := json.Unmarshal(pa.GetPatch(), &patch); err != nil {
			return true, nil, err
		}
		for key, value := range patch.Metadata.Annotations {
			_ = unstructured.SetNestedField(obj.Object, value, "metadata", "annotations", key)
		}
		for key, value := range patch.Metadata.Labels {
			_ = unstructured.SetNestedField(obj.Object, value, "metadata", "labels", key)
		}
		if err := fdyn.Tracker().Update(claimGVR, obj, namespace); err != nil {
			return true, nil, err
		}
		return true, obj, nil
	})

	drv, err := backend.NewKubernetes(backend.KubernetesConfig{
		Name:      name,
		Namespace: namespace,
		Provider: backend.ProviderSpec{
			Kind:         backend.KindClaims,
			Template:     "sandbox",
			ReadyTimeout: 5 * time.Second,
			APIGroup:     claimGVR.Group,
			APIVersion:   claimGVR.Version,
			Resource:     claimGVR.Resource,
			ObjectKind:   "SandboxClaim",
		},
		SSHUser:       "sandbox",
		SSHPrivateKey: "/keys/pool",
		HostKeyPolicy: "accept-new",
		Client:        fake.NewSimpleClientset(),
		Dynamic:       dyn,
	})
	if err != nil {
		t.Fatalf("kubernetes backend: %v", err)
	}
	return drv, dyn
}

// MoveSandbox rewrites the claim's status to select a different sandbox, which is what the claim
// controller does when the object behind a claim is deleted: the claim keeps its name and UID, and the
// replacement appears under a new name. The old FQDN stops resolving; the new one is
// "<sandbox>.<namespace>.svc:22".
func MoveSandbox(t *testing.T, dyn dynamic.Interface, namespace, claim, sandbox string) {
	t.Helper()
	setSandboxStatus(t, dyn, namespace, claim, map[string]any{
		"name":        sandbox,
		"serviceFQDN": fmt.Sprintf("%s.%s.svc:22", sandbox, namespace),
	})
}

// ClearSandboxEndpoint leaves the claim in place with no dialable address: the state between a deleted
// instance and its replacement.
func ClearSandboxEndpoint(t *testing.T, dyn dynamic.Interface, namespace, claim string) {
	t.Helper()
	setSandboxStatus(t, dyn, namespace, claim, map[string]any{"name": "pending"})
}

// DeleteClaim removes a claim the way an operator (or the reaper) would, and fails the test if it is
// still there.
func DeleteClaim(t *testing.T, dyn dynamic.Interface, namespace, name string) {
	t.Helper()
	ctx := context.Background()
	if err := dyn.Resource(claimGVR).Namespace(namespace).Delete(ctx, name, metav1.DeleteOptions{}); err != nil {
		t.Fatalf("delete claim %s: %v", name, err)
	}
	if _, err := dyn.Resource(claimGVR).Namespace(namespace).Get(ctx, name, metav1.GetOptions{}); err == nil {
		t.Fatalf("claim %s is still present after the delete", name)
	}
}

func setSandboxStatus(t *testing.T, dyn dynamic.Interface, namespace, claim string, sandbox map[string]any) {
	t.Helper()
	ctx := context.Background()
	obj, err := dyn.Resource(claimGVR).Namespace(namespace).Get(ctx, claim, metav1.GetOptions{})
	if err != nil {
		t.Fatalf("get claim %s: %v", claim, err)
	}
	obj.Object["status"] = map[string]any{"sandbox": sandbox}
	if _, err := dyn.Resource(claimGVR).Namespace(namespace).Update(ctx, obj, metav1.UpdateOptions{}); err != nil {
		t.Fatalf("update claim %s: %v", claim, err)
	}
}
