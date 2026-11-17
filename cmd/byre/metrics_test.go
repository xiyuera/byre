package main

import (
	"context"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"byre/internal/backend"
	"byre/internal/backend/backendtest"
	"byre/internal/lease"
)

// stubUnreachable stands in for the front end's counter. The test is about the exposition, not about
// opening a listener, so the only thing it needs is the number.
type stubUnreachable struct{ n int64 }

func (s stubUnreachable) UnreachableTotal() int64 { return s.n }

// TestMetricsReportADeadInstance reads /metrics the way an operator does, after the one event the incident
// produced in silence: a lease whose instance was deleted.
//
// The assertion is on the response body, not on the manager's fields, because those fields were already
// correct before this change — the failure was that nothing exposed them.
func TestMetricsReportADeadInstance(t *testing.T) {
	ctx := context.Background()
	drv, cs := backendtest.NewWorkloadDriver(t, "lab", "instances")
	m, err := lease.New(6*time.Minute, time.Minute, map[string]backend.Backend{"lab": drv})
	if err != nil {
		t.Fatalf("lease manager: %v", err)
	}

	first, err := m.Acquire(ctx, "alice", "lab")
	if err != nil {
		t.Fatalf("first acquire: %v", err)
	}
	backendtest.DeletePod(t, cs, "instances", first.Instance.ID)
	if _, err := m.Acquire(ctx, "alice", "lab"); err != nil {
		t.Fatalf("acquire after the instance was deleted: %v", err)
	}

	rec := httptest.NewRecorder()
	metricsHandler(m, map[string]backend.Backend{"lab": drv}, stubUnreachable{n: 1}).
		ServeHTTP(rec, httptest.NewRequest("GET", "/metrics", nil))

	if rec.Code != 200 {
		t.Fatalf("GET /metrics = %d, want 200", rec.Code)
	}
	body := rec.Body.String()
	for _, want := range []string{
		"byre_lease_dead_instance_total 1",
		"byre_lease_liveness_errors_total 0",
		"byre_lease_unreachable_total 1",
	} {
		if !strings.Contains(body, want) {
			t.Fatalf("/metrics does not carry %q:\n%s", want, body)
		}
	}
	for _, line := range strings.Split(body, "\n") {
		if strings.HasPrefix(line, "byre_lease_dead_instance_total ") {
			t.Logf("raw exposition line: %s", line)
		}
	}
}
