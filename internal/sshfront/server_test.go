package sshfront

import (
	"context"
	"crypto/ed25519"
	"crypto/rand"
	"encoding/pem"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"

	"golang.org/x/crypto/ssh"

	"byre/internal/backend"
	"byre/internal/lease"
)

// ---------------------------------------------------------------- fake target instance

// startFakeTarget runs a minimal SSH server that behaves like an instance: exec prints its name,
// direct-tcpip reports the destination it was asked to dial.
func startFakeTarget(t *testing.T, name string) string {
	t.Helper()
	cfg := &ssh.ServerConfig{
		PublicKeyCallback: func(ssh.ConnMetadata, ssh.PublicKey) (*ssh.Permissions, error) { return nil, nil },
	}
	cfg.AddHostKey(newSigner(t))

	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatalf("fake target listen: %v", err)
	}
	t.Cleanup(func() { _ = ln.Close() })

	go func() {
		for {
			c, err := ln.Accept()
			if err != nil {
				return
			}
			go serveFakeTarget(c, cfg, name)
		}
	}()
	return ln.Addr().String()
}

func serveFakeTarget(c net.Conn, cfg *ssh.ServerConfig, name string) {
	defer c.Close()
	conn, chans, reqs, err := ssh.NewServerConn(c, cfg)
	if err != nil {
		return
	}
	defer conn.Close()

	// Remote forwards (`ssh -R`): the target is the side that listens. Answering them is what lets the
	// gateway's port-0 handling be tested end to end.
	listeners := map[string]net.Listener{}
	var listenersMu sync.Mutex
	go func() {
		for req := range reqs {
			switch req.Type {
			case "tcpip-forward":
				var p struct {
					BindAddr string
					BindPort uint32
				}
				if err := ssh.Unmarshal(req.Payload, &p); err != nil {
					_ = req.Reply(false, nil)
					continue
				}
				ln, err := net.Listen("tcp", net.JoinHostPort(p.BindAddr, strconv.Itoa(int(p.BindPort))))
				if err != nil {
					_ = req.Reply(false, nil)
					continue
				}
				bound := ln.Addr().(*net.TCPAddr)
				listenersMu.Lock()
				listeners[net.JoinHostPort(p.BindAddr, strconv.Itoa(int(bound.Port)))] = ln
				listenersMu.Unlock()
				go func() {
					for {
						conn, err := ln.Accept()
						if err != nil {
							return
						}
						_ = conn.Close()
					}
				}()
				_ = req.Reply(true, ssh.Marshal(struct{ Port uint32 }{uint32(bound.Port)}))
			case "cancel-tcpip-forward":
				var p struct {
					BindAddr string
					BindPort uint32
				}
				_ = ssh.Unmarshal(req.Payload, &p)
				key := net.JoinHostPort(p.BindAddr, strconv.Itoa(int(p.BindPort)))
				listenersMu.Lock()
				ln, ok := listeners[key]
				delete(listeners, key)
				listenersMu.Unlock()
				if ok {
					_ = ln.Close()
				}
				_ = req.Reply(ok, nil)
			default:
				if req.WantReply {
					_ = req.Reply(false, nil)
				}
			}
		}
	}()

	for newCh := range chans {
		switch newCh.ChannelType() {
		case "session":
			ch, chReqs, err := newCh.Accept()
			if err != nil {
				continue
			}
			go func() {
				for req := range chReqs {
					if req.Type == "subsystem" {
						var kind struct{ Name string }
						_ = ssh.Unmarshal(req.Payload, &kind)
						if req.WantReply {
							_ = req.Reply(true, nil)
						}
						_, _ = fmt.Fprintf(ch, "subsystem:%s\n", kind.Name)
						_, _ = ch.SendRequest("exit-status", false, ssh.Marshal(struct{ Status uint32 }{0}))
						_ = ch.Close()
						continue
					}
					if req.Type != "exec" && req.Type != "shell" {
						if req.WantReply {
							_ = req.Reply(false, nil)
						}
						continue
					}
					if req.WantReply {
						_ = req.Reply(true, nil)
					}
					_, _ = fmt.Fprintf(ch, "%s\n", name)
					_, _ = ch.SendRequest("exit-status", false, ssh.Marshal(struct{ Status uint32 }{0}))
					_ = ch.Close()
				}
			}()
		case "direct-tcpip":
			var d struct {
				Host string
				Port uint32
			}
			_ = ssh.Unmarshal(newCh.ExtraData(), &d)
			ch, chReqs, err := newCh.Accept()
			if err != nil {
				continue
			}
			go ssh.DiscardRequests(chReqs)
			_, _ = fmt.Fprintf(ch, "dialed:%s:%d\n", d.Host, d.Port)
			_ = ch.Close()
		default:
			_ = newCh.Reject(ssh.UnknownChannelType, "unsupported")
		}
	}
}

// ---------------------------------------------------------------- helpers

func newSigner(t *testing.T) ssh.Signer {
	t.Helper()
	s, _ := newKeyPair(t)
	return s
}

func newKeyPair(t *testing.T) (ssh.Signer, ed25519.PrivateKey) {
	t.Helper()
	_, priv, err := ed25519.GenerateKey(rand.Reader)
	if err != nil {
		t.Fatalf("generate key: %v", err)
	}
	s, err := ssh.NewSignerFromKey(priv)
	if err != nil {
		t.Fatalf("signer: %v", err)
	}
	return s, priv
}

func writeKeyFile(t *testing.T, priv ed25519.PrivateKey) string {
	t.Helper()
	block, err := ssh.MarshalPrivateKey(priv, "")
	if err != nil {
		t.Fatalf("marshal key: %v", err)
	}
	// Block.Bytes is DER; the file must hold the PEM armour that ssh.ParsePrivateKey expects.
	pemBytes := pem.EncodeToMemory(block)
	path := filepath.Join(t.TempDir(), "id_key")
	if err := os.WriteFile(path, pemBytes, 0o600); err != nil {
		t.Fatalf("write key: %v", err)
	}
	return path
}

type harness struct {
	addr     string
	endpoint string // the instance the static backend leases (its ID is the address)
	manager  *lease.Manager
	srv      *Server
	identity string // fingerprint of the allowed client key
	signer   ssh.Signer
}

// startGateway wires a real front end against one fake target through the real dialler.
func startGateway(t *testing.T) *harness {
	t.Helper()
	return startGatewayWith(t, "", nil)
}

// startGatewayWith builds the same harness over a custom endpoint or a custom driver.
// driver == nil means "static backend over endpoint"; endpoint == "" means "a fresh fake target".
func startGatewayWith(t *testing.T, endpoint string, driver backend.Backend) *harness {
	t.Helper()
	return startGatewayDial(t, endpoint, driver, nil)
}

// startGatewayDial is startGatewayWith with the dialler under the test's control.
//
// The dial path is where a disappeared instance is detected, so a test about it cannot rely on "a port
// nothing listens on": it has to decide, per instance, whether the dial succeeds and what the failure
// looks like.
func startGatewayDial(t *testing.T, endpoint string, driver backend.Backend, dial DialFunc) *harness {
	t.Helper()

	clientSigner, _ := newKeyPair(t)
	_, poolPriv := newKeyPair(t)
	poolKeyPath := writeKeyFile(t, poolPriv)

	var drv backend.Backend
	if driver != nil {
		drv = driver
	} else {
		if endpoint == "" {
			endpoint = startFakeTarget(t, "fake-1")
		}
		static, err := backend.NewStatic(backend.StaticConfig{
			Name:          "lab",
			Endpoints:     []string{endpoint},
			User:          "sandbox",
			PrivateKey:    poolKeyPath,
			HostKeyPolicy: "insecure", // the fake target's host key is generated per test
		})
		if err != nil {
			t.Fatalf("static backend: %v", err)
		}
		drv = static
	}

	mgr, err := lease.New(360*time.Second, 60*time.Second, map[string]backend.Backend{"lab": drv})
	if err != nil {
		t.Fatalf("lease manager: %v", err)
	}

	fp := Fingerprint(clientSigner.PublicKey())
	logger := slog.New(slog.NewTextHandler(io.Discard, nil))
	if os.Getenv("BYRE_TEST_LOG") != "" {
		logger = slog.New(slog.NewTextHandler(os.Stderr, &slog.HandlerOptions{Level: slog.LevelDebug}))
	}
	srv, err := New(Config{
		Listen: "127.0.0.1:0",
		Identities: map[string]Identity{
			fp: {Fingerprint: fp, Backend: "lab", Aliases: []string{"test"}},
		},
		Leases:     mgr,
		Logger:     logger,
		DialTarget: dial,
	})
	if err != nil {
		t.Fatalf("front end: %v", err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	t.Cleanup(func() {
		cancel()
		_ = srv.Close()
	})
	go func() { _ = srv.Serve(ctx) }()

	deadline := time.Now().Add(3 * time.Second)
	for srv.Addr() == "" && time.Now().Before(deadline) {
		time.Sleep(10 * time.Millisecond)
	}
	if srv.Addr() == "" {
		t.Fatal("front end did not bind")
	}
	return &harness{addr: srv.Addr(), endpoint: endpoint, manager: mgr, srv: srv, identity: fp, signer: clientSigner}
}

func (h *harness) dial(t *testing.T, signer ssh.Signer) (*ssh.Client, error) {
	t.Helper()
	return ssh.Dial("tcp", h.addr, &ssh.ClientConfig{
		User:            "client",
		Auth:            []ssh.AuthMethod{ssh.PublicKeys(signer)},
		HostKeyCallback: ssh.InsecureIgnoreHostKey(),
		Timeout:         5 * time.Second,
	})
}

// ---------------------------------------------------------------- tests

func TestExecRunsOnLeasedInstance(t *testing.T) {
	h := startGateway(t)
	c, err := h.dial(t, h.signer)
	if err != nil {
		t.Fatalf("dial gateway: %v", err)
	}
	defer c.Close()

	out := runCommand(t, c, "hostname")
	if strings.TrimSpace(out) != "fake-1" {
		t.Fatalf("exec output = %q, want fake-1", out)
	}
}

func TestSecondConnectionReusesInstance(t *testing.T) {
	h := startGateway(t)

	for i := 0; i < 3; i++ {
		c, err := h.dial(t, h.signer)
		if err != nil {
			t.Fatalf("dial #%d: %v", i, err)
		}
		out := runCommand(t, c, "hostname")
		if strings.TrimSpace(out) != "fake-1" {
			t.Fatalf("connection #%d reached %q, want fake-1", i, strings.TrimSpace(out))
		}
		_ = c.Close()
	}

	// Same identity must still map to the same instance, and the backend must not have been asked twice.
	var e *lease.Entry
	deadline := time.Now().Add(2 * time.Second)
	for time.Now().Before(deadline) {
		if e = h.manager.Lookup(h.identity); e != nil {
			break
		}
		time.Sleep(5 * time.Millisecond)
	}
	if e == nil {
		t.Fatal("lease disappeared between connections")
	}
	// The static driver names an instance after its endpoint, so this is an exact assertion rather than
	// the tautology it used to be ("either fake-1 or an address is fine" accepted anything).
	if e.Instance.ID != h.endpoint {
		t.Fatalf("instance id = %q, want %q", e.Instance.ID, h.endpoint)
	}
}

func TestLocalForwardReachesTarget(t *testing.T) {
	h := startGateway(t)
	c, err := h.dial(t, h.signer)
	if err != nil {
		t.Fatalf("dial gateway: %v", err)
	}
	defer c.Close()

	conn, err := c.Dial("tcp", "127.0.0.1:9999")
	if err != nil {
		t.Fatalf("direct-tcpip: %v", err)
	}
	defer conn.Close()
	buf, err := io.ReadAll(conn)
	if err != nil {
		t.Fatalf("read from forwarded conn: %v", err)
	}
	if got := strings.TrimSpace(string(buf)); got != "dialed:127.0.0.1:9999" {
		t.Fatalf("forwarded output = %q", got)
	}
}

func TestUnknownKeyIsRejected(t *testing.T) {
	h := startGateway(t)
	other := newSigner(t)
	if c, err := h.dial(t, other); err == nil {
		_ = c.Close()
		t.Fatal("unknown key was accepted")
	}
}

// TestLeaseRecordedForConfiguredBackend: after a connection is authenticated, the lease records the
// backend the identity is configured on. (The refusal of a *different* backend is tested in the lease
// package; the old name of this test claimed a refusal the front end never performs.)
func TestLeaseRecordedForConfiguredBackend(t *testing.T) {
	h := startGateway(t)
	if e := h.manager.Lookup(h.identity); e != nil {
		t.Fatalf("unexpected pre-existing lease: %+v", e)
	}
	c, err := h.dial(t, h.signer)
	if err != nil {
		t.Fatalf("dial: %v", err)
	}
	defer c.Close()
	if e := waitForLease(t, h, 2*time.Second); e.Backend != "lab" {
		t.Fatalf("lease recorded against backend %q, want lab", e.Backend)
	}
}

// waitForLease polls until the front end has recorded a lease for the allowed identity.
// The lease is written by the server goroutine after it finishes the handshake, so a client's
// Dial returning first is expected: reading the map straight after Dial is a race, not a defect.
func waitForLease(t *testing.T, h *harness, timeout time.Duration) *lease.Entry {
	t.Helper()
	deadline := time.Now().Add(timeout)
	for {
		if e := h.manager.Lookup(h.identity); e != nil {
			return e
		}
		if time.Now().After(deadline) {
			t.Fatalf("no lease recorded for the allowed key within %s", timeout)
		}
		time.Sleep(5 * time.Millisecond)
	}
}

func runCommand(t *testing.T, c *ssh.Client, cmd string) string {
	t.Helper()
	sess, err := c.NewSession()
	if err != nil {
		t.Fatalf("new session: %v", err)
	}
	defer sess.Close()
	out, err := sess.Output(cmd)
	if err != nil {
		t.Fatalf("run %q: %v", cmd, err)
	}
	return string(out)
}

// TestSubsystemPassThrough covers must-have #5's "scp-sftp pass-through": sftp is a session subsystem,
// so the gateway must forward the request, not just exec.
func TestSubsystemPassThrough(t *testing.T) {
	h := startGateway(t)
	c, err := h.dial(t, h.signer)
	if err != nil {
		t.Fatalf("dial gateway: %v", err)
	}
	defer c.Close()

	sess, err := c.NewSession()
	if err != nil {
		t.Fatalf("new session: %v", err)
	}
	defer sess.Close()

	out, err := sess.StdoutPipe()
	if err != nil {
		t.Fatalf("stdout pipe: %v", err)
	}
	if err := sess.RequestSubsystem("sftp"); err != nil {
		t.Fatalf("request subsystem: %v", err)
	}
	buf, err := io.ReadAll(out)
	if err != nil {
		t.Fatalf("read subsystem output: %v", err)
	}
	if got := strings.TrimSpace(string(buf)); got != "subsystem:sftp" {
		t.Fatalf("subsystem output = %q, want subsystem:sftp", got)
	}
}

// failingBackend simulates a driver that cannot provide an instance.
type failingBackend struct{ err error }

func (f *failingBackend) Name() string { return "lab" }
func (f *failingBackend) Acquire(context.Context, string) (backend.Instance, error) {
	return backend.Instance{}, f.err
}
func (f *failingBackend) Release(context.Context, backend.Instance) error  { return nil }
func (f *failingBackend) List(context.Context) ([]backend.Instance, error) { return nil, nil }
func (f *failingBackend) Adopt(context.Context, []backend.Instance) error  { return nil }

// TestBackendFailureRefusesSession: authentication succeeds (the key is known), but no instance can be
// obtained, so the client must be disconnected and no lease may be left behind.
func TestBackendFailureRefusesSession(t *testing.T) {
	h := startGatewayWith(t, "", &failingBackend{err: errors.New("no capacity")})

	c, err := h.dial(t, h.signer)
	if err != nil {
		t.Fatalf("authentication should succeed: %v", err)
	}
	defer c.Close()

	sess, err := c.NewSession()
	if err == nil {
		_ = sess.Close()
		t.Fatal("a session was opened although the backend could not provide an instance")
	}
	if e := h.manager.Lookup(h.identity); e != nil {
		t.Fatalf("lease left behind after a failed acquire: %+v", e)
	}
}

// TestDialFailureInvalidatesLease: the lease is obtained but the instance refuses connections (a dead
// pod, a bounced listener). The client must be disconnected **and the lease dropped** — the old
// behaviour kept it, which handed the same unreachable instance to every reconnect for ttl+grace while
// healthy capacity sat idle (F5, probe E4).
func TestDialFailureInvalidatesLease(t *testing.T) {
	h := startGatewayWith(t, "127.0.0.1:1", nil) // port 1: connection refused on any sane host

	c, err := h.dial(t, h.signer)
	if err != nil {
		t.Fatalf("authentication should succeed: %v", err)
	}
	defer c.Close()

	if _, err := c.NewSession(); err == nil {
		t.Fatal("session opened against an unreachable instance")
	}

	deadline := time.Now().Add(3 * time.Second)
	for time.Now().Before(deadline) {
		if h.manager.Lookup(h.identity) == nil {
			return
		}
		time.Sleep(10 * time.Millisecond)
	}
	t.Fatalf("the lease survived a failed dial: %+v", h.manager.Lookup(h.identity))
}

// TestDeadInstanceIsReplacedOnReconnect is the end-to-end half of F5: the first connection lands on a
// dead endpoint, and the second one must get the healthy instance instead of the same address.
func TestDeadInstanceIsReplacedOnReconnect(t *testing.T) {
	alive := startFakeTarget(t, "fake-1")
	_, poolPriv := newKeyPair(t)
	keyPath := writeKeyFile(t, poolPriv)
	drv, err := backend.NewStatic(backend.StaticConfig{
		Name:          "lab",
		Endpoints:     []string{"127.0.0.1:1", alive},
		User:          "sandbox",
		PrivateKey:    keyPath,
		HostKeyPolicy: "insecure",
	})
	if err != nil {
		t.Fatalf("static backend: %v", err)
	}
	h := startGatewayWith(t, "", drv)

	first, err := h.dial(t, h.signer)
	if err != nil {
		t.Fatalf("first dial: %v", err)
	}
	if sess, err := first.NewSession(); err == nil {
		_ = sess.Close()
		t.Fatal("the first connection reached the dead endpoint")
	}
	_ = first.Close()

	// Give the handler time to record the failed dial and drop the lease.
	deadline := time.Now().Add(3 * time.Second)
	for time.Now().Before(deadline) && h.manager.Lookup(h.identity) != nil {
		time.Sleep(10 * time.Millisecond)
	}

	second, err := h.dial(t, h.signer)
	if err != nil {
		t.Fatalf("second dial: %v", err)
	}
	defer second.Close()
	if got := strings.TrimSpace(runCommand(t, second, "hostname")); got != "fake-1" {
		t.Fatalf("second connection reached %q, want the healthy instance", got)
	}
}

// TestCloseDropsLiveSessions is the F6 regression (probe E6): Close() returned only when the client
// hung up, so a rolling update waited out the grace period and was killed by SIGKILL.
func TestCloseDropsLiveSessions(t *testing.T) {
	h := startGateway(t)
	c, err := h.dial(t, h.signer)
	if err != nil {
		t.Fatalf("dial: %v", err)
	}
	defer c.Close()
	if got := strings.TrimSpace(runCommand(t, c, "hostname")); got != "fake-1" {
		t.Fatalf("session did not reach the instance: %q", got)
	}

	closed := make(chan struct{})
	go func() {
		_ = h.srv.Close()
		close(closed)
	}()
	select {
	case <-closed:
	case <-time.After(5 * time.Second):
		t.Fatal("Close() did not return while one client session was open")
	}

	// The session must be gone, not merely unwatched.
	if sess, err := c.NewSession(); err == nil {
		_ = sess.Close()
		t.Fatal("a session started after Close()")
	}
}

// TestTwoRemoteForwardsOnPortZero is the F13 regression: `-R 0:host:port` asks the server to pick the
// port, so two such requests are identical on the wire — keying the listener set by the requested port
// made the second one look like a duplicate and refuse it.
func TestTwoRemoteForwardsOnPortZero(t *testing.T) {
	h := startGateway(t)
	c, err := h.dial(t, h.signer)
	if err != nil {
		t.Fatalf("dial: %v", err)
	}
	defer c.Close()

	first, err := c.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatalf("first -R 0 forward refused: %v", err)
	}
	defer first.Close()
	second, err := c.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatalf("second -R 0 forward refused (the two requests share a key): %v", err)
	}
	defer second.Close()

	p1 := first.Addr().(*net.TCPAddr).Port
	p2 := second.Addr().(*net.TCPAddr).Port
	if p1 == 0 || p2 == 0 || p1 == p2 {
		t.Fatalf("allocated ports = %d and %d, want two distinct non-zero ports", p1, p2)
	}
}
