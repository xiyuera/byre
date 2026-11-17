package main

import (
	"testing"
	"time"
)

// TestShutdownSequenceOrder pins the order that makes a rolling update survivable: readiness goes red
// before the wait, and the sessions are closed last. Closing the listener first would cut clients that
// are still being routed to this pod; skipping the wait would drop them at the same instant the endpoint
// is removed.
func TestShutdownSequenceOrder(t *testing.T) {
	var got []string
	start := time.Now()
	var waited time.Duration

	shutdownSequence(60*time.Millisecond,
		func() { got = append(got, "not-ready") },
		func() { got = append(got, "health"); waited = time.Since(start) },
		func() { got = append(got, "sessions") },
	)

	want := []string{"not-ready", "health", "sessions"}
	if len(got) != len(want) {
		t.Fatalf("call order = %v, want %v", got, want)
	}
	for i := range want {
		if got[i] != want[i] {
			t.Fatalf("call order = %v, want %v", got, want)
		}
	}
	if waited < 60*time.Millisecond {
		t.Fatalf("stopped accepting health after %v, want the delay (%v) actually waited", waited, 60*time.Millisecond)
	}
}

// TestShutdownSequenceToleratesNoDelay: a zero or negative -shutdown-delay must close immediately rather
// than panic or sleep forever. The flag is a duration, and `-shutdown-delay 0` is a legitimate "drop the
// window" for a lab.
func TestShutdownSequenceToleratesNoDelay(t *testing.T) {
	for _, d := range []time.Duration{0, -time.Second} {
		closed := false
		start := time.Now()
		shutdownSequence(d, nil, nil, func() { closed = true })
		if !closed {
			t.Fatalf("delay %v: sessions were not closed", d)
		}
		if elapsed := time.Since(start); elapsed > time.Second {
			t.Fatalf("delay %v: took %v, want an immediate close", d, elapsed)
		}
	}
}

// TestShutdownSequenceWithoutCallbacks is the nil-guard: main passes real functions, but a test or a
// future caller must not have to.
func TestShutdownSequenceWithoutCallbacks(t *testing.T) {
	shutdownSequence(0, nil, nil, nil)
}
