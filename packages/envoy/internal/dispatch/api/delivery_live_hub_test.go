package api

import (
	"context"
	"encoding/json"
	"net/http"
	"reflect"
	"slices"
	"strconv"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/sjawhar/envoy/internal/dispatch/delivery"
	"github.com/sjawhar/envoy/internal/dispatch/model"
	"github.com/sjawhar/envoy/internal/dispatch/store"
)

// hubRig is a delivery hub on the API test server: the handler a test writes through, the store
// behind it, and the hub, run on its own lifetime, which the test's cleanup ends and waits out.
type hubRig struct {
	handler  http.Handler
	database *store.Store
	deps     Deps
	hub      *deliveryHub

	mu    sync.Mutex
	notes []deliveryHubNote
}

// newHubRig starts a hub on a new test server; configure, when given, sets the hub's seams before
// anything subscribes.
func newHubRig(t *testing.T, configure func(*deliveryHub)) *hubRig {
	t.Helper()
	handler, database, deps := newTestServer(t, testServerOptions{})
	return hubOn(t, handler, database, deps, deliveryHubClock{}, configure)
}

// hubOn starts a hub on an existing test server, as a second Dispatch task's hub would.
func hubOn(t *testing.T, handler http.Handler, database *store.Store, deps Deps, clock hubClock, configure func(*deliveryHub)) *hubRig {
	t.Helper()
	rig := &hubRig{handler: handler, database: database, deps: deps}
	lifetime, cancel := context.WithCancel(context.Background())
	rig.hub = newDeliveryHub(directServer(deps), lifetime, clock)
	rig.hub.observe = func(note deliveryHubNote) {
		rig.mu.Lock()
		rig.notes = append(rig.notes, note)
		rig.mu.Unlock()
	}
	if configure != nil {
		configure(rig.hub)
	}
	t.Cleanup(func() {
		cancel()
		rig.hub.wait()
	})
	return rig
}

// mark is how many notes the hub has made, for counting the ones after it.
func (r *hubRig) mark() int {
	r.mu.Lock()
	defer r.mu.Unlock()
	return len(r.notes)
}

// count is how many notes since mark match.
func (r *hubRig) count(since int, match func(deliveryHubNote) bool) int {
	r.mu.Lock()
	defer r.mu.Unlock()
	n := 0
	for _, note := range r.notes[since:] {
		if match(note) {
			n++
		}
	}
	return n
}

// waitFor waits up to within for a note since mark that matches.
func (r *hubRig) waitFor(t *testing.T, since int, within time.Duration, what string, match func(deliveryHubNote) bool) {
	t.Helper()
	deadline := time.Now().Add(within)
	for r.count(since, match) == 0 {
		if time.Now().After(deadline) {
			t.Fatalf("no %s within %s", what, within)
		}
		time.Sleep(10 * time.Millisecond)
	}
}

func recomputeBegin(cause string) func(deliveryHubNote) bool {
	return func(note deliveryHubNote) bool {
		return note.what == "recompute begin" && (cause == "" || note.cause == cause)
	}
}

// fakeLiveConn is a socket as the hub sees it, recording every frame it is sent.
type fakeLiveConn struct {
	frames chan any
	full   atomic.Bool
}

func newFakeLiveConn() *fakeLiveConn { return &fakeLiveConn{frames: make(chan any, 256)} }

func (c *fakeLiveConn) Send(frame any) bool {
	if c.full.Load() {
		return false
	}
	select {
	case c.frames <- frame:
		return true
	default:
		return false
	}
}

// next is the next frame the conn is sent within within.
func (c *fakeLiveConn) next(t *testing.T, within time.Duration, what string) any {
	t.Helper()
	select {
	case frame := <-c.frames:
		return frame
	case <-time.After(within):
		t.Fatalf("no frame within %s, waiting for %s", within, what)
		return nil
	}
}

// none fails when the conn is sent a frame within within.
func (c *fakeLiveConn) none(t *testing.T, within time.Duration, after string) {
	t.Helper()
	select {
	case frame := <-c.frames:
		raw, _ := json.Marshal(frame)
		t.Fatalf("after %s, a frame nobody should be sent: %s", after, raw)
	case <-time.After(within):
	}
}

func (c *fakeLiveConn) snapshot(t *testing.T, within time.Duration, sub string) DeliveryLiveSnapshot {
	t.Helper()
	frame := c.next(t, within, "the snapshot of "+sub)
	snapshot, ok := frame.(DeliveryLiveSnapshot)
	if !ok || snapshot.Sub != sub {
		raw, _ := json.Marshal(frame)
		t.Fatalf("frame = %s, want the snapshot of %s", raw, sub)
	}
	return snapshot
}

func (c *fakeLiveConn) delta(t *testing.T, within time.Duration, sub string) DeliveryLiveDelta {
	t.Helper()
	frame := c.next(t, within, "a delta of "+sub)
	delta, ok := frame.(DeliveryLiveDelta)
	if !ok || delta.Sub != sub {
		raw, _ := json.Marshal(frame)
		t.Fatalf("frame = %s, want a delta of %s", raw, sub)
	}
	return delta
}

func (c *fakeLiveConn) refusal(t *testing.T, within time.Duration, sub, code string) DeliveryLiveError {
	t.Helper()
	frame := c.next(t, within, code+" for "+sub)
	refusal, ok := frame.(DeliveryLiveError)
	if !ok || refusal.Sub != sub || refusal.Code != code {
		raw, _ := json.Marshal(frame)
		t.Fatalf("frame = %s, want %s for %s", raw, code, sub)
	}
	return refusal
}

// The views the tests subscribe to.
func timelineRange(from, to time.Time, query string) DeliveryLiveView {
	f, tt := from.Format(time.RFC3339Nano), to.Format(time.RFC3339Nano)
	return DeliveryLiveView{Kind: deliveryLiveTimeline, Window: &DeliveryLiveWindow{From: &f, To: &tt}, Query: &query}
}

func measuresRange(from, to time.Time, query string) DeliveryLiveView {
	view := timelineRange(from, to, query)
	view.Kind = deliveryLiveMeasures
	return view
}

func presetView(kind string, days int, query string) DeliveryLiveView {
	return DeliveryLiveView{Kind: kind, Window: &DeliveryLiveWindow{Preset: &days}, Query: &query}
}

func livePipelineView(at *time.Time) DeliveryLiveView {
	if at == nil {
		return DeliveryLiveView{Kind: deliveryLivePipeline}
	}
	s := at.Format(time.RFC3339Nano)
	return DeliveryLiveView{Kind: deliveryLivePipeline, At: &s}
}

func subscribeFrame(sub string, view DeliveryLiveView) DeliveryLiveSubscribe {
	return DeliveryLiveSubscribe{Type: "subscribe", Sub: sub, View: view}
}

// seedHubSettings writes the delivery settings the hub tests run under.
func seedHubSettings(t *testing.T, database *store.Store) {
	t.Helper()
	if _, err := delivery.PutSettings(t.Context(), database.Pool, delivery.DeliverySettings{
		DeployRepo: "acme/widgets", DeployWorkflowPath: ".github/workflows/deploy.yml",
		ProductionJobName: "release / release", PRChecksWorkflowPath: ".github/workflows/pr-checks.yml",
		PopulationAuthors: []string{"octocat"},
	}, model.Actor{Kind: "system", ID: "test"}); err != nil {
		t.Fatalf("seed settings: %v", err)
	}
}

// hubPR is acme/widgets#n by octocat, opened two hours before it merged at merged.
func hubPR(n int, merged time.Time) delivery.DeliveryPullRequest {
	opened := merged.Add(-2 * time.Hour)
	return delivery.DeliveryPullRequest{
		Repo: "acme/widgets", Number: n, Title: "feat: change " + strconv.Itoa(n),
		URL: "https://github.com/acme/widgets/pull/" + strconv.Itoa(n), Author: "octocat",
		MergedAt: &merged, CreatedAt: &opened,
	}
}

func upsertPR(t *testing.T, database *store.Store, pr delivery.DeliveryPullRequest) {
	t.Helper()
	if err := delivery.UpsertPullRequest(context.Background(), database.Pool, pr); err != nil {
		t.Fatalf("upsert %s#%d: %v", pr.Repo, pr.Number, err)
	}
}

// upsertDeploy writes deploy run id on main with its production job, headed at head and done at done.
func upsertDeploy(t *testing.T, database *store.Store, id int64, head, done time.Time) {
	t.Helper()
	ctx := context.Background()
	success := delivery.DeliveryRunConclusionSuccess
	jobSuccess := delivery.DeliveryJobConclusionSuccess
	if err := delivery.UpsertRun(ctx, database.Pool, delivery.DeliveryRun{
		Repo: "acme/widgets", RunID: id, Kind: delivery.DeliveryRunKindDeploy, HeadSHA: "sha" + strconv.FormatInt(id, 10),
		HeadCommitAt: head, StartedAt: head, CompletedAt: &done, Conclusion: &success,
		URL: "https://github.com/acme/widgets/actions/runs/" + strconv.FormatInt(id, 10), HeadBranch: new("main"), Event: new("push"),
	}); err != nil {
		t.Fatalf("upsert deploy run %d: %v", id, err)
	}
	if err := delivery.UpsertRunJobs(ctx, database.Pool, "acme/widgets", id, []delivery.DeliveryRunJob{
		{Repo: "acme/widgets", RunID: id, Name: "release / release", StartedAt: &head, CompletedAt: &done, Conclusion: &jobSuccess},
	}, true); err != nil {
		t.Fatalf("upsert deploy run %d's job: %v", id, err)
	}
}

// getBody is the GET route's body for target.
func getBody(t *testing.T, handler http.Handler, target string) json.RawMessage {
	t.Helper()
	response := dispatchRequest(t, handler, http.MethodGet, target, nil, "alice")
	if response.Code != http.StatusOK {
		t.Fatalf("GET %s: status = %d, body = %s", target, response.Code, response.Body.String())
	}
	return json.RawMessage(response.Body.Bytes())
}

// windowTarget is the GET route of kind for the window a body answered.
func windowTarget(t *testing.T, kind string, body json.RawMessage, query string) string {
	t.Helper()
	var shape struct {
		Window delivery.DeliveryWindowView `json:"window"`
	}
	if err := json.Unmarshal(body, &shape); err != nil {
		t.Fatalf("read the body's window: %v", err)
	}
	target := "/api/v1/delivery/" + kind + "?from=" + shape.Window.From.Format(time.RFC3339Nano) + "&to=" + shape.Window.To.Format(time.RFC3339Nano)
	if query != "" {
		target += "&" + query
	}
	return target
}

// withoutComputedAt is a body decoded, without the computed_at it moves on every compute.
func withoutComputedAt(t *testing.T, body json.RawMessage) map[string]any {
	t.Helper()
	var decoded map[string]any
	if err := json.Unmarshal(body, &decoded); err != nil {
		t.Fatalf("decode body: %v", err)
	}
	delete(decoded, "computed_at")
	if measures, ok := decoded["measures"].(map[string]any); ok {
		delete(measures, "computed_at")
	}
	return decoded
}

func requireSameBody(t *testing.T, what string, got, want json.RawMessage) {
	t.Helper()
	if g, w := withoutComputedAt(t, got), withoutComputedAt(t, want); !reflect.DeepEqual(g, w) {
		t.Fatalf("%s: the live body differs from the GET route's:\nlive: %s\nGET:  %s", what, got, want)
	}
}

// requireLandsOnGET applies delta to the body held as a page applies it and requires the GET route's
// body for the delta's window.
func requireLandsOnGET(t *testing.T, handler http.Handler, kind string, held json.RawMessage, delta DeliveryLiveDelta, query string) {
	t.Helper()
	encoded := mustEncodeLive(t, kind, held)
	applied := liveApply(t, encoded, delta)
	raw, err := json.Marshal(applied.body)
	if err != nil {
		t.Fatalf("encode the applied body: %v", err)
	}
	want := mustEncodeLive(t, kind, getBody(t, handler, windowTarget(t, kind, raw, query)))
	if got, wantBody := liveComparable(t, applied), liveComparable(t, want); !reflect.DeepEqual(got, wantBody) {
		t.Fatalf("the delta does not land on the GET route's body:\napplied: %v\nGET:     %v", got, wantBody)
	}
}

func TestASubscriberGetsTheGETBodyAsItsSnapshot(t *testing.T) {
	rig := newHubRig(t, nil)
	seedHubSettings(t, rig.database)
	now := time.Now().UTC()
	fixed := time.Date(2024, 6, 3, 12, 0, 0, 0, time.UTC)
	for i, merged := range []time.Time{fixed, fixed.Add(3 * time.Hour), now.Add(-26 * time.Hour), now.Add(-2 * time.Hour)} {
		upsertPR(t, rig.database, hubPR(i+1, merged))
	}
	upsertDeploy(t, rig.database, 100, fixed.Add(time.Hour), fixed.Add(2*time.Hour))
	upsertDeploy(t, rig.database, 101, now.Add(-25*time.Hour), now.Add(-24*time.Hour))

	from, to := time.Date(2024, 6, 1, 0, 0, 0, 0, time.UTC), time.Date(2024, 6, 8, 0, 0, 0, 0, time.UTC)
	at := fixed.Add(30 * time.Hour)
	conn := newFakeLiveConn()
	for _, test := range []struct {
		sub   string
		view  DeliveryLiveView
		kind  string
		query string
		get   string
	}{
		{sub: "timeline", view: timelineRange(from, to, "author=octocat"), kind: deliveryLiveTimeline, query: "author=octocat"},
		{sub: "measures", view: measuresRange(from, to, ""), kind: deliveryLiveMeasures},
		{sub: "pipeline", view: livePipelineView(&at), get: "/api/v1/delivery/pipeline?at=" + at.Format(time.RFC3339)},
		{sub: "preset", view: presetView(deliveryLiveTimeline, 7, ""), kind: deliveryLiveTimeline},
	} {
		rig.hub.Subscribe(conn, subscribeFrame(test.sub, test.view))
		snapshot := conn.snapshot(t, 5*time.Second, test.sub)
		target := test.get
		if target == "" {
			target = windowTarget(t, test.kind, snapshot.Body, test.query)
		}
		requireSameBody(t, test.sub, snapshot.Body, getBody(t, rig.handler, target))
		if snapshot.Seq == "" || len(snapshot.Revs.Fields) == 0 {
			t.Fatalf("%s snapshot seq = %q, revs = %+v; want its change position and every revision", test.sub, snapshot.Seq, snapshot.Revs)
		}
	}
}

func TestAMergedPullRequestReachesTheSubscriberAsOneDelta(t *testing.T) {
	rig := newHubRig(t, nil)
	seedHubSettings(t, rig.database)
	now := time.Now().UTC()
	upsertPR(t, rig.database, hubPR(1, now.Add(-30*time.Hour)))
	conn := newFakeLiveConn()
	rig.hub.Subscribe(conn, subscribeFrame("p", presetView(deliveryLiveTimeline, 7, "")))
	snapshot := conn.snapshot(t, 5*time.Second, "p")

	upsertPR(t, rig.database, hubPR(2, time.Now().UTC().Add(-time.Hour)))
	delta := conn.delta(t, 3*time.Second, "p")
	set, items, mapsChanged, counts := liveSections(delta)
	if !slices.Equal(items, []string{"prs"}) || !slices.Equal(liveUpsertIDs(t, delta, "prs"), []string{"acme/widgets#2"}) || len(delta.Items["prs"].Remove) != 0 {
		t.Fatalf("delta items = %v, prs upsert %v remove %v; want acme/widgets#2 upserted alone", items, liveUpsertIDs(t, delta, "prs"), delta.Items["prs"].Remove)
	}
	if !slices.Equal(set, []string{"measures", "window"}) || len(mapsChanged) != 0 {
		t.Fatalf("delta set = %v, maps = %v; want measures and window, and no map", set, mapsChanged)
	}
	var held struct {
		FacetCounts map[string]map[string]int `json:"facet_counts"`
		ColorCounts map[string]map[string]int `json:"color_counts"`
	}
	if err := json.Unmarshal(snapshot.Body, &held); err != nil {
		t.Fatalf("decode the snapshot's counts: %v", err)
	}
	for _, name := range counts {
		before := map[string]map[string]map[string]int{"facet_counts": held.FacetCounts, "color_counts": held.ColorCounts}[name]
		for facet, values := range liveCounts(delta, name) {
			for value, count := range values {
				if previous, ok := before[facet][value]; ok && previous == count {
					t.Fatalf("delta %s[%s][%s] = %d, the count the page already holds", name, facet, value, count)
				}
			}
		}
	}
	if len(counts) == 0 {
		t.Fatal("the merge moved no count")
	}
	requireLandsOnGET(t, rig.handler, deliveryLiveTimeline, snapshot.Body, delta, "")
	conn.none(t, time.Second, "the merge's one delta")
}

func TestAReconcilePassThatFindsNothingNewSendsNothing(t *testing.T) {
	rig := newHubRig(t, nil)
	seedHubSettings(t, rig.database)
	if err := delivery.RecordReconcileSuccess(t.Context(), rig.database.Pool, time.Now().UTC()); err != nil {
		t.Fatalf("record a reconcile: %v", err)
	}
	now := time.Now().UTC()
	prs := []delivery.DeliveryPullRequest{hubPR(1, now.Add(-30*time.Hour)), hubPR(2, now.Add(-3*time.Hour))}
	for _, pr := range prs {
		upsertPR(t, rig.database, pr)
	}
	upsertDeploy(t, rig.database, 100, now.Add(-29*time.Hour), now.Add(-28*time.Hour))
	conn := newFakeLiveConn()
	rig.hub.Subscribe(conn, subscribeFrame("p", presetView(deliveryLiveTimeline, 7, "")))
	conn.snapshot(t, 5*time.Second, "p")

	// The pass writes every row again as it was, and records its success.
	for _, pr := range prs {
		upsertPR(t, rig.database, pr)
	}
	upsertDeploy(t, rig.database, 100, now.Add(-29*time.Hour), now.Add(-28*time.Hour))
	if err := delivery.RecordReconcileSuccess(t.Context(), rig.database.Pool, time.Now().UTC()); err != nil {
		t.Fatalf("record the pass: %v", err)
	}
	conn.none(t, 5*time.Second, "a reconcile pass that found nothing new")
}

func TestIdenticalViewsShareOneRecompute(t *testing.T) {
	rig := newHubRig(t, nil)
	seedHubSettings(t, rig.database)
	upsertPR(t, rig.database, hubPR(1, time.Now().UTC().Add(-30*time.Hour)))
	first, second := newFakeLiveConn(), newFakeLiveConn()
	start := rig.mark()
	rig.hub.Subscribe(first, subscribeFrame("a", presetView(deliveryLiveTimeline, 7, "author=octocat&repo=acme/widgets")))
	first.snapshot(t, 5*time.Second, "a")
	// The same view, its query's parameters in another order.
	rig.hub.Subscribe(second, subscribeFrame("b", presetView(deliveryLiveTimeline, 7, "repo=acme%2Fwidgets&author=octocat")))
	second.snapshot(t, 5*time.Second, "b")
	if n := rig.count(start, recomputeBegin("")); n != 1 {
		t.Fatalf("recomputes for two identical views = %d, want 1", n)
	}

	merged := rig.mark()
	upsertPR(t, rig.database, hubPR(2, time.Now().UTC().Add(-time.Hour)))
	first.delta(t, 3*time.Second, "a")
	second.delta(t, 3*time.Second, "b")
	if n := rig.count(merged, recomputeBegin(deliveryCauseChange)); n != 1 {
		t.Fatalf("recomputes for one merge both views show = %d, want 1", n)
	}
}

func TestABurstCoalescesIntoOneRecompute(t *testing.T) {
	rig := newHubRig(t, nil)
	seedHubSettings(t, rig.database)
	conn := newFakeLiveConn()
	rig.hub.Subscribe(conn, subscribeFrame("p", presetView(deliveryLiveTimeline, 7, "")))
	conn.snapshot(t, 5*time.Second, "p")

	start := rig.mark()
	now := time.Now().UTC()
	began := time.Now()
	for n := 1; n <= 50; n++ {
		upsertPR(t, rig.database, hubPR(n, now.Add(-time.Duration(n)*time.Minute)))
	}
	if took := time.Since(began); took > 2*time.Second {
		t.Skipf("50 upserts took %s, longer than the coalescing's two seconds; the machine is too slow to judge it", took)
	}
	delta := conn.delta(t, 5*time.Second, "p")
	if got := len(liveUpsertIDs(t, delta, "prs")); got != 50 {
		t.Fatalf("the burst's delta upserts %d pull requests, want all 50", got)
	}
	conn.none(t, time.Second, "the burst's one delta")
	if n := rig.count(start, recomputeBegin(deliveryCauseChange)); n != 1 {
		t.Fatalf("recomputes for a burst of 50 upserts = %d, want 1", n)
	}
}

func TestOneRecomputeAtATime(t *testing.T) {
	var inFlight, most atomic.Int32
	rig := newHubRig(t, func(hub *deliveryHub) {
		observe := hub.observe
		hub.observe = func(note deliveryHubNote) {
			switch note.what {
			case "recompute begin":
				n := inFlight.Add(1)
				for {
					seen := most.Load()
					if n <= seen || most.CompareAndSwap(seen, n) {
						break
					}
				}
				// Hold the recompute open, so a second one would overlap it.
				time.Sleep(50 * time.Millisecond)
			case "recompute end":
				inFlight.Add(-1)
			}
			observe(note)
		}
	})
	seedHubSettings(t, rig.database)
	conn := newFakeLiveConn()
	views := []DeliveryLiveView{
		presetView(deliveryLiveTimeline, 7, ""), presetView(deliveryLiveTimeline, 14, ""),
		presetView(deliveryLiveMeasures, 7, ""), presetView(deliveryLiveMeasures, 28, ""), livePipelineView(nil),
	}
	for i, view := range views {
		rig.hub.Subscribe(conn, subscribeFrame("v"+strconv.Itoa(i), view))
	}
	for range views {
		if _, ok := conn.next(t, 10*time.Second, "every view's snapshot").(DeliveryLiveSnapshot); !ok {
			t.Fatal("want a snapshot for every view")
		}
	}
	upsertPR(t, rig.database, hubPR(1, time.Now().UTC().Add(-time.Hour)))
	for range views {
		if _, ok := conn.next(t, 10*time.Second, "every view's delta").(DeliveryLiveDelta); !ok {
			t.Fatal("want a delta for every view")
		}
	}
	if n := most.Load(); n != 1 {
		t.Fatalf("recomputes at once = %d, want 1", n)
	}
}

func TestAChangeOutsideAWindowRecomputesNothing(t *testing.T) {
	rig := newHubRig(t, nil)
	seedHubSettings(t, rig.database)
	conn := newFakeLiveConn()
	from, to := time.Date(2024, 6, 1, 0, 0, 0, 0, time.UTC), time.Date(2024, 6, 8, 0, 0, 0, 0, time.UTC)
	rig.hub.Subscribe(conn, subscribeFrame("m", measuresRange(from, to, "")))
	conn.snapshot(t, 5*time.Second, "m")

	start := rig.mark()
	upsertPR(t, rig.database, hubPR(1, time.Now().UTC()))
	rig.hub.catchUpNow()
	conn.none(t, viewMaxWait, "a merge outside the view's window")
	if n := rig.count(start, recomputeBegin("")); n != 0 {
		t.Fatalf("recomputes for a merge outside the window = %d, want 0", n)
	}
}

func TestNotConfiguredIsAnErrorFrameThenASnapshotOnceConfigured(t *testing.T) {
	rig := newHubRig(t, nil)
	conn := newFakeLiveConn()
	view := presetView(deliveryLiveTimeline, 7, "")
	rig.hub.Subscribe(conn, subscribeFrame("p", view))
	conn.refusal(t, 5*time.Second, "p", "DELIVERY_NOT_CONFIGURED")

	// A change while delivery is still not configured tells the page nothing more.
	start := rig.mark()
	upsertPR(t, rig.database, hubPR(1, time.Now().UTC().Add(-time.Hour)))
	rig.waitFor(t, start, 5*time.Second, "a recompute of the merge", recomputeBegin(deliveryCauseChange))
	conn.none(t, time.Second, "a second recompute while delivery is not configured")

	seedHubSettings(t, rig.database)
	snapshot := conn.snapshot(t, 5*time.Second, "p")
	requireSameBody(t, "the first snapshot once configured", snapshot.Body, getBody(t, rig.handler, windowTarget(t, deliveryLiveTimeline, snapshot.Body, "")))
}
