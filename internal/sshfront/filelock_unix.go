//go:build unix

package sshfront

import (
	"os"
	"syscall"
)

// lockFile takes an exclusive advisory lock on f. known_hosts is appended to by whichever connection
// discovers an unknown host first, and two appends that interleave produce a file knownhosts refuses
// to parse — which by design fails every later connection.
func lockFile(f *os.File) error {
	return syscall.Flock(int(f.Fd()), syscall.LOCK_EX)
}

func unlockFile(f *os.File) error {
	return syscall.Flock(int(f.Fd()), syscall.LOCK_UN)
}
