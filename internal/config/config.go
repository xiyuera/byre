// Package config holds byre's declarative configuration and its validation rules.
//
// The gateway deliberately knows nothing about what an "instance" physically is. A backend declares
// three things and nothing else: how an instance is obtained (claims, workloads, endpoints, or
// command), how it is reached over SSH, and how it is returned. Anything that speaks SSH can
// therefore be described in configuration instead of code.
//
// Validation is strict and fail-fast: unknown fields, unknown provision values, duplicate identities or
// missing required fields abort startup instead of silently falling back to a default.
package config

import (
	"crypto/sha256"
	"encoding/base64"
	"errors"
	"fmt"
	"net"
	"os"
	"path/filepath"
	"strings"
	"time"

	"gopkg.in/yaml.v3"
)

// Provision is how an instance comes into existence. These names are deliberately about behaviour,
// not about a product: nothing in this list says "kubernetes".
const (
	ProvisionClaims    = "claims"    // ask a controller to hand one over (any declarative claim API)
	ProvisionWorkloads = "workloads" // create the workload ourselves from an image
	ProvisionEndpoints = "endpoints" // no provisioning: a fixed set of reachable SSH endpoints
	ProvisionCommand   = "command"   // an external program provisions and describes the instance
)

// Defaults applied before validation.
const (
	DefaultListen         = ":2222"
	DefaultTTL            = 360 * time.Second
	DefaultGrace          = 60 * time.Second
	DefaultPoolSize       = 2
	DefaultSSHPort        = 22
	DefaultCommandTimeout = 120 * time.Second
	DefaultReadyTimeout   = 120 * time.Second
)

// Duration is a time.Duration that is written as a string ("360s") in YAML.
//
// It remembers whether the key was present at all. An explicit `ttl: 0s` is a value somebody wrote, and
// substituting the default over it turns a configuration mistake into a working gateway with a
// different lifetime than the file says (F7).
type Duration struct {
	value time.Duration
	set   bool
}

// NewDuration builds a Duration that counts as present. It exists for code and tests that do not come
// from a YAML document.
func NewDuration(d time.Duration) Duration { return Duration{value: d, set: true} }

// UnmarshalYAML implements yaml.Unmarshaler.
func (d *Duration) UnmarshalYAML(node *yaml.Node) error {
	var s string
	if err := node.Decode(&s); err != nil {
		return fmt.Errorf("duration must be a string such as \"360s\": %w", err)
	}
	v, err := time.ParseDuration(s)
	if err != nil {
		return fmt.Errorf("invalid duration %q: %w", s, err)
	}
	d.value = v
	d.set = true
	return nil
}

// MarshalYAML writes the duration back as the string the file used.
func (d Duration) MarshalYAML() (any, error) { return d.value.String(), nil }

// Std returns the value as a time.Duration.
func (d Duration) Std() time.Duration { return d.value }

// IsSet reports whether the key appeared in the document.
func (d Duration) IsSet() bool { return d.set }

// String implements fmt.Stringer.
func (d Duration) String() string { return d.value.String() }

// Number is an integer that remembers whether the key was present, so that an explicit 0 (which is
// meaningful for pool_size) is not mistaken for a missing key.
type Number struct {
	value int
	set   bool
}

// NewNumber builds a Number that counts as present.
func NewNumber(v int) Number { return Number{value: v, set: true} }

// UnmarshalYAML implements yaml.Unmarshaler.
func (n *Number) UnmarshalYAML(node *yaml.Node) error {
	var v int
	if err := node.Decode(&v); err != nil {
		return fmt.Errorf("expected an integer: %w", err)
	}
	n.value = v
	n.set = true
	return nil
}

// MarshalYAML writes the number back as an integer.
func (n Number) MarshalYAML() (any, error) { return n.value, nil }

// Value returns the configured integer.
func (n Number) Value() int { return n.value }

// IsSet reports whether the key appeared in the document.
func (n Number) IsSet() bool { return n.set }

// Config is the root document.
type Config struct {
	Listen string `yaml:"listen"`
	// HostKey is the SSH host key clients pin. Without it a restart silently changes the fingerprint and
	// every client with strict checking fails; generating one on the fly is fine only for tests, which
	// is why Validate rejects an empty value unless AllowEphemeralHostKey was set on the command line.
	HostKey    string     `yaml:"host_key"`
	Lease      Lease      `yaml:"lease"`
	Backends   []Backend  `yaml:"backends"`
	Identities []Identity `yaml:"identities"`

	// AllowEphemeralHostKey is set by the binary from -allow-ephemeral-hostkey, never from YAML: a
	// deployment must not be able to opt out of a stable fingerprint by editing its own config.
	AllowEphemeralHostKey bool `yaml:"-"`
}

// Lease holds the timing and pool parameters of a lease.
type Lease struct {
	TTL   Duration `yaml:"ttl"`   // idle before an instance is reclaimed
	Grace Duration `yaml:"grace"` // extra wait after a disconnect before reclaiming

	// PoolSize is a pointer because 0 is a documented value ("no warm capacity: create on demand")
	// and a plain int cannot tell it apart from "not set". With an int the documented rotation
	// procedure — shrink the pool to 0, replace the key, grow it back — silently did nothing, because
	// the zero was replaced by the default before anyone looked at it.
	PoolSize *int `yaml:"pool_size"`
}

// PoolSizeValue returns the pool size after defaults: explicit 0 stays 0.
func (l Lease) PoolSizeValue() int {
	if l.PoolSize == nil {
		return DefaultPoolSize
	}
	return *l.PoolSize
}

// Backend describes one way of obtaining instances.
type Backend struct {
	Name string `yaml:"name"`
	// Provision selects the driver and is the only required discriminator:
	// claims | workloads | endpoints | command.
	Provision string `yaml:"provision"`
	// Type is the pre-0.3 spelling of Provision. It exists so that a stale config fails with a sentence
	// rather than "unknown field", and is rejected when set.
	Type       string   `yaml:"type"`
	Namespace  string   `yaml:"namespace"`  // claims, workloads
	Kubeconfig string   `yaml:"kubeconfig"` // claims, workloads: empty = in-cluster
	Endpoints  []string `yaml:"endpoints"`  // provision: endpoints
	Command    Command  `yaml:"command"`    // provision: command
	Provider   Provider `yaml:"provider"`   // provision: claims, workloads
	SSH        SSH      `yaml:"ssh"`        // every provision kind: how the instance is reached
}

// Command provisions instances by running a program. It is the escape hatch that keeps the gateway
// usable on infrastructure nobody wrote a driver for: cloud instances, Incus/LXD, a CMDB, a queue.
//
// The program prints one JSON object per acquire/list call:
//
//	{"id":"vm-17","addr":"192.0.2.17:22","user":"ci","private_key":"/path/key"}
//
// `user` and `private_key` are optional and fall back to the backend's ssh block. Without a `list`
// command the gateway cannot adopt instances after a restart, so orphans from this backend are the
// operator's problem - stated here rather than discovered later.
type Command struct {
	Acquire []string `yaml:"acquire"`
	Release []string `yaml:"release"`
	List    []string `yaml:"list"`
	Timeout Duration `yaml:"timeout"`
	Env     []string `yaml:"env"` // KEY=VALUE added to every invocation
}

// Provider declares what the instance is and how it is started.
//
// api_group / api_version / resource / object_kind exist so that no CRD is hardcoded: the same binary drives
// any declarative claim API, and the field names are configuration.
type Provider struct {
	// Selectors for a declarative claim API. Which CRD it is, is configuration: the binary knows the
	// shape (create an object, wait for an address in its status), not the product.
	Template      string   `yaml:"template"`
	WarmPool      string   `yaml:"warm_pool"`
	APIGroup      string   `yaml:"api_group"`
	APIVersion    string   `yaml:"api_version"`
	Resource      string   `yaml:"resource"`
	ObjectKind    string   `yaml:"object_kind"`
	EndpointField []string `yaml:"endpoint_field"`

	// Workload creation, when no controller does it for us.
	Image        string   `yaml:"image"`
	StartCommand []string `yaml:"start_command"`

	ReadyTimeout Duration `yaml:"ready_timeout"`

	// RunAsRoot lets a pod instance start as uid 0. Default false: the driver then requires a non-root
	// user with no capabilities and the default seccomp profile.
	RunAsRoot bool `yaml:"run_as_root"`
}

// SSH is how the gateway reaches an instance.
type SSH struct {
	Port          Number `yaml:"port"`
	User          string `yaml:"user"`
	PrivateKey    string `yaml:"private_key"`
	HostKeyPolicy string `yaml:"host_key_policy"` // strict | accept-new | insecure
}

// Identity maps a client key to a backend.
type Identity struct {
	Fingerprint string   `yaml:"fingerprint"`
	Backend     string   `yaml:"backend"`
	Aliases     []string `yaml:"aliases"` // log labels only, never used for routing
}

// Option adjusts what Load accepts beyond the file itself.
type Option func(*Config)

// AllowEphemeralHostKey permits a configuration without host_key. It exists for tests and for
// -check-config of a lab file; a deployment that ships without host_key makes every restart change the
// gateway's fingerprint, which is the failure this project already hit once (ADR-0006).
func AllowEphemeralHostKey(allow bool) Option {
	return func(c *Config) { c.AllowEphemeralHostKey = allow }
}

// Load reads, defaults and validates a configuration file.
func Load(path string, opts ...Option) (*Config, error) {
	f, err := os.Open(path)
	if err != nil {
		return nil, fmt.Errorf("open config %s: %w", path, err)
	}
	defer f.Close()

	var cfg Config
	dec := yaml.NewDecoder(f)
	dec.KnownFields(true) // unknown keys are a configuration error, not a silent no-op
	if err := dec.Decode(&cfg); err != nil {
		return nil, fmt.Errorf("parse config %s: %w", path, err)
	}
	for _, opt := range opts {
		opt(&cfg)
	}
	cfg.ApplyDefaults()
	if err := cfg.Validate(); err != nil {
		return nil, fmt.Errorf("invalid config %s: %w", path, err)
	}
	return &cfg, nil
}

// ApplyDefaults fills every optional field that the document did not mention. A field that is present
// keeps its value even when that value is zero: `pool_size: 0` means "no warm capacity", and `ttl: 0s`
// must be rejected by Validate rather than quietly replaced by an hour.
func (c *Config) ApplyDefaults() {
	if strings.TrimSpace(c.Listen) == "" {
		c.Listen = DefaultListen
	}
	if !c.Lease.TTL.IsSet() {
		c.Lease.TTL = NewDuration(DefaultTTL)
	}
	if !c.Lease.Grace.IsSet() {
		c.Lease.Grace = NewDuration(DefaultGrace)
	}
	if c.Lease.PoolSize == nil {
		def := DefaultPoolSize
		c.Lease.PoolSize = &def
	}
	for i := range c.Backends {
		b := &c.Backends[i]
		if !b.SSH.Port.IsSet() {
			b.SSH.Port = NewNumber(DefaultSSHPort)
		}
		if strings.TrimSpace(b.SSH.HostKeyPolicy) == "" {
			b.SSH.HostKeyPolicy = "accept-new"
		}
		if !b.Provider.ReadyTimeout.IsSet() {
			b.Provider.ReadyTimeout = NewDuration(DefaultReadyTimeout)
		}
		if b.Provision == ProvisionCommand && !b.Command.Timeout.IsSet() {
			b.Command.Timeout = NewDuration(DefaultCommandTimeout)
		}
	}
}

// Validate checks every invariant and returns all violations at once.
func (c *Config) Validate() error {
	var errs []error
	fail := func(format string, args ...any) { errs = append(errs, fmt.Errorf(format, args...)) }

	if strings.TrimSpace(c.HostKey) == "" && !c.AllowEphemeralHostKey {
		fail("host_key is required: without it every restart changes the gateway's fingerprint and every strict client stops connecting " +
			"(use -allow-ephemeral-hostkey only for tests)")
	}
	if c.Lease.TTL.Std() <= 0 {
		fail("lease.ttl must be > 0")
	}
	if c.Lease.Grace.Std() < 0 {
		fail("lease.grace must be >= 0")
	}
	if c.Lease.PoolSizeValue() < 0 {
		fail("lease.pool_size must be >= 0")
	}
	if len(c.Backends) == 0 {
		fail("at least one backend is required")
	}
	if p := strings.TrimSpace(c.HostKey); p != "" {
		// Checked here rather than at startup: a path that cannot be written is a first install that
		// dies halfway (the process cannot persist the key it just generated), and -check-config exists
		// to catch exactly that before a rollout.
		if err := checkHostKeyPath(p); err != nil {
			fail("host_key: %v", err)
		}
	}

	names := map[string]bool{}
	for i, b := range c.Backends {
		where := fmt.Sprintf("backends[%d]", i)
		if strings.TrimSpace(b.Name) == "" {
			fail("%s: name is required", where)
		} else if names[b.Name] {
			fail("%s: duplicate backend name %q", where, b.Name)
		}
		names[b.Name] = true

		if strings.TrimSpace(b.Type) != "" {
			fail("%s (%s): backend.type was replaced by backend.provision (%s|%s|%s|%s); the value %q is not used",
				where, b.Name, ProvisionClaims, ProvisionWorkloads, ProvisionEndpoints, ProvisionCommand, b.Type)
		}
		validateProvision(where, b, fail)

		if strings.TrimSpace(b.SSH.User) == "" {
			fail("%s (%s): ssh.user is required", where, b.Name)
		}
		if strings.TrimSpace(b.SSH.PrivateKey) == "" {
			fail("%s (%s): ssh.private_key is required", where, b.Name)
		}
		if port := b.SSH.Port.Value(); port < 1 || port > 65535 {
			fail("%s (%s): ssh.port %d out of range", where, b.Name, port)
		}
		switch b.SSH.HostKeyPolicy {
		case "strict", "accept-new", "insecure":
		default:
			fail("%s (%s): unknown ssh.host_key_policy %q (want strict|accept-new|insecure)", where, b.Name, b.SSH.HostKeyPolicy)
		}
	}

	seen := map[string]bool{}
	for i, id := range c.Identities {
		where := fmt.Sprintf("identities[%d]", i)
		fp, err := NormalizeFingerprint(id.Fingerprint)
		if err != nil {
			fail("%s: %v", where, err)
			continue
		}
		if seen[fp] {
			fail("%s: duplicate fingerprint %s", where, fp)
		}
		seen[fp] = true
		if id.Backend == "" {
			fail("%s (%s): backend is required", where, fp)
		} else if !names[id.Backend] {
			fail("%s (%s): unknown backend %q", where, fp, id.Backend)
		}
	}
	if len(c.Identities) == 0 {
		fail("at least one identity is required")
	}

	return errors.Join(errs...)
}

// Warnings lists conditions that are legal but wrong for a deployment. They are not errors because
// ADR-0006 keeps an ephemeral host key available for tests; -check-config prints them so an operator
// does not discover the choice from a client that refuses to connect after a restart.
func (c *Config) Warnings() []string {
	var out []string
	if strings.TrimSpace(c.HostKey) == "" {
		out = append(out, "host_key is empty: the gateway will generate an ephemeral host key, so its "+
			"fingerprint changes on every restart and clients cannot pin it (ADR-0006 allows this for tests only)")
	}
	return out
}

// checkHostKeyPath answers one question: can this path hold a persisted key?
//
// Three cases, mirroring what the front end does at startup — an existing file is used as is, a missing
// file is generated on first start (so the nearest existing ancestor must be writable), and anything
// else (a directory, an unreadable file) is a configuration error.
func checkHostKeyPath(path string) error {
	switch fi, err := os.Stat(path); {
	case err == nil:
		if fi.IsDir() {
			return fmt.Errorf("%s is a directory, not a key file", path)
		}
		return nil
	case !errors.Is(err, os.ErrNotExist):
		return fmt.Errorf("cannot stat %s: %w", path, err)
	}

	// The file does not exist yet: it is created on first start, so the deepest existing ancestor has
	// to be writable. Probing with a temp file is deliberate — permission bits lie under ACLs, read-only
	// mounts and root squashing, and this is the failure ADR-0006 records from a real install.
	dir := filepath.Dir(path)
	for {
		fi, err := os.Stat(dir)
		if err == nil {
			if !fi.IsDir() {
				return fmt.Errorf("%s exists and is not a directory, so %s cannot be created", dir, path)
			}
			f, err := os.CreateTemp(dir, ".byre-hostkey-check-*")
			if err != nil {
				return fmt.Errorf("%s does not exist and %s is not writable (%v); a first start could "+
					"not persist a generated key", path, dir, err)
			}
			name := f.Name()
			_ = f.Close()
			_ = os.Remove(name)
			return nil
		}
		if !errors.Is(err, os.ErrNotExist) {
			return fmt.Errorf("cannot stat %s: %w", dir, err)
		}
		parent := filepath.Dir(dir)
		if parent == dir {
			return fmt.Errorf("no existing ancestor for %s", path)
		}
		dir = parent
	}
}

func validateProvision(where string, b Backend, fail func(string, ...any)) {
	// An explicit zero is a configuration mistake, not a request for a default: both timeouts are
	// "how long to wait", and waiting for nothing fails every acquisition.
	if b.Provider.ReadyTimeout.IsSet() && b.Provider.ReadyTimeout.Std() <= 0 {
		fail("%s (%s): provider.ready_timeout must be > 0", where, b.Name)
	}
	if b.Command.Timeout.IsSet() && b.Command.Timeout.Std() <= 0 {
		fail("%s (%s): command.timeout must be > 0", where, b.Name)
	}
	switch b.Provision {
	case ProvisionClaims:
		if strings.TrimSpace(b.Namespace) == "" {
			fail("%s (%s): namespace is required for provision %s", where, b.Name, b.Provision)
		}
		// Claim APIs differ: some instantiate a named template, some only claim from a warm pool.
		if strings.TrimSpace(b.Provider.Template) == "" && strings.TrimSpace(b.Provider.WarmPool) == "" {
			fail("%s (%s): provider.template or provider.warm_pool is required for provision %s", where, b.Name, b.Provision)
		}
		if strings.TrimSpace(b.Provider.APIGroup) == "" ||
			strings.TrimSpace(b.Provider.APIVersion) == "" ||
			strings.TrimSpace(b.Provider.Resource) == "" ||
			strings.TrimSpace(b.Provider.ObjectKind) == "" {
			fail("%s (%s): provider.api_group, api_version, resource and object_kind are required for provision %s (no API is built in)",
				where, b.Name, b.Provision)
		}
	case ProvisionWorkloads:
		if strings.TrimSpace(b.Namespace) == "" {
			fail("%s (%s): namespace is required for provision %s", where, b.Name, b.Provision)
		}
		if strings.TrimSpace(b.Provider.Image) == "" {
			fail("%s (%s): provider.image is required for provision %s", where, b.Name, b.Provision)
		}
	case ProvisionEndpoints:
		if len(b.Endpoints) == 0 {
			fail("%s (%s): endpoints are required for provision %s", where, b.Name, b.Provision)
		}
		for _, ep := range b.Endpoints {
			host, port, err := net.SplitHostPort(ep)
			if err != nil || host == "" || port == "" {
				fail("%s (%s): endpoint %q must be host:port", where, b.Name, ep)
			}
		}
	case ProvisionCommand:
		if len(b.Command.Acquire) == 0 {
			fail("%s (%s): command.acquire is required for provision %s", where, b.Name, b.Provision)
		}
		// A backend without list is legal: it simply cannot be adopted after a restart.
	case "":
		fail("%s (%s): provision is required (%s|%s|%s|%s)",
			where, b.Name, ProvisionClaims, ProvisionWorkloads, ProvisionEndpoints, ProvisionCommand)
	default:
		fail("%s (%s): unknown provision %q (want %s|%s|%s|%s)", where, b.Name, b.Provision,
			ProvisionClaims, ProvisionWorkloads, ProvisionEndpoints, ProvisionCommand)
	}
}

// NormalizeFingerprint canonicalises an SSH public-key fingerprint.
//
// Accepted inputs: "SHA256:AbC...=" and the bare "AbC...=" body; the returned form is always the bare
// unpadded base64 body, so two spellings of the same key never become two identities.
//
// The decoded length is checked, not just the base64 alphabet: sha256 fingerprints are 32 bytes, and a
// truncated one decodes happily into a shorter string that can never match a real client key. That
// failure is silent — the gateway starts, accepts nobody, and -check-config said the file was fine.
func NormalizeFingerprint(s string) (string, error) {
	raw := strings.TrimSpace(s)
	if raw == "" {
		return "", errors.New("fingerprint is empty")
	}
	if strings.HasPrefix(raw, "SHA256:") {
		raw = raw[len("SHA256:"):]
	}
	raw = strings.TrimRight(raw, "=")
	if raw == "" {
		return "", fmt.Errorf("fingerprint %q has no body", s)
	}
	sum, err := base64.RawStdEncoding.DecodeString(raw)
	if err != nil {
		return "", fmt.Errorf("fingerprint %q is not base64: %w", s, err)
	}
	if len(sum) != sha256.Size {
		return "", fmt.Errorf("fingerprint %q decodes to %d bytes, want %d (a SHA256 fingerprint)",
			s, len(sum), sha256.Size)
	}
	return raw, nil
}
