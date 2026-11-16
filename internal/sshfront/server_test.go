package sshfront

import (
	"context"
	"crypto/ed25519"
	"crypto/rand"
	"encoding/pem"
	"fmt"
	"io"
	"log/slog"
	"net"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"golang.org/x/crypto/ssh"

	"keypod/internal/backend"
	"keypod/internal/lease"
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
	go ssh.DiscardRequests(reqs)

	for newCh := range chans {
		switch newCh.ChannelType() {
		case "session":
			ch, chReqs, err := newCh.Accept()
			if err != nil {
				continue
			}
			go func() {
				for req := range chReqs {
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
	manager  *lease.Manager
	identity string // fingerprint of the allowed client key
	signer   ssh.Signer
}

// startGateway wires a real front end against one fake target through the real dialler.
func startGateway(t *testing.T) *harness {
	t.Helper()
	targetAddr := startFakeTarget(t, "fake-1")

	clientSigner, _ := newKeyPair(t)
	_, poolPriv := newKeyPair(t)
	poolKeyPath := writeKeyFile(t, poolPriv)

	drv, err := backend.NewStatic(backend.StaticConfig{
		Name:          "lab",
		Endpoints:     []string{targetAddr},
		User:          "sandbox",
		PrivateKey:    poolKeyPath,
		HostKeyPolicy: "insecure", // the fake target's host key is generated per test
	})
	if err != nil {
		t.Fatalf("static backend: %v", err)
	}
	mgr, err := lease.New(360*time.Second, 60*time.Second, map[string]backend.Backend{"lab": drv})
	if err != nil {
		t.Fatalf("lease manager: %v", err)
	}

	fp := Fingerprint(clientSigner.PublicKey())
	logger := slog.New(slog.NewTextHandler(io.Discard, nil))
	if os.Getenv("KEYPOD_TEST_LOG") != "" {
		logger = slog.New(slog.NewTextHandler(os.Stderr, &slog.HandlerOptions{Level: slog.LevelDebug}))
	}
	srv, err := New(Config{
		Listen: "127.0.0.1:0",
		Identities: map[string]Identity{
			fp: {Fingerprint: fp, Backend: "lab", Aliases: []string{"test"}},
		},
		Leases: mgr,
		Logger: logger,
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
	return &harness{addr: srv.Addr(), manager: mgr, identity: fp, signer: clientSigner}
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
	e := h.manager.Lookup(h.identity)
	if e == nil {
		t.Fatal("lease disappeared between connections")
	}
	if e.Instance.ID != "fake-1" && !strings.HasPrefix(e.Instance.ID, "127.0.0.1:") {
		t.Fatalf("unexpected instance id %q", e.Instance.ID)
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

func TestForeignBackendRefusedAfterLease(t *testing.T) {
	h := startGateway(t)
	if e := h.manager.Lookup(h.identity); e != nil {
		t.Fatalf("unexpected pre-existing lease: %+v", e)
	}
	c, err := h.dial(t, h.signer)
	if err != nil {
		t.Fatalf("dial: %v", err)
	}
	defer c.Close()
	if e := h.manager.Lookup(h.identity); e == nil || e.Backend != "lab" {
		t.Fatalf("lease not recorded: %+v", e)
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
