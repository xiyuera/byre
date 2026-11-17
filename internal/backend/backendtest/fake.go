// Package backendtest holds the fakes that tests of the packages above share.
//
// It is a normal package rather than a _test.go file because three packages need the same fake: the
// driver's own tests, the lease tests and the ssh front end tests. The last two have to observe what
// happens when an instance disappears underneath a lease, and a private copy in each package would
// drift — the drift would show up as a test that passes for the wrong reason.
package backendtest

import (
	"context"
	"fmt"
	"sync/atomic"
	"testing"
	"time"

	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/runtime/schema"
	"k8s.io/apimachinery/pkg/types"
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
