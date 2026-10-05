package daemon

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"log/slog"
	"net"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/nats-io/nats.go"

	"github.com/sjawhar/legion/daemon/internal/bootprobe"
	"github.com/sjawhar/legion/daemon/internal/dispatch"
	"github.com/sjawhar/legion/daemon/internal/store"
)

// fastReadiness overrides readinessRetry and readinessLogInterval for a test's lifetime, restored
// on cleanup, so a gate that would otherwise wait up to a minute between attempts costs the suite
// nothing.
func fastReadiness(t *testing.T, retry bootprobe.Retry, logInterval time.Duration) {
	t.Helper()
	previousRetry, previousLogInterval := readinessRetry, readinessLogInterval
	readinessRetry, readinessLogInterval = retry, logInterval
	t.Cleanup(func() { readinessRetry, readinessLogInterval = previousRetry, previousLogInterval })
}

// RED (LEGION-580): before this fix, a boot against a NATS address nothing answered on exited at
// once ("legion start: connect Envoy NATS: dial tcp …: i/o timeout"), the first of the 71
// identical crashes the 2026-10-01 06:33–06:52 episode shows. Here a TCP listener opens late — the
// address refuses every connection until it does — and awaitReady must keep retrying under
// dependencyUnavailable's classification of a dial failure, never returning until the listener is
// there, and never giving up before it is.
func TestAwaitReadyKeepsRetryingADialAnAddressRefusesUntilItsListenerOpensLate(t *testing.T) {
	fastReadiness(t, bootprobe.Retry{Initial: 2 * time.Millisecond, Max: 8 * time.Millisecond}, time.Millisecond)

	reserve, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatalf("reserve a port: %v", err)
	}
	addr := reserve.Addr().String()
	reserve.Close() // nothing answers here until the listener below opens.

	var attempts atomic.Int64
	opened := make(chan struct{})
	go func() {
		time.Sleep(30 * time.Millisecond) // several refused dials happen first.
		listener, err := net.Listen("tcp", addr)
		if err != nil {
			t.Errorf("open the late listener on %s: %v", addr, err)
			return
		}
		close(opened)
		defer listener.Close()
		conn, err := listener.Accept()
		if err == nil {
			conn.Close()
		}
	}()

	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	dial := func(ctx context.Context) error {
		attempts.Add(1)
		conn, err := (&net.Dialer{Timeout: 200 * time.Millisecond}).DialContext(ctx, "tcp", addr)
		if err != nil {
			return err
		}
		return conn.Close()
	}

	if err := awaitReady(ctx, "connect Envoy NATS", quietLogger(), dependencyUnavailable, dial); err != nil {
		t.Fatalf("awaitReady returned %v; a late-opening listener must eventually satisfy it, not exit", err)
	}
	select {
	case <-opened:
	default:
		t.Fatal("awaitReady passed before the listener ever opened, so it proves nothing about retrying")
	}
	if n := attempts.Load(); n < 2 {
		t.Fatalf("attempts = %d, want at least 2: the gate must have retried the refused dial, not succeeded on the first try", n)
	}
}

// RED (LEGION-580): the 2026-10-04 02:47–02:53 episode's 25 crashes are this exact shape —
// "legion start: list Dispatch issues for admission: <html>…503 Service Temporarily
// Unavailable…</html>" — the boot's one ListIssues call during reconcile hitting Dispatch mid
// outage. Here a real dispatch.HTTPClient answers 503 twice, exactly as that raw HTML body did
// (no JSON error envelope, so dispatch.Error carries an empty Code), then 200, and awaitReady must
// complete the call once it does, having retried the two 503s under dependencyUnavailable.
func TestAwaitReadyCompletesADispatchCallThatAnswers503TwiceThenSucceeds(t *testing.T) {
	fastReadiness(t, bootprobe.Retry{Initial: time.Millisecond, Max: 4 * time.Millisecond}, time.Millisecond)

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
	err := awaitReady(ctx, "list Dispatch issues for admission", quietLogger(), dependencyUnavailable, func(ctx context.Context) error {
		_, err := client.ListIssues(ctx, "ACME", nil)
		return err
	})
	if err != nil {
		t.Fatalf("awaitReady returned %v; two 503s must be ridden out, not a boot refusal", err)
	}
	if got := requests.Load(); got != 3 {
		t.Fatalf("Dispatch saw %d requests, want exactly 3 (two 503s then the 200 that completed the call)", got)
	}
}

// RED (LEGION-580): a bad DSN is store.Open's ParseConfigError, no network attempted at all —
// exactly the misconfiguration the spec says stays a loud exit. Run against dependencyUnavailable
// through awaitReady proves the gate never retries it and returns at once, matching the existing
// TestRunRefusesAnUnreachablePostgresByHostAndNotByPassword's own immediate refusal.
func TestAwaitReadyRefusesABadDSNAtOnceWithNoRetry(t *testing.T) {
	fastReadiness(t, bootprobe.Retry{Initial: time.Minute, Max: time.Minute}, time.Minute) // a retry here would hang the test.

	var attempts atomic.Int64
	ctx, cancel := context.WithTimeout(context.Background(), time.Second)
	defer cancel()
	err := awaitReady(ctx, "connect to Postgres", quietLogger(), dependencyUnavailable, func(context.Context) error {
		attempts.Add(1)
		return pgBadDSNError(t)
	})
	if err == nil {
		t.Fatal("awaitReady passed on a bad DSN; it must refuse at once")
	}
	if n := attempts.Load(); n != 1 {
		t.Fatalf("attempts = %d, want exactly 1: a misconfiguration is refused on the first attempt, never retried", n)
	}
	if ctx.Err() != nil {
		t.Fatalf("awaitReady took its whole budget (%v) rather than refusing at once", ctx.Err())
	}
}

// pgBadDSNError is store.Open's own refusal for a DSN pgx cannot parse, used here only to prove
// dependencyUnavailable classifies it a refusal; store_test.go covers store.Open's own behavior.
func pgBadDSNError(t *testing.T) error {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), time.Second)
	defer cancel()
	_, err := store.Open(ctx, "not a valid dsn at all://:::")
	if err == nil {
		t.Fatal("a malformed DSN must fail to open")
	}
	return err
}

// The schedule itself — one second doubling to one minute, held there — is bootprobe.Delay's own
// contract and is proven once in internal/bootprobe's TestDelayDoublesThenHoldsAtItsCap; this
// checks awaitReady actually uses readinessRetry via bootprobe.Delay by asserting the wait between
// two failed attempts grows, not that it stays flat.
func TestAwaitReadyWaitsLongerAfterEachFailedAttempt(t *testing.T) {
	fastReadiness(t, bootprobe.Retry{Initial: 5 * time.Millisecond, Max: 200 * time.Millisecond}, time.Millisecond)

	var timestamps []time.Time
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
	defer cancel()
	attempt := 0
	_ = awaitReady(ctx, "test dependency", quietLogger(), dependencyUnavailable, func(context.Context) error {
		timestamps = append(timestamps, time.Now())
		attempt++
		if attempt >= 4 {
			return nil
		}
		return &net.OpError{Op: "dial", Err: errors.New("connection refused")}
	})
	if len(timestamps) != 4 {
		t.Fatalf("got %d attempts, want 4", len(timestamps))
	}
	first := timestamps[1].Sub(timestamps[0])
	second := timestamps[2].Sub(timestamps[1])
	if second < first {
		t.Fatalf("the wait before attempt 3 (%s) was not longer than before attempt 2 (%s); the backoff is not growing", second, first)
	}
}

// awaitReady logs at most once per readinessLogInterval, never once per attempt: doubling from a
// tiny initial delay still means several attempts land inside one log interval, and only the
// first of them may be logged.
func TestAwaitReadyLogsAtMostOncePerInterval(t *testing.T) {
	fastReadiness(t, bootprobe.Retry{Initial: time.Millisecond, Max: 2 * time.Millisecond}, 50*time.Millisecond)

	var logged bytes.Buffer
	log := slog.New(slog.NewTextHandler(&logged, nil))
	ctx, cancel := context.WithTimeout(context.Background(), 200*time.Millisecond)
	defer cancel()
	attempts := 0
	err := awaitReady(ctx, "test dependency", log, dependencyUnavailable, func(context.Context) error {
		attempts++
		return &net.OpError{Op: "dial", Err: errors.New("connection refused")}
	})
	if err == nil {
		t.Fatal("awaitReady passed; it should have run out its ctx and returned an error")
	}
	if attempts < 10 {
		t.Fatalf("only %d attempts ran in 200ms at a 1-2ms backoff; the test cannot tell logging apart from attempts", attempts)
	}
	lines := strings.Count(logged.String(), "is not ready yet")
	if lines < 1 {
		t.Fatal("awaitReady never logged that it was waiting")
	}
	if lines >= attempts {
		t.Fatalf("logged %d times across %d attempts; it must log far less often than it retries", lines, attempts)
	}
}

// dependencyUnavailable's table: every shape the audit found (NATS not up yet, Dispatch's 503
// with no JSON body) answers true; a genuine Dispatch refusal, and anything not network-shaped at
// all, answers false — the misconfiguration a wait cannot fix.
func TestDependencyUnavailableClassifiesEachErrorShape(t *testing.T) {
	for _, testCase := range []struct {
		name string
		err  error
		want bool
	}{
		{"a Dispatch 503 with no JSON body, exactly the raw HTML episode 3 crashed on",
			&dispatch.Error{Status: 503, Message: "<html>503 Service Temporarily Unavailable</html>"}, true},
		{"a Dispatch 401, which Dispatch's own PermanentRefusal rides out rather than calls permanent",
			&dispatch.Error{Status: 401, Code: "UNAUTHORIZED", Message: "bad token"}, true},
		{"a genuine Dispatch 404 with its own error code: the project or route named is not there",
			&dispatch.Error{Status: 404, Code: "NOT_FOUND", Message: "no such project"}, false},
		{"NATS's own no-servers-available error", nats.ErrNoServers, true},
		{"a dial the network refused, exactly episode 1's shape",
			&net.OpError{Op: "dial", Net: "tcp", Err: errors.New("connection refused")}, true},
		{"a dial that timed out, exactly the dial tcp …: i/o timeout crash text",
			&net.OpError{Op: "dial", Net: "tcp", Err: errTimeout{}}, true},
		{"a plain error with no network or Dispatch shape at all", errors.New("schema mismatch: column removed"), false},
	} {
		t.Run(testCase.name, func(t *testing.T) {
			if got := dependencyUnavailable(testCase.err); got != testCase.want {
				t.Fatalf("dependencyUnavailable(%v) = %t, want %t", testCase.err, got, testCase.want)
			}
		})
	}
}

// errTimeout is a net.Error that times out, for an *net.OpError wrapping one.
type errTimeout struct{}

func (errTimeout) Error() string   { return "i/o timeout" }
func (errTimeout) Timeout() bool   { return true }
func (errTimeout) Temporary() bool { return true }

var _ net.Error = errTimeout{}
