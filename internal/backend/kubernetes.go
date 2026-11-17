package backend

import (
	"bufio"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net"
	"strings"
	"time"

	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	"k8s.io/apimachinery/pkg/api/resource"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/runtime/schema"
	"k8s.io/apimachinery/pkg/types"
	"k8s.io/apimachinery/pkg/util/wait"
	"k8s.io/client-go/dynamic"
	"k8s.io/client-go/kubernetes"
	"k8s.io/client-go/rest"
	"k8s.io/client-go/tools/clientcmd"

	"byre/internal/metrics"
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
	// LabelUID records which object the binding belongs to. Without it a recycled name (delete, then
	// create something else under the same name) silently inherits the previous identity.
	LabelUID = "byre.io/uid"
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

	// AllowRoot lets a pod instance start as uid 0. The default is to require a non-root user with no
	// capabilities, because an SSH image that "needs root" usually means its entrypoint was never
	// written for an unprivileged container. Set it for images that genuinely cannot run otherwise.
	AllowRoot bool

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

	// DryRun validates the configuration and returns a driver without cluster clients. -check-config
	// uses it so that a configuration file can be checked on a workstation that has no cluster
	// credentials; it says nothing about whether the cluster would accept the objects.
	DryRun bool
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
			return nil, fmt.Errorf("kubernetes backend %q: provider.api_group, api_version, resource and object_kind are required for kind %s", cfg.Name, KindClaims)
		}
		if len(cfg.Provider.EndpointField) == 0 {
			cfg.Provider.EndpointField = []string{"status", "sandbox", "serviceFQDN"}
		}
	default:
		return nil, fmt.Errorf("kubernetes backend %q: unknown provision %q (want %s|%s)", cfg.Name, cfg.Provider.Kind, KindWorkloads, KindClaims)
	}

	log := cfg.Logger
	if log == nil {
		log = slog.Default()
	}

	k := &Kubernetes{cfg: cfg, client: cfg.Client, dyn: cfg.Dynamic, log: log}
	if cfg.DryRun {
		return k, nil
	}
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
	ann := bindingAnnotations(in, identity)
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
	ann := bindingAnnotations(in, identity)
	if err := k.annotate(ctx, in.ID, ann); err != nil {
		return fmt.Errorf("annotate %s: %w", in.ID, err)
	}
	return nil
}

// bindingAnnotations is what a restarted process reads back: who holds the instance, and which object
// that lease was made against. The UID is written whenever the driver knows it; an object without one
// (a driver that has no such concept) keeps the older two-key form.
func bindingAnnotations(in Instance, identity string) map[string]string {
	ann := map[string]string{LabelIdentity: identity, LabelInstance: in.ID}
	if in.UID != "" {
		ann[LabelUID] = in.UID
	}
	return ann
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

// Alive implements backend.Liveness with one single-object read.
//
// This is the check the gateway was missing. A deleted pod takes its address with it, but nothing tells
// the lease table: the entry stays, and every reconnect from that identity is dialled at an address
// where no process listens — the client sees its connection closed for as long as ttl+grace (seven
// minutes by default). Nothing on the object can answer the question either, because a deleted object
// is not returned by List at all. Only a positive query answers "is it still there", and a GET on the
// name is the cheapest one that does.
//
// Three outcomes, and the caller treats them differently:
//   - the object exists: nil;
//   - NotFound, or a deletion in progress: ErrInstanceGone, wrapped with the reason;
//   - anything else (apiserver unreachable, forbidden): a plain error, meaning "unknown".
//
// A terminating object counts as gone. Its address is on its way out, and a client reconnected to an
// instance in that state is re-syncing its work onto something that is about to stop.
func (k *Kubernetes) Alive(ctx context.Context, in Instance) error {
	switch k.cfg.Provider.Kind {
	case KindWorkloads:
		pod, err := k.client.CoreV1().Pods(k.cfg.Namespace).Get(ctx, in.ID, metav1.GetOptions{})
		if apierrors.IsNotFound(err) {
			return fmt.Errorf("%w: pod %s", ErrInstanceGone, in.ID)
		}
		if err != nil {
			return fmt.Errorf("get pod %s: %w", in.ID, err)
		}
		if pod.DeletionTimestamp != nil {
			return fmt.Errorf("%w: pod %s is terminating (deleted at %s)",
				ErrInstanceGone, in.ID, pod.DeletionTimestamp.UTC().Format(time.RFC3339))
		}
		return nil
	case KindClaims:
		obj, err := k.dyn.Resource(k.gvr()).Namespace(k.cfg.Namespace).Get(ctx, in.ID, metav1.GetOptions{})
		if apierrors.IsNotFound(err) {
			return fmt.Errorf("%w: %s %s", ErrInstanceGone, k.claimKind(), in.ID)
		}
		if err != nil {
			return fmt.Errorf("get %s %s: %w", k.cfg.Provider.Resource, in.ID, err)
		}
		if obj.GetDeletionTimestamp() != nil {
			return fmt.Errorf("%w: %s %s is terminating", ErrInstanceGone, k.claimKind(), in.ID)
		}
		return nil
	default:
		return fmt.Errorf("unsupported provider kind %q", k.cfg.Provider.Kind)
	}
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
			out = append(out, k.boundInstance(k.instanceFromPod(&pods.Items[i])))
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
				out = append(out, k.boundInstance(k.instance(addr, item.GetName(), string(item.GetUID()), item.GetAnnotations())))
			}
		}
		return out, nil
	}
	return nil, nil
}

// boundInstance decides whether a listed object still holds the identity its annotation claims.
//
// A name is not an instance. If the object behind a lease was deleted and recreated under the same
// name, the new object carries none of the old one's state, and handing it to the client that held the
// lease would be a silent data loss ("where did my files go"). The UID annotation is the only thing
// that distinguishes the two, so an object whose annotation names a different UID is returned
// *unbound*: adoption then treats it as stale capacity and destroys it, instead of inheriting a
// stranger's instance.
func (k *Kubernetes) boundInstance(in Instance) Instance {
	if in.Annotations[LabelIdentity] == "" {
		return in
	}
	annotated := in.Annotations[LabelUID]
	if annotated == "" {
		// Objects created before v0.1.3 carry no UID. Treating those as stale would destroy every
		// live lease on upgrade; they are adopted as before and Adopt backfills the annotation.
		k.log.Info("binding without uid annotation", "instance", in.ID, "object_uid", in.UID,
			"action", "adopting and backfilling")
		return in
	}
	if annotated == in.UID {
		return in
	}
	k.log.Warn("stale-binding", "backend", k.cfg.Name, "instance", in.ID,
		"annotated_uid", annotated, "object_uid", in.UID,
		"reason", "object was replaced under the same name; the identity is not inherited")
	ann := make(map[string]string, len(in.Annotations))
	for name, v := range in.Annotations {
		ann[name] = v
	}
	delete(ann, LabelIdentity)
	in.Annotations = ann
	return in
}

// Adopt re-registers instances found after a restart. The driver itself keeps no state, so adoption
// only refreshes the annotations that the lease manager reads — in practice, backfilling the UID on
// objects that were bound by an older version.
func (k *Kubernetes) Adopt(ctx context.Context, ins []Instance) error {
	var firstErr error
	for _, in := range ins {
		if in.Annotations[LabelIdentity] == "" || in.UID == "" {
			continue
		}
		if in.Annotations[LabelUID] == in.UID {
			continue
		}
		if err := k.annotate(ctx, in.ID, map[string]string{LabelUID: in.UID}); err != nil && firstErr == nil {
			firstErr = fmt.Errorf("backfill uid on %s: %w", in.ID, err)
		}
	}
	return firstErr
}

// AnonymousInstancesAreStale implements StaleCapacity. Every object this driver creates carries
// LabelIdentity, so a listed object without it is warm capacity from an earlier process: it belongs to
// nobody, and the pool is refilled right after adoption.
func (k *Kubernetes) AnonymousInstancesAreStale() bool { return true }

var _ StaleCapacity = (*Kubernetes)(nil)

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
			// Defaults are conservative on purpose: an instance runs an SSH daemon and nothing else,
			// so it has no business holding capabilities, escalating privileges or escaping the
			// default seccomp profile. An image that cannot start under these settings is the
			// interesting case — it is usually a root-only entrypoint, not a real requirement.
			SecurityContext: k.podSecurityContext(),
			Containers: []corev1.Container{{
				Name:            "instance",
				Image:           k.cfg.Provider.Image,
				Command:         k.cfg.Provider.StartCommand,
				Ports:           []corev1.ContainerPort{{ContainerPort: 22, Protocol: corev1.ProtocolTCP}},
				SecurityContext: k.containerSecurityContext(),
				Resources: corev1.ResourceRequirements{
					Requests: corev1.ResourceList{
						corev1.ResourceCPU:    resource.MustParse("10m"),
						corev1.ResourceMemory: resource.MustParse("32Mi"),
					},
					Limits: corev1.ResourceList{
						corev1.ResourceCPU:    resource.MustParse("500m"),
						corev1.ResourceMemory: resource.MustParse("512Mi"),
					},
				},
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
	return k.instance(addr, created.Name, string(created.UID), created.Annotations), nil
}

// podSecurityContext is the pod-level hardening applied to every instance.
func (k *Kubernetes) podSecurityContext() *corev1.PodSecurityContext {
	sc := &corev1.PodSecurityContext{
		SeccompProfile: &corev1.SeccompProfile{Type: corev1.SeccompProfileTypeRuntimeDefault},
	}
	if !k.cfg.Provider.AllowRoot {
		sc.RunAsNonRoot = boolPtr(true)
	}
	return sc
}

// containerSecurityContext is the per-container hardening: no privilege escalation, no capabilities.
// Dropping ALL also drops the ones sshd would use to change uid, which is the point — the instance
// serves one user whose key the gateway already holds.
func (k *Kubernetes) containerSecurityContext() *corev1.SecurityContext {
	sc := &corev1.SecurityContext{
		AllowPrivilegeEscalation: boolPtr(false),
		Capabilities:             &corev1.Capabilities{Drop: []corev1.Capability{"ALL"}},
	}
	if k.cfg.Provider.AllowRoot {
		// runAsUser is left to the image when root is allowed, but escalation stays off.
		sc.RunAsNonRoot = boolPtr(false)
	} else {
		sc.RunAsNonRoot = boolPtr(true)
	}
	return sc
}

func boolPtr(b bool) *bool { return &b }

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
			// PodReady is the kubelet's verdict on the container's readiness probe, not proof that port
			// 22 answers: images that start sshd from an entrypoint script are Ready first and dialable
			// later. ADR-0005 makes readiness "one address answers on 22 from here", and the claim path
			// already asks exactly that, so the pod path asks it too.
			if err := probeAddr(ctx, addr); err != nil {
				return false, nil
			}
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
	return k.instance(addr, pod.Name, string(pod.UID), pod.Annotations)
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
	return k.instance(addr, created.GetName(), string(created.GetUID()), created.GetAnnotations()), nil
}

// probeAddr is how readiness is judged: the port answers *and* the peer speaks SSH.
//
// Name resolution is not trustworthy here — on a network with fake-IP DNS, any nonsense name answers
// with a synthetic address — so the question starts with "can I open the port". But a TCP connect is
// not enough either: a half-started sshd, a Service pointing at the wrong port, or a proxy that accepts
// every connection passes a bare dial and then breaks every session with "connection reset". Reading
// the version banner is the cheapest observation that distinguishes "listening" from "serving SSH".
// Tests replace this variable to simulate lag without a cluster.
var probeAddr = sshBannerProbe

// bannerTimeout bounds the read after the TCP connect. sshd sends its banner before the client says
// anything, so a peer that has not spoken within this window is not going to.
const bannerTimeout = 2 * time.Second

// sshBannerProbe connects, reads the first protocol line, and requires it to announce SSH 2.0.
func sshBannerProbe(ctx context.Context, addr string) error {
	d := net.Dialer{Timeout: 2 * time.Second}
	conn, err := d.DialContext(ctx, "tcp", addr)
	if err != nil {
		return err
	}
	defer conn.Close()

	deadline := time.Now().Add(bannerTimeout)
	if dl, ok := ctx.Deadline(); ok && dl.Before(deadline) {
		deadline = dl
	}
	_ = conn.SetReadDeadline(deadline)

	// RFC 4253 allows a handful of lines before the version string; nothing in this deployment emits
	// them, and accepting arbitrary leading lines would let a chatty non-SSH peer pass the probe. The
	// first line must therefore be the banner.
	line, err := bufio.NewReader(io.LimitReader(conn, 255)).ReadString('\n')
	if err != nil {
		// EOF, a read deadline or the 255-byte cap before the CRLF means the peer stopped
		// mid-banner. Accepting a truncated line would report ready for exactly the half-started
		// process this probe exists to catch: a peer that manages "SSH-2.0-" and then hangs or dies
		// looks like a healthy one until the first real session fails.
		metrics.IncHandshakeFail()
		return fmt.Errorf("%s: tcp connected but no complete ssh banner: %w", addr, err)
	}
	line = strings.TrimRight(line, "\r\n")
	if !strings.HasPrefix(line, "SSH-2.") {
		metrics.IncHandshakeFail()
		return fmt.Errorf("%s: tcp connected but banner is not SSH-2: %q", addr, line)
	}
	return nil
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

func (k *Kubernetes) instance(addr, id, uid string, annotations map[string]string) Instance {
	return Instance{
		ID:            id,
		UID:           uid,
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
