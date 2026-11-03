package daemon

import (
	"bytes"
	"context"
	"fmt"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"slices"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/sjawhar/legion/daemon/internal/claim"
	"github.com/sjawhar/legion/daemon/internal/classify"
	"github.com/sjawhar/legion/daemon/internal/ghrepo"
	"github.com/sjawhar/legion/daemon/internal/intake"
	"github.com/sjawhar/legion/daemon/internal/phase"
	"github.com/sjawhar/legion/daemon/internal/record"
	"github.com/sjawhar/legion/daemon/internal/workflow"
)

// pullRequestStandIn is GitHub's REST API for acme/widgets as the per-issue read reads it:
// pull request 86 on base main, open on legion/CAPTURE-1 of acme/widgets itself at head, last
// updated at updated, with no reviews, whose base requires pr-checks-result and is unprotected.
// Listed by its branch (GET /pulls?head=acme:legion/CAPTURE-1&state=open), it is the one open pull
// request there; each such list is counted in listed. Paths match in any case, as GitHub's do.
func pullRequestStandIn(t *testing.T, head string, updated time.Time, listed *atomic.Int32) *httptest.Server {
	t.Helper()
	pull := `{"number":86,"state":"open","html_url":"https://github.com/acme/widgets/pull/86","body":"Dispatch: CAPTURE-1",` +
		`"base":{"ref":"main"},"head":{"ref":"legion/CAPTURE-1","sha":"` + head + `","repo":{"full_name":"acme/widgets"}},"updated_at":"` + updated.Format(time.RFC3339) + `","mergeable":true,"mergeable_state":"clean"}`
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Header.Get("Authorization") != "Bearer outbox-token" {
			http.Error(w, `{"message":"Bad credentials"}`, http.StatusUnauthorized)
			return
		}
		switch strings.ToLower(r.URL.Path) {
		case "/repos/acme/widgets/pulls":
			if r.URL.Query().Get("head") != "acme:legion/CAPTURE-1" || r.URL.Query().Get("state") != "open" {
				t.Errorf("listed pull requests by %q, want the open ones on acme:legion/CAPTURE-1", r.URL.RawQuery)
				w.Write([]byte(`[]`))
				return
			}
			listed.Add(1)
			w.Write([]byte(`[` + pull + `]`))
		case "/repos/acme/widgets/pulls/86":
			w.Write([]byte(pull))
		case "/repos/acme/widgets/pulls/86/reviews":
			w.Write([]byte(`[]`))
		case "/repos/acme/widgets/rules/branches/main":
			w.Write([]byte(`[{"type":"required_status_checks","parameters":{"required_status_checks":[{"context":"pr-checks-result"}]}}]`))
		case "/repos/acme/widgets/branches/main":
			w.Write([]byte(`{"name":"main","protected":false}`))
		default:
			http.NotFound(w, r)
		}
	}))
	t.Cleanup(server.Close)
	return server
}

// headReads is how many facts the read applied from its own read of a pull request. The count is
// by kind and project alone: a synchronize the read makes names the boot that made it, since two
// reads of the same head are two moves to apply, while a push the read rebuilds names only the
// two commits it moved between, so that the same push rebuilt by a later read is one fact
// (readEventID). Matching the boot would see the first kind and never the second.
func headReads(t *testing.T, pool *pgxpool.Pool, kind string) int {
	t.Helper()
	var n int
	if err := pool.QueryRow(context.Background(),
		`select count(*) from processed_events where source = 'github' and event_id like $1`, kind+":CAPTURE:%").Scan(&n); err != nil {
		t.Fatalf("read the %s facts the read applied: %v", kind, err)
	}
	return n
}

// The daemon moves a pull request's recorded head only on a webhook, and an event queue hours
// behind left more than half of one project's records at an old head, so reviews and READY were
// judged against it. The per-issue read already reads each open pull request every two
// minutes: when GitHub's head is not the recorded one, it applies the synchronize the webhook would
// have carried, with GitHub's updated_at, so the handler's own clock fence decides it. A head that
// did not move applies nothing, and a read older than the record's clock moves nothing.
func TestTheReadFollowsAHeadWhoseSynchronizeHasNotArrived(t *testing.T) {
	read := time.Date(2026, 10, 26, 20, 0, 0, 0, time.UTC)
	for _, tc := range []struct {
		name     string
		github   string
		recorded time.Time
		head     string
		clock    time.Time
		facts    int
	}{
		{"the head moved", "pushed", time.Time{}, "pushed", read, 1},
		{"the head did not move", "code", time.Time{}, "code", time.Time{}, 0},
		{"the record's clock is after the read", "pushed", read.Add(time.Minute), "code", read.Add(time.Minute), 1},
	} {
		t.Run(tc.name, func(t *testing.T) {
			pool := isolatedOutboxPool(t)
			seedCapture(t, pool, phase.Testing, "testing", record.PhaseRow{Issue: "CAPTURE-1", Role: claim.RoleTester, Claim: "test-claim"})
			if !tc.recorded.IsZero() {
				if _, err := pool.Exec(context.Background(), "update pull_requests set head_updated_at = $1 where issue = 'CAPTURE-1'", tc.recorded); err != nil {
					t.Fatalf("seed the record's clock: %v", err)
				}
			}
			w := readRuntime(pool, pullRequestStandIn(t, tc.github, read, &atomic.Int32{}).URL, quietLogger())
			readCapture(t, w)
			_, pr := capturePhase(t, pool)
			if pr.HeadSHA != tc.head || !pr.HeadUpdatedAt.Equal(tc.clock) {
				t.Fatalf("after the read the record is at %q (clock %v), want %q (clock %v)", pr.HeadSHA, pr.HeadUpdatedAt, tc.head, tc.clock)
			}
			if got := headReads(t, pool, "head-read"); got != tc.facts {
				t.Fatalf("the read applied %d head facts, want %d", got, tc.facts)
			}
		})
	}
}

// A reviewer approves the head it read, and the implementer pushes above it; the push webhook is
// lost with the synchronize. The read rebuilds the push from the branch's activity and follows the
// new head with it, so the push the webhook would have carried is recorded, and that webhook
// arriving afterwards decides nothing.
func TestTheReadFollowsAPushAboveAnApprovalAsANewHead(t *testing.T) {
	read := time.Date(2026, 10, 26, 20, 0, 0, 0, time.UTC)
	pool := isolatedOutboxPool(t)
	seedCapture(t, pool, phase.Reviewing, "needs_review", record.PhaseRow{Issue: "CAPTURE-1", Role: claim.RoleReviewer, Claim: "review-claim"})
	github := &activityGitHub{}
	github.moved("pushed", read, activityEntry("legion/CAPTURE-1", "push", "code", "pushed", implementerLogin, read.Add(-time.Minute)))
	w := reviewAppRuntime(pool, github.serve(t).URL, quietLogger())
	readCapture(t, w)
	_, pr := capturePhase(t, pool)
	if pr.HeadSHA != "pushed" {
		t.Fatalf("after the read the record is at %q, want pushed", pr.HeadSHA)
	}
	if n := headReads(t, pool, "push-read"); n != 1 {
		t.Fatalf("the read applied %d pushes it rebuilt from the branch's activity, want the one code..pushed", n)
	}
	lateWebhookPushDecidesNothing(t, pool, w, webhookPush("code", "pushed", implementerLogin))
}

// The read lists its records once, when it starts, and a webhook can move one before the read
// reaches it. followHead compares GitHub's head with the record as it stands then, so a head the
// webhook already recorded applies nothing again.
func TestFollowHeadComparesWithTheRecordAsItStandsNow(t *testing.T) {
	read := time.Date(2026, 10, 26, 20, 0, 0, 0, time.UTC)
	pool := isolatedOutboxPool(t)
	seedCapture(t, pool, phase.Testing, "testing", record.PhaseRow{Issue: "CAPTURE-1", Role: claim.RoleTester, Claim: "test-claim"})
	_, listed := capturePhase(t, pool)
	if _, err := pool.Exec(context.Background(), "update pull_requests set head_sha = 'pushed' where issue = 'CAPTURE-1'"); err != nil {
		t.Fatalf("move the record as the webhook would: %v", err)
	}
	w := readRuntime(pool, pullRequestStandIn(t, "pushed", read, &atomic.Int32{}).URL, quietLogger())
	github, pull, err := w.readPullRequest(context.Background(), *listed)
	if err != nil {
		t.Fatalf("read the pull request: %v", err)
	}
	got, err := w.followHead(context.Background(), github, *listed, pull)
	if err != nil || got.HeadSHA != "pushed" {
		t.Fatalf("followHead = %q, %v; want the record at pushed", got.HeadSHA, err)
	}
	if n := headReads(t, pool, "head-read"); n != 0 {
		t.Fatalf("followHead applied %d head facts, want none", n)
	}
}

// A pull request GitHub shows open on an issue's branch that the workflow does not record - here an
// opened older than the closed record's clock - is found again by every read of the issue, applied
// and declined again, and the issue stays where it is. Once the record changes (a new generation
// removes the closed pull request), the same read records it and the issue moves on.
func TestAnOpenedTheWorkflowDoesNotRecordIsAppliedAgainByEveryRead(t *testing.T) {
	opened := time.Date(2026, 10, 26, 19, 30, 0, 0, time.UTC)
	pool := isolatedOutboxPool(t)
	seedDeclinedOpened(t, pool, opened)
	var logs syncBuffer
	var listed atomic.Int32
	w := readRuntime(pool, pullRequestStandIn(t, "handoff", opened, &listed).URL, slog.New(slog.NewTextHandler(&logs, nil)))
	warnings := func() int {
		return strings.Count(logs.String(), "GitHub holds an open pull request for an issue")
	}
	for range 3 {
		readCapture(t, w)
	}
	if listed.Load() != 3 || warnings() != 3 || headReads(t, pool, "opened-read") != 3 {
		t.Fatalf("three reads listed the branch %d times, warned %d times and applied %d opened facts, want 3, 3 and 3",
			listed.Load(), warnings(), headReads(t, pool, "opened-read"))
	}
	if got, _ := capturePhase(t, pool); got != phase.Implementing {
		t.Fatalf("the issue is in %s, want implementing: the workflow records no late opened", got)
	}
	if _, err := pool.Exec(context.Background(), "delete from pull_requests where issue = 'CAPTURE-1'"); err != nil {
		t.Fatalf("remove the closed pull request as a new generation would: %v", err)
	}
	readCapture(t, w)
	if got, pr := capturePhase(t, pool); got != phase.Testing || pr == nil || pr.Number != 86 {
		t.Fatalf("after the record changed the issue is in %s with %+v, want testing on #86", got, pr)
	}
	if got := headReads(t, pool, "opened-read"); got != 4 {
		t.Fatalf("the read applied %d opened facts in all, want 4", got)
	}
}

// seedDeclinedOpened records CAPTURE-1 in implementing with its implementer's handoff and its pull
// request 86 closed at a clock an hour after opened, so an opened of 86 at opened is late and the
// workflow declines it.
func seedDeclinedOpened(t *testing.T, pool *pgxpool.Pool, opened time.Time) {
	t.Helper()
	seedUnrecorded(t, pool, "handoff")
	if err := pgx.BeginFunc(context.Background(), pool, func(tx pgx.Tx) error {
		return record.NewStore().PutPullRequest(context.Background(), tx, record.PullRequest{State: record.PullRequestClosed, Issue: "CAPTURE-1", Repo: "acme/widgets",
			Number: 86, Branch: "legion/CAPTURE-1", HeadSHA: "handoff", HeadUpdatedAt: opened.Add(time.Hour), Failing: []string{}})
	}); err != nil {
		t.Fatalf("seed the closed pull request: %v", err)
	}
}

// The read does not take the list's head filter on trust: a filter GitHub did not apply as meant
// would list other pull requests, so it takes one only when its head is the issue's branch, and one
// on another branch records nothing.
func TestTheReadTakesOnlyAPullRequestOnTheIssuesBranch(t *testing.T) {
	pool := isolatedOutboxPool(t)
	seedUnrecorded(t, pool, "handoff")
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/repos/acme/widgets/pulls" {
			http.NotFound(w, r)
			return
		}
		w.Write([]byte(`[{"number":91,"state":"open","base":{"ref":"main"},"head":{"ref":"it/IT-1","sha":"other"},"updated_at":"2026-10-26T19:30:00Z"}]`))
	}))
	t.Cleanup(server.Close)
	readCapture(t, readRuntime(pool, server.URL, quietLogger()))
	if got, pr := capturePhase(t, pool); got != phase.Implementing || pr != nil {
		t.Fatalf("after the read the issue is in %s with %+v, want implementing with no pull request", got, pr)
	}
	if got := headReads(t, pool, "opened-read"); got != 0 {
		t.Fatalf("the read applied %d opened facts, want none", got)
	}
}

// LateLifecycle lets a clock equal to the record's through, so a read can be no newer than a
// webhook that moved the record after the read listed it. followHead then keeps the record: the
// read is no newer than what moved it, and following it could move the head back. A read newer
// than the record's clock is followed.
func TestFollowHeadKeepsARecordMovedSinceTheListByWhatItReadNoLater(t *testing.T) {
	at := time.Date(2026, 10, 26, 20, 0, 0, 0, time.UTC)
	for _, tc := range []struct {
		name string
		read time.Time
		want string
	}{
		{"a read of the same second", at, "pushed"},
		{"a read after the record's clock", at.Add(time.Second), "older"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			pool := isolatedOutboxPool(t)
			seedCapture(t, pool, phase.Testing, "testing", record.PhaseRow{Issue: "CAPTURE-1", Role: claim.RoleTester, Claim: "test-claim"})
			_, listed := capturePhase(t, pool)
			if _, err := pool.Exec(context.Background(), "update pull_requests set head_sha = 'pushed', head_updated_at = $1 where issue = 'CAPTURE-1'", at); err != nil {
				t.Fatalf("move the record as the webhook would: %v", err)
			}
			w := readRuntime(pool, pullRequestStandIn(t, "older", tc.read, &atomic.Int32{}).URL, quietLogger())
			github, pull, err := w.readPullRequest(context.Background(), *listed)
			if err != nil {
				t.Fatalf("read the pull request: %v", err)
			}
			got, err := w.followHead(context.Background(), github, *listed, pull)
			if err != nil || got.HeadSHA != tc.want {
				t.Fatalf("followHead = %q, %v; want the record at %s", got.HeadSHA, err, tc.want)
			}
			if _, pr := capturePhase(t, pool); pr.HeadSHA != tc.want {
				t.Fatalf("the stored record is at %q, want %s", pr.HeadSHA, tc.want)
			}
		})
	}
}

// seedUnrecorded records CAPTURE-1 in implementing with no pull request, its implementer's
// handoff the commit handoff ("" for none).
func seedUnrecorded(t *testing.T, pool *pgxpool.Pool, handoff string) {
	t.Helper()
	records := record.NewStore()
	if err := pgx.BeginFunc(context.Background(), pool, func(tx pgx.Tx) error {
		ctx := context.Background()
		if err := records.PutIssue(ctx, tx, record.Issue{Key: "CAPTURE-1", Repo: "acme/widgets", DefaultBranch: "main", GitHubRepo: "acme/widgets", Tree: "CAPTURE-1", Project: "CAPTURE", Title: "root",
			Phase: phase.Implementing, Generation: 1, Status: "in_progress", Rank: "U"}); err != nil {
			return err
		}
		return records.PutPhase(ctx, tx, record.PhaseRow{Issue: "CAPTURE-1", Role: claim.RoleImplementer, Claim: "implement-claim", HandoffCommit: handoff, Summary: "implemented"})
	}); err != nil {
		t.Fatalf("seed the issue: %v", err)
	}
}

// An implementer's completion moves its issue to testing only once the pull request is recorded,
// and the record comes only from the opened webhook: one lost, or hours back in the queue, held
// the issue in implementing with its pull request open on GitHub. The read looks up the open pull
// request on the branch of each implementing issue with a recorded handoff and no open record, and
// applies the opened the webhook would have carried, which records it and moves the issue on; a
// pull request the record holds closed is applied as its reopen. It names the repository as a
// webhook's fact does, the mapping's spelling when the mapping serves it, so the webhooks that
// follow find the record. An issue whose implementer has not completed, or whose pull request is
// recorded open or merged, costs no read.
func TestTheReadRecordsAnOpenPullRequestWhoseOpenedHasNotArrived(t *testing.T) {
	opened := time.Date(2026, 10, 26, 19, 30, 0, 0, time.UTC)
	for _, tc := range []struct {
		name     string
		handoff  string
		recorded record.PullRequestState
		served   string
		want     phase.Phase
		listed   int32
		repo     string
	}{
		{"the implementer completed", "handoff", "", "", phase.Testing, 1, "acme/widgets"},
		{"the implementer completed on a pull request recorded closed", "handoff", record.PullRequestClosed, "", phase.Testing, 1, "acme/widgets"},
		{"the mapping spells the repository otherwise", "handoff", "", "Acme/Widgets", phase.Testing, 1, "Acme/Widgets"},
		{"the implementer has not completed", "", "", "", phase.Implementing, 0, ""},
		{"the pull request is recorded open", "handoff", record.PullRequestOpen, "", phase.Implementing, 0, ""},
		{"the pull request is recorded merged", "handoff", record.PullRequestMerged, "", phase.Implementing, 0, ""},
	} {
		t.Run(tc.name, func(t *testing.T) {
			pool := isolatedOutboxPool(t)
			seedUnrecorded(t, pool, tc.handoff)
			if tc.recorded != "" {
				if err := pgx.BeginFunc(context.Background(), pool, func(tx pgx.Tx) error {
					return record.NewStore().PutPullRequest(context.Background(), tx, record.PullRequest{State: tc.recorded, Issue: "CAPTURE-1", Repo: "acme/widgets",
						Number: 86, Branch: "legion/CAPTURE-1", HeadSHA: "handoff", HeadUpdatedAt: opened.Add(-time.Hour), Failing: []string{}})
				}); err != nil {
					t.Fatalf("seed the recorded pull request: %v", err)
				}
			}
			var listed atomic.Int32
			w := readRuntime(pool, pullRequestStandIn(t, "handoff", opened, &listed).URL, quietLogger())
			if tc.served != "" {
				w.repositories = newRepositories("CAPTURE", ghrepo.Renames{})
				w.repositories.set([]mappedRepository{{Repo: ghrepo.MustParse(tc.served), Default: true}}, nil)
			}
			w.readIssuesAtStart(context.Background())
			got, pr := capturePhase(t, pool)
			if got != tc.want {
				t.Fatalf("after the read the issue is in %s, want %s", got, tc.want)
			}
			if listed.Load() != tc.listed {
				t.Fatalf("the read listed the branch's pull requests %d times, want %d", listed.Load(), tc.listed)
			}
			if tc.listed == 0 {
				if (pr != nil) != (tc.recorded != "") {
					t.Fatalf("the read left the record %+v, want it as seeded", pr)
				}
				return
			}
			if pr == nil || pr.State != record.PullRequestOpen || pr.Repo != tc.repo || pr.Number != 86 || pr.HeadSHA != "handoff" || pr.Branch != "legion/CAPTURE-1" || !pr.HeadUpdatedAt.Equal(opened) {
				t.Fatalf("after the read the record is %+v, want #86 of %s open on legion/CAPTURE-1 at handoff, clock %v", pr, tc.repo, opened)
			}
			if got := headReads(t, pool, "opened-read"); got != 1 {
				t.Fatalf("the read applied %d opened facts, want 1", got)
			}
		})
	}
}

// pathReads counts a stand-in's requests by path, its query left out.
type pathReads struct {
	mu    sync.Mutex
	paths map[string]int
}

func (r *pathReads) add(path string) {
	r.mu.Lock()
	defer r.mu.Unlock()
	if r.paths == nil {
		r.paths = map[string]int{}
	}
	r.paths[path]++
}

// counts is every path read, with how many times.
func (r *pathReads) counts() map[string]int {
	r.mu.Lock()
	defer r.mu.Unlock()
	counts := map[string]int{}
	for path, n := range r.paths {
		counts[path] = n
	}
	return counts
}

// tablesSnapshot is every row of tables as JSON.
func tablesSnapshot(t *testing.T, pool *pgxpool.Pool, tables ...string) string {
	t.Helper()
	var texts []string
	for _, table := range tables {
		var rows string
		if err := pool.QueryRow(context.Background(), "select coalesce(json_agg(to_jsonb(t) order by to_jsonb(t)::text), '[]')::text from "+table+" t").Scan(&rows); err != nil {
			t.Fatalf("read the %s: %v", table, err)
		}
		texts = append(texts, table+": "+rows)
	}
	return strings.Join(texts, "\n")
}

// lateWebhookPushDecidesNothing applies push as the webhook of a push the read already rebuilt
// would carry it, arriving afterwards, and fails t when it changed the issues, their phases, the
// production checks, the outbox, or the pull request's judgement, pushes or head run. The record's
// moves may change: the copy is kept as its move's push or a repeat of it (classify.ApplyPush),
// which only a head's next arrival by that move reads.
func lateWebhookPushDecidesNothing(t *testing.T, pool *pgxpool.Pool, w *workflowRuntime, push intake.Push) {
	t.Helper()
	others := []string{"issues", "phases", "production_checks", "outbox"}
	before := tablesSnapshot(t, pool, others...)
	_, prBefore := capturePhase(t, pool)
	if _, err := intake.ApplyFact(context.Background(), pool, "github", "late-webhook", push, w.handlers...); err != nil {
		t.Fatalf("apply the late webhook: %v", err)
	}
	if after := tablesSnapshot(t, pool, others...); after != before {
		t.Fatalf("the late push webhook %s..%s changed the records:\nbefore %s\nafter  %s", push.Before, push.After, before, after)
	}
	_, prAfter := capturePhase(t, pool)
	if classify.JudgementChanged(*prBefore, *prAfter) || !slices.Equal(prAfter.Pushes, prBefore.Pushes) || (prAfter.Run == nil) != (prBefore.Run == nil) {
		t.Fatalf("the late push webhook %s..%s changed the pull request: judgement or pushes %+v, run open %t; was %+v, run open %t",
			push.Before, push.After, prAfter.Pushes, prAfter.Run != nil, prBefore.Pushes, prBefore.Run != nil)
	}
}

// reviewAppLogin and implementerLogin are the bot accounts GitHub names the review App's and the
// implement App's pushes by, in the push webhook's pusher.name and in the branch's activity alike.
const (
	reviewAppLogin   = "legion-review[bot]"
	implementerLogin = "legion-implementer[bot]"
)

// activityEntry is one entry of GitHub's activity of branch (GET /activity) as GitHub answers it:
// its kind (push, force_push, branch_creation, branch_deletion), the commits the branch held before
// and after it, when, and its actor, of type Bot for an App's <slug>[bot].
func activityEntry(branch, kind, before, after, actor string, at time.Time) string {
	actorType := "User"
	if strings.HasSuffix(actor, "[bot]") {
		actorType = "Bot"
	}
	return fmt.Sprintf(`{"id":%d,"node_id":"RA_%s","before":%q,"after":%q,"ref":"refs/heads/%s","timestamp":%q,"activity_type":%q,`+
		`"actor":{"login":%q,"id":41898282,"node_id":"BOT_1","type":%q,"site_admin":false}}`,
		at.Unix(), after, before, after, branch, at.Format(time.RFC3339), kind, actor, actorType)
}

// activityGitHub is GitHub's REST API for acme/widgets as the per-issue read reads it after push
// webhooks of legion/CAPTURE-1 were lost: pull request 86 open on that branch at head, last
// updated at updated, with no reviews, on base main, which requires pr-checks-result and is
// unprotected; and the branch's activity, newest first, is activity, or GitHub answers
// activityStatus when it is set. Every request is counted in reads.
type activityGitHub struct {
	mu             sync.Mutex
	head           string
	updated        time.Time
	activity       []string
	activityStatus int
	reads          pathReads
}

// moved has GitHub show pull request 86 at head, last updated at updated, and the branch's
// activity as activity, newest first.
func (g *activityGitHub) moved(head string, updated time.Time, activity ...string) {
	g.mu.Lock()
	defer g.mu.Unlock()
	g.head, g.updated, g.activity = head, updated, activity
}

func (g *activityGitHub) serve(t *testing.T) *httptest.Server {
	t.Helper()
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		g.reads.add(r.URL.Path)
		if r.Header.Get("Authorization") != "Bearer outbox-token" {
			http.Error(w, `{"message":"Bad credentials"}`, http.StatusUnauthorized)
			return
		}
		g.mu.Lock()
		defer g.mu.Unlock()
		pull := fmt.Sprintf(`{"number":86,"state":"open","html_url":"https://github.com/acme/widgets/pull/86","body":"","base":{"ref":"main"},`+
			`"head":{"ref":"legion/CAPTURE-1","sha":%q,"repo":{"full_name":"acme/widgets"}},"updated_at":%q,"mergeable":true,"mergeable_state":"clean"}`,
			g.head, g.updated.Format(time.RFC3339))
		switch r.URL.Path {
		case "/repos/acme/widgets/activity":
			if ref, perPage := r.URL.Query().Get("ref"), r.URL.Query().Get("per_page"); ref != "refs/heads/legion/CAPTURE-1" || perPage != "100" {
				t.Errorf("read the activity of %q, %q to a page; want refs/heads/legion/CAPTURE-1, 100", ref, perPage)
			}
			if g.activityStatus != 0 {
				http.Error(w, `{"message":"Server Error"}`, g.activityStatus)
				return
			}
			w.Write([]byte("[" + strings.Join(g.activity, ",") + "]"))
		case "/repos/acme/widgets/pulls":
			w.Write([]byte("[" + pull + "]"))
		case "/repos/acme/widgets/pulls/86":
			w.Write([]byte(pull))
		case "/repos/acme/widgets/pulls/86/reviews":
			w.Write([]byte(`[]`))
		case "/repos/acme/widgets/rules/branches/main":
			w.Write([]byte(`[{"type":"required_status_checks","parameters":{"required_status_checks":[{"context":"pr-checks-result"}]}}]`))
		case "/repos/acme/widgets/branches/main":
			w.Write([]byte(`{"name":"main","protected":false}`))
		default:
			http.NotFound(w, r)
		}
	}))
	t.Cleanup(server.Close)
	return server
}

// reviewAppRuntime is readRuntime whose workflow knows the review App by reviewAppLogin, as a
// workflow boot always does.
func reviewAppRuntime(pool *pgxpool.Pool, api string, log *slog.Logger) *workflowRuntime {
	w := readRuntime(pool, api, log)
	w.engine = workflow.New(record.NewStore(), workflow.Config{Project: "CAPTURE", Repositories: testRepositories,
		ReviewAppLogin: reviewAppLogin, ReadMergeState: w.queueMergeState}, log)
	w.handlers, w.reviewAppLogin = []intake.Handler{w.engine}, reviewAppLogin
	return w
}

// webhookPush is the intake.Push the push webhook of legion/CAPTURE-1 from before to after carries
// (intake's decodePush): its two commits and its pusher.
func webhookPush(before, after, pusher string) intake.Push {
	return intake.Push{Repo: "acme/widgets", Branch: "legion/CAPTURE-1", Before: before, After: after, Pusher: pusher}
}

// The review App pushes the tester's red tests twice, and both push webhooks are lost, with the
// synchronize. The read reads the branch's activity, follows it back from GitHub's head to the
// recorded one, and applies each push, oldest first, as its webhook would have carried it, then
// the synchronize, so the head is judged with its chain of pushes and no head run stays open. The
// push webhooks arriving afterwards decide nothing.
func TestTheReadRebuildsTheReviewAppsRedTestPushesLost(t *testing.T) {
	at := time.Date(2026, 10, 26, 20, 0, 0, 0, time.UTC)
	pool := isolatedOutboxPool(t)
	seedCapture(t, pool, phase.Testing, "testing", record.PhaseRow{Issue: "CAPTURE-1", Role: claim.RoleTester, Claim: "test-claim"})
	github := &activityGitHub{}
	github.moved("more-red-tests", at,
		activityEntry("legion/CAPTURE-1", "push", "red-tests", "more-red-tests", reviewAppLogin, at.Add(-time.Minute)),
		activityEntry("legion/CAPTURE-1", "push", "code", "red-tests", reviewAppLogin, at.Add(-2*time.Minute)),
		activityEntry("legion/CAPTURE-1", "push", "base", "code", implementerLogin, at.Add(-time.Hour)))
	w := reviewAppRuntime(pool, github.serve(t).URL, quietLogger())
	readCapture(t, w)
	_, pr := capturePhase(t, pool)
	if pr.HeadSHA != "more-red-tests" || pr.Run != nil {
		t.Fatalf("after the read the record is at %q with a head run %+v, want more-red-tests judged with its pushes", pr.HeadSHA, pr.Run)
	}
	if got := headReads(t, pool, "push-read"); got != 2 {
		t.Fatalf("the read applied %d push facts, want 2", got)
	}
	if counts := github.reads.counts(); counts["/repos/acme/widgets/activity"] != 1 {
		t.Fatalf("the read read %v, want the branch's activity once", counts)
	}
	lateWebhookPushDecidesNothing(t, pool, w, webhookPush("code", "red-tests", reviewAppLogin))
	lateWebhookPushDecidesNothing(t, pool, w, webhookPush("red-tests", "more-red-tests", reviewAppLogin))
}

// The implementer pushes onto the recorded head and the push webhook is lost with the synchronize,
// whether the branch's activity calls the move a push or a force push. The read rebuilds it from
// that activity, so the head is judged with its push, and the push webhook arriving afterwards
// decides nothing.
func TestTheReadRebuildsAPushLostWhateverTheActivityCallsIt(t *testing.T) {
	at := time.Date(2026, 10, 26, 20, 0, 0, 0, time.UTC)
	for _, kind := range []string{"push", "force_push"} {
		t.Run(kind, func(t *testing.T) {
			pool := isolatedOutboxPool(t)
			seedCapture(t, pool, phase.Implementing, "in_progress")
			github := &activityGitHub{}
			github.moved("rewritten", at, activityEntry("legion/CAPTURE-1", kind, "code", "rewritten", implementerLogin, at.Add(-time.Minute)))
			w := reviewAppRuntime(pool, github.serve(t).URL, quietLogger())
			readCapture(t, w)
			_, pr := capturePhase(t, pool)
			if pr.HeadSHA != "rewritten" || pr.Run != nil || headReads(t, pool, "push-read") != 1 {
				t.Fatalf("after the read head %q run %+v, want rewritten judged with the push the read rebuilt", pr.HeadSHA, pr.Run)
			}
			lateWebhookPushDecidesNothing(t, pool, w, webhookPush("code", "rewritten", implementerLogin))
		})
	}
}

// When the branch's activity does not lead back from GitHub's head to the recorded one - the
// recorded head is not on the page, the way back is no push (the branch was deleted and pushed
// again), or GitHub fails the read - the read applies no push, logs a WARN saying why, and applies
// the synchronize as it did before it read the activity.
func TestTheReadAppliesNoPushWhenTheActivityCannotBeFollowed(t *testing.T) {
	at := time.Date(2026, 10, 26, 20, 0, 0, 0, time.UTC)
	zero := strings.Repeat("0", 40)
	for _, tc := range []struct {
		name     string
		activity []string
		status   int
	}{
		{"the recorded head is not on the page", []string{activityEntry("legion/CAPTURE-1", "push", "other", "pushed", implementerLogin, at)}, 0},
		{"the way back is no push", []string{activityEntry("legion/CAPTURE-1", "branch_creation", zero, "pushed", implementerLogin, at),
			activityEntry("legion/CAPTURE-1", "branch_deletion", "code", zero, implementerLogin, at.Add(-time.Minute))}, 0},
		{"GitHub fails the read", nil, http.StatusBadGateway},
	} {
		t.Run(tc.name, func(t *testing.T) {
			pool := isolatedOutboxPool(t)
			seedCapture(t, pool, phase.Testing, "testing", record.PhaseRow{Issue: "CAPTURE-1", Role: claim.RoleTester, Claim: "test-claim"})
			github := &activityGitHub{activityStatus: tc.status}
			github.moved("pushed", at, tc.activity...)
			var logged bytes.Buffer
			w := reviewAppRuntime(pool, github.serve(t).URL, slog.New(slog.NewJSONHandler(&logged, nil)))
			readCapture(t, w)
			_, pr := capturePhase(t, pool)
			if pr.HeadSHA != "pushed" || headReads(t, pool, "head-read") != 1 {
				t.Fatalf("after the read the record is at %q, want pushed by the synchronize", pr.HeadSHA)
			}
			if got := headReads(t, pool, "push-read"); got != 0 || len(pr.Pushes) != 0 {
				t.Fatalf("the read applied %d push facts and the record holds %+v, want none", got, pr.Pushes)
			}
			warned := false
			for _, line := range strings.Split(logged.String(), "\n") {
				warned = warned || (strings.Contains(line, `"level":"WARN"`) && strings.Contains(line, "the read cannot rebuild from GitHub") && strings.Contains(line, `"head":"pushed"`))
			}
			if !warned {
				t.Fatalf("the read logged %s, want a WARN that it cannot rebuild the pushes", logged.String())
			}
		})
	}
}
