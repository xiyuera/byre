//go:build !unix

package sshfront

import "os"

// lockFile is a no-op where flock is unavailable. The read-back check in appendKnownHost still runs;
// only the guarantee that two concurrent appends cannot interleave is lost.
func lockFile(*os.File) error { return nil }

func unlockFile(*os.File) error { return nil }
