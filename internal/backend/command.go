package backend

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"log/slog"
	"os/exec"
	"strings"
	"time"
)

// Command provisions instances by running a program, which is how the gateway reaches infrastructure that
// no Go driver covers: cloud instances, Incus/LXD, a CMDB, a queue of pre-baked VMs.
//
// The contract is one JSON object on stdout, nothing on stdout but that:
//
//	{"id":"vm-17","addr":"192.0.2.17:22","user":"ci","private_key":"/etc/keys/ci"}
//
// `user` and `private_key` may be omitted and fall back to the backend's ssh block, which is the common
// case when every instance of a backend shares one credential.
type CommandConfig struct {
	Name    string
	Acquire []string
	Release []string
	// List is optional. Without it the gateway cannot adopt this backend's instances after a restart,
	// so anything left behind stays behind — a limitation worth knowing at configuration time, not at
	// the first restart.
	List    []string
	Env     []string
	Timeout time.Duration

	User          string
	PrivateKey    string
	HostKeyPolicy string
	Logger        *slog.Logger
}

// Command is the driver. The zero value is not usable; use NewCommand.
type Command struct {
	cfg CommandConfig
	log *slog.Logger
}

// NewCommand validates the configuration instead of failing on the first acquire.
func NewCommand(cfg CommandConfig) (*Command, error) {
	if strings.TrimSpace(cfg.Name) == "" {
		return nil, fmt.Errorf("command backend: name is required")
	}
	if len(cfg.Acquire) == 0 || strings.TrimSpace(cfg.Acquire[0]) == "" {
		return nil, fmt.Errorf("command backend %q: command.acquire is required", cfg.Name)
	}
	if cfg.Timeout <= 0 {
		cfg.Timeout = 120 * time.Second
	}
	if strings.TrimSpace(cfg.HostKeyPolicy) == "" {
		cfg.HostKeyPolicy = "accept-new"
	}
	switch cfg.HostKeyPolicy {
	case "strict", "accept-new", "insecure":
	default:
		return nil, fmt.Errorf("command backend %q: unknown ssh.host_key_policy %q", cfg.Name, cfg.HostKeyPolicy)
	}
	if cfg.User == "" || cfg.PrivateKey == "" {
		return nil, fmt.Errorf("command backend %q: ssh.user and ssh.private_key are required (they are the fallback for instances that do not name their own)", cfg.Name)
	}
	log := cfg.Logger
	if log == nil {
		log = slog.Default()
	}
	return &Command{cfg: cfg, log: log.With("backend", cfg.Name)}, nil
}

func (c *Command) Name() string { return c.cfg.Name }

// run executes one configured program with a deadline and returns its stdout.
func (c *Command) run(ctx context.Context, argv []string, extraEnv []string) ([]byte, error) {
	ctx, cancel := context.WithTimeout(ctx, c.cfg.Timeout)
	defer cancel()
	cmd := exec.CommandContext(ctx, argv[0], argv[1:]...)
	cmd.Env = append(cmd.Environ(), c.cfg.Env...)
	cmd.Env = append(cmd.Env, extraEnv...)
	var stderr bytes.Buffer
	cmd.Stderr = &stderr
	out, err := cmd.Output()
	if err != nil {
		return nil, fmt.Errorf("run %q: %w (stderr: %s)", argv[0], err, strings.TrimSpace(stderr.String()))
	}
	return out, nil
}

type commandInstance struct {
	ID         string `json:"id"`
	Addr       string `json:"addr"`
	User       string `json:"user"`
	PrivateKey string `json:"private_key"`
}

func (c *Command) toInstance(ci commandInstance) (Instance, error) {
	in := Instance{
		ID:            ci.ID,
		Addr:          ci.Addr,
		User:          firstNonEmpty(ci.User, c.cfg.User),
		PrivateKey:    firstNonEmpty(ci.PrivateKey, c.cfg.PrivateKey),
		HostKeyPolicy: c.cfg.HostKeyPolicy,
	}
	if err := in.Validate(); err != nil {
		return Instance{}, fmt.Errorf("command backend %q returned an unusable instance: %w", c.cfg.Name, err)
	}
	return in, nil
}

// Acquire runs the provisioner and waits for it to describe the instance it made.
func (c *Command) Acquire(ctx context.Context, identity string) (Instance, error) {
	out, err := c.run(ctx, c.cfg.Acquire, []string{"BYRE_IDENTITY=" + identity})
	if err != nil {
		return Instance{}, err
	}
	var ci commandInstance
	if err := json.Unmarshal(out, &ci); err != nil {
		return Instance{}, fmt.Errorf("command backend %q: acquire printed %d bytes that are not one JSON object: %w",
			c.cfg.Name, len(out), err)
	}
	return c.toInstance(ci)
}

// Release runs the deprovisioner if there is one. A backend that provisions nothing to destroy simply
// omits it, and this stays a no-op rather than an error the reaper would retry forever.
func (c *Command) Release(ctx context.Context, in Instance) error {
	if len(c.cfg.Release) == 0 {
		c.log.Warn("release is a no-op: no command.release configured", "instance", in.ID)
		return nil
	}
	_, err := c.run(ctx, c.cfg.Release, []string{"BYRE_INSTANCE_ID=" + in.ID, "BYRE_INSTANCE_ADDR=" + in.Addr})
	return err
}

// List runs the configured lister, so a restarted gateway can adopt what it left running.
func (c *Command) List(ctx context.Context) ([]Instance, error) {
	if len(c.cfg.List) == 0 {
		return nil, nil
	}
	out, err := c.run(ctx, c.cfg.List, nil)
	if err != nil {
		return nil, err
	}
	var raw []commandInstance
	if err := json.Unmarshal(out, &raw); err != nil {
		return nil, fmt.Errorf("command backend %q: list printed something that is not a JSON array: %w", c.cfg.Name, err)
	}
	out2 := make([]Instance, 0, len(raw))
	for _, ci := range raw {
		in, err := c.toInstance(ci)
		if err != nil {
			c.log.Warn("skipping instance from list", "error", err)
			continue
		}
		out2 = append(out2, in)
	}
	return out2, nil
}

// Adopt is a no-op: the driver keeps no state beyond what the list command reports.
func (c *Command) Adopt(context.Context, []Instance) error { return nil }

func firstNonEmpty(vals ...string) string {
	for _, v := range vals {
		if strings.TrimSpace(v) != "" {
			return v
		}
	}
	return ""
}
