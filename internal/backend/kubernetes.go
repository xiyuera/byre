package backend

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"net"
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
	KindWorkloads = "workloads"
	KindClaims    = "claims"
)

// Labels applied to everything this driver creates, so a restarted gateway can find its own objects
// and nothing else.
const (
	LabelManaged  = "app.kubernetes.io/managed-by"
	LabelInstance = "byre.io/instance"
	LabelIdentity = "byre.io/identity"
)

// ProviderSpec is the declarative description of "what an instance is and how it is started".
//
// Nothing here is vendor-specific: which Kubernetes object to create is configuration, not code.
type ProviderSpec struct {
	Kind         string
	Template     string   // claims: name of the template to instantiate
	WarmPool     string   // claims: optional warm pool to claim against
	Image        string   // workloads: any image that speaks SSH
	StartCommand []string // workloads: optional override of the image entrypoint
	ReadyTimeout time.Duration

	// claims: the API coordinates. Required for that kind, because guessing a group or a resource
	// name would tie the binary to one installation. There is no default: no API is built in.
	APIGroup   string
	APIVersion string
	Resource   string
	ObjectKind string

	// claims: status field carrying the SSH endpoint ("host:port" or bare host).
	// Defaults to status.sandbox.serviceFQDN, which is the shape this project was developed against.
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
	case KindWorkloads:
		if cfg.Provider.Image == "" {
			return nil, fmt.Errorf("kubernetes backend %q: provider.image is required for kind %s", cfg.Name, KindWorkloads)
		}
	case KindClaims:
		if cfg.Provider.Template == "" && cfg.Provider.WarmPool == "" {
			return nil, fmt.Errorf("kubernetes backend %q: provider.template or provider.warm_pool is required for kind %s", cfg.Name, KindClaims)
		}
		if cfg.Provider.APIGroup == "" || cfg.Provider.APIVersion == "" || cfg.Provider.Resource == "" || cfg.Provider.ObjectKind == "" {
			return nil, fmt.Errorf("kubernetes backend %q: provider.api_group, api_version, claim_resource and claim_kind are required for kind %s", cfg.Name, KindClaims)
		}
		if len(cfg.Provider.EndpointField) == 0 {
			cfg.Provider.EndpointField = []string{"status", "sandbox", "serviceFQDN"}
		}
	default:
		return nil, fmt.Errorf("kubernetes backend %q: unknown provider.kind %q (want %s|%s)", cfg.Name, cfg.Provider.Kind, KindWorkloads, KindClaims)
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
	case KindWorkloads:
		return k.createPod(ctx)
	case KindClaims:
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

// Bind records the identity on a pooled instance (implements backend.Binder).
func (k *Kubernetes) Bind(ctx context.Context, in Instance, identity string) error {
	ann := map[string]string{LabelIdentity: identity, LabelInstance: in.ID}
	if err := k.annotate(ctx, in.ID, ann); err != nil {
		return fmt.Errorf("annotate %s: %w", in.ID, err)
	}
	return nil
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
	case KindWorkloads:
		_, err = k.client.CoreV1().Pods(k.cfg.Namespace).Patch(ctx, id, types.MergePatchType, patch, metav1.PatchOptions{})
	case KindClaims:
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
	case KindWorkloads:
		zero := int64(0)
		err = k.client.CoreV1().Pods(k.cfg.Namespace).Delete(ctx, in.ID, metav1.DeleteOptions{GracePeriodSeconds: &zero})
	case KindClaims:
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
	case KindWorkloads:
		pods, err := k.client.CoreV1().Pods(k.cfg.Namespace).List(ctx, metav1.ListOptions{
			LabelSelector: LabelManaged + "=byre",
		})
		if err != nil {
			return nil, fmt.Errorf("list pods: %w", err)
		}
		out := make([]Instance, 0, len(pods.Items))
		for i := range pods.Items {
			out = append(out, k.instanceFromPod(&pods.Items[i]))
		}
		return out, nil
	case KindClaims:
		list, err := k.dyn.Resource(k.gvr()).Namespace(k.cfg.Namespace).List(ctx, metav1.ListOptions{
			LabelSelector: LabelManaged + "=byre",
		})
		if err != nil {
			return nil, fmt.Errorf("list %s: %w", k.cfg.Provider.Resource, err)
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
	name := "byre-" + randomSuffix()
	pod := &corev1.Pod{
		ObjectMeta: metav1.ObjectMeta{
			Name:      name,
			Namespace: k.cfg.Namespace,
			Labels: map[string]string{
				LabelManaged: "byre",
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
		Resource: k.cfg.Provider.Resource,
	}
}

func (k *Kubernetes) createClaim(ctx context.Context) (Instance, error) {
	name := "byre-" + randomSuffix()
	// Only the selectors that were configured are sent: CRD schemas reject unknown or mutually
	// exclusive fields, so inventing one would make the backend unusable for that cluster.
	spec := map[string]any{}
	if k.cfg.Provider.WarmPool != "" {
		spec["warmPoolRef"] = map[string]any{"name": k.cfg.Provider.WarmPool}
	}
	if k.cfg.Provider.Template != "" {
		spec["templateRef"] = map[string]any{"name": k.cfg.Provider.Template}
	}
	obj := &unstructured.Unstructured{Object: map[string]any{
		"apiVersion": k.cfg.Provider.APIGroup + "/" + k.cfg.Provider.APIVersion,
		"kind":       k.claimKind(),
		"metadata": map[string]any{
			"name":      name,
			"namespace": k.cfg.Namespace,
			"labels":    map[string]any{LabelManaged: "byre"},
		},
		"spec": spec,
	}}

	created, err := k.dyn.Resource(k.gvr()).Namespace(k.cfg.Namespace).Create(ctx, obj, metav1.CreateOptions{})
	if err != nil {
		return Instance{}, fmt.Errorf("create %s: %w", k.cfg.Provider.Resource, err)
	}

	addr, err := k.waitForClaim(ctx, created.GetName())
	if err != nil {
		_ = k.Release(ctx, Instance{ID: created.GetName()})
		return Instance{}, err
	}
	return k.instance(addr, created.GetName(), nil), nil
}

// probeAddr is how readiness is judged: a TCP connect, not a DNS lookup.
//
// Name resolution is not trustworthy here — on a network with fake-IP DNS, any nonsense name answers
// with a synthetic address — so the only honest question is "can I open the port". Tests replace this
// variable to simulate lag without a cluster.
var probeAddr = func(ctx context.Context, addr string) error {
	d := net.Dialer{Timeout: 2 * time.Second}
	conn, err := d.DialContext(ctx, "tcp", addr)
	if err != nil {
		return err
	}
	return conn.Close()
}

func (k *Kubernetes) waitForClaim(ctx context.Context, name string) (string, error) {
	var addr string
	err := wait.PollUntilContextTimeout(ctx, 250*time.Millisecond, k.cfg.Provider.ReadyTimeout, true,
		func(ctx context.Context) (bool, error) {
			obj, err := k.dyn.Resource(k.gvr()).Namespace(k.cfg.Namespace).Get(ctx, name, metav1.GetOptions{})
			if err != nil {
				return false, err
			}
			// The controller publishes the endpoint before the Service is resolvable, and this network
			// adds fake-IP DNS on top. Readiness therefore means "one advertised address answers on
			// port 22 from here", tried in preference order.
			for _, candidate := range k.endpointCandidates(obj) {
				if err := probeAddr(ctx, candidate); err != nil {
					continue
				}
				addr = candidate
				return true, nil
			}
			return false, nil
		})
	if err != nil {
		return "", fmt.Errorf("wait for %s %s (endpoint field %s): %w",
			k.cfg.Provider.Resource, name, strings.Join(k.cfg.Provider.EndpointField, "."), err)
	}
	return addr, nil
}

// endpointFromObject reads the configured status field and normalises it to host:port.
func (k *Kubernetes) endpointFromObject(obj *unstructured.Unstructured) string {
	raw, found, err := unstructured.NestedString(obj.Object, k.cfg.Provider.EndpointField...)
	if err != nil || !found || raw == "" {
		return ""
	}
	return withDefaultPort(raw)
}

// endpointCandidates lists every address the object advertises, most preferred first.
//
// The service name appears in status before its DNS record does, and the pod IP is usable immediately;
// listing both lets readiness mean "one of these answers on 22" instead of "a string is present".
func (k *Kubernetes) endpointCandidates(obj *unstructured.Unstructured) []string {
	var out []string
	if a := k.endpointFromObject(obj); a != "" {
		out = append(out, a)
	}
	if ips, found, err := unstructured.NestedStringSlice(obj.Object, "status", "sandbox", "podIPs"); err == nil && found {
		for _, ip := range ips {
			out = append(out, withDefaultPort(ip))
		}
	}
	return out
}

func withDefaultPort(addr string) string {
	if strings.Contains(addr, ":") {
		return addr
	}
	return addr + ":22"
}

// claimKind returns the Kind to send. It is configured, not derived: "sandboxclaims" minus the plural
// "s" is "sandboxclaim", and the API server accepts only "SandboxClaim" — guessing here fails on the
// first real cluster.
func (k *Kubernetes) claimKind() string { return k.cfg.Provider.ObjectKind }

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
