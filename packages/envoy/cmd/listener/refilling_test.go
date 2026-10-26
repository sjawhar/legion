package main

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"reflect"
	"slices"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"
	"unsafe"

	natsgo "github.com/nats-io/nats.go"
	"github.com/nats-io/nats.go/jetstream"
	"github.com/sjawhar/envoy/internal/bus"
	"github.com/sjawhar/envoy/internal/contracts"
	"github.com/sjawhar/envoy/internal/kvwatch"
	"github.com/sjawhar/envoy/internal/logging"
	"github.com/sjawhar/envoy/internal/metrics"
	"github.com/sjawhar/envoy/internal/session"
	"github.com/sjawhar/envoy/internal/store"
	"github.com/sjawhar/envoy/internal/testnats"
)

// staleBound is the kvwatch stale bound the listener's session and interest copies run with: their
// buckets' MaxAge is 5 minutes or none, so the 2-minute default applies.
const staleBound = 2 * time.Minute

// assertListRefilling fails the test unless response is the list routes' refilling answer: 503,
// Retry-After: 5, and the refilling code beside its message.
func assertListRefilling(t *testing.T, response *http.Response) {
	t.Helper()
	body, err := io.ReadAll(response.Body)
	if err != nil {
		t.Fatalf("read the answer: %v", err)
	}
	if response.StatusCode != http.StatusServiceUnavailable {
		t.Fatalf("status %d %s, want 503 refilling", response.StatusCode, body)
	}
	if retryAfter := response.Header.Get("Retry-After"); retryAfter != "5" {
		t.Fatalf("Retry-After = %q, want \"5\" (body %s)", retryAfter, body)
	}
	if contentType := response.Header.Get("Content-Type"); contentType != "application/json" {
		t.Fatalf("Content-Type = %q, want application/json", contentType)
	}
	var answer map[string]any
	if err := json.Unmarshal(body, &answer); err != nil {
		t.Fatalf("decode the answer %s: %v", body, err)
	}
	want := map[string]any{"error": "envoy: the agent list is refilling; try again in a moment", "code": "refilling"}
	if !reflect.DeepEqual(answer, want) {
		t.Fatalf("body %s, want %v", body, want)
	}
}

// listenerClock is the injected clock the rig's watchers measure a stale episode on.
type listenerClock struct {
	mu  sync.Mutex
	now time.Time
}

func (c *listenerClock) Now() time.Time {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.now
}

func (c *listenerClock) Advance(d time.Duration) {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.now = c.now.Add(d)
}

// heldScanKV is a bucket's jetstream handle whose watch delivers every record but holds back the
// nil that ends its scan until release is closed, so the read it begins stays under way.
type heldScanKV struct {
	jetstream.KeyValue
	release <-chan struct{}
}

func (kv heldScanKV) Watch(ctx context.Context, keys string, opts ...jetstream.WatchOpt) (jetstream.KeyWatcher, error) {
	watcher, err := kv.KeyValue.Watch(ctx, keys, opts...)
	if err != nil {
		return nil, err
	}
	held := &heldScanWatcher{KeyWatcher: watcher, updates: make(chan jetstream.KeyValueEntry)}
	go func() {
		defer close(held.updates)
		scanning := true
		for entry := range watcher.Updates() {
			if entry == nil && scanning {
				scanning = false
				<-kv.release
			}
			held.updates <- entry
		}
	}()
	return held, nil
}

// heldScanWatcher is heldScanKV's watcher, whose updates its goroutine forwards from the real one.
type heldScanWatcher struct {
	jetstream.KeyWatcher
	updates chan jetstream.KeyValueEntry
}

func (w *heldScanWatcher) Updates() <-chan jetstream.KeyValueEntry { return w.updates }

// replaceWatcher ends the cache watcher of owner (a *session.SessionRegistry or *store.Registry)
// and puts in its place a new one named name, watching through the jetstream handle wrap makes of
// the old one's, feeding the same cache, built with options, and started: its first read is the
// copy's first. It runs before anything else reads owner. The stores take no clock or watch option
// for their watchers, so this is how a test injects one, as stallSessionReads swaps the session
// handle.
func replaceWatcher(t *testing.T, owner any, name string, wrap func(jetstream.KeyValue) jetstream.KeyValue, options ...kvwatch.Option) {
	t.Helper()
	field := reflect.ValueOf(owner).Elem().FieldByName("watcher")
	slot := reflect.NewAt(field.Type(), unsafe.Pointer(field.UnsafeAddr())).Elem()
	old := slot.Interface().(*kvwatch.Watcher)
	cacheField := reflect.ValueOf(old).Elem().FieldByName("cache")
	cache := reflect.NewAt(cacheField.Type(), unsafe.Pointer(cacheField.UnsafeAddr())).Elem().Interface().(kvwatch.Cache)
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	old.End(ctx)
	replacement := kvwatch.New(name, bus.NewKeyValue(old.KV().Legacy(), wrap(old.KV().WatchHandle()), old.KV().Streams()), cache, options...)
	slot.Set(reflect.ValueOf(replacement))
	replacement.Start()
}

// listRig is an in-process listener over its own NATS server: the interest and session stores, the
// /v1 routes behind the start gate main opens, the list gauges on /metrics, and the connection
// hooks main registers, with the stores' watchers on clock.
type listRig struct {
	server   *testnats.Server
	client   *bus.Client
	clock    *listenerClock
	registry *store.Registry
	sessions *session.SessionRegistry
	caches   []listenerCache
	handler  http.Handler
	// disconnected receives once the disconnect hooks main registers have run for a disconnect.
	disconnected chan struct{}
}

// listRigOptions shapes newListRig: release, when not nil, holds each store's first read until it
// is closed (heldScanKV), keyed "interest" and "session"; hookCaches, when not nil, is what the
// connection hooks get in place of the rig's caches.
type listRigOptions struct {
	release    map[string]<-chan struct{}
	hookCaches func([]listenerCache) []listenerCache
}

func newListRig(t *testing.T, options listRigOptions) *listRig {
	t.Helper()
	server, uri := testnats.Start(t)
	client, err := bus.ConnectOwningStream([]string{uri}, bus.WithReplicas(1))
	if err != nil {
		t.Fatalf("connect bus: %v", err)
	}
	t.Cleanup(client.Close)
	registry, err := store.Open(client.Conn, store.WithReplicas(1))
	if err != nil {
		t.Fatalf("open the interest store: %v", err)
	}
	sessions, err := session.OpenSessionRegistry(client.Conn, session.WithSessionReplicas(1))
	if err != nil {
		t.Fatalf("open the session store: %v", err)
	}
	rig := &listRig{
		server:       server,
		client:       client,
		clock:        &listenerClock{now: time.Now()},
		registry:     registry,
		sessions:     sessions,
		disconnected: make(chan struct{}, 8),
	}
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	for name, owner := range map[string]interface {
		WaitForCacheReady(context.Context) error
	}{"interest": registry, "session": sessions} {
		if err := owner.WaitForCacheReady(ctx); err != nil {
			t.Fatalf("warm the %s cache: %v", name, err)
		}
		wrap := func(kv jetstream.KeyValue) jetstream.KeyValue { return kv }
		if release, ok := options.release[name]; ok {
			wrap = func(kv jetstream.KeyValue) jetstream.KeyValue { return heldScanKV{KeyValue: kv, release: release} }
		}
		watcherName := map[string]string{"interest": "interest registry", "session": "session registry"}[name]
		replaceWatcher(t, owner, watcherName, wrap, kvwatch.WithNow(rig.clock.Now))
	}
	t.Cleanup(func() {
		registry.StopWatch()
		sessions.StopWatch()
	})
	rig.caches = listenerCaches(registry, sessions, nil)
	hookCaches := rig.caches
	if options.hookCaches != nil {
		hookCaches = options.hookCaches(rig.caches)
	}
	hookListenerKVWatchers(client, hookCaches)
	// Disconnect hooks run in the order they were added, so this one runs after main's. A hook runs
	// inside nats.go's callback, so it never blocks there.
	client.AddDisconnectHook(func() {
		select {
		case rig.disconnected <- struct{}{}:
		default:
		}
	})

	logger := logging.NewWithWriter("test", io.Discard)
	met := metrics.New()
	var served atomic.Pointer[listenerDeps]
	registerListGauges(met, &served)
	gate := newStartingGate("v1", logger)
	deps := &listenerDeps{client: client, registry: registry, sessions: sessions, caches: rig.caches, streamName: bus.Stream}
	openV1Routes(gate, deps, &served, "test-machine", logger, time.Now())
	mux := http.NewServeMux()
	mux.Handle("/metrics", met.Handler())
	mux.Handle("/v1/", gate)
	rig.handler = mux
	return rig
}

// get answers a GET of path from the rig's routes.
func (rig *listRig) get(path string) *http.Response {
	recorder := httptest.NewRecorder()
	rig.handler.ServeHTTP(recorder, httptest.NewRequest(http.MethodGet, path, nil))
	return recorder.Result()
}

// sessionIDs is the session ids a 200 GET /v1/sessions lists, failing the test on any other answer.
func (rig *listRig) sessionIDs(t *testing.T) []string {
	t.Helper()
	response := rig.get("/v1/sessions")
	var rows []sessionInfo
	if response.StatusCode != http.StatusOK || json.NewDecoder(response.Body).Decode(&rows) != nil {
		body, _ := io.ReadAll(response.Body)
		t.Fatalf("GET /v1/sessions: status %d %s, want a 200 list", response.StatusCode, body)
	}
	ids := make([]string, 0, len(rows))
	for _, row := range rows {
		ids = append(ids, row.SessionID)
	}
	return ids
}

// interestIDs is the session ids a 200 GET /v1/interests/ lists, failing the test on any other
// answer.
func (rig *listRig) interestIDs(t *testing.T) []string {
	t.Helper()
	response := rig.get("/v1/interests/")
	var rows []store.Interest
	if response.StatusCode != http.StatusOK || json.NewDecoder(response.Body).Decode(&rows) != nil {
		body, _ := io.ReadAll(response.Body)
		t.Fatalf("GET /v1/interests/: status %d %s, want a 200 list", response.StatusCode, body)
	}
	ids := make([]string, 0, len(rows))
	for _, row := range rows {
		ids = append(ids, row.SessionID)
	}
	return ids
}

// sample is gauge's sample on the rig's /metrics, and whether the scrape has one. The scrape always
// carries the gauge's TYPE line, which the test checks too.
func (rig *listRig) sample(t *testing.T, gauge string) (int64, bool) {
	t.Helper()
	body, err := io.ReadAll(rig.get("/metrics").Body)
	if err != nil {
		t.Fatalf("read /metrics: %v", err)
	}
	if !strings.Contains(string(body), "# TYPE "+gauge+" gauge\n") {
		t.Fatalf("/metrics lacks the TYPE line of %s:\n%s", gauge, body)
	}
	for _, line := range strings.Split(string(body), "\n") {
		if value, ok := strings.CutPrefix(line, gauge+" "); ok {
			n, err := strconv.ParseInt(value, 10, 64)
			if err != nil {
				t.Fatalf("/metrics sample %q: %v", line, err)
			}
			return n, true
		}
	}
	return 0, false
}

// assertNoSample fails the test when /metrics has a sample of gauge.
func (rig *listRig) assertNoSample(t *testing.T, gauge string) {
	t.Helper()
	if value, ok := rig.sample(t, gauge); ok {
		t.Fatalf("/metrics has %s %d while its list answers refilling, want no sample", gauge, value)
	}
}

// assertSample fails the test unless /metrics has gauge's sample want.
func (rig *listRig) assertSample(t *testing.T, gauge string, want int) {
	t.Helper()
	if value, ok := rig.sample(t, gauge); !ok || value != int64(want) {
		t.Fatalf("/metrics %s = %d (sample present: %v), want %d", gauge, value, ok, want)
	}
}

// assertBothListsRefilling fails the test unless both list routes answer refilling and neither
// list gauge has a sample.
func (rig *listRig) assertBothListsRefilling(t *testing.T) {
	t.Helper()
	assertListRefilling(t, rig.get("/v1/sessions"))
	assertListRefilling(t, rig.get("/v1/interests/"))
	rig.assertNoSample(t, "envoy_active_sessions")
	rig.assertNoSample(t, "envoy_active_interests")
}

// assertLists fails the test unless both lists answer 200 listing exactly ids, and both gauges
// count them.
func (rig *listRig) assertLists(t *testing.T, ids []string) {
	t.Helper()
	if got := rig.sessionIDs(t); !slices.Equal(got, ids) {
		t.Fatalf("GET /v1/sessions lists %v, want %v", got, ids)
	}
	if got := rig.interestIDs(t); !slices.Equal(got, ids) {
		t.Fatalf("GET /v1/interests/ lists %v, want %v", got, ids)
	}
	rig.assertSample(t, "envoy_active_sessions", len(ids))
	rig.assertSample(t, "envoy_active_interests", len(ids))
}

// awaitServed waits until both lists answer 200.
func (rig *listRig) awaitServed(t *testing.T, timeout time.Duration, what string) {
	t.Helper()
	waitFor(t, timeout, what, func() bool {
		return rig.get("/v1/sessions").StatusCode == http.StatusOK && rig.get("/v1/interests/").StatusCode == http.StatusOK
	})
}

// register stores each session id's session and interest through the rig's stores.
func (rig *listRig) register(t *testing.T, ids ...string) {
	t.Helper()
	for _, id := range ids {
		if err := rig.sessions.Put(id, session.SessionEntry{MachineID: "test-machine", Dir: "/test/" + id, SelfSubscribed: true}); err != nil {
			t.Fatalf("register %s's session: %v", id, err)
		}
		if _, err := rig.registry.Upsert(store.Interest{SessionID: id, MachineID: "test-machine", Dir: "/test/" + id}, []string{contracts.AgentSubject(id)}); err != nil {
			t.Fatalf("register %s's interest: %v", id, err)
		}
	}
}

// seed registers each session id (register) and waits until both lists hold exactly them.
func (rig *listRig) seed(t *testing.T, ids ...string) {
	t.Helper()
	rig.register(t, ids...)
	waitFor(t, 5*time.Second, "both lists to hold the seeded sessions", func() bool {
		if rig.get("/v1/sessions").StatusCode != http.StatusOK || rig.get("/v1/interests/").StatusCode != http.StatusOK {
			return false
		}
		return slices.Equal(rig.sessionIDs(t), ids) && slices.Equal(rig.interestIDs(t), ids)
	})
}

// awaitDisconnect waits until the disconnect hooks have run for the disconnect the test caused.
func (rig *listRig) awaitDisconnect(t *testing.T) {
	t.Helper()
	select {
	case <-rig.disconnected:
	case <-time.After(10 * time.Second):
		t.Fatal("the bus never ran its disconnect hooks")
	}
}

// newRelease returns a channel heldScanKV holds a scan's nil on, and the func that releases it,
// which the test's end calls too.
func newRelease(t *testing.T) (<-chan struct{}, func()) {
	t.Helper()
	ch := make(chan struct{})
	var once sync.Once
	release := func() { once.Do(func() { close(ch) }) }
	t.Cleanup(release)
	return ch, release
}

// Until a copy has completed its first read of its bucket, a list built from it could be missing
// any live agent, so both list routes answer the refilling 503 with Retry-After and neither gauge
// has a sample, though the copies already hold every record the reads delivered. The interest
// copy's read completing serves its own list, while the session list, whose rows join both copies,
// waits for the session copy's too; once both reads complete, both lists answer 200 with every
// record and the gauges count them.
func TestListsRefuseUntilTheirCopiesCompleteAFirstRead(t *testing.T) {
	interestHeld, releaseInterest := newRelease(t)
	sessionHeld, releaseSession := newRelease(t)
	rig := newListRig(t, listRigOptions{release: map[string]<-chan struct{}{"interest": interestHeld, "session": sessionHeld}})
	ids := []string{"ses_first_a", "ses_first_b"}
	rig.register(t, ids...)

	rig.assertBothListsRefilling(t)

	releaseInterest()
	waitFor(t, 5*time.Second, "the interest list once its copy's read completed", func() bool {
		return rig.get("/v1/interests/").StatusCode == http.StatusOK
	})
	if got := rig.interestIDs(t); !slices.Equal(got, ids) {
		t.Fatalf("GET /v1/interests/ lists %v, want %v", got, ids)
	}
	rig.assertSample(t, "envoy_active_interests", len(ids))
	assertListRefilling(t, rig.get("/v1/sessions"))
	rig.assertNoSample(t, "envoy_active_sessions")

	releaseSession()
	rig.awaitServed(t, 5*time.Second, "the session list once its copy's read completed")
	rig.assertLists(t, ids)
}

// A copy that stopped following its bucket is served for the stale bound, and past it the lists
// built from it answer refilling with no gauge sample. A stale session copy refuses the session
// list alone; a stale interest copy refuses both, since every session row joins it. A read that
// completes serves both lists again.
func TestListsRefuseAfterTheBoundWithAStaleCopy(t *testing.T) {
	for _, stale := range []string{"session", "interest"} {
		t.Run(stale+" copy", func(t *testing.T) {
			rig := newListRig(t, listRigOptions{})
			ids := []string{"ses_stale_a", "ses_stale_b"}
			rig.seed(t, ids...)
			var cache kvCache = rig.sessions
			if stale == "interest" {
				cache = rig.registry
			}
			cache.Disconnected()

			rig.clock.Advance(staleBound)
			rig.assertLists(t, ids)

			rig.clock.Advance(time.Second)
			assertListRefilling(t, rig.get("/v1/sessions"))
			rig.assertNoSample(t, "envoy_active_sessions")
			if stale == "session" {
				if got := rig.interestIDs(t); !slices.Equal(got, ids) {
					t.Fatalf("GET /v1/interests/ beside a stale session copy lists %v, want %v", got, ids)
				}
				rig.assertSample(t, "envoy_active_interests", len(ids))
			} else {
				assertListRefilling(t, rig.get("/v1/interests/"))
				rig.assertNoSample(t, "envoy_active_interests")
			}

			if err := cache.Rewatch(rig.client.Conn); err != nil {
				t.Fatalf("read the %s bucket again: %v", stale, err)
			}
			rig.awaitServed(t, 5*time.Second, "both lists once the "+stale+" copy read its bucket again")
			rig.assertLists(t, ids)
		})
	}
}

// A complete read of an empty bucket is a complete empty list: 200 [] from both routes, and gauges
// that read 0, the one case a 0 means no sessions.
func TestACompleteEmptyCopyListsNothingAndCountsZero(t *testing.T) {
	rig := newListRig(t, listRigOptions{})
	rig.awaitServed(t, 5*time.Second, "both lists over empty buckets")
	for _, path := range []string{"/v1/sessions", "/v1/interests/"} {
		if body, _ := io.ReadAll(rig.get(path).Body); string(body) != "[]\n" {
			t.Fatalf("GET %s over an empty bucket answered %q, want []", path, body)
		}
	}
	rig.assertLists(t, []string{})
}

// A list read answers from the copies alone: with NATS not answering (the connection stays up and
// every request waits out its deadline), GET /v1/sessions answers at once, a session the interest
// copy holds nothing for included, where a read-through of that session's interest would wait out
// its 10 s deadline. GET /v1/interests/ and the gauges read no bucket either.
func TestTheListsMakeNoNATSReadWhileNATSStalls(t *testing.T) {
	const bound = 5 * time.Second
	rig := newListRig(t, listRigOptions{})
	rig.seed(t, "ses_stall_joined")
	if err := rig.sessions.Put("ses_stall_unsubscribed", session.SessionEntry{MachineID: "test-machine", Dir: "/test/unsubscribed", SelfSubscribed: true}); err != nil {
		t.Fatalf("register a session with no interest: %v", err)
	}
	waitFor(t, 5*time.Second, "the session list to hold the session with no interest", func() bool {
		return len(rig.sessionIDs(t)) == 2
	})

	thaw := rig.server.Freeze(t)
	defer thaw()
	started := time.Now()
	response := rig.get("/v1/sessions")
	var rows []sessionInfo
	if response.StatusCode != http.StatusOK || json.NewDecoder(response.Body).Decode(&rows) != nil {
		t.Fatalf("GET /v1/sessions while NATS stalls: status %d, want a 200 list", response.StatusCode)
	}
	if elapsed := time.Since(started); elapsed > bound {
		t.Fatalf("GET /v1/sessions while NATS stalls took %s, want under %s: the list read a bucket", elapsed, bound)
	}
	topics := map[string][]string{}
	for _, row := range rows {
		topics[row.SessionID] = row.Topics
	}
	want := map[string][]string{"ses_stall_joined": {contracts.AgentSubject("ses_stall_joined")}, "ses_stall_unsubscribed": nil}
	if !reflect.DeepEqual(topics, want) {
		t.Fatalf("GET /v1/sessions while NATS stalls listed topics %v, want %v", topics, want)
	}
	if got := rig.interestIDs(t); !slices.Equal(got, []string{"ses_stall_joined"}) {
		t.Fatalf("GET /v1/interests/ while NATS stalls lists %v, want [ses_stall_joined]", got)
	}
	rig.assertSample(t, "envoy_active_sessions", 2)
	rig.assertSample(t, "envoy_active_interests", 1)
	if elapsed := time.Since(started); elapsed > bound {
		t.Fatalf("the list reads and the scrape while NATS stalls took %s, want under %s", elapsed, bound)
	}
}

// unreadRewatch is a cache whose Rewatch fails, signalling failed, as a reconnect hook's Rewatch
// fails when NATS refuses it, while the cache's own watcher keeps running.
type unreadRewatch struct {
	kvCache
	failed chan<- struct{}
}

func (c unreadRewatch) Rewatch(*natsgo.Conn) error {
	select {
	case c.failed <- struct{}{}:
	default:
	}
	return errors.New("the test refused this Rewatch")
}

// nats.go keeps a KV watcher's subscription open through an outage, so a NATS server that goes
// away ends no watcher, and only the bus's disconnect hook says the copies stopped following their
// buckets. Within the stale bound the lists still answer from the copies; past it, both answer
// refilling with Retry-After and no gauge sample. When the server returns, the reconnect hook reads
// every bucket again and the lists answer 200 with every session. A reconnect whose Rewatch failed
// leaves the copies needing a read, which the self-health monitor's next connected tick makes.
func TestListsRefuseAfterTheBoundWhileNATSIsDown(t *testing.T) {
	ids := []string{"ses_down_a", "ses_down_b", "ses_down_c"}
	refuseWhileDown := func(t *testing.T, rig *listRig) {
		t.Helper()
		rig.seed(t, ids...)
		rig.server.Stop(t)
		rig.awaitDisconnect(t)
		rig.clock.Advance(staleBound)
		rig.assertLists(t, ids)
		rig.clock.Advance(time.Second)
		rig.assertBothListsRefilling(t)
	}

	t.Run("the reconnect hook reads again", func(t *testing.T) {
		rig := newListRig(t, listRigOptions{})
		refuseWhileDown(t, rig)
		rig.server.Start(t)
		rig.awaitServed(t, 30*time.Second, "both lists once the reconnect hook read the buckets again")
		rig.assertLists(t, ids)
	})

	t.Run("the monitor reads what the reconnect left unread", func(t *testing.T) {
		failed := make(chan struct{}, 1)
		rig := newListRig(t, listRigOptions{hookCaches: func(caches []listenerCache) []listenerCache {
			unread := make([]listenerCache, len(caches))
			for i, c := range caches {
				unread[i] = listenerCache{name: c.name, cache: unreadRewatch{kvCache: c.cache, failed: failed}}
			}
			return unread
		}})
		refuseWhileDown(t, rig)
		rig.server.Start(t)
		select {
		case <-failed:
		case <-time.After(30 * time.Second):
			t.Fatal("the reconnect hook never ran its Rewatch")
		}
		var probeErr error
		waitFor(t, 10*time.Second, "the self-health probe to report a copy that needs a read", func() bool {
			probeErr = checkSelfHealth(rig.caches, nil)
			return errors.Is(probeErr, errCacheNeedsRead)
		})
		for _, c := range rig.caches {
			if err := c.cache.WatchErr(); err != nil {
				t.Fatalf("the %s watcher ended (%v): the copy must need a read with its watcher running", c.name, err)
			}
		}
		if !isUnrecoverableSelfHealthFailure(probeErr, rig.client, rig.caches) {
			t.Fatalf("the monitor reads %v as transient, want a rebuild", probeErr)
		}
		rig.assertBothListsRefilling(t)

		monitorUntilRecovered(t, "the copies the reconnect left unread", 10*time.Second, 10*time.Millisecond,
			func() error { return checkSelfHealth(rig.caches, nil) },
			func(err error) bool { return isUnrecoverableSelfHealthFailure(err, rig.client, rig.caches) },
			rig.client.Connected,
			func() error {
				return rebuildListenerDependencies(context.Background(), rig.client, rig.caches, nil, "", nil)
			},
		)
		rig.awaitServed(t, 10*time.Second, "both lists once the monitor read the buckets again")
		rig.assertLists(t, ids)
	})
}
