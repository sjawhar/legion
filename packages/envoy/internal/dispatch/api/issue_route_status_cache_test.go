package api

import (
	"context"
	"net/http"
	"net/http/httptest"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/sjawhar/envoy/internal/dispatch/envoy"
)

// detailReach reads one issue's detail and answers its reach as reachText writes it.
func detailReach(t *testing.T, handler http.Handler, key string) string {
	t.Helper()
	response := dispatchRequest(t, handler, http.MethodGet, "/api/v1/issues/"+key, nil, "alice")
	if response.Code != http.StatusOK {
		t.Fatalf("read %s: status=%d body=%s", key, response.Code, response.Body.String())
	}
	issue := decodeBody[routedIssue](t, response)
	return reachText(issue.RouteStatus, issue.RouteHolder)
}

// sreReach judges the route role:sre against roster, as reachText writes it.
func sreReach(roster routeRoster) string {
	route := "role:sre"
	reach := roster.reach(&route)
	return reachText(reach.RouteStatus, reach.RouteHolder)
}

// heldRosterServer is a server register builds whose listener's GET /v1/sessions lists ses-sre
// holding sre, but only once release is called: until then every call waits, so a test can hold
// one fetch open while other reads arrive. calls counts the requests that reached it. The fetch's
// bound, the listener client's window and the TTL are each a minute, so however long a loaded
// machine takes to release the fetch, it reads no timeout as unknown and no expiry calls again.
func heldRosterServer(t *testing.T) (served *server, calls *atomic.Int32, release func()) {
	t.Helper()
	calls = &atomic.Int32{}
	gate := make(chan struct{})
	listener := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/v1/sessions" {
			http.NotFound(w, r)
			return
		}
		calls.Add(1)
		<-gate
		_, _ = w.Write([]byte(`[{"session_id":"ses-sre","title":"worker","roles":["sre"]}]`))
	}))
	var once sync.Once
	release = func() { once.Do(func() { close(gate) }) }
	// Release before the close, which waits for every request in flight to finish.
	t.Cleanup(listener.Close)
	t.Cleanup(release)
	served = register(http.NewServeMux(), Deps{Envoy: envoy.New(listener.URL, envoy.WithTimeout(time.Minute))})
	served.rosters.ttl, served.rosters.timeout = time.Minute, time.Minute
	return served, calls, release
}

// awaitCalls waits until the listener has seen want calls, failing after a few seconds.
func awaitCalls(t *testing.T, calls *atomic.Int32, want int32) {
	t.Helper()
	deadline := time.Now().Add(5 * time.Second)
	for calls.Load() < want {
		if time.Now().After(deadline) {
			t.Fatalf("the listener saw %d calls, want %d", calls.Load(), want)
		}
		time.Sleep(time.Millisecond)
	}
}

// The deployment's TTLs are the ones the route_status comment promises: an answer serves five
// seconds, a listener that did not answer is left alone for one, and a fetch is bounded by
// routeRosterTimeout, no test's override. It reads the server register mounts, the only one a
// deployment runs.
func TestTheDeploymentsRosterCacheKeepsAnAnswerFiveSecondsAndASilenceOne(t *testing.T) {
	rosters := &register(http.NewServeMux(), Deps{}).rosters
	if rosters.ttl != 5*time.Second || rosters.silenceTTL != time.Second {
		t.Fatalf("the server a deployment runs keeps an answer %s and a silence %s, want 5s and 1s", rosters.ttl, rosters.silenceTTL)
	}
	if rosters.timeout != 0 || routeRosterTimeout != 2*time.Second {
		t.Fatalf("the server a deployment runs bounds a fetch by %s over routeRosterTimeout %s, want no override of 2s", rosters.timeout, routeRosterTimeout)
	}
}

// Two reads inside the TTL make one listener call, and the second reads the first's answer
// even though the listener's own answer has changed since: a route_status as old as the TTL is
// the contract.
func TestTwoIssueReadsWithinTheTTLMakeOneListenerCall(t *testing.T) {
	roster := &fakeRoster{}
	roster.set("ses-sre", "sre")
	handler, _, served := cachedRouteStatusServer(t, roster)
	// A minute, not the deployment's five seconds, so a slow machine's second read stays inside it;
	// the deployment's own value is TestTheDeploymentsRosterCacheKeepsAnAnswerFiveSecondsAndASilenceOne's.
	served.rosters.ttl = time.Minute
	key := routedIssueKey(t, handler, "role:sre")
	before := roster.lookups.Load()

	if got := detailReach(t, handler, key); got != "live ses-sre" {
		t.Fatalf("first read: %s, want live ses-sre", got)
	}
	roster.end("ses-sre")
	if got := detailReach(t, handler, key); got != "live ses-sre" {
		t.Fatalf("second read inside the TTL: %s, want the cached live ses-sre", got)
	}
	if calls := roster.lookups.Load() - before; calls != 1 {
		t.Fatalf("two reads inside the TTL made %d listener calls, want 1", calls)
	}
}

// A read after the TTL asks the listener again and reads its new answer. The silence TTL is
// left at a second, so a cache that kept an answered roster for the silence TTL would fail too.
func TestAnIssueReadAfterTheTTLAsksTheListenerAgain(t *testing.T) {
	roster := &fakeRoster{}
	roster.set("ses-sre", "sre")
	handler, _, served := cachedRouteStatusServer(t, roster)
	served.rosters.ttl = 50 * time.Millisecond
	key := routedIssueKey(t, handler, "role:sre")
	before := roster.lookups.Load()

	if got := detailReach(t, handler, key); got != "live ses-sre" {
		t.Fatalf("first read: %s, want live ses-sre", got)
	}
	roster.end("ses-sre")
	time.Sleep(100 * time.Millisecond)
	if got := detailReach(t, handler, key); got != "no_holder null" {
		t.Fatalf("read after the TTL: %s, want no_holder null", got)
	}
	if calls := roster.lookups.Load() - before; calls != 2 {
		t.Fatalf("two reads a TTL apart made %d listener calls, want 2", calls)
	}
}

// A listener that fails is asked once within the silence window, and every read in it answers
// as an unreachable listener always has: unknown on the detail, 503 for a judged filter. The
// answered-roster TTL is zero here, so a cache that held a failure for that TTL would ask again.
func TestAFailingListenerIsAskedOnceWithinTheSilenceWindow(t *testing.T) {
	roster := &fakeRoster{}
	roster.down.Store(true)
	handler, _, served := cachedRouteStatusServer(t, roster)
	// A minute, not the deployment's second, so a slow machine's three reads stay inside it;
	// the deployment's own value is TestTheDeploymentsRosterCacheKeepsAnAnswerFiveSecondsAndASilenceOne's.
	served.rosters.ttl, served.rosters.silenceTTL = 0, time.Minute
	key := routedIssueKey(t, handler, "role:sre")
	before := roster.lookups.Load()

	for read := 1; read <= 2; read++ {
		if got := detailReach(t, handler, key); got != "unknown null" {
			t.Fatalf("read %d with the listener down: %s, want unknown null", read, got)
		}
	}
	blind := dispatchRequest(t, handler, http.MethodGet, "/api/v1/issues?project=TEST&route_status=no_holder", nil, "alice")
	if blind.Code != http.StatusServiceUnavailable {
		t.Fatalf("route_status=no_holder with the listener down: status=%d body=%s, want 503", blind.Code, blind.Body.String())
	}
	if calls := roster.lookups.Load() - before; calls != 1 {
		t.Fatalf("three reads with the listener down made %d listener calls, want 1", calls)
	}
}

// A cached failure lasts only its silence window: once it has passed, a read asks the listener
// again and reads its answer. The answered TTL is a minute, so a cache that held a failure for
// that TTL, or for ever, would still read unknown.
func TestAListenerThatFailedIsAskedAgainOnceItsSilenceWindowPasses(t *testing.T) {
	roster := &fakeRoster{}
	roster.down.Store(true)
	handler, _, served := cachedRouteStatusServer(t, roster)
	served.rosters.ttl, served.rosters.silenceTTL = time.Minute, 50*time.Millisecond
	key := routedIssueKey(t, handler, "role:sre")
	before := roster.lookups.Load()

	if got := detailReach(t, handler, key); got != "unknown null" {
		t.Fatalf("read with the listener down: %s, want unknown null", got)
	}
	roster.down.Store(false)
	roster.set("ses-sre", "sre")
	time.Sleep(100 * time.Millisecond)
	if got := detailReach(t, handler, key); got != "live ses-sre" {
		t.Fatalf("read after the silence window: %s, want live ses-sre", got)
	}
	if calls := roster.lookups.Load() - before; calls != 2 {
		t.Fatalf("two reads a silence window apart made %d listener calls, want 2", calls)
	}
}

// A listener that does not answer holds a fetch only for the fetch's own bound, not the client's
// window (a minute here), and the reads after it read unknown at once without asking again. The
// bound is half a second, so even a loaded machine's request reaches the listener inside it.
func TestAListenerThatDoesNotAnswerHoldsAReadOnlyForTheFetchBound(t *testing.T) {
	served, calls, _ := heldRosterServer(t)
	served.rosters.timeout, served.rosters.silenceTTL = 500*time.Millisecond, time.Minute

	started := time.Now()
	if roster := served.readRouteRoster(context.Background()); roster.answered {
		t.Fatal("a read answered a roster though the listener never answered")
	}
	if waited := time.Since(started); waited > 10*time.Second {
		t.Fatalf("a read waited %s on a listener that does not answer, want about the 500ms fetch bound", waited)
	}
	first := calls.Load()
	if first != 1 {
		t.Fatalf("the first read made %d listener calls, want 1", first)
	}
	if roster := served.readRouteRoster(context.Background()); roster.answered {
		t.Fatal("a read inside the silence window answered a roster")
	}
	if got := calls.Load(); got != first {
		t.Fatalf("a read inside the silence window asked the listener again: %d calls, want %d", got, first)
	}
}

// Reads that miss a cold cache together share one listener call: each waits for the one fetch
// rather than starting its own.
func TestConcurrentReadsOnAColdCacheMakeOneListenerCall(t *testing.T) {
	served, calls, release := heldRosterServer(t)
	const readers = 16
	results := make([]routeRoster, readers)
	var started, done sync.WaitGroup
	started.Add(readers)
	done.Add(readers)
	for index := range readers {
		go func() {
			defer done.Done()
			started.Done()
			results[index] = served.readRouteRoster(context.Background())
		}()
	}
	started.Wait()
	awaitCalls(t, calls, 1)
	// Every reader has had time to reach the listener if it were going to: a cache without a
	// shared fetch shows more than one call here.
	time.Sleep(200 * time.Millisecond)
	release()
	done.Wait()
	if got := calls.Load(); got != 1 {
		t.Fatalf("%d concurrent reads on a cold cache made %d listener calls, want 1", readers, got)
	}
	for index, roster := range results {
		if got := sreReach(roster); got != "live ses-sre" {
			t.Fatalf("reader %d read %s, want the shared answer live ses-sre", index, got)
		}
	}
}

// A read whose request ends while it waits on the shared fetch returns at once, reading the
// route as unknown, whether it started that fetch or joined it; the fetch runs on for the other
// readers, and its answer is the one the cache keeps.
func TestAReadWhoseRequestEndsWhileItWaitsReturnsPromptly(t *testing.T) {
	served, calls, release := heldRosterServer(t)
	read := func(ctx context.Context) <-chan routeRoster {
		answer := make(chan routeRoster, 1)
		go func() { answer <- served.readRouteRoster(ctx) }()
		return answer
	}
	leaderCtx, cancelLeader := context.WithCancel(context.Background())
	leader := read(leaderCtx)
	awaitCalls(t, calls, 1)
	joinerCtx, cancelJoiner := context.WithCancel(context.Background())
	joiner := read(joinerCtx)
	patient := read(context.Background())
	time.Sleep(50 * time.Millisecond)

	cancelLeader()
	cancelJoiner()
	for name, answer := range map[string]<-chan routeRoster{"the read that started the fetch": leader, "a read that joined it": joiner} {
		select {
		case roster := <-answer:
			if roster.answered {
				t.Fatalf("%s answered a roster though the listener has not answered yet", name)
			}
		case <-time.After(time.Second):
			t.Fatalf("%s did not return within a second of its request ending", name)
		}
	}
	select {
	case <-patient:
		t.Fatal("a read whose request is still open returned before the listener answered")
	default:
	}

	release()
	select {
	case roster := <-patient:
		if got := sreReach(roster); got != "live ses-sre" {
			t.Fatalf("the read that waited read %s, want live ses-sre", got)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("the read that waited never returned")
	}
	if got := sreReach(served.readRouteRoster(context.Background())); got != "live ses-sre" {
		t.Fatalf("the cached answer reads %s, want live ses-sre: the fetch whose starter left must still fill the cache", got)
	}
	if got := calls.Load(); got != 1 {
		t.Fatalf("four reads made %d listener calls, want 1", got)
	}
}
