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
