// Package metrics holds the process-wide numbers behind /metrics.
//
// They are package-level rather than threaded through constructors because one process is one gateway:
// every number here answers "what has this process seen since it started", and there is exactly one
// such subject. Injecting a registry would thread a handle through the backend, lease and pool types
// for no gain; if a process ever hosts two gateways this decision has to be revisited.
//
// The counters exist because the failures they describe are invisible otherwise:
//   - a peer that accepts TCP and never speaks SSH looks like "connection reset" to every client;
//   - a pool that cannot be filled looks like a healthy process with a warm pool of zero;
//   - a release that fails leaves an object nobody tracks, and leaves no trace at all.
package metrics

import (
	"math"
	"sort"
	"sync"
	"time"
)

// fillBuckets are the histogram bounds for one pool fill, in seconds. A fill creates instances, so the
// interesting range spans "warm cache" (sub-second) to "image pull" (tens of seconds).
var fillBuckets = []float64{0.5, 1, 2.5, 5, 10, 30, 60}

var (
	handshakeFail counter
	fillErrors    counter
	releaseErrors counter

	// homeClaimsOrphan is a gauge, not a counter: reclaiming a claim is a deliberate operator action
	// and the number has to be able to go down.
	homeClaimsOrphan counter

	fillMu      sync.Mutex
	fillSum     float64
	fillCount   int64
	fillBucketN []int64

	acquireMu sync.Mutex
	acquires  map[acquireKey]int64
)

type acquireKey struct{ Backend, Result string }

// counter is an int64 with the two operations this package needs.
type counter struct {
	mu sync.Mutex
	n  int64
}

func (c *counter) Add(n int64) {
	c.mu.Lock()
	c.n += n
	c.mu.Unlock()
}

// Value reads the counter.
func (c *counter) Value() int64 {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.n
}

// set exists for gauges. Everything else in this package only ever increases, but "how many home
// claims have no identity behind them" can fall when an operator deletes one.
func (c *counter) set(n int64) {
	c.mu.Lock()
	c.n = n
	c.mu.Unlock()
}

func init() {
	fillBucketN = make([]int64, len(fillBuckets))
	acquires = make(map[acquireKey]int64)
}

// IncHandshakeFail records "the TCP port answered but the peer did not speak SSH". A dial error is a
// different failure and must not be counted here: one is a missing instance, the other is a broken one.
func IncHandshakeFail() { handshakeFail.Add(1) }

// HandshakeFailTotal reports the number of failed SSH banner reads.
func HandshakeFailTotal() int64 { return handshakeFail.Value() }

// IncPoolFillError records one failed instance creation during a pool fill.
func IncPoolFillErrors(n int64) { fillErrors.Add(n) }

// PoolFillErrorsTotal reports failed instance creations.
func PoolFillErrorsTotal() int64 { return fillErrors.Value() }

// IncReleaseError records one failed release attempt (a release is retried, so this counts attempts).
func IncReleaseError() { releaseErrors.Add(1) }

// ReleaseErrorsTotal reports failed release attempts.
func ReleaseErrorsTotal() int64 { return releaseErrors.Value() }

// ObserveFill records how long one pool fill took, including the failed creations inside it.
func ObserveFill(d time.Duration) {
	secs := d.Seconds()
	fillMu.Lock()
	fillSum += secs
	fillCount++
	for i, b := range fillBuckets {
		if secs <= b {
			fillBucketN[i]++
		}
	}
	fillMu.Unlock()
}

// FillSnapshot is a copy of the fill histogram, rendered by the /metrics handler.
type FillSnapshot struct {
	Count   int64
	Sum     float64
	Buckets []FillBucket
}

// FillBucket is one cumulative bucket of the fill histogram.
type FillBucket struct {
	LE    float64
	Count int64
}

// FillDurations snapshots the fill histogram.
func FillDurations() FillSnapshot {
	fillMu.Lock()
	defer fillMu.Unlock()
	out := FillSnapshot{Count: fillCount, Sum: fillSum, Buckets: make([]FillBucket, 0, len(fillBuckets)+1)}
	for i, b := range fillBuckets {
		out.Buckets = append(out.Buckets, FillBucket{LE: b, Count: fillBucketN[i]})
	}
	out.Buckets = append(out.Buckets, FillBucket{LE: math.Inf(1), Count: fillCount})
	return out
}

// ObserveAcquire counts one acquire by backend and outcome: "pool" (warm capacity), "created" (the
// driver had to provision) or "error".
func ObserveAcquire(backendName, result string) {
	acquireMu.Lock()
	acquires[acquireKey{Backend: backendName, Result: result}]++
	acquireMu.Unlock()
}

// AcquireTotal is one labelled sample of the acquire counter.
type AcquireTotal struct {
	Backend string
	Result  string
	Count   int64
}

// AcquireTotals snapshots the acquire counter in a stable order.
func AcquireTotals() []AcquireTotal {
	acquireMu.Lock()
	defer acquireMu.Unlock()
	out := make([]AcquireTotal, 0, len(acquires))
	for k, n := range acquires {
		out = append(out, AcquireTotal{Backend: k.Backend, Result: k.Result, Count: n})
	}
	sort.Slice(out, func(i, j int) bool {
		if out[i].Backend != out[j].Backend {
			return out[i].Backend < out[j].Backend
		}
		return out[i].Result < out[j].Result
	})
	return out
}

// SetHomeClaimsOrphan records how many home claims exist with no configured identity behind them.
//
// The scanner that feeds this never deletes anything. A claim holds data, and nothing at this layer can
// tell "the identity is gone" from "the identity is between connections" or "the key was rotated and
// the name is about to come back".
func SetHomeClaimsOrphan(n int) { homeClaimsOrphan.set(int64(n)) }

// HomeClaimsOrphan reports the value from the last scan.
func HomeClaimsOrphan() int { return int(homeClaimsOrphan.Value()) }
