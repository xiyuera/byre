package config

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

const goodFP = "SHA256:AAAABBBBCCCCDDDDEEEEFFFFGGGGHHHHIIIIJJJJ"

func baseConfig() *Config {
	return &Config{
		Listen: ":2222",
		Lease:  Lease{TTL: Duration(360 * time.Second), Grace: Duration(60 * time.Second), PoolSize: 2},
		Backends: []Backend{{
			Name:      "claims",
			Type:      TypeKubernetes,
			Namespace: "sandbox-system",
			Provider:  Provider{Kind: KindSandboxClaim, Template: "sandbox"},
			SSH:       SSH{Port: 22, User: "sandbox", PrivateKey: "/keys/pool", HostKeyPolicy: "accept-new"},
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
		Backends:   []Backend{{Name: "b", Type: TypeStatic, Endpoints: []string{"192.0.2.1:22"}, SSH: SSH{User: "u", PrivateKey: "/k"}}},
		Identities: []Identity{{Fingerprint: goodFP, Backend: "b"}},
	}
	c.ApplyDefaults()
	if c.Listen != DefaultListen {
		t.Errorf("listen = %q, want %q", c.Listen, DefaultListen)
	}
	if c.Lease.TTL.Std() != DefaultTTL {
		t.Errorf("ttl = %v, want %v", c.Lease.TTL, DefaultTTL)
	}
	if c.Backends[0].SSH.Port != DefaultSSHPort {
		t.Errorf("ssh.port = %d, want %d", c.Backends[0].SSH.Port, DefaultSSHPort)
	}
	if err := c.Validate(); err != nil {
		t.Fatalf("validate after defaults: %v", err)
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
		{"unknown type", func(c *Config) { c.Backends[0].Type = "docker" }, "unknown type"},
		{"missing namespace", func(c *Config) { c.Backends[0].Namespace = "" }, "namespace is required"},
		{"missing template", func(c *Config) { c.Backends[0].Provider.Template = "" }, "provider.template is required"},
		{"unknown kind", func(c *Config) { c.Backends[0].Provider.Kind = "vm" }, "unknown provider.kind"},
		{"missing image", func(c *Config) { c.Backends[0].Provider = Provider{Kind: KindPod} }, "provider.image is required"},
		{"missing ssh user", func(c *Config) { c.Backends[0].SSH.User = "" }, "ssh.user is required"},
		{"missing key", func(c *Config) { c.Backends[0].SSH.PrivateKey = "" }, "ssh.private_key is required"},
		{"bad policy", func(c *Config) { c.Backends[0].SSH.HostKeyPolicy = "yolo" }, "unknown ssh.host_key_policy"},
		{"unknown backend ref", func(c *Config) { c.Identities[0].Backend = "nope" }, "unknown backend"},
		{"duplicate backend", func(c *Config) { c.Backends = append(c.Backends, c.Backends[0]) }, "duplicate backend name"},
		{"duplicate identity", func(c *Config) {
			c.Identities = append(c.Identities, Identity{Fingerprint: goodFP, Backend: "claims"})
		}, "duplicate fingerprint"},
		{"static without endpoints", func(c *Config) {
			c.Backends[0] = Backend{Name: "s", Type: TypeStatic, SSH: SSH{Port: 22, User: "u", PrivateKey: "/k", HostKeyPolicy: "strict"}}
			c.Identities[0].Backend = "s"
		}, "endpoints are required"},
		{"static bad endpoint", func(c *Config) {
			c.Backends[0] = Backend{Name: "s", Type: TypeStatic, Endpoints: []string{"192.0.2.1"}, SSH: SSH{Port: 22, User: "u", PrivateKey: "/k", HostKeyPolicy: "strict"}}
			c.Identities[0].Backend = "s"
		}, "must be host:port"},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			c := baseConfig()
			c.ApplyDefaults()
			tc.mutate(c)
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
	tests := []struct {
		in, want string
		wantErr  bool
	}{
		{"SHA256:AAAABBBBCCCCDDDDEEEEFFFFGGGGHHHHIIIIJJJJ", "AAAABBBBCCCCDDDDEEEEFFFFGGGGHHHHIIIIJJJJ", false},
		{"AAAABBBBCCCCDDDDEEEEFFFFGGGGHHHHIIIIJJJJ=", "AAAABBBBCCCCDDDDEEEEFFFFGGGGHHHHIIIIJJJJ", false},
		{" not-base64!! ", "", true},
		{"", "", true},
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
lease:
  ttl: 360s
  grace: 60s
  pool_size: 2
backends:
  - name: claims
    type: kubernetes
    namespace: sandbox-system
    provider:
      kind: sandbox-claim
      template: sandbox
      warm_pool: sandbox
    ssh:
      port: 22
      user: sandbox
      private_key: /keys/pool
identities:
  - fingerprint: "SHA256:AAAABBBBCCCCDDDDEEEEFFFFGGGGHHHHIIIIJJJJ"
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
  - fingerprint: "SHA256:AAAABBBBCCCCDDDDEEEEFFFFGGGGHHHHIIIIJJJJ"
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
