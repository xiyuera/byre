package backend

import "context"

// SetReadyProbeForTest replaces the readiness probe process-wide and returns a function that restores the
// previous one.
//
// It exists for tests outside this package that drive a Kubernetes driver through a fake clientset: the
// fake hands out addresses (192.0.2.x) that nothing answers on, while readiness means "the advertised
// address answers and speaks SSH" (ADR-0005, and the banner check that followed it). Such a test is
// asserting the lease table or the front end, not the cluster, so the address never has to be real.
//
// Production never calls this. Tests in this package use withProbe instead, which does the same thing
// locally; this hook is exported because a variable of an unexported type cannot be reached from
// backendtest.
func SetReadyProbeForTest(fn func(ctx context.Context, addr string) error) (restore func()) {
	orig := probeAddr
	probeAddr = fn
	return func() { probeAddr = orig }
}
