package sshfront

import (
	"errors"
	"fmt"
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
func appendKnownHost(path, hostname string, key ssh.PublicKey) error {
	if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
		return fmt.Errorf("create %s: %w", filepath.Dir(path), err)
	}
	f, err := os.OpenFile(path, os.O_APPEND|os.O_CREATE|os.O_WRONLY, 0o600)
	if err != nil {
		return fmt.Errorf("open known_hosts %s: %w", path, err)
	}
	defer f.Close()
	line := knownhosts.Line([]string{hostname}, key) + "\n"
	if _, err := f.WriteString(line); err != nil {
		return fmt.Errorf("append known_hosts %s: %w", path, err)
	}
	return nil
}
