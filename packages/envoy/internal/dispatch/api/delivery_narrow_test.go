package api

import (
	"encoding/json"
	"net/http"
	"net/url"
	"reflect"
	"strconv"
	"testing"
	"time"

	"github.com/sjawhar/envoy/internal/dispatch/delivery"
	"github.com/sjawhar/envoy/internal/dispatch/delivery/measures"
	"github.com/sjawhar/envoy/internal/dispatch/model"
	"github.com/sjawhar/envoy/internal/dispatch/store"
)

// narrowFrom is the narrowing tests' window start: five past a UTC hour, so the next UTC midnight
// falls inside the window.
var narrowFrom = time.Date(2024, 3, 1, 5, 0, 0, 0, time.UTC)

const narrowWindow = 7 * 24 * time.Hour

// seedNarrowingFixture seeds a deploy repository's pull requests and deploy runs around
// narrowFrom: #1 shipped before it; #2 and #3 still waiting at it, both shipped an hour after it by
// run 301, whose head commit is before it; #4, #5, #6 and #7 merged inside it (#4 and #5 shipped
// three hours in by run 302, #6 of another repository and never tracked, #7 four days in and never
// shipped), a run that failed, and run 304, which ships nothing new. #5 and #6 name issues and
// sessions, so their titles
// leave with them.
func seedNarrowingFixture(t *testing.T, database *store.Store) delivery.DeliverySettings {
	t.Helper()
	ctx := t.Context()
	pool := database.Pool
	settings, err := delivery.PutSettings(ctx, pool, delivery.DeliverySettings{
		DeployRepo: "acme/widgets", DeployWorkflowPath: ".github/workflows/deploy.yml",
		ProductionJobName: "release / release", PRChecksWorkflowPath: ".github/workflows/pr-checks.yml",
		PopulationAuthors: []string{"octocat", "hubot"},
	}, model.Actor{Kind: "system", ID: "test"})
	if err != nil {
		t.Fatalf("seed settings: %v", err)
	}
	seedDeliveryIssueFacts(t, database)
	recordSessionTitles(t, database, narrowFrom.Add(-time.Hour), map[string]string{"s1": "Planner", "s2": "Reviewer"})

	at := func(d time.Duration) *time.Time { v := narrowFrom.Add(d); return &v }
	day := 24 * time.Hour
	acme1, acme3 := "ACME-1", "ACME-3"
	for _, pr := range []delivery.DeliveryPullRequest{
		{Repo: "acme/widgets", Number: 1, Title: "feat: shipped before", Author: "octocat", MergedAt: at(-3 * day)},
		{Repo: "acme/widgets", Number: 2, Title: "feat: ships an hour after from", Author: "octocat", MergedAt: at(-2 * day)},
		{Repo: "acme/widgets", Number: 3, Title: "feat: ships after from", Author: "hubot", MergedAt: at(-1 * day)},
		{Repo: "acme/widgets", Number: 4, Title: "feat: ships in two days", Author: "octocat", MergedAt: at(30 * time.Minute)},
		{Repo: "acme/widgets", Number: 5, Title: "feat: ships at once", Author: "octocat", MergedAt: at(time.Hour), IssueKey: &acme1, Sessions: []string{"s1"}},
		{Repo: "acme/other", Number: 6, Title: "feat: elsewhere", Author: "hubot", MergedAt: at(10 * time.Minute), IssueKey: &acme3, Sessions: []string{"s2"}},
		{Repo: "acme/widgets", Number: 7, Title: "fix: later", Author: "octocat", MergedAt: at(4 * day)},
	} {
		pr.URL = "https://github.com/" + pr.Repo + "/pull/" + strconv.Itoa(pr.Number)
		created := pr.MergedAt.Add(-2 * time.Hour)
		pr.CreatedAt = &created
		if pr.Sessions == nil {
			pr.Sessions = []string{}
		}
		if err := delivery.UpsertPullRequest(ctx, pool, pr); err != nil {
			t.Fatalf("seed PR %s#%d: %v", pr.Repo, pr.Number, err)
		}
	}
	success, failure := delivery.DeliveryRunConclusionSuccess, delivery.DeliveryRunConclusionFailure
	jobSuccess, jobFailure, jobSkipped := delivery.DeliveryJobConclusionSuccess, delivery.DeliveryJobConclusionFailure, delivery.DeliveryJobConclusionSkipped
	for _, run := range []struct {
		id               int64
		head, start, end time.Duration
		conclusion       delivery.DeliveryRunConclusion
		jobs             []delivery.DeliveryRunJob
	}{
		// Ships #1 before from.
		{id: 300, head: -60 * time.Hour, start: -59 * time.Hour, end: -58 * time.Hour, conclusion: success},
		// Its head is before from; ships #3 an hour after from.
		{id: 301, head: -12 * time.Hour, start: -11 * time.Hour, end: time.Hour, conclusion: success},
		// Ships #5 at once.
		{id: 302, head: 2 * time.Hour, start: 2 * time.Hour, end: 3 * time.Hour, conclusion: success},
		// Fails its build; ships nothing.
		{id: 303, head: 12 * time.Hour, start: 12 * time.Hour, end: 13 * time.Hour, conclusion: failure, jobs: []delivery.DeliveryRunJob{
			{Name: "build", StartedAt: at(12 * time.Hour), CompletedAt: at(12*time.Hour + 20*time.Minute), Conclusion: &jobFailure},
			{Name: "release / release", Conclusion: &jobSkipped},
		}},
		// Ships #4 two days in (its head after #4's merge).
		{id: 304, head: 40 * time.Minute, start: 46 * time.Hour, end: 2 * day, conclusion: success},
	} {
		if err := delivery.UpsertRun(ctx, pool, delivery.DeliveryRun{
			Repo: "acme/widgets", RunID: run.id, Kind: delivery.DeliveryRunKindDeploy, HeadSHA: "sha" + strconv.FormatInt(run.id, 10),
			HeadCommitAt: *at(run.head), StartedAt: *at(run.start), CompletedAt: at(run.end), Conclusion: new(run.conclusion),
			URL: "https://github.com/acme/widgets/actions/runs/" + strconv.FormatInt(run.id, 10), HeadBranch: new("main"), Event: new("push"),
		}); err != nil {
			t.Fatalf("seed run %d: %v", run.id, err)
		}
		jobs := run.jobs
		if jobs == nil {
			jobs = []delivery.DeliveryRunJob{{Name: "release / release", StartedAt: at(run.start), CompletedAt: at(run.end), Conclusion: &jobSuccess}}
		}
		for i := range jobs {
			jobs[i].Repo, jobs[i].RunID = "acme/widgets", run.id
		}
		if err := delivery.UpsertRunJobs(ctx, pool, "acme/widgets", run.id, jobs, true); err != nil {
			t.Fatalf("seed run %d's jobs: %v", run.id, err)
		}
	}
	return settings
}

// asJSONTree is v as decoded JSON, without its computed_at fields: what the routes answer, compared
// field for field.
func asJSONTree(t *testing.T, v any) map[string]any {
	t.Helper()
	data, ok := v.([]byte)
	if !ok {
		var err error
		if data, err = json.Marshal(v); err != nil {
			t.Fatalf("encode: %v", err)
		}
	}
	var tree map[string]any
	if err := json.Unmarshal(data, &tree); err != nil {
		t.Fatalf("decode: %v", err)
	}
	delete(tree, "computed_at")
	if inner, ok := tree["measures"].(map[string]any); ok {
		delete(inner, "computed_at")
	}
	return tree
}

func getRaw(t *testing.T, handler http.Handler, target string) []byte {
	t.Helper()
	response := dispatchRequest(t, handler, http.MethodGet, target, nil, "alice")
	if response.Code != http.StatusOK {
		t.Fatalf("GET %s: status = %d, body = %s", target, response.Code, response.Body.String())
	}
	return response.Body.Bytes()
}

func assertSameTree(t *testing.T, what string, got, want map[string]any) {
	t.Helper()
	if reflect.DeepEqual(got, want) {
		return
	}
	for key := range want {
		if !reflect.DeepEqual(got[key], want[key]) {
			gotJSON, _ := json.Marshal(got[key])
			wantJSON, _ := json.Marshal(want[key])
			t.Errorf("%s: %q differs\n narrowed: %s\n fresh:    %s", what, key, gotJSON, wantJSON)
		}
	}
	for key := range got {
		if _, ok := want[key]; !ok {
			t.Errorf("%s: narrowed answer has %q, fresh has none", what, key)
		}
	}
}

// TestANarrowedPopulationComputesWhatAFreshReadReturns: a population read for [from, to) and
// narrowed to a later start computes, for that later window, what GET answers for it (except
// computed_at): pull requests and runs leave, a deploy-repository pull request merged before the
// new start joins the waiting line until it ships, one of another repository does not, and the
// titles and counts keep only what the remaining rows name. Same for /measures.
func TestANarrowedPopulationComputesWhatAFreshReadReturns(t *testing.T) {
	handler, database, deps := newTestServer(t, testServerOptions{})
	settings := seedNarrowingFixture(t, database)
	s := directServer(deps)
	ctx := t.Context()
	from, to := narrowFrom, narrowFrom.Add(narrowWindow)
	oldestMerge := narrowFrom.Add(10 * time.Minute)
	nextMidnight := time.Date(2024, 3, 2, 0, 0, 0, 0, time.UTC)

	for _, rawQuery := range []string{"", "author=octocat", "q=feat&priority=P0"} {
		query, err := url.ParseQuery(rawQuery)
		if err != nil {
			t.Fatalf("parse %q: %v", rawQuery, err)
		}
		timeline, err := s.readDeliveryPopulation(ctx, settings, from, to, query, true)
		if err != nil {
			t.Fatalf("read the timeline population: %v", err)
		}
		measured, err := s.readDeliveryPopulation(ctx, settings, from, to, query, false)
		if err != nil {
			t.Fatalf("read the measures population: %v", err)
		}
		for _, narrowedFrom := range []time.Time{from.Add(time.Minute), oldestMerge.Add(time.Second), nextMidnight, from.Add(3 * 24 * time.Hour)} {
			narrowedTo := to.Add(narrowedFrom.Sub(from))
			window := "from=" + narrowedFrom.Format(time.RFC3339) + "&to=" + narrowedTo.Format(time.RFC3339)
			if rawQuery != "" {
				window += "&" + rawQuery
			}
			label := window

			got := deliveryTimelineCompute(settings, timeline.narrowedTo(settings, narrowedFrom), narrowedFrom, narrowedTo, narrowedTo)
			assertSameTree(t, "timeline "+label, asJSONTree(t, got), asJSONTree(t, getRaw(t, handler, "/api/v1/delivery/timeline?"+window)))

			gotMeasures := deliveryMeasuresCompute(settings, measured.narrowedTo(settings, narrowedFrom), narrowedFrom, narrowedTo, narrowedTo)
			assertSameTree(t, "measures "+label, asJSONTree(t, gotMeasures), asJSONTree(t, getRaw(t, handler, "/api/v1/delivery/measures?"+window)))
		}
	}

	// The fixture moves what the test claims it does: just after from, #2 and #3 still wait and
	// ship an hour in; by from+1d they have shipped, and #4, #5 and #6 have left the pull requests
	// without joining the waiting line (#4 and #5 shipped three hours in by run 302, whose head
	// follows their merges; #6 is another repository's); by from+3d only #7 remains, and #6's issue
	// and session titles have gone.
	timeline, err := s.readDeliveryPopulation(ctx, settings, from, to, url.Values{}, true)
	if err != nil {
		t.Fatalf("read: %v", err)
	}
	narrowedBody := func(at time.Time) deliveryTimelineWithMeasures {
		return deliveryTimelineCompute(settings, timeline.narrowedTo(settings, at), at, to.Add(at.Sub(from)), at)
	}
	shipsAt := func(waiting delivery.DeliveryWaitingPRView, merged, deployed time.Duration) bool {
		return waiting.MergedAt.Equal(narrowFrom.Add(merged)) && waiting.DeployedAt != nil && waiting.DeployedAt.Equal(narrowFrom.Add(deployed))
	}
	day := 24 * time.Hour
	early := narrowedBody(from.Add(time.Minute))
	if len(early.Waiting) != 2 || !shipsAt(early.Waiting[0], -2*day, time.Hour) || !shipsAt(early.Waiting[1], -day, time.Hour) {
		t.Fatalf("waiting at from+1m = %+v, want #2 and #3, each shipping an hour in", early.Waiting)
	}
	mid := narrowedBody(from.Add(day))
	if len(mid.Waiting) != 0 || len(mid.PRs) != 1 {
		t.Fatalf("at from+1d: waiting = %+v and prs = %v, want no waiting and only #7", mid.Waiting, deliveryPRIDs(mid.PRs))
	}
	late := narrowedBody(from.Add(3 * day))
	if ids := deliveryPRIDs(late.PRs); !reflect.DeepEqual(ids, []string{"acme/widgets#7"}) {
		t.Fatalf("prs at from+3d = %v, want only #7", ids)
	}
	if len(late.Waiting) != 0 {
		t.Fatalf("waiting at from+3d = %+v, want none (#2, #3 and #4 shipped before it; #6 is another repository's)", late.Waiting)
	}
	if _, named := late.IssueTitles["ACME-3"]; named {
		t.Fatalf("issue_titles still names ACME-3 after #6 left: %v", late.IssueTitles)
	}
	if _, named := late.SessionTitles["s2"]; named {
		t.Fatalf("session_titles still names s2 after #6 left: %v", late.SessionTitles)
	}
}

// TestANarrowedPipelinePopulationComputesWhatAFreshReadReturns: the Pipeline population read at
// `at` and narrowed to the next UTC midnight, and to just past the earliest timed result's 24-hour
// edge, computes what GET answers for that `at` (except computed_at).
func TestANarrowedPipelinePopulationComputesWhatAFreshReadReturns(t *testing.T) {
	handler, database, deps := newTestServer(t, testServerOptions{})
	settings := pipelineSettings()
	seedPipelineFixture(t, database, settings)
	settings, err := delivery.GetSettings(t.Context(), database.Pool)
	if err != nil {
		t.Fatalf("read settings: %v", err)
	}
	for _, pr := range []struct {
		number          int
		created, merged time.Time
	}{
		{11, pipelineFixtureTime(32, 7, 0), pipelineFixtureTime(32, 7, 10)},
		{12, pipelineFixtureTime(32, 9, 0), pipelineFixtureTime(32, 10, 0)},
		{13, pipelineFixtureTime(27, 9, 0), pipelineFixtureTime(27, 10, 0)},
	} {
		if err := delivery.UpsertPullRequest(t.Context(), database.Pool, delivery.DeliveryPullRequest{
			Repo: "acme/widgets", Number: pr.number, Title: "feat", Author: "octocat", Sessions: []string{},
			URL: "https://github.com/acme/widgets/pull/" + strconv.Itoa(pr.number), CreatedAt: &pr.created, MergedAt: &pr.merged,
		}); err != nil {
			t.Fatalf("seed PR %d: %v", pr.number, err)
		}
	}
	at, err := time.Parse(time.RFC3339, pipelineAt)
	if err != nil {
		t.Fatal(err)
	}
	pop, err := directServer(deps).readPipelinePopulation(t.Context(), settings, at)
	if err != nil {
		t.Fatalf("read the pipeline population: %v", err)
	}
	// The earliest timed run completed in the 24 hours before at is run 3, at 13:30 on Oct 1.
	failingEdge := pipelineFixtureTime(31, 13, 30).Add(measures.FailingWindow + time.Second)
	for _, later := range []time.Time{failingEdge, time.Date(2026, 10, 3, 0, 0, 0, 0, time.UTC)} {
		got := deliveryPipelineCompute(settings, pop.narrowedTo(settings, later), later, later)
		want := getRaw(t, handler, "/api/v1/delivery/pipeline?at="+later.Format(time.RFC3339))
		assertSameTree(t, "pipeline at "+later.Format(time.RFC3339), asJSONTree(t, got), asJSONTree(t, want))
	}
}

// TestNextExpiryIsTheOldestMergeLeaving: with nothing else leaving sooner, a preset window's next
// expiry is just past the oldest pull request's merge plus the window's length.
func TestNextExpiryIsTheOldestMergeLeaving(t *testing.T) {
	now := time.Date(2026, 9, 10, 12, 0, 0, 0, time.UTC)
	n := 7 * 24 * time.Hour
	oldest := now.Add(-n + time.Hour)
	pop := deliveryPopulation{prs: []delivery.DeliveryPullRequest{
		{Repo: "acme/widgets", Number: 1, MergedAt: &oldest},
		{Repo: "acme/widgets", Number: 2, MergedAt: new(now.Add(-time.Hour))},
	}}
	want := oldest.Add(n).Add(time.Nanosecond)
	if got := nextExpiry(pop, n, now); !got.Equal(want) {
		t.Fatalf("nextExpiry = %v, want %v (the oldest merge leaving)", got, want)
	}
}

// TestNextExpiryIsMidnightWhenNothingLeavesSooner: with every row well inside the window, the next
// expiry is the next UTC midnight, when the daily series moves.
func TestNextExpiryIsMidnightWhenNothingLeavesSooner(t *testing.T) {
	now := time.Date(2026, 9, 10, 12, 0, 0, 0, time.UTC)
	n := 7 * 24 * time.Hour
	recent := now.Add(-time.Hour)
	pop := deliveryPopulation{
		prs:        []delivery.DeliveryPullRequest{{Repo: "acme/widgets", Number: 1, MergedAt: &recent}},
		windowRuns: []delivery.DeliveryRun{{RunID: 9, StartedAt: recent, HeadCommitAt: recent}},
	}
	want := time.Date(2026, 9, 11, 0, 0, 0, 0, time.UTC)
	if got := nextExpiry(pop, n, now); !got.Equal(want) {
		t.Fatalf("nextExpiry = %v, want %v (the next UTC midnight)", got, want)
	}
}

// TestPipelineNextExpiryIsTheFailingWindowEdge: the Pipeline's next expiry is just past the
// earliest timed PR-checks result's completion plus FailingWindow when that comes before the next
// UTC midnight, and the midnight otherwise.
func TestPipelineNextExpiryIsTheFailingWindowEdge(t *testing.T) {
	now := time.Date(2026, 9, 10, 12, 0, 0, 0, time.UTC)
	completed := now.Add(-20 * time.Hour)
	pop := pipelinePopulation{checks: delivery.RunsWithJobs{Runs: []delivery.DeliveryRun{
		{RunID: 1, StartedAt: completed.Add(-time.Hour), CompletedAt: &completed, Conclusion: new(delivery.DeliveryRunConclusionFailure)},
		{RunID: 2, StartedAt: now.Add(-time.Hour), CompletedAt: new(now.Add(-30 * time.Minute)), Conclusion: new(delivery.DeliveryRunConclusionSuccess)},
	}}}
	want := completed.Add(measures.FailingWindow).Add(time.Nanosecond)
	if got := pipelineNextExpiry(pop, now); !got.Equal(want) {
		t.Fatalf("pipelineNextExpiry = %v, want %v (run 1 leaving the failing window)", got, want)
	}
	late := time.Date(2026, 9, 10, 22, 0, 0, 0, time.UTC)
	if got, midnight := pipelineNextExpiry(pop, late), time.Date(2026, 9, 11, 0, 0, 0, 0, time.UTC); !got.Equal(midnight) {
		t.Fatalf("pipelineNextExpiry at 22:00 = %v, want the next UTC midnight %v", got, midnight)
	}
}
