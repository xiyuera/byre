// Package version carries build metadata injected at link time.
package version

// Version is the semantic version or git describe output.
var Version = "dev"

// Commit is the short commit hash the binary was built from.
var Commit = "none"

// BuildDate is the UTC build timestamp.
var BuildDate = "unknown"

// String renders the build stamp used by -version and /healthz.
func String() string {
	return Version + " (" + Commit + ", " + BuildDate + ")"
}
