package delivery

// A measurement, not a check: how long the intake takes to handle a burst of merged pull requests
// when every GitHub call takes 200 ms. It uses only what the package's tests had before the intake
// ran its messages per key, so the same file measures the build before that change and after it.

import (
	"context"
	"fmt"
	"net/http"
	"os"
	"reflect"
	"strconv"
	"testing"
	"time"

	"github.com/sjawhar/envoy/internal/bus"
	"github.com/sjawhar/envoy/internal/dispatch/events"
	"github.com/sjawhar/envoy/internal/testnats"
)

// burstGitHubLatency is what the fake GitHub adds to every call it answers.
const burstGitHubLatency = 200 * time.Millisecond

// withLatency answers every request through next once latency has passed.
func withLatency(latency time.Duration, next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		time.Sleep(latency)
		next.ServeHTTP(w, r)
	})
}

// startIntakesForBurst calls StartIntakes with the arguments every build's StartIntakes takes
// first, and the zero value of any it takes after them (the settings wake, which leaves the
// settings read once), so the file compiles against either signature.
func startIntakesForBurst(t *testing.T, ctx context.Context, connect func(string) (*bus.Client, error), args ...any) func() {
	t.Helper()
	start := reflect.ValueOf(StartIntakes)
	in := []reflect.Value{reflect.ValueOf(ctx), reflect.ValueOf(connect)}
	for _, arg := range args {
		in = append(in, reflect.ValueOf(arg))
	}
	for i := len(in); i < start.Type().NumIn(); i++ {
		in = append(in, reflect.Zero(start.Type().In(i)))
	}
	out := start.Call(in)
	if err, _ := out[1].Interface().(error); err != nil {
		t.Fatalf("start the intakes: %v", err)
	}
	return out[0].Interface().(func())
}

// TestIntakeBurstOfMergedPullRequests publishes 20 merged pull requests, each its own, and logs how
// long the intake took to acknowledge them all with 200 ms added to every GitHub call. Run with
// DISPATCH_INTAKE_BURST=1.
func TestIntakeBurstOfMergedPullRequests(t *testing.T) {
	if os.Getenv("DISPATCH_INTAKE_BURST") != "1" {
		t.Skip("a measurement: set DISPATCH_INTAKE_BURST=1 to run it")
	}
	const burst = 20
	pool, ctx := deliveryTestPool(t)
	seedDeliverySettings(t, ctx, pool)

	fake := newFakeGitHub(t)
	fake.handle("GET /repos/acme/widgets/pulls/{number}", func(w http.ResponseWriter, r *http.Request) {
		number, err := strconv.Atoi(r.PathValue("number"))
		if err != nil {
			t.Errorf("parse pull request number %q: %v", r.PathValue("number"), err)
		}
		mustEncode(t, w, map[string]any{
			"number": number, "title": fmt.Sprintf("feat: widget %d", number), "html_url": fmt.Sprintf("https://github.com/acme/widgets/pull/%d", number),
			"user": map[string]any{"login": "octocat"}, "labels": []any{map[string]any{"name": "non-task"}},
			"created_at": "2024-01-01T00:00:00Z", "merged_at": "2024-01-01T02:00:00Z",
			"merge_commit_sha": fmt.Sprintf("abc%d", number), "additions": 10, "deletions": 2, "body": "",
		})
	})
	fake.handle("GET /repos/acme/widgets/pulls/{number}/commits", func(w http.ResponseWriter, r *http.Request) {
		mustEncode(t, w, []any{map[string]any{"commit": map[string]any{
			"message": "feat: a widget", "author": map[string]any{"date": "2024-01-01T00:00:00Z"},
		}}})
	})
	// Every route above and the fake's own (installation, token) answer after the latency.
	routes := fake.mux
	fake.mux = http.NewServeMux()
	fake.mux.Handle("/", withLatency(burstGitHubLatency, routes))
	github := fake.newTestClient()

	natsURL := testnats.URL(t)
	publisher := intakeTestClient(t)
	runCtx, cancel := context.WithCancel(ctx)
	stop := startIntakesForBurst(t, runCtx, func(name string) (*bus.Client, error) {
		return bus.Connect([]string{natsURL}, bus.WithName(name))
	}, pool, github, events.NewBroker())
	t.Cleanup(func() {
		cancel()
		stop()
	})
	awaitBoundDurable(t, publisher)

	started := time.Now()
	for number := 1; number <= burst; number++ {
		publishPullRequestEnvelope(t, publisher, "acme", "widgets", number, map[string]string{
			"kind": "pr", "action": "closed", "merged": "true", "repo": "acme/widgets",
			"number": fmt.Sprint(number), "author": "octocat",
		})
	}
	awaitAcknowledged(t, publisher)
	elapsed := time.Since(started)

	var stored int
	if err := pool.QueryRow(ctx, "select count(*) from delivery_pull_requests").Scan(&stored); err != nil {
		t.Fatalf("count pull requests: %v", err)
	}
	if stored != burst {
		t.Fatalf("stored %d pull requests, want %d", stored, burst)
	}
	t.Logf("%d merged pull requests acknowledged in %s with %s added to every GitHub call", burst, elapsed.Round(time.Millisecond), burstGitHubLatency)
}
