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
	"errors"
	"fmt"
	"log/slog"
	"net"
	"os"
	"sync"
	"time"

	"golang.org/x/crypto/ssh"

	"keypod/internal/backend"
	"keypod/internal/lease"
)

// Permission extension keys carried on an authenticated connection.
const (
	ExtIdentity = "keypod/identity"
	ExtBackend  = "keypod/backend"
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

	mu     sync.Mutex
	ln     net.Listener
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
		s.connWG.Add(1)
		go func() {
			defer s.connWG.Done()
			s.handleConn(ctx, raw)
		}()
	}
}

// Close stops the listener and waits for handlers to finish.
func (s *Server) Close() error {
	s.mu.Lock()
	ln := s.ln
	s.mu.Unlock()
	if ln != nil {
		_ = ln.Close()
	}
	s.connWG.Wait()
	return nil
}

func (s *Server) serverConfig() *ssh.ServerConfig {
	cfg := &ssh.ServerConfig{
		ServerVersion: "SSH-2.0-keypod",
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

	entry, err := s.cfg.Leases.Acquire(ctx, fp, backendName)
	if err != nil {
		s.log.Warn("no instance for identity", "identity", short, "backend", backendName, "error", err)
		_ = conn.Close()
		return
	}
	defer s.cfg.Leases.ReleaseConn(fp)

	target, err := s.dial(ctx, entry.Instance)
	if err != nil {
		s.log.Warn("cannot reach instance", "identity", short, "instance", entry.Instance.ID, "error", err)
		_ = conn.Close()
		return
	}
	defer target.Close()

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

func loadOrCreateHostKey(path string) (ssh.Signer, error) {
	if path == "" {
		_, priv, err := ed25519.GenerateKey(rand.Reader)
		if err != nil {
			return nil, fmt.Errorf("generate host key: %w", err)
		}
		return ssh.NewSignerFromKey(priv)
	}
	b, err := os.ReadFile(path)
	if err != nil {
		return nil, fmt.Errorf("read host key %s: %w", path, err)
	}
	signer, err := ssh.ParsePrivateKey(b)
	if err != nil {
		return nil, fmt.Errorf("parse host key %s: %w", path, err)
	}
	return signer, nil
}
