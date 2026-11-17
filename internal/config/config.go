// Package config holds byre's declarative configuration and its validation rules.
//
// The gateway deliberately knows nothing about what an "instance" physically is. A backend declares
// three things and nothing else: how an instance is obtained (sandbox-claim, a bare pod, or a list of
// static endpoints), how it is reached over SSH, and how it is returned. Anything that speaks SSH can
// therefore be described in configuration instead of code.
//
// Validation is strict and fail-fast: unknown fields, unknown backend types, duplicate identities or
// missing required fields abort startup instead of silently falling back to a default.
package config

import (
	"encoding/base64"
	"errors"
	"fmt"
	"net"
	"os"
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
type Duration time.Duration

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
	*d = Duration(v)
	return nil
}

// Std returns the value as a time.Duration.
func (d Duration) Std() time.Duration { return time.Duration(d) }

// String implements fmt.Stringer.
func (d Duration) String() string { return time.Duration(d).String() }

// Config is the root document.
type Config struct {
	Listen string `yaml:"listen"`
	// HostKey is the SSH host key clients pin. Without it a restart silently changes the fingerprint and
	// every client with strict checking fails; generating one on the fly is fine only for tests.
	HostKey    string     `yaml:"host_key"`
	Lease      Lease      `yaml:"lease"`
	Backends   []Backend  `yaml:"backends"`
	Identities []Identity `yaml:"identities"`
}

// Lease holds the timing and pool parameters of a lease.
type Lease struct {
	TTL      Duration `yaml:"ttl"`       // idle before an instance is reclaimed
	Grace    Duration `yaml:"grace"`     // extra wait after a disconnect before reclaiming
	PoolSize int      `yaml:"pool_size"` // warm instances kept ready
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
// api_group / api_version / claim_resource exist so that no CRD is hardcoded: the same binary drives
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
}

// SSH is how the gateway reaches an instance.
type SSH struct {
	Port          int    `yaml:"port"`
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

// Load reads, defaults and validates a configuration file.
func Load(path string) (*Config, error) {
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
	cfg.ApplyDefaults()
	if err := cfg.Validate(); err != nil {
		return nil, fmt.Errorf("invalid config %s: %w", path, err)
	}
	return &cfg, nil
}

// ApplyDefaults fills every optional field with its documented default.
func (c *Config) ApplyDefaults() {
	if strings.TrimSpace(c.Listen) == "" {
		c.Listen = DefaultListen
	}
	if c.Lease.TTL == 0 {
		c.Lease.TTL = Duration(DefaultTTL)
	}
	if c.Lease.Grace == 0 {
		c.Lease.Grace = Duration(DefaultGrace)
	}
	if c.Lease.PoolSize == 0 {
		c.Lease.PoolSize = DefaultPoolSize
	}
	for i := range c.Backends {
		b := &c.Backends[i]
		if b.SSH.Port == 0 {
			b.SSH.Port = DefaultSSHPort
		}
		if strings.TrimSpace(b.SSH.HostKeyPolicy) == "" {
			b.SSH.HostKeyPolicy = "accept-new"
		}
		if b.Provider.ReadyTimeout == 0 {
			b.Provider.ReadyTimeout = Duration(DefaultReadyTimeout)
		}
		if b.Provision == ProvisionCommand && b.Command.Timeout == 0 {
			b.Command.Timeout = Duration(DefaultCommandTimeout)
		}
	}
}

// Validate checks every invariant and returns all violations at once.
func (c *Config) Validate() error {
	var errs []error
	fail := func(format string, args ...any) { errs = append(errs, fmt.Errorf(format, args...)) }

	if c.Lease.TTL.Std() <= 0 {
		fail("lease.ttl must be > 0")
	}
	if c.Lease.Grace.Std() < 0 {
		fail("lease.grace must be >= 0")
	}
	if c.Lease.PoolSize < 0 {
		fail("lease.pool_size must be >= 0")
	}
	if len(c.Backends) == 0 {
		fail("at least one backend is required")
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
		if b.SSH.Port < 1 || b.SSH.Port > 65535 {
			fail("%s (%s): ssh.port %d out of range", where, b.Name, b.SSH.Port)
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

func validateProvision(where string, b Backend, fail func(string, ...any)) {
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
		if len(b.Command.List) == 0 {
			// Not an error: a backend that cannot list simply cannot be adopted after a restart.
			_ = b
		}
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
	if _, err := base64.RawStdEncoding.DecodeString(raw); err != nil {
		return "", fmt.Errorf("fingerprint %q is not base64: %w", s, err)
	}
	return raw, nil
}
