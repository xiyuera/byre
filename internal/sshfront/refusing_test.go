package sshfront

import (
	"context"
	"fmt"
	"net"
	"sync"
	"testing"

	"golang.org/x/crypto/ssh"

	"byre/internal/backend"
	"byre/internal/lease"
)

// startRefusingTarget accepts authentication and refuses every session channel. That is the shape of an
// instance whose container was OOM-killed and restarted into a state where its sshd will not take new
// sessions: the handshake succeeds, so the dial succeeds and Alive() answers "it is still there", and the
// client hangs up on its first session. ADR-0013 part 5 is about exactly this.
func startRefusingTarget(t *testing.T) string {
	t.Helper()
	cfg := &ssh.ServerConfig{
		PublicKeyCallback: func(ssh.ConnMetadata, ssh.PublicKey) (*ssh.Permissions, error) { return nil, nil },
	}
	cfg.AddHostKey(newSigner(t))

	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatalf("refusing target listen: %v", err)
	}
	t.Cleanup(func() { _ = ln.Close() })

	go func() {
		for {
			c, err := ln.Accept()
			if err != nil {
				return
			}
			go func(c net.Conn) {
				defer c.Close()
				conn, chans, reqs, err := ssh.NewServerConn(c, cfg)
				if err != nil {
					return
				}
				defer conn.Close()
				go ssh.DiscardRequests(reqs)
				for newCh := range chans {
					_ = newCh.Reject(ssh.Prohibited, "no sessions")
				}
			}(c)
		}
	}()
	return ln.Addr().String()
}

// stagedDriver hands out a fixed sequence of addresses, so a test can make the first instance refuse
// sessions and the replacement work, and then assert which one the client ended up on.
type stagedDriver struct {
	mu    sync.Mutex
	key   string
	addrs []string
	n     int
	given []string
}

func (d *stagedDriver) Name() string { return "lab" }

func (d *stagedDriver) Acquire(_ context.Context, identity string) (backend.Instance, error) {
	d.mu.Lock()
	defer d.mu.Unlock()
	addr := d.addrs[len(d.addrs)-1]
	if d.n < len(d.addrs) {
		addr = d.addrs[d.n]
	}
	d.n++
	id := fmt.Sprintf("lab-%d", d.n)
	d.given = append(d.given, id+"@"+addr)
	return backend.Instance{
		ID:            id,
		Addr:          addr,
		User:          "sandbox",
		PrivateKey:    d.key,
		HostKeyPolicy: "insecure",
		Annotations:   map[string]string{backend.LabelIdentity: identity},
	}, nil
}

func (d *stagedDriver) Release(context.Context, backend.Instance) error { return nil }
func (d *stagedDriver) List(context.Context) ([]backend.Instance, error) {
	return nil, nil
}
func (d *stagedDriver) Adopt(context.Context, []backend.Instance) error { return nil }

func (d *stagedDriver) acquired() []string {
	d.mu.Lock()
	defer d.mu.Unlock()
	return append([]string(nil), d.given...)
}

// attemptSession connects to the front end and asks for a session, which is what a client actually does.
// h.dial alone only proves the handshake with the front end; a refusal by the instance surfaces here.
func (h *harness) attemptSession(t *testing.T) error {
	t.Helper()
	c, err := h.dial(t, h.signer)
	if err != nil {
		return err
	}
	defer c.Close()
	sess, serr := c.NewSession()
	if serr == nil {
		_ = sess.Close()
	}
	return serr
}

// Three refusals is the threshold: the first two attempts fail and leave the lease in place, the third
// replaces the instance, and because that happens inside the same connection attempt the client's third
// try succeeds instead of hanging up.
func TestInstanceRefusingSessionsIsReplaced(t *testing.T) {
	_, priv := newKeyPair(t)
	keyPath := writeKeyFile(t, priv)
	driver := &stagedDriver{
		key:   keyPath,
		addrs: []string{startRefusingTarget(t), startFakeTarget(t, "healthy")},
	}

	h := startGatewayDial(t, "", driver, nil)

	for attempt := 1; attempt < lease.UnusableThreshold; attempt++ {
		if err := h.attemptSession(t); err == nil {
			t.Fatalf("attempt %d opened a session against an instance that refuses them", attempt)
		}
	}

	if err := h.attemptSession(t); err != nil {
		t.Fatalf("the attempt that should have been moved to a healthy instance failed: %v", err)
	}

	acquired := driver.acquired()
	if len(acquired) < 2 {
		t.Fatalf("acquired %v, want the refusing instance and then a replacement", acquired)
	}
	if acquired[0] == acquired[len(acquired)-1] {
		t.Errorf("acquired %v: the last instance is the refusing one, so no replacement was provisioned", acquired)
	}
}

// Below the threshold the lease must be kept: the client's next attempt has to be aimed at its own
// instance, not at a fresh one, because a restarting sshd refuses sessions for a moment and replacing on
// every such moment would churn pods.
func TestRefusalsBelowTheThresholdKeepTheLease(t *testing.T) {
	_, priv := newKeyPair(t)
	driver := &stagedDriver{
		key:   writeKeyFile(t, priv),
		addrs: []string{startRefusingTarget(t), startFakeTarget(t, "healthy")},
	}
	h := startGatewayDial(t, "", driver, nil)

	for attempt := 1; attempt < lease.UnusableThreshold; attempt++ {
		if err := h.attemptSession(t); err == nil {
			t.Fatalf("attempt %d opened a session against a refusing instance", attempt)
		}
		if got := driver.acquired(); len(got) != 1 {
			t.Fatalf("acquired %v after %d refusal(s); the lease was replaced below the threshold", got, attempt)
		}
	}
}
