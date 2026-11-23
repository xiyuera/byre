// Package sshfront terminates SSH from clients, resolves each connection to a lease, and splices the
// client's channels onto a gateway-owned connection to the leased instance.
//
// The front end deliberately understands no client-specific protocol: it authenticates, allocates a
// lease, then forwards channels and requests verbatim.
package sshfront

import (
	"context"
	"crypto/ed25519"
	"crypto/rand"
	"crypto/sha256"
	"encoding/base64"
	"encoding/pem"
	"errors"
	"fmt"
	"log/slog"
	"net"
	"os"
	"path/filepath"
	"sync"
	"sync/atomic"
	"time"

	"golang.org/x/crypto/ssh"

	"byre/internal/backend"
	"byre/internal/lease"
)

// Permission extension keys carried on an authenticated connection.
const (
	ExtIdentity = "byre/identity"
	ExtBackend  = "byre/backend"
)

// Identity is one allowed client, resolved from configuration.
type Identity struct {
	Fingerprint string // normalised: hex-free base64 body
	Backend     string
	Aliases     []string
}

// Config is everything the front end needs. Targets are dialled through DialTarget, which tests
// replace with an in-process connection.
type Config struct {
	Listen      string
	HostKeyPath string // empty = ephemeral key generated at startup
	Identities  map[string]Identity
	Leases      *lease.Manager
	Logger      *slog.Logger
	DialTarget  DialFunc
}

// DialFunc opens a client connection to a leased instance.
type DialFunc func(ctx context.Context, in backend.Instance) (*ssh.Client, error)

// Server is the SSH front end.
type Server struct {
	cfg     Config
	hostKey ssh.Signer
	log     *slog.Logger
	dial    DialFunc

	// unreachable counts dial failures against instances that still exist. It is kept apart from the
	// manager's dead-instance counter because the remedy differs: one is a stale binding, the other is an
	// instance-level fault.
	unreachable atomic.Int64

	mu     sync.Mutex
	ln     net.Listener
	conns  map[net.Conn]struct{} // live client connections, closed by Close
	closed bool
	connWG sync.WaitGroup
}

// New validates the configuration and prepares host key material.
func New(cfg Config) (*Server, error) {
	if len(cfg.Identities) == 0 {
		return nil, errors.New("sshfront: at least one identity is required")
	}
	if cfg.Leases == nil {
		return nil, errors.New("sshfront: a lease manager is required")
	}
	for fp, id := range cfg.Identities {
		if fp == "" {
			return nil, errors.New("sshfront: identity with empty fingerprint")
		}
		if id.Backend == "" {
			return nil, fmt.Errorf("sshfront: identity %s has no backend", fp)
		}
	}
	log := cfg.Logger
	if log == nil {
		log = slog.Default()
	}
	dial := cfg.DialTarget
	if dial == nil {
		dial = DialInstance
	}
	signer, err := loadOrCreateHostKey(cfg.HostKeyPath)
	if err != nil {
		return nil, err
	}
	return &Server{cfg: cfg, hostKey: signer, log: log, dial: dial}, nil
}

// Addr reports the bound address (after Listen).
func (s *Server) Addr() string {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.ln == nil {
		return ""
	}
	return s.ln.Addr().String()
}

// Listen binds the configured address.
func (s *Server) Listen() error {
	ln, err := net.Listen("tcp", s.cfg.Listen)
	if err != nil {
		return fmt.Errorf("listen %s: %w", s.cfg.Listen, err)
	}
	s.mu.Lock()
	s.ln = ln
	s.mu.Unlock()
	return nil
}

// Serve accepts connections until ctx is cancelled.
func (s *Server) Serve(ctx context.Context) error {
	s.mu.Lock()
	ln := s.ln
	s.mu.Unlock()
	if ln == nil {
		if err := s.Listen(); err != nil {
			return err
		}
		s.mu.Lock()
		ln = s.ln
		s.mu.Unlock()
	}
	s.log.Info("ssh front end listening", "addr", s.Addr(), "identities", len(s.cfg.Identities))

	go func() {
		<-ctx.Done()
		_ = ln.Close()
	}()

	for {
		raw, err := ln.Accept()
		if err != nil {
			if ctx.Err() != nil {
				return nil
			}
			return fmt.Errorf("accept: %w", err)
		}
		if !s.track(raw) {
			// Close() ran between Accept and here: the listener is gone, stop accepting.
			_ = raw.Close()
			return nil
		}
		s.connWG.Add(1)
		go func() {
			defer s.connWG.Done()
			defer s.untrack(raw)
			s.handleConn(ctx, raw)
		}()
	}
}

// track registers a live connection. It reports false when the server is already closing, so a
// connection accepted in that window is not served.
func (s *Server) track(raw net.Conn) bool {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.closed {
		return false
	}
	if s.conns == nil {
		s.conns = make(map[net.Conn]struct{})
	}
	s.conns[raw] = struct{}{}
	return true
}

func (s *Server) untrack(raw net.Conn) {
	s.mu.Lock()
	delete(s.conns, raw)
	s.mu.Unlock()
}

// Close stops the listener, drops every live client connection and waits for the handlers to finish.
//
// Dropping the connections is the part that makes shutdown quick: an SSH session is a blocking read
// loop that only notices the process is going away when its socket dies. Waiting on connWG alone is
// what turned a rolling update into 30 seconds of SIGTERM followed by SIGKILL.
func (s *Server) Close() error {
	s.mu.Lock()
	s.closed = true
	ln := s.ln
	live := make([]net.Conn, 0, len(s.conns))
	for c := range s.conns {
		live = append(live, c)
	}
	s.mu.Unlock()

	if ln != nil {
		_ = ln.Close()
	}
	for _, c := range live {
		_ = c.Close()
	}
	s.connWG.Wait()
	return nil
}

func (s *Server) serverConfig() *ssh.ServerConfig {
	cfg := &ssh.ServerConfig{
		ServerVersion: "SSH-2.0-byre",
		PublicKeyCallback: func(_ ssh.ConnMetadata, key ssh.PublicKey) (*ssh.Permissions, error) {
			fp := Fingerprint(key)
			id, ok := s.cfg.Identities[fp]
			if !ok {
				// Deliberately vague: do not tell an unknown key whether it was close to a known one.
				return nil, errors.New("unknown public key")
			}
			return &ssh.Permissions{Extensions: map[string]string{
				ExtIdentity: fp,
				ExtBackend:  id.Backend,
			}}, nil
		},
	}
	cfg.AddHostKey(s.hostKey)
	return cfg
}

func (s *Server) handleConn(ctx context.Context, raw net.Conn) {
	defer raw.Close()

	conn, chans, reqs, err := ssh.NewServerConn(raw, s.serverConfig())
	if err != nil {
		s.log.Debug("handshake failed", "remote", raw.RemoteAddr().String(), "error", err)
		return
	}
	defer conn.Close()

	fp := conn.Permissions.Extensions[ExtIdentity]
	backendName := conn.Permissions.Extensions[ExtBackend]
	short := ShortIdentity(fp)

	// leaseAndDial releases its own connection reference on failure; on success this connection owns it.
	entry, target, err := s.leaseAndDial(ctx, fp, backendName, short)
	if target != nil {
		defer target.Close()
	}
	if err != nil {
		_ = conn.Close()
		return
	}
	defer s.cfg.Leases.ReleaseConn(fp)

	s.log.Info("session open",
		"identity", short,
		"backend", entry.Backend,
		"instance", entry.Instance.ID,
	)

	listeners := newListenerSet()
	defer listeners.closeAll()

	go s.handleGlobalRequests(conn, reqs, target, listeners)

	for newCh := range chans {
		s.cfg.Leases.KeepAlive(fp)
		go spliceChannel(newCh, target, s.log)
	}

	s.log.Info("session closed", "identity", short, "instance", entry.Instance.ID)
}

// leaseAndDial resolves an identity to a connected target, and classifies a failed dial instead of only
// logging it.
//
// A dial error is ambiguous: the object may be gone (a deleted pod, whose address nothing answers on any
// more) or present but not answering (a wedged sshd, a refused port). Before this classification the front
// end closed the client either way and left the lease in place, so a deleted instance kept receiving every
// reconnect from that identity until the reaper ran — ttl+grace, seven minutes by default. From the
// client's side that looked like a host that hung up, because that is exactly what it was talking to.
//
// The backend answers the question, not the error string: only a definitive "gone" replaces the instance.
// When the object still exists the failure belongs to the instance, the lease is kept, and the attempt is
// counted as unreachable.
//
// On every failure path this function drops the connection reference it took, so a client that hangs up
// after a failed dial leaves an idle lease rather than a stuck ref that no reaper can reclaim.
func (s *Server) leaseAndDial(ctx context.Context, fp, backendName, short string) (lease.Entry, *ssh.Client, error) {
	entry, err := s.cfg.Leases.Acquire(ctx, fp, backendName)
	if err != nil {
		s.log.Warn("no instance for identity", "identity", short, "backend", backendName, "error", err)
		return lease.Entry{}, nil, err
	}

	target, err := s.dial(ctx, entry.Instance)
	if err == nil {
		// A successful dial covers TCP and authentication, not the session. Probing for one costs a round
		// trip and catches the instance that answers the handshake and then refuses every channel
		// (ADR-0013, part 5).
		//
		// A failed probe counts against the instance but does *not* refuse the connection. A client may be
		// here to forward a port, which needs no session at all -- direct-tcpip is spliced through
		// separately -- and refusing on its behalf would break something that works. The count is what
		// eventually replaces the instance; until then this client sees exactly what it would have seen
		// before the probe existed.
		if perr := probeSession(target); perr != nil {
			if s.cfg.Leases.RecordUnusable(ctx, fp, entry.Instance) {
				_ = target.Close()
				s.log.Warn("instance refused sessions repeatedly; replacing it",
					"identity", short, "instance", entry.Instance.ID)
				return s.provisionReplacement(ctx, fp, backendName, short, entry.Instance.ID, "refused sessions")
			}
			s.log.Warn("instance refused a probe session; allowing the connection anyway",
				"identity", short, "instance", entry.Instance.ID, "error", perr)
		} else {
			// The probe opened a session, which is the only proof available here that this instance works.
			// NoteHealthy must not be called on connection setup: a connection succeeds against an instance
			// that refuses every session, and clearing the streak there would keep the instance forever.
			s.cfg.Leases.NoteHealthy(entry.Instance)
		}
		return entry, target, nil
	}
	s.log.Warn("cannot reach instance", "identity", short, "instance", entry.Instance.ID, "error", err)

	dropped, probeErr := s.cfg.Leases.DiscardIfGone(ctx, fp, entry.Instance)
	if probeErr != nil {
		s.log.Warn("instance liveness unknown after a dial failure; keeping the lease",
			"identity", short, "instance", entry.Instance.ID, "error", probeErr)
	}
	if !dropped {
		// The instance exists and did not answer: an instance-level fault. Keeping the lease means the next
		// attempt is still aimed at the client's own instance, not at a stranger's — but this connection is
		// over, so its reference goes with it and the reaper may reclaim the lease.
		s.unreachable.Add(1)
		s.cfg.Leases.ReleaseConn(fp)
		return lease.Entry{}, nil, err
	}

	return s.provisionReplacement(ctx, fp, backendName, short, entry.Instance.ID, "instance disappeared")
}

// provisionReplacement acquires a fresh instance and dials it, inside the same connection attempt: the
// client has not seen an error yet, so if the replacement is reachable its session simply continues on a
// new instance -- the whole difference between "the gateway moved me" and "the host hung up".
//
// One retry, not a loop. A second failure is a different problem, and an unbounded retry here would turn
// one bad instance into a stream of pods.
func (s *Server) provisionReplacement(ctx context.Context, fp, backendName, short, oldID, reason string) (lease.Entry, *ssh.Client, error) {
	fresh, err := s.cfg.Leases.Acquire(ctx, fp, backendName)
	if err != nil {
		s.log.Warn("no replacement instance after a dead lease",
			"identity", short, "backend", backendName, "reason", reason, "error", err)
		return lease.Entry{}, nil, err
	}
	replacement, dialErr := s.dial(ctx, fresh.Instance)
	if dialErr != nil {
		s.log.Warn("cannot reach the replacement instance",
			"identity", short, "instance", fresh.Instance.ID, "error", dialErr)
		s.unreachable.Add(1)
		s.cfg.Leases.ReleaseConn(fp)
		return fresh, nil, dialErr
	}
	s.log.Info("lease replaced",
		"identity", short, "backend", backendName,
		"old_instance", oldID, "new_instance", fresh.Instance.ID, "reason", reason)
	return fresh, replacement, nil
}

// probeSession proves the instance will accept a session, not merely a handshake.
//
// ssh.Dial covers the TCP connection and the authentication. An instance whose container restarted can
// answer both and then refuse every channel with "Session open refused by peer". Opening and immediately
// closing a session costs one round trip on a path that has already paid several.
func probeSession(c *ssh.Client) error {
	sess, err := c.NewSession()
	if err != nil {
		return err
	}
	// Failing to close means the channel died between opening and closing, which is not a healthy
	// instance either; report it rather than swallowing it.
	return sess.Close()
}

// UnreachableTotal reports dial failures against instances that still exist.
func (s *Server) UnreachableTotal() int64 { return s.unreachable.Load() }

func (s *Server) handleGlobalRequests(conn *ssh.ServerConn, reqs <-chan *ssh.Request, target *ssh.Client, listeners *listenerSet) {
	for req := range reqs {
		switch req.Type {
		case "tcpip-forward":
			forwardRemote(conn, target, req, listeners)
		case "cancel-tcpip-forward":
			cancelRemote(req, listeners)
		case "keepalive@openssh.com":
			if req.WantReply {
				_ = req.Reply(true, nil)
			}
		default:
			if req.WantReply {
				_ = req.Reply(false, nil)
			}
		}
	}
}

// Fingerprint is the identity of a public key: base64(sha256(key)) without padding, matching
// `ssh-keygen -l -E sha256` output minus the "SHA256:" prefix.
func Fingerprint(key ssh.PublicKey) string {
	sum := sha256.Sum256(key.Marshal())
	return base64.RawStdEncoding.EncodeToString(sum[:])
}

// ShortIdentity truncates a fingerprint for logs: enough to correlate, not enough to reconstruct.
func ShortIdentity(fp string) string {
	if len(fp) <= 16 {
		return fp
	}
	return fp[:16]
}

// DialInstance is the production dialler: it reads the backend's key, verifies the instance host key
// according to policy, and returns a client suitable for channel splicing.
func DialInstance(ctx context.Context, in backend.Instance) (*ssh.Client, error) {
	keyBytes, err := os.ReadFile(in.PrivateKey)
	if err != nil {
		return nil, fmt.Errorf("read key %s: %w", in.PrivateKey, err)
	}
	signer, err := ssh.ParsePrivateKey(keyBytes)
	if err != nil {
		return nil, fmt.Errorf("parse key %s: %w", in.PrivateKey, err)
	}
	cb, err := hostKeyCallback(in.HostKeyPolicy)
	if err != nil {
		return nil, err
	}

	cfg := &ssh.ClientConfig{
		User:            in.User,
		Auth:            []ssh.AuthMethod{ssh.PublicKeys(signer)},
		HostKeyCallback: cb,
		Timeout:         15 * time.Second,
	}
	d := net.Dialer{Timeout: 15 * time.Second}
	netConn, err := d.DialContext(ctx, "tcp", in.Addr)
	if err != nil {
		return nil, fmt.Errorf("dial %s: %w", in.Addr, err)
	}
	c, chans, reqs, err := ssh.NewClientConn(netConn, in.Addr, cfg)
	if err != nil {
		_ = netConn.Close()
		return nil, fmt.Errorf("handshake with %s: %w", in.Addr, err)
	}
	return ssh.NewClient(c, chans, reqs), nil
}

// loadOrCreateHostKey returns the key clients pin.
//
// Three cases, each deliberate:
//   - path == "":            ephemeral key, generated in memory. Fine for tests, wrong for a deployment:
//     the fingerprint changes on every restart, so strict clients stop connecting.
//   - path exists:           load it; the fingerprint is stable across restarts, as clients expect.
//   - path missing:          generate and persist it, so a first install works without a manual step.
//     The path must be on durable storage (a volume); on an emptyDir this degrades to the first case.
func loadOrCreateHostKey(path string) (ssh.Signer, error) {
	if path == "" {
		_, priv, err := ed25519.GenerateKey(rand.Reader)
		if err != nil {
			return nil, fmt.Errorf("generate host key: %w", err)
		}
		slog.Warn("no host_key configured: using an ephemeral host key; clients cannot pin this gateway")
		return ssh.NewSignerFromKey(priv)
	}

	b, err := os.ReadFile(path)
	switch {
	case err == nil:
		signer, perr := ssh.ParsePrivateKey(b)
		if perr != nil {
			return nil, fmt.Errorf("parse host key %s: %w", path, perr)
		}
		return signer, nil
	case !errors.Is(err, os.ErrNotExist):
		return nil, fmt.Errorf("read host key %s: %w", path, err)
	}

	// First start on this volume: create the key and write it down before using it.
	_, priv, err := ed25519.GenerateKey(rand.Reader)
	if err != nil {
		return nil, fmt.Errorf("generate host key: %w", err)
	}
	block, err := ssh.MarshalPrivateKey(priv, "byre-host")
	if err != nil {
		return nil, fmt.Errorf("marshal host key: %w", err)
	}
	if dir := filepath.Dir(path); dir != "" {
		if err := os.MkdirAll(dir, 0o700); err != nil {
			return nil, fmt.Errorf("create host key directory %s: %w", dir, err)
		}
	}
	if err := os.WriteFile(path, pem.EncodeToMemory(block), 0o600); err != nil {
		return nil, fmt.Errorf("persist host key %s: %w", path, err)
	}
	signer, err := ssh.ParsePrivateKey(pem.EncodeToMemory(block))
	if err != nil {
		return nil, fmt.Errorf("parse freshly written host key: %w", err)
	}
	slog.Warn("generated a new host key and persisted it",
		"path", path, "fingerprint", ssh.FingerprintSHA256(signer.PublicKey()),
		"note", "clients that pinned a previous key must be updated")
	return signer, nil
}
