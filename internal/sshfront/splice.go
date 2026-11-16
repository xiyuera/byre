package sshfront

import (
	"fmt"
	"io"
	"log/slog"
	"net"
	"strconv"
	"sync"
	"time"

	"golang.org/x/crypto/ssh"
)

// directTCPIP is the channel payload of "direct-tcpip" (RFC 4254 §7.2), produced by `ssh -L`.
type directTCPIP struct {
	Host       string
	Port       uint32
	OriginHost string
	OriginPort uint32
}

// forwardedTCPIP is the channel payload of "forwarded-tcpip" (RFC 4254 §7.2), produced by `ssh -R`.
type forwardedTCPIP struct {
	Host       string
	Port       uint32
	OriginHost string
	OriginPort uint32
}

// spliceChannel fulfils one client channel against the target instance.
//
// Everything above the channel is opaque: after setup the payload is copied byte for byte, so none of
// the client's protocol (pty, exec, sftp, forwarding) has to be re-implemented here.
func spliceChannel(newCh ssh.NewChannel, target *ssh.Client, log *slog.Logger) {
	var err error
	switch newCh.ChannelType() {
	case "session":
		err = spliceSession(newCh, target)
	case "direct-tcpip":
		err = spliceDirectTCPIP(newCh, target)
	default:
		_ = newCh.Reject(ssh.UnknownChannelType, fmt.Sprintf("channel type %q is not proxied", newCh.ChannelType()))
		log.Warn("channel rejected", "type", newCh.ChannelType())
		return
	}
	if err != nil {
		log.Warn("channel failed", "type", newCh.ChannelType(), "error", err)
	}
}

func spliceSession(newCh ssh.NewChannel, target *ssh.Client) error {
	tch, treqs, err := target.OpenChannel("session", newCh.ExtraData())
	if err != nil {
		_ = newCh.Reject(ssh.ConnectionFailed, "target refused session: "+err.Error())
		return fmt.Errorf("open session on target: %w", err)
	}
	cch, creqs, err := newCh.Accept()
	if err != nil {
		_ = tch.Close()
		return fmt.Errorf("accept session: %w", err)
	}

	// Requests travel both ways: pty-req/shell/exec/window-change from the client, exit-status and
	// friends from the target. Dropping either direction breaks an interactive session.
	upstreamDone := make(chan struct{})
	go func() {
		forwardRequests(treqs, cch)
		close(upstreamDone)
	}()
	go forwardRequests(creqs, tch)

	clientDone := make(chan struct{})
	go func() {
		defer close(clientDone)
		_, _ = io.Copy(tch, cch)
		_ = tch.CloseWrite()
	}()

	_, _ = io.Copy(cch, tch)

	// A target sends exit-status immediately before closing the channel. Deliver it before the client
	// sees EOF, or the client reports "remote command exited without exit status".
	select {
	case <-upstreamDone:
	case <-time.After(2 * time.Second):
	}
	_ = cch.CloseWrite()
	select {
	case <-clientDone:
	case <-time.After(2 * time.Second):
	}
	_ = tch.Close()
	_ = cch.Close()
	return nil
}

func spliceDirectTCPIP(newCh ssh.NewChannel, target *ssh.Client) error {
	var d directTCPIP
	if err := ssh.Unmarshal(newCh.ExtraData(), &d); err != nil {
		_ = newCh.Reject(ssh.Prohibited, "malformed direct-tcpip payload")
		return fmt.Errorf("parse direct-tcpip: %w", err)
	}
	if d.Host == "" || d.Port == 0 || d.Port > 65535 {
		_ = newCh.Reject(ssh.Prohibited, "invalid destination")
		return fmt.Errorf("invalid direct-tcpip destination %q:%d", d.Host, d.Port)
	}

	addr := net.JoinHostPort(d.Host, strconv.FormatUint(uint64(d.Port), 10))
	upstream, err := target.Dial("tcp", addr)
	if err != nil {
		_ = newCh.Reject(ssh.ConnectionFailed, "target could not reach "+addr)
		return fmt.Errorf("dial %s via target: %w", addr, err)
	}

	cch, creqs, err := newCh.Accept()
	if err != nil {
		_ = upstream.Close()
		return fmt.Errorf("accept direct-tcpip: %w", err)
	}
	go ssh.DiscardRequests(creqs)

	go copyOneWay(cch, upstream)
	copyOneWay(upstream, cch)
	_ = cch.Close()
	_ = upstream.Close()
	return nil
}

// forwardRemote serves `ssh -R`: the target opens a listener, and each accepted connection is turned
// into a "forwarded-tcpip" channel towards the client.
func forwardRemote(client *ssh.ServerConn, target *ssh.Client, req *ssh.Request, listeners *listenerSet) {
	var p struct {
		BindAddr string
		BindPort uint32
	}
	if err := ssh.Unmarshal(req.Payload, &p); err != nil {
		_ = req.Reply(false, nil)
		return
	}

	addr := net.JoinHostPort(p.BindAddr, strconv.FormatUint(uint64(p.BindPort), 10))
	ln, err := target.Listen("tcp", addr)
	if err != nil {
		_ = req.Reply(false, nil)
		return
	}
	bound := ln.Addr().(*net.TCPAddr)
	key := fmt.Sprintf("%s:%d", p.BindAddr, p.BindPort)
	if !listeners.add(key, ln) {
		_ = ln.Close()
		_ = req.Reply(false, nil)
		return
	}

	if req.WantReply {
		// Port 0 means "pick one"; the client learns the port from the reply payload.
		_ = req.Reply(true, ssh.Marshal(struct{ Port uint32 }{uint32(bound.Port)}))
	}

	go func() {
		for {
			conn, err := ln.Accept()
			if err != nil {
				return
			}
			go func(c net.Conn) {
				defer c.Close()
				origin := c.RemoteAddr().(*net.TCPAddr)
				payload := ssh.Marshal(&forwardedTCPIP{
					Host:       p.BindAddr,
					Port:       uint32(bound.Port),
					OriginHost: origin.IP.String(),
					OriginPort: uint32(origin.Port),
				})
				ch, reqs, err := client.OpenChannel("forwarded-tcpip", payload)
				if err != nil {
					return
				}
				ssh.DiscardRequests(reqs)
				go copyOneWay(ch, c)
				copyOneWay(c, ch)
				_ = ch.Close()
			}(conn)
		}
	}()
}

func cancelRemote(req *ssh.Request, listeners *listenerSet) {
	var p struct {
		BindAddr string
		BindPort uint32
	}
	if err := ssh.Unmarshal(req.Payload, &p); err != nil {
		_ = req.Reply(false, nil)
		return
	}
	ok := listeners.remove(fmt.Sprintf("%s:%d", p.BindAddr, p.BindPort))
	_ = req.Reply(ok, nil)
}

// forwardRequests relays channel requests in one direction, answering the sender with the receiver's
// verdict whenever a reply was requested.
func forwardRequests(src <-chan *ssh.Request, dst ssh.Channel) {
	for req := range src {
		ok, err := dst.SendRequest(req.Type, req.WantReply, req.Payload)
		if req.WantReply {
			_ = req.Reply(ok && err == nil, nil)
		}
	}
}

func copyBoth(a, b ssh.Channel) {
	go copyOneWay(a, b)
	copyOneWay(b, a)
	_ = a.Close()
	_ = b.Close()
}

// copyOneWay copies src → dst and half-closes dst so the peer sees EOF instead of a hang.
func copyOneWay(dst io.Writer, src io.Reader) {
	_, _ = io.Copy(dst, src)
	if cw, ok := dst.(interface{ CloseWrite() error }); ok {
		_ = cw.CloseWrite()
	}
}

// listenerSet tracks remote forwardings so they can be cancelled and closed on disconnect.
type listenerSet struct {
	mu sync.Mutex
	m  map[string]net.Listener
}

func newListenerSet() *listenerSet { return &listenerSet{m: map[string]net.Listener{}} }

func (s *listenerSet) add(key string, ln net.Listener) bool {
	s.mu.Lock()
	defer s.mu.Unlock()
	if _, exists := s.m[key]; exists {
		return false
	}
	s.m[key] = ln
	return true
}

func (s *listenerSet) remove(key string) bool {
	s.mu.Lock()
	defer s.mu.Unlock()
	ln, ok := s.m[key]
	if !ok {
		return false
	}
	delete(s.m, key)
	_ = ln.Close()
	return true
}

func (s *listenerSet) closeAll() {
	s.mu.Lock()
	defer s.mu.Unlock()
	for k, ln := range s.m {
		_ = ln.Close()
		delete(s.m, k)
	}
}
