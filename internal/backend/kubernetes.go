package backend

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"strings"
	"time"

	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/runtime/schema"
	"k8s.io/apimachinery/pkg/types"
	"k8s.io/apimachinery/pkg/util/wait"
	"k8s.io/client-go/dynamic"
	"k8s.io/client-go/kubernetes"
	"k8s.io/client-go/rest"
	"k8s.io/client-go/tools/clientcmd"
)

// Provider kinds.
const (
	KindPod          = "pod"
	KindSandboxClaim = "sandbox-claim"
)

// Labels applied to everything this driver creates, so a restarted gateway can find its own objects
// and nothing else.
const (
	LabelManaged  = "app.kubernetes.io/managed-by"
	LabelInstance = "keypod.io/instance"
	LabelIdentity = "keypod.io/identity"
)

// ProviderSpec is the declarative description of "what an instance is and how it is started".
//
// Nothing here is vendor-specific: which Kubernetes object to create is configuration, not code.
type ProviderSpec struct {
	Kind         string
	Template     string   // sandbox-claim: name of the template to instantiate
	WarmPool     string   // sandbox-claim: optional warm pool to claim against
	Image        string   // pod: any image that speaks SSH
	StartCommand []string // pod: optional override of the image entrypoint
	ReadyTimeout time.Duration

	// sandbox-claim: the CRD coordinates. Required for that kind, because guessing a group or a
	// resource name would tie the binary to one installation.
	APIGroup      string
	APIVersion    string
	ClaimResource string

	// SandboxClaim status field that carries the SSH endpoint ("host:port" or bare host).
	// Defaults to status.sandbox.serviceFQDN.
	EndpointField []string
}

// KubernetesConfig configures the driver.
type KubernetesConfig struct {
	Name          string
	Namespace     string
	Provider      ProviderSpec
	SSHUser       string
	SSHPrivateKey string
	HostKeyPolicy string

	// Kubeconfig is an explicit path; empty means in-cluster credentials.
	Kubeconfig string

	// Clients are injectable for tests; when nil they are built from Kubeconfig / in-cluster config.
	Client  kubernetes.Interface
	Dynamic dynamic.Interface
	Logger  *slog.Logger
}

// Kubernetes creates instances from Kubernetes objects: a bare pod, or a declarative claim.
type Kubernetes struct {
	cfg    KubernetesConfig
	client kubernetes.Interface
	dyn    dynamic.Interface
	log    *slog.Logger
}

// NewKubernetes validates the configuration and builds clients when they were not injected.
func NewKubernetes(cfg KubernetesConfig) (*Kubernetes, error) {
	if cfg.Name == "" {
		return nil, errors.New("kubernetes backend: name is required")
	}
	if cfg.Namespace == "" {
		return nil, fmt.Errorf("kubernetes backend %q: namespace is required", cfg.Name)
	}
	if cfg.SSHUser == "" || cfg.SSHPrivateKey == "" {
		return nil, fmt.Errorf("kubernetes backend %q: ssh user and private key are required", cfg.Name)
	}
	if cfg.Provider.ReadyTimeout <= 0 {
		cfg.Provider.ReadyTimeout = 120 * time.Second
	}

	switch cfg.Provider.Kind {
	case KindPod:
		if cfg.Provider.Image == "" {
			return nil, fmt.Errorf("kubernetes backend %q: provider.image is required for kind %s", cfg.Name, KindPod)
		}
	case KindSandboxClaim:
		if cfg.Provider.Template == "" {
			return nil, fmt.Errorf("kubernetes backend %q: provider.template is required for kind %s", cfg.Name, KindSandboxClaim)
		}
		if cfg.Provider.APIGroup == "" || cfg.Provider.APIVersion == "" || cfg.Provider.ClaimResource == "" {
			return nil, fmt.Errorf("kubernetes backend %q: provider.api_group, api_version and claim_resource are required for kind %s", cfg.Name, KindSandboxClaim)
		}
		if len(cfg.Provider.EndpointField) == 0 {
			cfg.Provider.EndpointField = []string{"status", "sandbox", "serviceFQDN"}
		}
	default:
		return nil, fmt.Errorf("kubernetes backend %q: unknown provider.kind %q (want %s|%s)", cfg.Name, cfg.Provider.Kind, KindPod, KindSandboxClaim)
	}

	log := cfg.Logger
	if log == nil {
		log = slog.Default()
	}

	k := &Kubernetes{cfg: cfg, client: cfg.Client, dyn: cfg.Dynamic, log: log}
	if k.client == nil || k.dyn == nil {
		restCfg, err := buildRestConfig(cfg.Kubeconfig)
		if err != nil {
			return nil, err
		}
		if k.client == nil {
			c, err := kubernetes.NewForConfig(restCfg)
			if err != nil {
				return nil, fmt.Errorf("kubernetes client: %w", err)
			}
			k.client = c
		}
		if k.dyn == nil {
			d, err := dynamic.NewForConfig(restCfg)
			if err != nil {
				return nil, fmt.Errorf("dynamic client: %w", err)
			}
			k.dyn = d
		}
	}
	return k, nil
}

// Name implements Backend.
func (k *Kubernetes) Name() string { return k.cfg.Name }

// Create provisions one instance without binding it to an identity (warm capacity).
func (k *Kubernetes) Create(ctx context.Context) (Instance, error) {
	switch k.cfg.Provider.Kind {
	case KindPod:
		return k.createPod(ctx)
	case KindSandboxClaim:
		return k.createClaim(ctx)
	default:
		return Instance{}, fmt.Errorf("unsupported provider kind %q", k.cfg.Provider.Kind)
	}
}

// Acquire provisions an instance for one identity and records that binding on the object itself.
//
// The annotation is what makes a restart survivable: after a crash the gateway lists its own objects,
// reads the identity back, and hands the same instance to the same client instead of starting over.
func (k *Kubernetes) Acquire(ctx context.Context, identity string) (Instance, error) {
	in, err := k.Create(ctx)
	if err != nil {
		return Instance{}, err
	}
	ann := map[string]string{LabelIdentity: identity, LabelInstance: in.ID}
	if err := k.annotate(ctx, in.ID, ann); err != nil {
		// An unannotated instance would become an orphan on restart; give it back instead.
		_ = k.Release(ctx, in)
		return Instance{}, fmt.Errorf("annotate %s: %w", in.ID, err)
	}
	merged := map[string]string{}
	for k, v := range in.Annotations {
		merged[k] = v
	}
	for k, v := range ann {
		merged[k] = v
	}
	in.Annotations = merged
	return in, nil
}

// annotate merges annotations onto the object behind an instance.
func (k *Kubernetes) annotate(ctx context.Context, id string, ann map[string]string) error {
	patch, err := json.Marshal(map[string]any{
		"metadata": map[string]any{"annotations": ann},
	})
	if err != nil {
		return err
	}
	switch k.cfg.Provider.Kind {
	case KindPod:
		_, err = k.client.CoreV1().Pods(k.cfg.Namespace).Patch(ctx, id, types.MergePatchType, patch, metav1.PatchOptions{})
	case KindSandboxClaim:
		_, err = k.dyn.Resource(k.gvr()).Namespace(k.cfg.Namespace).Patch(ctx, id, types.MergePatchType, patch, metav1.PatchOptions{})
	default:
		err = fmt.Errorf("unsupported provider kind %q", k.cfg.Provider.Kind)
	}
	return err
}

// Release deletes the object behind an instance. Idempotent: a missing object is not an error.
func (k *Kubernetes) Release(ctx context.Context, in Instance) error {
	var err error
	switch k.cfg.Provider.Kind {
	case KindPod:
		zero := int64(0)
		err = k.client.CoreV1().Pods(k.cfg.Namespace).Delete(ctx, in.ID, metav1.DeleteOptions{GracePeriodSeconds: &zero})
	case KindSandboxClaim:
		err = k.dyn.Resource(k.gvr()).Namespace(k.cfg.Namespace).Delete(ctx, in.ID, metav1.DeleteOptions{})
	}
	if apierrors.IsNotFound(err) {
		return nil
	}
	if err != nil {
		return fmt.Errorf("delete %s: %w", in.ID, err)
	}
	return nil
}

// List returns instances this driver created (by label), so a restarted process can adopt them.
func (k *Kubernetes) List(ctx context.Context) ([]Instance, error) {
	switch k.cfg.Provider.Kind {
	case KindPod:
		pods, err := k.client.CoreV1().Pods(k.cfg.Namespace).List(ctx, metav1.ListOptions{
			LabelSelector: LabelManaged + "=keypod",
		})
		if err != nil {
			return nil, fmt.Errorf("list pods: %w", err)
		}
		out := make([]Instance, 0, len(pods.Items))
		for i := range pods.Items {
			out = append(out, k.instanceFromPod(&pods.Items[i]))
		}
		return out, nil
	case KindSandboxClaim:
		list, err := k.dyn.Resource(k.gvr()).Namespace(k.cfg.Namespace).List(ctx, metav1.ListOptions{
			LabelSelector: LabelManaged + "=keypod",
		})
		if err != nil {
			return nil, fmt.Errorf("list %s: %w", k.cfg.Provider.ClaimResource, err)
		}
		out := make([]Instance, 0, len(list.Items))
		for i := range list.Items {
			item := &list.Items[i]
			if addr := k.endpointFromObject(item); addr != "" {
				out = append(out, k.instance(addr, item.GetName(), item.GetAnnotations()))
			}
		}
		return out, nil
	default:
		return nil, nil
	}
}

// Adopt re-registers instances found after a restart. The driver itself keeps no state, so adoption
// only refreshes labels/annotations that the lease manager reads.
func (k *Kubernetes) Adopt(ctx context.Context, ins []Instance) error {
	return nil
}

// ---------------------------------------------------------------- pods

func (k *Kubernetes) createPod(ctx context.Context) (Instance, error) {
	name := "keypod-" + randomSuffix()
	pod := &corev1.Pod{
		ObjectMeta: metav1.ObjectMeta{
			Name:      name,
			Namespace: k.cfg.Namespace,
			Labels: map[string]string{
				LabelManaged: "keypod",
			},
		},
		Spec: corev1.PodSpec{
			RestartPolicy: corev1.RestartPolicyNever,
			Containers: []corev1.Container{{
				Name:    "instance",
				Image:   k.cfg.Provider.Image,
				Command: k.cfg.Provider.StartCommand,
				Ports:   []corev1.ContainerPort{{ContainerPort: 22, Protocol: corev1.ProtocolTCP}},
			}},
		},
	}
	created, err := k.client.CoreV1().Pods(k.cfg.Namespace).Create(ctx, pod, metav1.CreateOptions{})
	if err != nil {
		return Instance{}, fmt.Errorf("create pod: %w", err)
	}

	addr, err := k.waitForPod(ctx, created.Name)
	if err != nil {
		_ = k.Release(ctx, Instance{ID: created.Name})
		return Instance{}, err
	}
	return k.instance(addr, created.Name, created.Annotations), nil
}

func (k *Kubernetes) waitForPod(ctx context.Context, name string) (string, error) {
	var addr string
	err := wait.PollUntilContextTimeout(ctx, 250*time.Millisecond, k.cfg.Provider.ReadyTimeout, true,
		func(ctx context.Context) (bool, error) {
			pod, err := k.client.CoreV1().Pods(k.cfg.Namespace).Get(ctx, name, metav1.GetOptions{})
			if err != nil {
				return false, err
			}
			if pod.Status.Phase == corev1.PodFailed {
				return false, fmt.Errorf("pod %s failed: %s", name, pod.Status.Message)
			}
			if pod.Status.PodIP == "" || !podReady(pod) {
				return false, nil
			}
			addr = fmt.Sprintf("%s:%d", pod.Status.PodIP, 22)
			return true, nil
		})
	if err != nil {
		return "", fmt.Errorf("wait for pod %s: %w", name, err)
	}
	return addr, nil
}

func podReady(pod *corev1.Pod) bool {
	for _, c := range pod.Status.Conditions {
		if c.Type == corev1.PodReady {
			return c.Status == corev1.ConditionTrue
		}
	}
	return false
}

func (k *Kubernetes) instanceFromPod(pod *corev1.Pod) Instance {
	addr := ""
	if pod.Status.PodIP != "" {
		addr = fmt.Sprintf("%s:%d", pod.Status.PodIP, 22)
	}
	return k.instance(addr, pod.Name, pod.Annotations)
}

// ---------------------------------------------------------------- sandbox claims

func (k *Kubernetes) gvr() schema.GroupVersionResource {
	return schema.GroupVersionResource{
		Group:    k.cfg.Provider.APIGroup,
		Version:  k.cfg.Provider.APIVersion,
		Resource: k.cfg.Provider.ClaimResource,
	}
}

func (k *Kubernetes) createClaim(ctx context.Context) (Instance, error) {
	name := "keypod-" + randomSuffix()
	spec := map[string]any{
		"templateRef": map[string]any{"name": k.cfg.Provider.Template},
	}
	if k.cfg.Provider.WarmPool != "" {
		spec["warmPoolRef"] = map[string]any{"name": k.cfg.Provider.WarmPool}
	}
	obj := &unstructured.Unstructured{Object: map[string]any{
		"apiVersion": k.cfg.Provider.APIGroup + "/" + k.cfg.Provider.APIVersion,
		"kind":       claimKind(k.cfg.Provider.ClaimResource),
		"metadata": map[string]any{
			"name":      name,
			"namespace": k.cfg.Namespace,
			"labels":    map[string]any{LabelManaged: "keypod"},
		},
		"spec": spec,
	}}

	created, err := k.dyn.Resource(k.gvr()).Namespace(k.cfg.Namespace).Create(ctx, obj, metav1.CreateOptions{})
	if err != nil {
		return Instance{}, fmt.Errorf("create %s: %w", k.cfg.Provider.ClaimResource, err)
	}

	addr, err := k.waitForClaim(ctx, created.GetName())
	if err != nil {
		_ = k.Release(ctx, Instance{ID: created.GetName()})
		return Instance{}, err
	}
	return k.instance(addr, created.GetName(), nil), nil
}

func (k *Kubernetes) waitForClaim(ctx context.Context, name string) (string, error) {
	var addr string
	err := wait.PollUntilContextTimeout(ctx, 250*time.Millisecond, k.cfg.Provider.ReadyTimeout, true,
		func(ctx context.Context) (bool, error) {
			obj, err := k.dyn.Resource(k.gvr()).Namespace(k.cfg.Namespace).Get(ctx, name, metav1.GetOptions{})
			if err != nil {
				return false, err
			}
			if a := k.endpointFromObject(obj); a != "" {
				addr = a
				return true, nil
			}
			return false, nil
		})
	if err != nil {
		return "", fmt.Errorf("wait for %s %s (endpoint field %s): %w",
			k.cfg.Provider.ClaimResource, name, strings.Join(k.cfg.Provider.EndpointField, "."), err)
	}
	return addr, nil
}

// endpointFromObject reads the configured status field and normalises it to host:port.
func (k *Kubernetes) endpointFromObject(obj *unstructured.Unstructured) string {
	raw, found, err := unstructured.NestedString(obj.Object, k.cfg.Provider.EndpointField...)
	if err != nil || !found || raw == "" {
		return ""
	}
	if strings.Contains(raw, ":") {
		return raw
	}
	return raw + ":22"
}

func claimKind(resource string) string {
	// "sandboxclaims" -> "SandboxClaim": the CRD kind is conventionally the singular in lower camel.
	s := strings.TrimSuffix(resource, "s")
	if s == "" {
		return resource
	}
	return strings.ToUpper(s[:1]) + s[1:]
}

// ---------------------------------------------------------------- helpers

func (k *Kubernetes) instance(addr, id string, annotations map[string]string) Instance {
	return Instance{
		ID:            id,
		Addr:          addr,
		User:          k.cfg.SSHUser,
		PrivateKey:    k.cfg.SSHPrivateKey,
		HostKeyPolicy: k.cfg.HostKeyPolicy,
		Annotations:   annotations,
	}
}

func buildRestConfig(kubeconfig string) (*rest.Config, error) {
	if kubeconfig != "" {
		cfg, err := clientcmd.BuildConfigFromFlags("", kubeconfig)
		if err != nil {
			return nil, fmt.Errorf("kubeconfig %s: %w", kubeconfig, err)
		}
		return cfg, nil
	}
	cfg, err := rest.InClusterConfig()
	if err != nil {
		return nil, fmt.Errorf("in-cluster config (set provider kubeconfig outside a cluster): %w", err)
	}
	return cfg, nil
}
