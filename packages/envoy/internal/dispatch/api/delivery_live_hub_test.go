package api

import (
	"context"
	"encoding/json"
	"net/http"
	"reflect"
	"slices"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/sjawhar/envoy/internal/dispatch/delivery"
	"github.com/sjawhar/envoy/internal/dispatch/docs"
	"github.com/sjawhar/envoy/internal/dispatch/events"
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
	stop  func()
}

// newHubRig starts a hub on a new test server; configure, when given, sets the hub's seams before
// anything subscribes.
func newHubRig(t *testing.T, configure func(*deliveryHub)) *hubRig {
	t.Helper()
	handler, database, deps := newTestServer(t, testServerOptions{})
	return hubOn(t, handler, database, deps, deliveryHubClock{}, configure)
}

// newClockedHubRig is newHubRig on a clock the test moves.
func newClockedHubRig(t *testing.T, clock *fakeHubClock) *hubRig {
	t.Helper()
	handler, database, deps := newTestServer(t, testServerOptions{})
	return hubOn(t, handler, database, deps, clock, nil)
}

// fakeHubClock is the real clock moved forward by Advance: Now is the wall clock plus every
// advance so far, and a timer fires once the wall clock plus the advance reaches its moment, so a
// test moves the hub past a threshold or a midnight without waiting for it. A clock from
// newStillHubClock stands still between advances instead: Now is its start plus the advances. Its
// timers still fire on the wall clock as well, but the hub's one timer only wakes its worker, which
// compares each view's due moment with Now (deliveryHub.work, runDue), so how long the test's own
// writes take decides nothing.
type fakeHubClock struct {
	mu     sync.Mutex
	still  time.Time
	offset time.Duration
	timers []*fakeHubTimer
}

type fakeHubTimer struct {
	clock *fakeHubClock
	at    time.Time
	f     func()
	real  *time.Timer
	done  atomic.Bool
}

// newStillHubClock is a fakeHubClock standing still at the present until Advance moves it.
func newStillHubClock() *fakeHubClock { return &fakeHubClock{still: time.Now().UTC()} }

func (c *fakeHubClock) nowLocked() time.Time {
	if c.still.IsZero() {
		return time.Now().UTC().Add(c.offset)
	}
	return c.still.Add(c.offset)
}

func (c *fakeHubClock) Now() time.Time {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.nowLocked()
}

func (c *fakeHubClock) AfterFunc(d time.Duration, f func()) hubTimer {
	c.mu.Lock()
	defer c.mu.Unlock()
	timer := &fakeHubTimer{clock: c, at: c.nowLocked().Add(d), f: f}
	timer.real = time.AfterFunc(d, timer.fire)
	c.timers = append(c.timers, timer)
	return timer
}

func (t *fakeHubTimer) fire() {
	if t.done.CompareAndSwap(false, true) {
		t.f()
	}
}

func (t *fakeHubTimer) Stop() bool {
	t.real.Stop()
	return t.done.CompareAndSwap(false, true)
}

// Advance moves the clock forward by d and fires every timer whose moment it reached.
func (c *fakeHubClock) Advance(d time.Duration) {
	c.mu.Lock()
	c.offset += d
	now := c.nowLocked()
	var due []*fakeHubTimer
	kept := c.timers[:0]
	for _, timer := range c.timers {
		if timer.done.Load() {
			continue
		}
		if !timer.at.After(now) {
			due = append(due, timer)
			continue
		}
		kept = append(kept, timer)
	}
	c.timers = kept
	c.mu.Unlock()
	for _, timer := range due {
		timer.real.Stop()
		timer.fire()
	}
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
	rig.stop = sync.OnceFunc(func() {
		cancel()
		rig.hub.wait()
	})
	t.Cleanup(rig.stop)
	return rig
}

// cursors is where the hub stands in the delivery change log and the event log.
func (r *hubRig) cursors() (changeSeq, eventCursor int64) {
	r.hub.mu.Lock()
	defer r.hub.mu.Unlock()
	return r.hub.changeSeq, r.hub.eventCursor
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
func requireLandsOnGET(t *testing.T, handler http.Handler, kind string, held json.RawMessage, delta DeliveryLiveDelta, query string) json.RawMessage {
	t.Helper()
	applied := applyLiveDelta(t, kind, held, delta)
	want := mustEncodeLive(t, kind, getBody(t, handler, windowTarget(t, kind, applied, query)))
	if got, wantBody := liveComparable(t, mustEncodeLive(t, kind, applied)), liveComparable(t, want); !reflect.DeepEqual(got, wantBody) {
		t.Fatalf("the delta does not land on the GET route's body:\napplied: %v\nGET:     %v", got, wantBody)
	}
	return applied
}

// applyLiveDelta is the body held with delta applied, as a page applies it.
func applyLiveDelta(t *testing.T, kind string, held json.RawMessage, delta DeliveryLiveDelta) json.RawMessage {
	t.Helper()
	applied := liveApply(t, mustEncodeLive(t, kind, held), delta)
	raw, err := json.Marshal(applied.body)
	if err != nil {
		t.Fatalf("encode the applied body: %v", err)
	}
	return raw
}

// truncateChanges empties the delivery change log and restarts its numbering, as e2e/seed.ts:150's
// reset does.
func truncateChanges(t *testing.T, database *store.Store) {
	t.Helper()
	if _, err := database.Pool.Exec(context.Background(), `truncate delivery_changes restart identity`); err != nil {
		t.Fatalf("truncate delivery_changes: %v", err)
	}
}

// latestChange is the newest delivery change's seq.
func latestChange(t *testing.T, database *store.Store) int64 {
	t.Helper()
	seq, err := delivery.LatestChangeSeq(context.Background(), database.Pool)
	if err != nil {
		t.Fatalf("read the newest change: %v", err)
	}
	return seq
}

func TestAProductionJobFlipsTheWaitingPullRequestsItShips(t *testing.T) {
	rig := newHubRig(t, nil)
	seedHubSettings(t, rig.database)
	now := time.Now().UTC()
	for n := 1; n <= 3; n++ {
		upsertPR(t, rig.database, hubPR(n, now.Add(-time.Duration(10-n)*time.Hour)))
	}
	// The deploy run is in progress: its production job has not finished.
	head, started := now.Add(-5*time.Hour), now.Add(-4*time.Hour)
	if err := delivery.UpsertRun(t.Context(), rig.database.Pool, delivery.DeliveryRun{
		Repo: "acme/widgets", RunID: 300, Kind: delivery.DeliveryRunKindDeploy, HeadSHA: "sha300",
		HeadCommitAt: head, StartedAt: started, URL: "https://github.com/acme/widgets/actions/runs/300",
		HeadBranch: new("main"), Event: new("push"),
	}); err != nil {
		t.Fatalf("upsert the running deploy: %v", err)
	}
	if err := delivery.UpsertRunJobs(t.Context(), rig.database.Pool, "acme/widgets", 300, []delivery.DeliveryRunJob{
		{Repo: "acme/widgets", RunID: 300, Name: "release / release", StartedAt: &started},
	}, false); err != nil {
		t.Fatalf("upsert the running production job: %v", err)
	}
	conn := newFakeLiveConn()
	rig.hub.Subscribe(conn, subscribeFrame("p", presetView(deliveryLiveTimeline, 7, "")))
	snapshot := conn.snapshot(t, 5*time.Second, "p")

	// The production job finishes: every pull request the run's head holds ships.
	done := now.Add(-3 * time.Hour)
	success := delivery.DeliveryJobConclusionSuccess
	if err := delivery.UpsertRunJobs(t.Context(), rig.database.Pool, "acme/widgets", 300, []delivery.DeliveryRunJob{
		{Repo: "acme/widgets", RunID: 300, Name: "release / release", StartedAt: &started, CompletedAt: &done, Conclusion: &success},
	}, false); err != nil {
		t.Fatalf("finish the production job: %v", err)
	}
	delta := conn.delta(t, 3*time.Second, "p")
	if got := liveUpsertIDs(t, delta, "prs"); !slices.Equal(got, []string{"acme/widgets#1", "acme/widgets#2", "acme/widgets#3"}) {
		t.Fatalf("the job's delta upserts %v, want the three pull requests run 300 ships", got)
	}
	if got := liveUpsertIDs(t, delta, "runs"); !slices.Equal(got, []string{"300"}) {
		t.Fatalf("the job's delta upserts runs %v, want run 300", got)
	}
	requireLandsOnGET(t, rig.handler, deliveryLiveTimeline, snapshot.Body, delta, "")
}

func TestAPRChecksRunMarksOnlyPipelineViews(t *testing.T) {
	rig := newHubRig(t, nil)
	seedHubSettings(t, rig.database)
	conn := newFakeLiveConn()
	rig.hub.Subscribe(conn, subscribeFrame("t", presetView(deliveryLiveTimeline, 7, "")))
	rig.hub.Subscribe(conn, subscribeFrame("m", presetView(deliveryLiveMeasures, 7, "")))
	rig.hub.Subscribe(conn, subscribeFrame("p", livePipelineView(nil)))
	for range 3 {
		if _, ok := conn.next(t, 5*time.Second, "each view's snapshot").(DeliveryLiveSnapshot); !ok {
			t.Fatal("want a snapshot for each view")
		}
	}

	start := rig.mark()
	now := time.Now().UTC()
	started, done := now.Add(-time.Hour), now.Add(-50*time.Minute)
	success := delivery.DeliveryRunConclusionSuccess
	if err := delivery.UpsertRun(t.Context(), rig.database.Pool, delivery.DeliveryRun{
		Repo: "acme/widgets", RunID: 400, Kind: delivery.DeliveryRunKindPRChecks, HeadSHA: "sha400",
		HeadCommitAt: started, StartedAt: started, CompletedAt: &done, Conclusion: &success,
		URL: "https://github.com/acme/widgets/actions/runs/400", HeadBranch: new("feature"), Event: new("pull_request"),
	}); err != nil {
		t.Fatalf("upsert the PR-checks run: %v", err)
	}
	if delta := conn.delta(t, 3*time.Second, "p"); len(delta.Set) == 0 {
		t.Fatalf("the pipeline delta sets nothing: %+v", delta)
	}
	conn.none(t, time.Second, "the pipeline's delta")
	if n := rig.count(start, func(note deliveryHubNote) bool {
		return note.what == "recompute begin" && !strings.HasPrefix(note.view, deliveryLivePipeline)
	}); n != 0 {
		t.Fatalf("a PR-checks run recomputed %d timeline or measures views, want only the pipeline view", n)
	}
}

func TestASessionGroupEditReachesEveryView(t *testing.T) {
	rig := newHubRig(t, nil)
	seedHubSettings(t, rig.database)
	pr := hubPR(1, time.Now().UTC().Add(-3*time.Hour))
	pr.Sessions = []string{"s-1"}
	upsertPR(t, rig.database, pr)
	conn := newFakeLiveConn()
	rig.hub.Subscribe(conn, subscribeFrame("t", presetView(deliveryLiveTimeline, 7, "")))
	rig.hub.Subscribe(conn, subscribeFrame("m", presetView(deliveryLiveMeasures, 7, "")))
	rig.hub.Subscribe(conn, subscribeFrame("p", livePipelineView(nil)))
	for range 3 {
		if _, ok := conn.next(t, 5*time.Second, "each view's snapshot").(DeliveryLiveSnapshot); !ok {
			t.Fatal("want a snapshot for each view")
		}
	}

	start := rig.mark()
	if response := dispatchRequest(t, rig.handler, http.MethodPost, "/api/v1/delivery/session-groups", map[string]any{
		"name": "Night shift", "sessions": []string{"s-1"},
	}, "alice"); response.Code != http.StatusCreated {
		t.Fatalf("create the session group: status = %d, body = %s", response.Code, response.Body.String())
	}
	delta := conn.delta(t, 3*time.Second, "t")
	if got := liveUpsertIDs(t, delta, "session_groups"); len(got) != 1 {
		t.Fatalf("the timeline's delta upserts session groups %v, want the new group", got)
	}
	rig.waitFor(t, start, 3*time.Second, "the measures view's recompute", func(note deliveryHubNote) bool {
		return note.what == "recompute end" && strings.HasPrefix(note.view, deliveryLiveMeasures)
	})
	rig.waitFor(t, start, 3*time.Second, "the pipeline view's recompute", func(note deliveryHubNote) bool {
		return note.what == "recompute end" && strings.HasPrefix(note.view, deliveryLivePipeline)
	})
}

func TestAResetChangeLogIsCaughtAndEveryViewRecomputed(t *testing.T) {
	rig := newHubRig(t, nil)
	seedHubSettings(t, rig.database)
	now := time.Now().UTC()
	for n := 1; n <= 40; n++ {
		upsertPR(t, rig.database, hubPR(n, now.Add(-time.Duration(100+n)*time.Hour)))
	}
	if seq := latestChange(t, rig.database); seq < 40 {
		t.Fatalf("changes before the reset reach seq %d, want at least 40", seq)
	}
	conn := newFakeLiveConn()
	rig.hub.Subscribe(conn, subscribeFrame("t", presetView(deliveryLiveTimeline, 7, "")))
	rig.hub.Subscribe(conn, subscribeFrame("m", measuresRange(time.Date(2024, 6, 1, 0, 0, 0, 0, time.UTC), time.Date(2024, 6, 8, 0, 0, 0, 0, time.UTC), "")))
	var timeline json.RawMessage
	for range 2 {
		if snapshot, ok := conn.next(t, 5*time.Second, "each view's snapshot").(DeliveryLiveSnapshot); !ok {
			t.Fatal("want a snapshot for each view")
		} else if snapshot.Sub == "t" {
			timeline = snapshot.Body
		}
	}

	// The log restarts at seq 1: one merge, below the hub's cursor.
	truncateChanges(t, rig.database)
	start := rig.mark()
	upsertPR(t, rig.database, hubPR(41, time.Now().UTC().Add(-time.Hour)))
	delta := conn.delta(t, 3*time.Second, "t")
	if got := liveUpsertIDs(t, delta, "prs"); !slices.Contains(got, "acme/widgets#41") {
		t.Fatalf("after the reset the delta upserts %v, want acme/widgets#41", got)
	}
	timeline = requireLandsOnGET(t, rig.handler, deliveryLiveTimeline, timeline, delta, "")
	rig.waitFor(t, start, 3*time.Second, "the measures view's recompute", func(note deliveryHubNote) bool {
		return note.what == "recompute begin" && strings.HasPrefix(note.view, deliveryLiveMeasures)
	})
	if seq, _ := rig.cursors(); seq != 1 {
		t.Fatalf("the hub's change cursor after the reset = %d, want 1", seq)
	}

	// Again: the hub's cursor moves to 2, then the log restarts and three merges (seq 1-3) commit
	// together before the hub reads. The log holds a row at the cursor, written at another time than
	// the hub read for it, so that is a reset too.
	upsertPR(t, rig.database, hubPR(42, time.Now().UTC().Add(-50*time.Minute)))
	timeline = requireLandsOnGET(t, rig.handler, deliveryLiveTimeline, timeline, conn.delta(t, 3*time.Second, "t"), "")
	if seq, _ := rig.cursors(); seq != 2 {
		t.Fatalf("the hub's change cursor = %d, want 2", seq)
	}
	truncateChanges(t, rig.database)
	start = rig.mark()
	merged := time.Now().UTC().Add(-30 * time.Minute)
	if _, err := rig.database.Pool.Exec(t.Context(), `
		insert into delivery_pull_requests (repo, number, title, url, author, created_at, merged_at)
		select 'acme/widgets', n, 'feat: change ' || n, 'https://github.com/acme/widgets/pull/' || n, 'octocat', $1::timestamptz, $1::timestamptz
		from generate_series(43, 45) n
	`, merged); err != nil {
		t.Fatalf("merge three pull requests in one commit: %v", err)
	}
	delta = conn.delta(t, 3*time.Second, "t")
	if got := slices.Sorted(slices.Values(liveUpsertIDs(t, delta, "prs"))); !slices.Equal(got, []string{"acme/widgets#43", "acme/widgets#44", "acme/widgets#45"}) {
		t.Fatalf("after the second reset the delta upserts %v, want the three merges", got)
	}
	requireLandsOnGET(t, rig.handler, deliveryLiveTimeline, timeline, delta, "")
	rig.waitFor(t, start, 3*time.Second, "the measures view's recompute after the second reset", func(note deliveryHubNote) bool {
		return note.what == "recompute begin" && strings.HasPrefix(note.view, deliveryLiveMeasures)
	})
	if seq, _ := rig.cursors(); seq != 3 {
		t.Fatalf("the hub's change cursor after the second reset = %d, want 3", seq)
	}
}

func TestResumeWithRevsGetsOnlyWhatChanged(t *testing.T) {
	handler, database, deps := newTestServer(t, testServerOptions{})
	seedHubSettings(t, database)
	now := time.Now().UTC()
	for n := 1; n <= 4; n++ {
		upsertPR(t, database, hubPR(n, now.Add(-time.Duration(10*n)*time.Hour)))
	}
	first := hubOn(t, handler, database, deps, deliveryHubClock{}, nil)
	conn := newFakeLiveConn()
	view := timelineRange(now.Add(-7*24*time.Hour), now, "")
	first.hub.Subscribe(conn, subscribeFrame("t", view))
	snapshot := conn.snapshot(t, 5*time.Second, "t")
	// The task the page was on stops; two pull requests change before the page reaches another.
	first.stop()
	changed := hubPR(2, now.Add(-20*time.Hour))
	changed.Title = "feat: a title the page has not seen"
	upsertPR(t, database, changed)
	upsertPR(t, database, hubPR(5, now.Add(-time.Hour)))

	second := hubOn(t, handler, database, deps, deliveryHubClock{}, nil)
	resumed := newFakeLiveConn()
	frame := subscribeFrame("t", view)
	frame.Resume = &DeliveryLiveResume{Seq: snapshot.Seq, Revs: snapshot.Revs}
	second.hub.Subscribe(resumed, frame)
	delta := resumed.delta(t, 5*time.Second, "t")
	if got := liveUpsertIDs(t, delta, "prs"); !slices.Equal(got, []string{"acme/widgets#2", "acme/widgets#5"}) {
		t.Fatalf("the resume's delta upserts %v, want the two pull requests that changed", got)
	}
	requireLandsOnGET(t, handler, deliveryLiveTimeline, snapshot.Body, delta, "")
}

func TestResumeFromBeforeTheKeptDayGetsASnapshot(t *testing.T) {
	rig := newHubRig(t, nil)
	seedHubSettings(t, rig.database)
	now := time.Now().UTC()
	for n := 1; n <= 3; n++ {
		upsertPR(t, rig.database, hubPR(n, now.Add(-time.Duration(n)*time.Hour)))
	}
	// The change log keeps a day: rows older than that are pruned, so the oldest kept is past seq 2.
	if _, err := rig.database.Pool.Exec(t.Context(), `update delivery_changes set at = now() - interval '2 days' where seq <= 2`); err != nil {
		t.Fatalf("age the first changes: %v", err)
	}
	if _, err := delivery.PruneChanges(t.Context(), rig.database.Pool, time.Now()); err != nil {
		t.Fatalf("prune the change log: %v", err)
	}
	conn := newFakeLiveConn()
	frame := subscribeFrame("t", presetView(deliveryLiveTimeline, 7, ""))
	frame.Resume = &DeliveryLiveResume{Seq: "1", Revs: DeliveryLiveRevs{
		Items: map[string]map[string]string{}, Maps: map[string]map[string]string{},
		Counts: map[string]map[string]map[string]string{}, Fields: map[string]string{},
	}}
	rig.hub.Subscribe(conn, frame)
	snapshot := conn.snapshot(t, 5*time.Second, "t")
	requireSameBody(t, "a resume from before the kept day", snapshot.Body, getBody(t, rig.handler, windowTarget(t, deliveryLiveTimeline, snapshot.Body, "")))

	// A resume from a kept change answers the delta.
	kept := newFakeLiveConn()
	frame.Sub, frame.Resume = "u", &DeliveryLiveResume{Seq: snapshot.Seq, Revs: snapshot.Revs}
	rig.hub.Subscribe(kept, frame)
	kept.none(t, time.Second, "a resume from the change the view's body reflects")
}

func TestAReconnectCatchesUpWhatTheListenerMissed(t *testing.T) {
	var blind atomic.Bool
	rig := newHubRig(t, func(hub *deliveryHub) {
		dial := hub.dial
		hub.dial = func(ctx context.Context) (deliveryListenerConn, error) {
			if blind.Load() {
				return nil, context.DeadlineExceeded
			}
			return dial(ctx)
		}
		hub.backoff = func(int) time.Duration { return 50 * time.Millisecond }
	})
	seedHubSettings(t, rig.database)
	conn := newFakeLiveConn()
	rig.hub.Subscribe(conn, subscribeFrame("t", presetView(deliveryLiveTimeline, 7, "")))
	snapshot := conn.snapshot(t, 5*time.Second, "t")
	rig.waitFor(t, 0, 5*time.Second, "the listener's first connection", func(note deliveryHubNote) bool { return note.what == "connected" })
	start := rig.mark()

	// The listener loses its connection and cannot dial again; a merge commits while it is blind.
	blind.Store(true)
	terminateListener(t, rig.database)
	upsertPR(t, rig.database, hubPR(1, time.Now().UTC().Add(-time.Hour)))
	conn.none(t, viewMaxWait+time.Second, "a merge the listener could not hear")
	blind.Store(false)
	rig.waitFor(t, start, 5*time.Second, "the listener's reconnection", func(note deliveryHubNote) bool { return note.what == "connected" })
	delta := conn.delta(t, 3*time.Second, "t")
	if got := liveUpsertIDs(t, delta, "prs"); !slices.Equal(got, []string{"acme/widgets#1"}) {
		t.Fatalf("after the reconnection the delta upserts %v, want the merge it missed", got)
	}
	requireLandsOnGET(t, rig.handler, deliveryLiveTimeline, snapshot.Body, delta, "")
}

// terminateListener ends every backend of this test's database that holds a LISTEN, whose last
// statement was the listener's LISTEN or its idle ping.
func terminateListener(t *testing.T, database *store.Store) {
	t.Helper()
	var ended int
	if err := database.Pool.QueryRow(context.Background(), `
		select count(*) from (
			select pg_terminate_backend(pid) from pg_stat_activity
			where datname = current_database() and (query like 'listen %' or query = '-- ping') and pid <> pg_backend_pid()
		) ended
	`).Scan(&ended); err != nil || ended == 0 {
		t.Fatalf("terminate the listener's backend: ended %d, err %v", ended, err)
	}
}

// createHubIssue creates issue title in project HUB (creating the project the first time), and
// returns its key.
func createHubIssue(t *testing.T, handler http.Handler, fields map[string]any) string {
	t.Helper()
	dispatchRequest(t, handler, http.MethodPost, "/api/v1/projects", map[string]string{"key": "HUB", "name": "Hub"}, "alice")
	body := map[string]any{"project": "HUB"}
	for name, value := range fields {
		body[name] = value
	}
	response := dispatchRequest(t, handler, http.MethodPost, "/api/v1/issues", body, "alice")
	if response.Code != http.StatusCreated {
		t.Fatalf("create issue %v: status = %d, body = %s", fields, response.Code, response.Body.String())
	}
	return decodeBody[struct {
		Key string `json:"key"`
	}](t, response).Key
}

func patchHubIssue(t *testing.T, handler http.Handler, key string, patch map[string]any) {
	t.Helper()
	if response := dispatchRequest(t, handler, http.MethodPatch, "/api/v1/issues/"+key, patch, "alice"); response.Code != http.StatusOK {
		t.Fatalf("patch %s %v: status = %d, body = %s", key, patch, response.Code, response.Body.String())
	}
}

// prNaming is pull request n merged at merged, naming issue.
func prNaming(n int, merged time.Time, issue string) delivery.DeliveryPullRequest {
	pr := hubPR(n, merged)
	pr.IssueKey = &issue
	return pr
}

// seedHubComponents gives project HUB a component tree, platform > api.
func seedHubComponents(t *testing.T, database *store.Store) {
	t.Helper()
	for _, statement := range []string{
		`insert into architecture_snapshots (id, project_key, commit, files) values (901, 'HUB', 'c1', '{}')`,
		`insert into components (project_key, id, title, prose, parent, snapshot_id) values
			('HUB', 'platform', 'The platform', '', null, 901), ('HUB', 'api', 'Public API', '', 'platform', 901)`,
	} {
		if _, err := database.Pool.Exec(context.Background(), statement); err != nil {
			t.Fatalf("seed components: %v\n%s", err, statement)
		}
	}
}

func TestAnIssueTitleChangeReachesPagesThatNameTheIssue(t *testing.T) {
	rig := newHubRig(t, nil)
	seedHubSettings(t, rig.database)
	named := createHubIssue(t, rig.handler, map[string]any{"title": "Ship widgets"})
	other := createHubIssue(t, rig.handler, map[string]any{"title": "Tidy docs"})
	now := time.Now().UTC()
	upsertPR(t, rig.database, prNaming(1, now.Add(-3*time.Hour), named))
	upsertPR(t, rig.database, prNaming(2, time.Date(2024, 6, 3, 12, 0, 0, 0, time.UTC), other))
	conn := newFakeLiveConn()
	rig.hub.Subscribe(conn, subscribeFrame("names", presetView(deliveryLiveTimeline, 7, "")))
	rig.hub.Subscribe(conn, subscribeFrame("other", timelineRange(time.Date(2024, 6, 1, 0, 0, 0, 0, time.UTC), time.Date(2024, 6, 8, 0, 0, 0, 0, time.UTC), "")))
	var held json.RawMessage
	for range 2 {
		snapshot, ok := conn.next(t, 5*time.Second, "each view's snapshot").(DeliveryLiveSnapshot)
		if !ok {
			t.Fatal("want a snapshot for each view")
		}
		if snapshot.Sub == "names" {
			held = snapshot.Body
		}
	}

	start := rig.mark()
	patchHubIssue(t, rig.handler, named, map[string]any{"title": "Ship the widgets"})
	delta := conn.delta(t, 3*time.Second, "names")
	if got := liveMapValues(t, delta, "issue_titles"); got[named] != "Ship the widgets" {
		t.Fatalf("the delta's issue_titles = %v, want %s retitled", got, named)
	}
	requireLandsOnGET(t, rig.handler, deliveryLiveTimeline, held, delta, "")
	conn.none(t, time.Second, "a title change the other page does not name")
	if n := rig.count(start, func(note deliveryHubNote) bool {
		return note.what == "recompute begin" && strings.Contains(note.view, "2024-06-01")
	}); n != 0 {
		t.Fatalf("a title change the other page does not name recomputed it %d times, want 0", n)
	}
}

func TestAParentsComponentChangeReachesPagesHoldingItsChildren(t *testing.T) {
	rig := newHubRig(t, nil)
	seedHubSettings(t, rig.database)
	parent := createHubIssue(t, rig.handler, map[string]any{"title": "Platform work"})
	seedHubComponents(t, rig.database)
	child := createHubIssue(t, rig.handler, map[string]any{"title": "A child", "parent": parent})
	upsertPR(t, rig.database, prNaming(1, time.Now().UTC().Add(-3*time.Hour), child))
	conn := newFakeLiveConn()
	rig.hub.Subscribe(conn, subscribeFrame("t", presetView(deliveryLiveTimeline, 7, "")))
	snapshot := conn.snapshot(t, 5*time.Second, "t")

	// The child inherits what its parent chooses.
	patchHubIssue(t, rig.handler, parent, map[string]any{"components": map[string]any{"mode": "explicit", "ids": []string{"api"}}})
	delta := conn.delta(t, 3*time.Second, "t")
	if got := liveUpsertIDs(t, delta, "prs"); !slices.Equal(got, []string{"acme/widgets#1"}) {
		t.Fatalf("the parent's component change upserts %v, want the child's pull request", got)
	}
	requireLandsOnGET(t, rig.handler, deliveryLiveTimeline, snapshot.Body, delta, "")
}

// unownedP0Fields is the unowned_p0 and status a body of kind shows, decoded: a measures body's
// own, a timeline's from its measures.
func unownedP0Fields(t *testing.T, body json.RawMessage, kind string) map[string]any {
	t.Helper()
	if kind == deliveryLiveTimeline {
		var timeline struct {
			Measures json.RawMessage `json:"measures"`
		}
		if err := json.Unmarshal(body, &timeline); err != nil {
			t.Fatalf("decode the timeline's measures: %v", err)
		}
		body = timeline.Measures
	}
	var decoded map[string]any
	if err := json.Unmarshal(body, &decoded); err != nil {
		t.Fatalf("decode the measures: %v", err)
	}
	return map[string]any{"unowned_p0": decoded["unowned_p0"], "status": decoded["status"]}
}

func TestAnUnownedP0ChangeReachesEveryTimelineAndMeasuresView(t *testing.T) {
	rig := newHubRig(t, nil)
	seedHubSettings(t, rig.database)
	upsertPR(t, rig.database, hubPR(1, time.Now().UTC().Add(-3*time.Hour)))
	seed := createHubIssue(t, rig.handler, map[string]any{"title": "Raised later", "priority": 1})
	conn := newFakeLiveConn()
	held := map[string]json.RawMessage{}
	kinds := map[string]string{"t": deliveryLiveTimeline, "m": deliveryLiveMeasures}
	rig.hub.Subscribe(conn, subscribeFrame("t", presetView(deliveryLiveTimeline, 7, "")))
	rig.hub.Subscribe(conn, subscribeFrame("m", presetView(deliveryLiveMeasures, 28, "")))
	for range 2 {
		snapshot, ok := conn.next(t, 5*time.Second, "each view's snapshot").(DeliveryLiveSnapshot)
		if !ok {
			t.Fatal("want a snapshot for each view")
		}
		held[snapshot.Sub] = snapshot.Body
	}
	var created string
	for _, step := range []struct {
		name string
		act  func()
	}{
		{"a P0 created", func() {
			created = createHubIssue(t, rig.handler, map[string]any{"title": "Production is down", "priority": 0})
		}},
		{"a P0 claimed", func() {
			if response := dispatchRequest(t, rig.handler, http.MethodPost, "/api/v1/issues/"+created+"/claim", map[string]any{}, "alice"); response.Code != http.StatusOK {
				t.Fatalf("claim: status = %d, body = %s", response.Code, response.Body.String())
			}
		}},
		{"a claimed P0 released", func() {
			if response := dispatchRequest(t, rig.handler, http.MethodDelete, "/api/v1/issues/"+created+"/claim", nil, "alice"); response.Code != http.StatusOK {
				t.Fatalf("release: status = %d, body = %s", response.Code, response.Body.String())
			}
		}},
		{"a P0 closed", func() { patchHubIssue(t, rig.handler, created, map[string]any{"status": "done"}) }},
		{"a P1 raised to P0", func() { patchHubIssue(t, rig.handler, seed, map[string]any{"priority": 0}) }},
		{"a P0 routed", func() { patchHubIssue(t, rig.handler, seed, map[string]any{"route": "role:reviewer"}) }},
		{"a routed P0 unrouted", func() { patchHubIssue(t, rig.handler, seed, map[string]any{"route": ""}) }},
		{"a P0 lowered to P1", func() { patchHubIssue(t, rig.handler, seed, map[string]any{"priority": 1}) }},
	} {
		step.act()
		for range 2 {
			delta, ok := conn.next(t, 3*time.Second, "each view's delta after "+step.name).(DeliveryLiveDelta)
			if !ok {
				t.Fatalf("after %s: want a delta for each view", step.name)
			}
			kind := kinds[delta.Sub]
			applied := applyLiveDelta(t, kind, held[delta.Sub], delta)
			// GET /measures for the window the view answers.
			want := unownedP0Fields(t, getBody(t, rig.handler, windowTarget(t, deliveryLiveMeasures, applied, "")), deliveryLiveMeasures)
			if got := unownedP0Fields(t, applied, kind); !reflect.DeepEqual(got, want) {
				t.Fatalf("after %s, %s's measures hold %v; GET /measures answers %v", step.name, delta.Sub, got, want)
			}
			held[delta.Sub] = applied
		}
	}
}

func TestAnArchitectureSyncReachesEveryView(t *testing.T) {
	rig := newHubRig(t, nil)
	seedHubSettings(t, rig.database)
	createHubIssue(t, rig.handler, map[string]any{"title": "Anything"})
	conn := newFakeLiveConn()
	rig.hub.Subscribe(conn, subscribeFrame("t", presetView(deliveryLiveTimeline, 7, "")))
	rig.hub.Subscribe(conn, subscribeFrame("m", measuresRange(time.Date(2024, 6, 1, 0, 0, 0, 0, time.UTC), time.Date(2024, 6, 8, 0, 0, 0, 0, time.UTC), "")))
	rig.hub.Subscribe(conn, subscribeFrame("p", livePipelineView(nil)))
	for range 3 {
		if _, ok := conn.next(t, 5*time.Second, "each view's snapshot").(DeliveryLiveSnapshot); !ok {
			t.Fatal("want a snapshot for each view")
		}
	}
	start := rig.mark()
	appendHubEvent(t, rig, projectOwner("HUB").event("architecture.synced", model.Actor{Kind: "system", ID: "architecture-importer"}, map[string]any{"commit": "c2"}))
	for _, kind := range []string{deliveryLiveTimeline, deliveryLiveMeasures, deliveryLivePipeline} {
		rig.waitFor(t, start, 3*time.Second, "the "+kind+" view's recompute", func(note deliveryHubNote) bool {
			return note.what == "recompute begin" && strings.HasPrefix(note.view, kind)
		})
	}
}

// appendHubEvent commits event as a route does, and publishes it to the server's broker.
func appendHubEvent(t *testing.T, rig *hubRig, event model.Event) model.Event {
	t.Helper()
	ctx := store.WithTransactionTracking(context.Background())
	tx, err := rig.database.Pool.Begin(ctx)
	if err != nil {
		t.Fatalf("begin: %v", err)
	}
	defer tx.Rollback(ctx)
	appended, err := rig.deps.Events.Append(ctx, tx, event)
	if err != nil {
		t.Fatalf("append %s: %v", event.Type, err)
	}
	if err := tx.Commit(ctx); err != nil {
		t.Fatalf("commit %s: %v", event.Type, err)
	}
	rig.deps.Events.Publish(appended)
	return appended
}

func TestAResetEventLogIsCaughtAndEveryViewRecomputed(t *testing.T) {
	rig := newHubRig(t, nil)
	seedHubSettings(t, rig.database)
	key := createHubIssue(t, rig.handler, map[string]any{"title": "Ship widgets"})
	for range 299 {
		appendHubEvent(t, rig, projectOwner("HUB").event("user_state.updated", model.Actor{Kind: "user", ID: "alice"}, map[string]any{}))
	}
	upsertPR(t, rig.database, prNaming(1, time.Now().UTC().Add(-3*time.Hour), key))
	conn := newFakeLiveConn()
	rig.hub.Subscribe(conn, subscribeFrame("t", presetView(deliveryLiveTimeline, 7, "")))
	held := conn.snapshot(t, 5*time.Second, "t").Body
	rig.hub.catchUpNow()
	if _, cursor := rig.cursors(); cursor < 300 {
		t.Fatalf("the hub's event cursor = %d, want at least 300", cursor)
	}

	// The event log restarts at id 1, as e2e/seed.ts:20 and :150 reset it; then a title the view
	// names changes, its event (id 1) below the hub's cursor.
	if _, err := rig.database.Pool.Exec(t.Context(), `truncate events restart identity cascade`); err != nil {
		t.Fatalf("truncate events: %v", err)
	}
	patchHubIssue(t, rig.handler, key, map[string]any{"title": "Ship the widgets"})
	delta := conn.delta(t, 3*time.Second, "t")
	if got := liveMapValues(t, delta, "issue_titles"); got[key] != "Ship the widgets" {
		t.Fatalf("after the event log's reset the delta's issue_titles = %v, want %s retitled", got, key)
	}
	held = requireLandsOnGET(t, rig.handler, deliveryLiveTimeline, held, delta, "")
	if _, cursor := rig.cursors(); cursor != 1 {
		t.Fatalf("the hub's event cursor after the reset = %d, want 1", cursor)
	}

	// Again, with the log refilled past the old cursor before the hub reads: the row at its cursor
	// was created at another time than the hub read, so that is a reset too.
	if _, err := rig.database.Pool.Exec(t.Context(), `truncate events restart identity cascade`); err != nil {
		t.Fatalf("truncate events: %v", err)
	}
	// The retitle commits before the refill: the refill's insert fires 0107's events_notify trigger
	// itself, so the hub can read the reset and recompute before the test's own notification.
	if _, err := rig.database.Pool.Exec(t.Context(), `update issues set title = 'Ship every widget' where key = $1`, key); err != nil {
		t.Fatalf("retitle the issue without an event: %v", err)
	}
	if _, err := rig.database.Pool.Exec(t.Context(), `
		insert into events (project_key, seq, type, actor, payload, notify, created_at)
		select 'HUB', n, 'user_state.updated', '{"kind":"user","id":"alice"}', '{}', false, now() - interval '1 hour'
		from generate_series(1, 5) n
	`); err != nil {
		t.Fatalf("refill the event log: %v", err)
	}
	if _, err := rig.database.Pool.Exec(t.Context(), `update projects set last_seq = 5 where key = 'HUB'`); err != nil {
		t.Fatalf("move the project's sequence: %v", err)
	}
	notifyChannel(t, rig.database, dispatchEventsChannel, "")
	delta = conn.delta(t, 3*time.Second, "t")
	if got := liveMapValues(t, delta, "issue_titles"); got[key] != "Ship every widget" {
		t.Fatalf("after the second reset the delta's issue_titles = %v, want %s retitled", got, key)
	}
	requireLandsOnGET(t, rig.handler, deliveryLiveTimeline, held, delta, "")
}

func TestACatchUpPastIgnoredEventsAdvancesTheCursor(t *testing.T) {
	rig := newHubRig(t, nil)
	seedHubSettings(t, rig.database)
	createHubIssue(t, rig.handler, map[string]any{"title": "Anything"})
	conn := newFakeLiveConn()
	rig.hub.Subscribe(conn, subscribeFrame("t", presetView(deliveryLiveTimeline, 7, "")))
	conn.snapshot(t, 5*time.Second, "t")
	rig.hub.catchUpNow()
	_, before := rig.cursors()

	// 500 events no view reads, written without the broker, as another task's would be. The insert
	// fires 0107's events_notify trigger itself, so the hub can catch up past them before the test's
	// own notification; the mark is taken first so that catch-up counts.
	start := rig.mark()
	if _, err := rig.database.Pool.Exec(t.Context(), `
		insert into events (project_key, seq, type, actor, payload, notify)
		select 'HUB', (select last_seq from projects where key = 'HUB') + n, 'artifact.version', '{"kind":"user","id":"alice"}', '{}', false
		from generate_series(1, 500) n
	`); err != nil {
		t.Fatalf("append 500 events: %v", err)
	}
	var newest int64
	if err := rig.database.Pool.QueryRow(t.Context(), `select max(id) from events`).Scan(&newest); err != nil {
		t.Fatalf("read the newest event: %v", err)
	}
	notifyChannel(t, rig.database, dispatchEventsChannel, "")
	rig.waitFor(t, start, 3*time.Second, "the catch-up", func(note deliveryHubNote) bool {
		return note.what == "events caught up" && note.through == newest
	})
	if n := rig.count(start, func(note deliveryHubNote) bool {
		return note.what == "events caught up" && note.from == before && note.through == newest
	}); n != 1 {
		t.Fatalf("catch-ups from the cursor before the 500 events = %d, want 1", n)
	}
	second := rig.mark()
	notifyChannel(t, rig.database, dispatchEventsChannel, "")
	rig.waitFor(t, second, 3*time.Second, "the second catch-up", func(note deliveryHubNote) bool { return note.what == "events caught up" })
	if n := rig.count(second, func(note deliveryHubNote) bool { return note.what == "events caught up" && note.from != newest }); n != 0 {
		t.Fatalf("the second notification scanned from below %d again", newest)
	}
	conn.none(t, time.Second, "500 events no view reads")
}

func TestADroppedBrokerSubscriptionIsRestoredWithoutMissingAnEvent(t *testing.T) {
	release := make(chan struct{})
	releaseOnce := sync.OnceFunc(func() { close(release) })
	rig := newHubRig(t, func(hub *deliveryHub) {
		// The listener never connects, so only the broker or the store can tell the hub of an event.
		hub.dial = func(context.Context) (deliveryListenerConn, error) { return nil, context.DeadlineExceeded }
		hub.backoff = func(int) time.Duration { return time.Hour }
		observe := hub.observe
		hub.observe = func(note deliveryHubNote) {
			observe(note)
			if note.what == "broker dropped" {
				// Hold the hub between losing its subscription and taking a new one, while the test
				// commits an event.
				<-release
			}
		}
	})
	// Registered after the rig's, so it runs first: a failed test never leaves the hub held.
	t.Cleanup(releaseOnce)
	seedHubSettings(t, rig.database)
	key := createHubIssue(t, rig.handler, map[string]any{"title": "Ship widgets"})
	upsertPR(t, rig.database, prNaming(1, time.Now().UTC().Add(-3*time.Hour), key))
	conn := newFakeLiveConn()
	rig.hub.Subscribe(conn, subscribeFrame("t", presetView(deliveryLiveTimeline, 7, "")))
	snapshot := conn.snapshot(t, 5*time.Second, "t")

	start := rig.mark()
	rig.deps.Events.CloseAll()
	rig.waitFor(t, start, 3*time.Second, "the hub losing its subscription", func(note deliveryHubNote) bool { return note.what == "broker dropped" })
	patchHubIssue(t, rig.handler, key, map[string]any{"title": "Ship the widgets"})
	releaseOnce()
	delta := conn.delta(t, 3*time.Second, "t")
	if got := liveMapValues(t, delta, "issue_titles"); got[key] != "Ship the widgets" {
		t.Fatalf("the delta's issue_titles = %v, want %s retitled", got, key)
	}
	requireLandsOnGET(t, rig.handler, deliveryLiveTimeline, snapshot.Body, delta, "")
}

func TestAnEventAnotherTaskCommittedReachesThisHub(t *testing.T) {
	rig := newHubRig(t, nil)
	seedHubSettings(t, rig.database)
	key := createHubIssue(t, rig.handler, map[string]any{"title": "Ship widgets"})
	upsertPR(t, rig.database, prNaming(1, time.Now().UTC().Add(-3*time.Hour), key))
	conn := newFakeLiveConn()
	rig.hub.Subscribe(conn, subscribeFrame("t", presetView(deliveryLiveTimeline, 7, "")))
	snapshot := conn.snapshot(t, 5*time.Second, "t")

	// Another task: its own broker and documents, on the same database.
	other := events.NewBroker()
	documents := docs.New(docs.Deps{Store: rig.database, Events: other, ServerURL: "https://dispatch.example", Settle: 20 * time.Millisecond})
	t.Cleanup(func() { _ = documents.Shutdown(context.Background()) })
	deps, err := NewDeps(DepsInput{
		Store: rig.database, Identity: headerIdentity(rig.database), AgentTokens: sharedAgentTokens(t, "agent-token"),
		ServerURL: "https://dispatch.example", Docs: documents, Events: other,
	})
	if err != nil {
		t.Fatalf("the other task's dependencies: %v", err)
	}
	otherTask := http.NewServeMux()
	Register(otherTask, deps)
	patchHubIssue(t, otherTask, key, map[string]any{"title": "Ship the widgets"})
	delta := conn.delta(t, 3*time.Second, "t")
	if got := liveMapValues(t, delta, "issue_titles"); got[key] != "Ship the widgets" {
		t.Fatalf("the delta's issue_titles = %v, want %s retitled by the other task", got, key)
	}
	requireLandsOnGET(t, rig.handler, deliveryLiveTimeline, snapshot.Body, delta, "")
}

// acquires is how many connections the shared pool has handed out so far.
func acquires(database *store.Store) int64 { return database.Pool.Stat().AcquireCount() }

// waitConnected waits for the hub's listener to have connected and caught both logs up, after
// which the hub reads nothing until something changes.
func (r *hubRig) waitConnected(t *testing.T) {
	t.Helper()
	r.waitFor(t, 0, 5*time.Second, "the listener's connection", func(note deliveryHubNote) bool { return note.what == "connected" })
}

// freshness is the next frame conn is sent, which must be a freshness frame.
func (c *fakeLiveConn) freshness(t *testing.T, within time.Duration, what string) DeliveryLiveFreshness {
	t.Helper()
	frame := c.next(t, within, what)
	fresh, ok := frame.(DeliveryLiveFreshness)
	if !ok {
		raw, _ := json.Marshal(frame)
		t.Fatalf("frame = %s, want %s", raw, what)
	}
	return fresh
}

func TestAPresetViewSendsItsOldestPullRequestLeavingWithoutAReadingTheDatabase(t *testing.T) {
	rig := newHubRig(t, nil)
	seedHubSettings(t, rig.database)
	now := time.Now().UTC()
	upsertPR(t, rig.database, hubPR(1, now.Add(-28*24*time.Hour+2*time.Second)))
	upsertPR(t, rig.database, hubPR(2, now.Add(-time.Hour)))
	conn := newFakeLiveConn()
	rig.hub.Subscribe(conn, subscribeFrame("p", presetView(deliveryLiveTimeline, 28, "")))
	snapshot := conn.snapshot(t, 5*time.Second, "p")
	rig.waitConnected(t)
	before := acquires(rig.database)

	delta := conn.delta(t, 4*time.Second, "p")
	if got := acquires(rig.database); got != before {
		t.Fatalf("the shared pool handed out %d connections while the oldest pull request left, want none", got-before)
	}
	if got := delta.Items["prs"].Remove; !slices.Equal(got, []string{"acme/widgets#1"}) {
		t.Fatalf("the expiry's delta removes %v, want acme/widgets#1", got)
	}
	conn.none(t, time.Second, "the expiry's one delta")
	requireLandsOnGET(t, rig.handler, deliveryLiveTimeline, snapshot.Body, delta, "")
}

func TestAPipelineViewMovesItsDaysAtMidnightWithoutReadingTheDatabase(t *testing.T) {
	clock := &fakeHubClock{}
	rig := newClockedHubRig(t, clock)
	seedHubSettings(t, rig.database)
	conn := newFakeLiveConn()
	rig.hub.Subscribe(conn, subscribeFrame("p", livePipelineView(nil)))
	conn.snapshot(t, 5*time.Second, "p")
	rig.waitConnected(t)
	before := acquires(rig.database)

	now := clock.Now()
	clock.Advance(nextUTCMidnight(now).Sub(now) + time.Second)
	delta := conn.delta(t, 3*time.Second, "p")
	if got := acquires(rig.database); got != before {
		t.Fatalf("the shared pool handed out %d connections at midnight, want none", got-before)
	}
	var days []string
	if err := json.Unmarshal(delta.Set["days"], &days); err != nil {
		t.Fatalf("decode the delta's days %s: %v", delta.Set["days"], err)
	}
	var want struct {
		Days []string `json:"days"`
	}
	if err := json.Unmarshal(getBody(t, rig.handler, "/api/v1/delivery/pipeline?at="+clock.Now().Format(time.RFC3339Nano)), &want); err != nil {
		t.Fatalf("decode GET /pipeline: %v", err)
	}
	if !slices.Equal(days, want.Days) {
		t.Fatalf("after midnight the delta's days = %v, GET /pipeline answers %v", days, want.Days)
	}
}

func TestAPresetRecomputeWhereOnlyTheWindowMovedSendsNothing(t *testing.T) {
	rig := newHubRig(t, nil)
	seedHubSettings(t, rig.database)
	// A pull request of another repository, merged before the window: no view shows it, but a change
	// to it is before the timeline's end, so it recomputes the view.
	old := hubPR(1, time.Now().UTC().Add(-30*24*time.Hour))
	old.Repo, old.URL = "acme/other", "https://github.com/acme/other/pull/1"
	upsertPR(t, rig.database, old)
	conn := newFakeLiveConn()
	rig.hub.Subscribe(conn, subscribeFrame("p", presetView(deliveryLiveTimeline, 7, "")))
	conn.snapshot(t, 5*time.Second, "p")

	start := rig.mark()
	old.Title = "feat: a title no view shows"
	upsertPR(t, rig.database, old)
	rig.waitFor(t, start, 3*time.Second, "the recompute of the change", func(note deliveryHubNote) bool {
		return note.what == "recompute end" && note.cause == deliveryCauseChange
	})
	conn.none(t, time.Second, "a recompute whose window alone moved")
}

func TestFreshnessIsSentOnlyWhenItsVerdictChanges(t *testing.T) {
	clock := &fakeHubClock{}
	rig := newClockedHubRig(t, clock)
	seedHubSettings(t, rig.database)
	conn := newFakeLiveConn()
	// A custom range has no expiry, so the clock's move sends nothing but freshness.
	rig.hub.Subscribe(conn, subscribeFrame("m", measuresRange(time.Date(2024, 6, 1, 0, 0, 0, 0, time.UTC), time.Date(2024, 6, 8, 0, 0, 0, 0, time.UTC), "")))
	conn.snapshot(t, 5*time.Second, "m")
	rig.waitConnected(t)
	pool := rig.database.Pool

	for range 2 {
		if err := delivery.RecordReconcileSuccess(t.Context(), pool, clock.Now()); err != nil {
			t.Fatalf("record a reconcile: %v", err)
		}
		conn.none(t, time.Second, "a reconcile that changed no verdict")
	}
	if err := delivery.RecordReconcileError(t.Context(), pool, "GitHub answered 502"); err != nil {
		t.Fatalf("record a reconcile error: %v", err)
	}
	failed := conn.freshness(t, 3*time.Second, "the freshness frame naming the error")
	if failed.Freshness.LastError == nil || *failed.Freshness.LastError != "GitHub answered 502" || len(failed.Stale) != 0 {
		t.Fatalf("the error's freshness frame = %+v, want last_error \"GitHub answered 502\" and nothing stale", failed)
	}
	if err := delivery.RecordReconcileSuccess(t.Context(), pool, clock.Now()); err != nil {
		t.Fatalf("record the next reconcile: %v", err)
	}
	cleared := conn.freshness(t, 3*time.Second, "the freshness frame clearing the error")
	if cleared.Freshness.LastError != nil || len(cleared.Stale) != 0 {
		t.Fatalf("the clearing freshness frame = %+v, want no last_error and nothing stale", cleared)
	}
	conn.none(t, time.Second, "the clearing frame")

	// No reconcile for three intervals.
	clock.Advance(deliveryStaleAfter + time.Second)
	stale := conn.freshness(t, 3*time.Second, "the freshness frame naming the stale sources")
	if !slices.Equal(stale.Stale, []string{"prs", "runs", "ci"}) {
		t.Fatalf("the stale freshness frame names %v, want [prs runs ci]", stale.Stale)
	}
	conn.none(t, time.Second, "the stale frame")
}

func TestAnUnfetchablePullRequestSendsAFreshnessFrame(t *testing.T) {
	rig := newHubRig(t, nil)
	seedHubSettings(t, rig.database)
	upsertPR(t, rig.database, hubPR(1, time.Now().UTC().Add(-3*time.Hour)))
	conn := newFakeLiveConn()
	rig.hub.Subscribe(conn, subscribeFrame("p", presetView(deliveryLiveTimeline, 7, "")))
	snapshot := conn.snapshot(t, 5*time.Second, "p")
	rig.waitConnected(t)

	if err := delivery.MarkPullRequestUnfetchable(t.Context(), rig.database.Pool, "acme/widgets", 1, "the pull request is gone"); err != nil {
		t.Fatalf("mark the pull request unfetchable: %v", err)
	}
	delta := conn.delta(t, 3*time.Second, "p")
	if got := liveUpsertIDs(t, delta, "prs"); !slices.Equal(got, []string{"acme/widgets#1"}) {
		t.Fatalf("the mark's delta upserts %v, want acme/widgets#1", got)
	}
	requireLandsOnGET(t, rig.handler, deliveryLiveTimeline, snapshot.Body, delta, "")
	fresh := conn.freshness(t, 3*time.Second, "the freshness frame carrying the unfetchable count")
	if fresh.Freshness.UnfetchableCount != 1 {
		t.Fatalf("the freshness frame's unfetchable_count = %d, want 1", fresh.Freshness.UnfetchableCount)
	}
	conn.none(t, time.Second, "the mark's delta and freshness frame")
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

// TestABurstCoalescesIntoOneRecompute writes 50 merges on a clock that stands still, so the burst
// is one instant to the hub however long the writes take: nothing is due until the clock moves
// viewQuiet past the last mark, and then one recompute sends all 50 in one delta.
func TestABurstCoalescesIntoOneRecompute(t *testing.T) {
	clock := newStillHubClock()
	rig := newClockedHubRig(t, clock)
	seedHubSettings(t, rig.database)
	conn := newFakeLiveConn()
	rig.hub.Subscribe(conn, subscribeFrame("p", presetView(deliveryLiveTimeline, 7, "")))
	conn.snapshot(t, 5*time.Second, "p")

	start := rig.mark()
	now := clock.Now()
	for n := 1; n <= 50; n++ {
		upsertPR(t, rig.database, hubPR(n, now.Add(-time.Duration(n)*time.Minute)))
	}
	rig.hub.catchUpNow()
	conn.none(t, 2*viewQuiet, "a burst the clock has not moved past")
	if n := rig.count(start, recomputeBegin(deliveryCauseChange)); n != 0 {
		t.Fatalf("recomputes before the burst's quiet passed = %d, want 0", n)
	}

	clock.Advance(viewQuiet)
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
