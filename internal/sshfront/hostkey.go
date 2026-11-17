package sshfront

import (
	"errors"
	"fmt"
	"io"
	"net"
	"os"
	"path/filepath"

	"golang.org/x/crypto/ssh"
	"golang.org/x/crypto/ssh/knownhosts"
)

// hostKeyCallback builds the verifier for the target instance's host key.
//
// policy:
//   - strict:     the host must already be in known_hosts; unknown and changed keys both fail.
//   - accept-new: trust on first use — an unknown host is appended, a *changed* key still fails.
//   - insecure:   accept anything. Only acceptable for throwaway instances.
func hostKeyCallback(policy string) (ssh.HostKeyCallback, error) {
	switch policy {
	case "insecure":
		return ssh.InsecureIgnoreHostKey(), nil
	case "strict":
		path, err := knownHostsPath()
		if err != nil {
			return nil, err
		}
		cb, err := knownhosts.New(path)
		if err != nil {
			return nil, fmt.Errorf("strict host key policy needs a readable known_hosts (%s): %w", path, err)
		}
		return cb, nil
	case "accept-new":
		path, err := knownHostsPath()
		if err != nil {
			return nil, err
		}
		return trustOnFirstUse(path)
	default:
		return nil, fmt.Errorf("unknown host key policy %q", policy)
	}
}

func knownHostsPath() (string, error) {
	home, err := os.UserHomeDir()
	if err != nil {
		return "", fmt.Errorf("cannot determine home directory for known_hosts: %w", err)
	}
	return filepath.Join(home, ".ssh", "known_hosts"), nil
}

func trustOnFirstUse(path string) (ssh.HostKeyCallback, error) {
	base, baseErr := knownhosts.New(path)
	if baseErr != nil && !errors.Is(baseErr, os.ErrNotExist) {
		// A malformed known_hosts must not silently degrade into "trust everything".
		return nil, fmt.Errorf("read known_hosts %s: %w", path, baseErr)
	}

	return func(hostname string, remote net.Addr, key ssh.PublicKey) error {
		if base != nil {
			err := base(hostname, remote, key)
			if err == nil {
				return nil
			}
			var keyErr *knownhosts.KeyError
			if !errors.As(err, &keyErr) || len(keyErr.Want) > 0 {
				// Known host, different key: refuse, exactly like strict mode.
				return err
			}
		}
		return appendKnownHost(path, hostname, key)
	}, nil
}

// appendKnownHost records a key for an unknown host and verifies the write landed.
//
// The read-back matters: a known_hosts that silently lost a line does not degrade to "trust anything",
// it refuses every later connection (see trustOnFirstUse). "The write returned no error" is not the
// same statement as "the line is on disk", and the second one is the only one worth acting on.
func appendKnownHost(path, hostname string, key ssh.PublicKey) error {
	dir := filepath.Dir(path)
	if err := os.MkdirAll(dir, 0o700); err != nil {
		return fmt.Errorf("create %s: %w", dir, err)
	}
	line := knownhosts.Line([]string{hostname}, key) + "\n"

	f, err := os.OpenFile(path, os.O_RDWR|os.O_CREATE|os.O_APPEND, 0o600)
	if err != nil {
		return fmt.Errorf("open known_hosts %s: %w", path, err)
	}
	defer f.Close()

	// Two connections can discover the same host at the same moment; without a lock their lines can
	// interleave into an unparsable file, which refuses every connection from then on.
	if err := lockFile(f); err != nil {
		return fmt.Errorf("lock known_hosts %s: %w", path, err)
	}
	defer func() { _ = unlockFile(f) }()

	if _, err := f.WriteString(line); err != nil {
		return fmt.Errorf("append known_hosts %s: %w", path, err)
	}
	if err := f.Sync(); err != nil {
		return fmt.Errorf("flush known_hosts %s: %w", path, err)
	}

	end, err := f.Seek(0, io.SeekEnd)
	if err != nil {
		return fmt.Errorf("verify known_hosts %s: %w", path, err)
	}
	start := end - int64(len(line))
	if start < 0 {
		return fmt.Errorf("verify known_hosts %s: file is shorter than the line just written", path)
	}
	got := make([]byte, len(line))
	if _, err := f.ReadAt(got, start); err != nil {
		return fmt.Errorf("verify known_hosts %s: %w", path, err)
	}
	if string(got) != line {
		return fmt.Errorf("known_hosts %s does not contain the line that was just written", path)
	}
	return nil
}
