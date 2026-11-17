package config

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

// goodFP is base64(sha256("")), i.e. 32 bytes: a fingerprint's decoded length is now checked, so a
// made-up alphabet-only string is no longer accepted anywhere.
const goodFP = "SHA256:47DEQpj8HBSa+/TImW+5JCeuQeRkm5NMpJWZG3hSuFU"

func intPtr(n int) *int { return &n }

func baseConfig() *Config {
	return &Config{
		Listen:  ":2222",
		HostKey: "/var/lib/byre/hostkey/id_ed25519",
		Lease: Lease{
			TTL:      NewDuration(360 * time.Second),
			Grace:    NewDuration(60 * time.Second),
			PoolSize: intPtr(2),
		},
		Backends: []Backend{{
			Name:      "claims",
			Provision: ProvisionClaims,
			Namespace: "sandbox-system",
			Provider: Provider{
				Template: "sandbox",
				APIGroup: "example.io", APIVersion: "v1", Resource: "sandboxclaims", ObjectKind: "SandboxClaim",
			},
			SSH: SSH{Port: NewNumber(22), User: "sandbox", PrivateKey: "/keys/pool", HostKeyPolicy: "accept-new"},
		}},
		Identities: []Identity{{Fingerprint: goodFP, Backend: "claims"}},
	}
}

func TestValidateAcceptsMinimalConfig(t *testing.T) {
	c := baseConfig()
	c.ApplyDefaults()
	if err := c.Validate(); err != nil {
		t.Fatalf("expected valid config, got: %v", err)
	}
}

func TestValidateFillsDefaults(t *testing.T) {
	c := &Config{
		HostKey:    "/var/lib/byre/hostkey/id_ed25519",
		Backends:   []Backend{{Name: "b", Provision: ProvisionEndpoints, Endpoints: []string{"192.0.2.1:22"}, SSH: SSH{User: "u", PrivateKey: "/k"}}},
		Identities: []Identity{{Fingerprint: goodFP, Backend: "b"}},
	}
	c.ApplyDefaults()
	if c.Listen != DefaultListen {
		t.Errorf("listen = %q, want %q", c.Listen, DefaultListen)
	}
	if c.Lease.TTL.Std() != DefaultTTL {
		t.Errorf("ttl = %v, want %v", c.Lease.TTL, DefaultTTL)
	}
	if c.Backends[0].SSH.Port.Value() != DefaultSSHPort {
		t.Errorf("ssh.port = %d, want %d", c.Backends[0].SSH.Port.Value(), DefaultSSHPort)
	}
	if err := c.Validate(); err != nil {
		t.Fatalf("validate after defaults: %v", err)
	}
}

// TestApplyDefaultsKeepsExplicitZero is the F7 regression: an explicit `pool_size: 0` is the documented
// way to run one smoke test without warm pods, and it used to be replaced by the default of 2.
func TestApplyDefaultsKeepsExplicitZero(t *testing.T) {
	path := writeTemp(t, `
host_key: /var/lib/byre/hostkey/id_ed25519
lease:
  ttl: 360s
  grace: 0s
  pool_size: 0
backends:
  - name: s
    provision: endpoints
    endpoints: ["192.0.2.1:22"]
    ssh: {port: 22, user: u, private_key: /k}
identities:
  - fingerprint: "`+goodFP+`"
    backend: s
`)
	cfg, err := Load(path)
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	if got := cfg.Lease.PoolSizeValue(); got != 0 {
		t.Fatalf("pool_size = %d, want the explicit 0 kept", got)
	}
	if got := cfg.Lease.Grace.Std(); got != 0 {
		t.Fatalf("grace = %v, want the explicit 0s kept", got)
	}
	if cfg.Lease.PoolSize == nil || !cfg.Lease.Grace.IsSet() {
		t.Fatal("an explicitly written key must be recorded as present")
	}
}

// TestExplicitZeroIsNotRepaired: the fields where zero cannot mean anything are rejected instead of
// being silently replaced by a default.
func TestExplicitZeroIsNotRepaired(t *testing.T) {
	tests := []struct{ name, body, want string }{
		{"ttl", "lease: {ttl: 0s}", "lease.ttl must be > 0"},
		{"ssh port", "backends:\n  - name: s\n    provision: endpoints\n    endpoints: [\"192.0.2.1:22\"]\n    ssh: {port: 0, user: u, private_key: /k}", "ssh.port 0 out of range"},
		{"ready timeout", "backends:\n  - name: s\n    provision: endpoints\n    endpoints: [\"192.0.2.1:22\"]\n    provider: {ready_timeout: 0s}\n    ssh: {user: u, private_key: /k}", "provider.ready_timeout must be > 0"},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			body := "host_key: /k\n" + tc.body + "\nidentities:\n  - fingerprint: \"" + goodFP + "\"\n    backend: s\n"
			path := writeTemp(t, body)
			_, err := Load(path)
			if err == nil || !strings.Contains(err.Error(), tc.want) {
				t.Fatalf("Load error = %v, want it to contain %q", err, tc.want)
			}
		})
	}
}

// TestHostKeyIsRequired is the F8 regression: an empty host_key changes the fingerprint on every
// restart, and one deployment already lost every strict client that way. Only the command-line option
// may relax it.
func TestHostKeyIsRequired(t *testing.T) {
	body := `
lease: {ttl: 360s}
backends:
  - name: s
    provision: endpoints
    endpoints: ["192.0.2.1:22"]
    ssh: {user: u, private_key: /k}
identities:
  - fingerprint: "` + goodFP + `"
    backend: s
`
	path := writeTemp(t, body)
	if _, err := Load(path); err == nil || !strings.Contains(err.Error(), "host_key is required") {
		t.Fatalf("Load error = %v, want a host_key rejection", err)
	}
	if _, err := Load(path, AllowEphemeralHostKey(true)); err != nil {
		t.Fatalf("Load with -allow-ephemeral-hostkey: %v", err)
	}
}

func TestValidateRejects(t *testing.T) {
	tests := []struct {
		name    string
		mutate  func(*Config)
		wantSub string
	}{
		{"no backends", func(c *Config) { c.Backends = nil }, "at least one backend"},
		{"no identities", func(c *Config) { c.Identities = nil }, "at least one identity"},
		{"no host key", func(c *Config) { c.HostKey = "" }, "host_key is required"},
		{"legacy type field", func(c *Config) { c.Backends[0].Type = "static" }, "was replaced by backend.provision"},
		{"missing namespace", func(c *Config) { c.Backends[0].Namespace = "" }, "namespace is required"},
		{"no selector", func(c *Config) {
			c.Backends[0].Provider.Template = ""
			c.Backends[0].Provider.WarmPool = ""
		}, "provider.template or provider.warm_pool is required"},
		{"missing gvk", func(c *Config) { c.Backends[0].Provider.APIGroup = "" }, "api_group, api_version, resource and object_kind"},
		{"missing endpoint field is fine", func(c *Config) { c.Backends[0].Provider.EndpointField = nil }, ""},
		{"missing image", func(c *Config) {
			c.Backends[0] = Backend{Name: "w", Provision: ProvisionWorkloads, Namespace: "ns",
				SSH: SSH{Port: NewNumber(22), User: "u", PrivateKey: "/k", HostKeyPolicy: "strict"}}
			c.Identities[0].Backend = "w"
		}, "provider.image is required"},
		{"missing ssh user", func(c *Config) { c.Backends[0].SSH.User = "" }, "ssh.user is required"},
		{"missing key", func(c *Config) { c.Backends[0].SSH.PrivateKey = "" }, "ssh.private_key is required"},
		{"bad policy", func(c *Config) { c.Backends[0].SSH.HostKeyPolicy = "yolo" }, "unknown ssh.host_key_policy"},
		{"unknown backend ref", func(c *Config) { c.Identities[0].Backend = "nope" }, "unknown backend"},
		{"duplicate backend", func(c *Config) { c.Backends = append(c.Backends, c.Backends[0]) }, "duplicate backend name"},
		{"duplicate identity", func(c *Config) {
			c.Identities = append(c.Identities, Identity{Fingerprint: goodFP, Backend: "claims"})
		}, "duplicate fingerprint"},
		{"static without endpoints", func(c *Config) {
			c.Backends[0] = Backend{Name: "s", Provision: ProvisionEndpoints, SSH: SSH{Port: NewNumber(22), User: "u", PrivateKey: "/k", HostKeyPolicy: "strict"}}
			c.Identities[0].Backend = "s"
		}, "endpoints are required"},
		{"static bad endpoint", func(c *Config) {
			c.Backends[0] = Backend{Name: "s", Provision: ProvisionEndpoints, Endpoints: []string{"192.0.2.1"}, SSH: SSH{Port: NewNumber(22), User: "u", PrivateKey: "/k", HostKeyPolicy: "strict"}}
			c.Identities[0].Backend = "s"
		}, "must be host:port"},
		{"truncated fingerprint", func(c *Config) {
			c.Identities[0].Fingerprint = "SHA256:AAAABBBBCCCCDDDDEEEEFFFFGGGGHHHHIIIIJJJJ"
		}, "decodes to 30 bytes"},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			c := baseConfig()
			c.ApplyDefaults()
			tc.mutate(c)
			if tc.wantSub == "" {
				if err := c.Validate(); err != nil {
					t.Fatalf("expected valid config, got %v", err)
				}
				return
			}
			err := c.Validate()
			if err == nil {
				t.Fatalf("expected error containing %q, got nil", tc.wantSub)
			}
			if !strings.Contains(err.Error(), tc.wantSub) {
				t.Fatalf("error = %v, want it to contain %q", err, tc.wantSub)
			}
		})
	}
}

func TestNormalizeFingerprint(t *testing.T) {
	body := strings.TrimPrefix(goodFP, "SHA256:")
	tests := []struct {
		in, want string
		wantErr  bool
	}{
		{goodFP, body, false},
		{body + "=", body, false},
		{" not-base64!! ", "", true},
		{"", "", true},
		// 30 bytes: valid base64, impossible fingerprint. Accepting it means a gateway that starts
		// cleanly and matches no client.
		{"AAAABBBBCCCCDDDDEEEEFFFFGGGGHHHHIIIIJJJJ", "", true},
	}
	for _, tc := range tests {
		got, err := NormalizeFingerprint(tc.in)
		if tc.wantErr {
			if err == nil {
				t.Errorf("NormalizeFingerprint(%q) = %q, want error", tc.in, got)
			}
			continue
		}
		if err != nil {
			t.Errorf("NormalizeFingerprint(%q) unexpected error: %v", tc.in, err)
			continue
		}
		if got != tc.want {
			t.Errorf("NormalizeFingerprint(%q) = %q, want %q", tc.in, got, tc.want)
		}
	}
}

func writeTemp(t *testing.T, body string) string {
	t.Helper()
	dir := t.TempDir()
	path := filepath.Join(dir, "config.yaml")
	if err := os.WriteFile(path, []byte(body), 0o600); err != nil {
		t.Fatalf("write temp config: %v", err)
	}
	return path
}

func TestLoadValidFile(t *testing.T) {
	path := writeTemp(t, `
listen: ":2222"
host_key: /var/lib/byre/hostkey/id_ed25519
lease:
  ttl: 360s
  grace: 60s
  pool_size: 2
backends:
  - name: claims
    provision: claims
    namespace: sandbox-system
    provider:
      template: sandbox
      warm_pool: sandbox
      api_group: example.io
      api_version: v1
      resource: sandboxclaims
      object_kind: SandboxClaim
    ssh:
      port: 22
      user: sandbox
      private_key: /keys/pool
identities:
  - fingerprint: "`+goodFP+`"
    backend: claims
`)
	cfg, err := Load(path)
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	if cfg.Lease.TTL.Std() != 360*time.Second {
		t.Errorf("ttl = %v", cfg.Lease.TTL)
	}
	if cfg.Backends[0].SSH.HostKeyPolicy != "accept-new" {
		t.Errorf("host_key_policy default not applied: %q", cfg.Backends[0].SSH.HostKeyPolicy)
	}
	if got := cfg.Backends[0].Provider.ReadyTimeout.Std(); got != DefaultReadyTimeout {
		t.Errorf("ready_timeout = %v, want the default %v", got, DefaultReadyTimeout)
	}
}

func TestLoadRejectsUnknownField(t *testing.T) {
	path := writeTemp(t, `
listen: ":2222"
nonsense: true
backends:
  - name: b
    type: static
    endpoints: ["192.0.2.1:22"]
    ssh: {user: u, private_key: /k}
identities:
  - fingerprint: "`+goodFP+`"
    backend: b
`)
	if _, err := Load(path); err == nil || !strings.Contains(err.Error(), "nonsense") {
		t.Fatalf("expected unknown-field error, got %v", err)
	}
}

func TestLoadRejectsBadDuration(t *testing.T) {
	path := writeTemp(t, `
lease:
  ttl: 6 minutes
backends: []
identities: []
`)
	if _, err := Load(path); err == nil || !strings.Contains(err.Error(), "invalid duration") {
		t.Fatalf("expected duration error, got %v", err)
	}
}

// TestExplicitZeroPoolSizeSurvivesDefaults is the regression for a documented procedure that did not
// exist: "scale the pool to 0, rotate the key, scale it back" was impossible while 0 was rewritten into
// the default of 2 before validation saw it.
func TestExplicitZeroPoolSizeSurvivesDefaults(t *testing.T) {
	c := baseConfig()
	c.Lease.PoolSize = intPtr(0)
	c.ApplyDefaults()
	if got := c.Lease.PoolSizeValue(); got != 0 {
		t.Fatalf("explicit pool_size: 0 became %d", got)
	}
	if err := c.Validate(); err != nil {
		t.Fatalf("validate: %v", err)
	}

	// Absent means the default, and the default is materialised so logs and Validate agree.
	c2 := baseConfig()
	c2.Lease.PoolSize = nil
	c2.ApplyDefaults()
	if got := c2.Lease.PoolSizeValue(); got != DefaultPoolSize {
		t.Fatalf("absent pool_size became %d, want %d", got, DefaultPoolSize)
	}
	if c2.Lease.PoolSize == nil {
		t.Fatal("ApplyDefaults left pool_size unset")
	}
}

func TestLoadKeepsExplicitZeroPoolSize(t *testing.T) {
	path := writeTemp(t, `
listen: ":2222"
host_key: /var/lib/byre/hostkey/id_ed25519
lease:
  pool_size: 0
backends:
  - name: b
    provision: endpoints
    endpoints: ["192.0.2.1:22"]
    ssh: {user: u, private_key: /k}
identities:
  - fingerprint: "SHA256:47DEQpj8HBSa+/TImW+5JCeuQeRkm5NMpJWZG3hSuFU"
    backend: b
`)
	cfg, err := Load(path)
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	if got := cfg.Lease.PoolSizeValue(); got != 0 {
		t.Fatalf("pool_size from file = %d, want 0", got)
	}
}

// TestHostKeyPathIsChecked: -check-config has to catch the first-install failure ADR-0006 records — a
// configured host_key whose directory cannot hold the generated key.
func TestHostKeyPathIsChecked(t *testing.T) {
	ok := baseConfig()
	ok.HostKey = filepath.Join(t.TempDir(), "id_ed25519")
	ok.ApplyDefaults()
	if err := ok.Validate(); err != nil {
		t.Fatalf("a writable host_key path must validate: %v", err)
	}

	bad := baseConfig()
	bad.HostKey = "/proc/byre/hostkey/id_ed25519"
	bad.ApplyDefaults()
	err := bad.Validate()
	if err == nil || !strings.Contains(err.Error(), "host_key") {
		t.Fatalf("err = %v, want a host_key violation", err)
	}

	asDir := baseConfig()
	asDir.HostKey = t.TempDir()
	asDir.ApplyDefaults()
	if err := asDir.Validate(); err == nil || !strings.Contains(err.Error(), "directory") {
		t.Fatalf("err = %v, want 'is a directory'", err)
	}
}

// TestEmptyHostKeyNeedsTheEphemeralOptIn: ADR-0006 keeps the ephemeral key available for tests, but an
// empty host_key is no longer something a config file drifts into: it fails validation unless the binary
// was started with -allow-ephemeral-hostkey, and even then the omission is reported.
func TestEmptyHostKeyNeedsTheEphemeralOptIn(t *testing.T) {
	c := baseConfig()
	c.HostKey = ""
	c.ApplyDefaults()
	if err := c.Validate(); err == nil || !strings.Contains(err.Error(), "host_key is required") {
		t.Fatalf("err = %v, want host_key is required", err)
	}

	c.AllowEphemeralHostKey = true
	if err := c.Validate(); err != nil {
		t.Fatalf("validate with the explicit opt-in: %v", err)
	}
	if w := c.Warnings(); len(w) != 1 || !strings.Contains(w[0], "host_key") {
		t.Fatalf("warnings = %v, want one about host_key", w)
	}

	c.HostKey = filepath.Join(t.TempDir(), "id_ed25519")
	if got := c.Warnings(); len(got) != 0 {
		t.Fatalf("warnings = %v, want none when host_key is set", got)
	}
}
