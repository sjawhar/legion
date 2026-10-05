package daemon

import (
	"context"
	"fmt"
	"net/http"
	"net/http/httptest"
	"sync/atomic"
	"testing"
	"time"

	"github.com/sjawhar/legion/daemon/internal/bootprobe"
	"github.com/sjawhar/legion/daemon/internal/dispatch"
)

// fastReadiness overrides readinessRetry for a test's lifetime, restored on cleanup, so a gate
// that would otherwise wait up to a minute between attempts costs the suite nothing.
func fastReadiness(t *testing.T, retry bootprobe.Retry) {
	t.Helper()
	previous := readinessRetry
	readinessRetry = retry
	t.Cleanup(func() { readinessRetry = previous })
}

// RED (LEGION-580): before this fix, a boot against Dispatch answering 503 exited at once
// ("legion start: list Dispatch issues for admission: <html>…503 Service Temporarily
// Unavailable…</html>"), the shape 25 of the 96 audited crashes share. Here bootprobe.Run,
// readinessRetry and dispatchReconcileOutcome — the exact pieces daemon.go's run() wires
// together — drive a real dispatch.HTTPClient against an httptest.Server that answers 503 twice
// with the same raw-HTML body the journal shows, then 200: the gate rides out both 503s and
// completes the call. TestRunWaitsThroughADispatch503BeforeServing (daemon_test.go) proves the
// same thing through the actual daemon.Run() wiring, not just these pieces in isolation.
func TestDispatchReconcileOutcomeRidesOutA503TwiceThenSucceeds(t *testing.T) {
	fastReadiness(t, bootprobe.Retry{Initial: time.Millisecond, Max: 4 * time.Millisecond})

	var requests atomic.Int64
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if requests.Add(1) <= 2 {
			w.WriteHeader(http.StatusServiceUnavailable)
			fmt.Fprint(w, "<html><head><title>503 Service Temporarily Unavailable</title></head>"+
				"<body><center><h1>503 Service Temporarily Unavailable</h1></center></body></html>")
			return
		}
		w.Header().Set("Content-Type", "application/json")
		w.Write([]byte("[]"))
	}))
	defer server.Close()

	client := dispatch.New(server.URL, "test-token")
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
	defer cancel()
	err := bootprobe.Run(ctx, "list Dispatch issues for admission", readinessRetry, quietLogger(), func(ctx context.Context) bootprobe.Outcome {
		_, err := client.ListIssues(ctx, "ACME", nil)
		return dispatchReconcileOutcome(err)
	})
	if err != nil {
		t.Fatalf("bootprobe.Run returned %v; two 503s must be ridden out, not a boot refusal", err)
	}
	if got := requests.Load(); got != 3 {
		t.Fatalf("Dispatch saw %d requests, want exactly 3 (two 503s then the 200 that completed the call)", got)
	}
}

// A genuine Dispatch refusal — a 404 with its own error code, the project or route named is not
// there — is returned as a refusal at once, never retried: dispatchReconcileOutcome's two
// predicates (dispatch.Unreachable, natsauth.Unreachable) both answer false for it, and
// bootprobe.Run returns on the first Outcome.Refusal.
func TestDispatchReconcileOutcomeRefusesAtOnceOnAGenuineRefusal(t *testing.T) {
	fastReadiness(t, bootprobe.Retry{Initial: time.Minute, Max: time.Minute}) // a retry here would hang the test.

	var requests atomic.Int64
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		requests.Add(1)
		w.WriteHeader(http.StatusNotFound)
		w.Header().Set("Content-Type", "application/json")
		fmt.Fprint(w, `{"code":"NOT_FOUND","error":"no such project"}`)
	}))
	defer server.Close()

	client := dispatch.New(server.URL, "test-token")
	ctx, cancel := context.WithTimeout(context.Background(), time.Second)
	defer cancel()
	err := bootprobe.Run(ctx, "list Dispatch issues for admission", readinessRetry, quietLogger(), func(ctx context.Context) bootprobe.Outcome {
		_, err := client.ListIssues(ctx, "ACME", nil)
		return dispatchReconcileOutcome(err)
	})
	if err == nil {
		t.Fatal("bootprobe.Run passed on a genuine 404; it must refuse at once")
	}
	if n := requests.Load(); n != 1 {
		t.Fatalf("Dispatch saw %d requests, want exactly 1: a genuine refusal is never retried", n)
	}
}

// boundedAttempt gives each attempt its own deadline inside the daemon's unbounded ctx (LEGION-580,
// correctness review finding 2): a Postgres that accepts a connection and then never answers must
// not hang the whole boot, only the one attempt, which then counts as a failed attempt under
// readinessRetry like any other.
func TestBoundedAttemptCutsOffAnAttemptThatNeverReturnsAtItsOwnDeadlineNotTheOuterOne(t *testing.T) {
	never := make(chan struct{})
	t.Cleanup(func() { close(never) })

	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second) // the outer, daemon-lifetime ctx.
	defer cancel()

	deadline := 20 * time.Millisecond
	start := time.Now()
	err := boundedAttempt(ctx, deadline, func(attempt context.Context) error {
		select {
		case <-attempt.Done():
			return attempt.Err()
		case <-never:
			return nil
		}
	})
	elapsed := time.Since(start)
	if err == nil {
		t.Fatal("an attempt that never returns on its own must be cut off by its own deadline")
	}
	if elapsed > deadline+time.Second {
		t.Fatalf("the attempt ran for %s, want well under its %s deadline", elapsed, deadline)
	}
	if ctx.Err() != nil {
		t.Fatalf("the outer ctx ended (%v); only the attempt's own deadline should have fired", ctx.Err())
	}
}
