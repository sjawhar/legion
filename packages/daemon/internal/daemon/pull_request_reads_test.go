package daemon

import (
	"bytes"
	"context"
	"fmt"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"slices"
	"strconv"
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

// pullRequestStandIn is GitHub's REST API for acme/widgets as the required-checks pass reads it:
// pull request 86 on base main, open on legion/CAPTURE-1 of acme/widgets itself at head, last
// updated at updated, with no reviews, whose base requires pr-checks-result and is unprotected.
// The repository's open pull requests (GET /pulls?state=open) are 86 alone; each such listing is
// counted in listed. Paths match in any case, as GitHub's do.
func pullRequestStandIn(t *testing.T, head string, updated time.Time, listed *atomic.Int32) *httptest.Server {
	t.Helper()
	pull := `{"number":86,"state":"open","html_url":"https://github.com/acme/widgets/pull/86","body":"Dispatch: CAPTURE-1",` +
		`"base":{"ref":"main"},"head":{"ref":"legion/CAPTURE-1","sha":"` + head + `","repo":{"full_name":"acme/widgets"}},"updated_at":"` + updated.Format(time.RFC3339) + `","mergeable":true}`
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Header.Get("Authorization") != "Bearer outbox-token" {
			http.Error(w, `{"message":"Bad credentials"}`, http.StatusUnauthorized)
			return
		}
		switch strings.ToLower(r.URL.Path) {
		case "/repos/acme/widgets/pulls":
			if r.URL.Query().Get("state") != "open" || r.URL.Query().Get("head") != "" {
				t.Errorf("listed pull requests by %q, want every open one", r.URL.RawQuery)
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

// headReads is how many facts of kind the pass applied from its own reads of GitHub.
func headReads(t *testing.T, pool *pgxpool.Pool, kind string) int {
	t.Helper()
	var n int
	if err := pool.QueryRow(context.Background(),
		`select count(*) from processed_events where source = 'github' and event_id like $1`, kind+":CAPTURE:%").Scan(&n); err != nil {
		t.Fatalf("read the %s facts the pass applied: %v", kind, err)
	}
	return n
}

// The daemon moves a pull request's recorded head only on a webhook, and an event queue hours
// behind left more than half of one project's records at an old head, so reviews and READY were
// judged against it. The required-checks pass already reads each open pull request every two
// minutes: when GitHub's head is not the recorded one, it applies the synchronize the webhook would
// have carried, with GitHub's updated_at, so the handler's own clock fence decides it. A head that
// did not move applies nothing, and a read older than the record's clock moves nothing.
func TestThePassFollowsAHeadWhoseSynchronizeHasNotArrived(t *testing.T) {
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
			w := requiredRuntime(pool, pullRequestStandIn(t, tc.github, read, &atomic.Int32{}).URL, quietLogger())
			w.readRequiredChecks(context.Background())
			_, pr := capturePhase(t, pool)
			if pr.HeadSHA != tc.head || !pr.HeadUpdatedAt.Equal(tc.clock) {
				t.Fatalf("after the pass the record is at %q (clock %v), want %q (clock %v)", pr.HeadSHA, pr.HeadUpdatedAt, tc.head, tc.clock)
			}
			if got := headReads(t, pool, "head-read"); got != tc.facts {
				t.Fatalf("the pass applied %d head facts, want %d", got, tc.facts)
			}
		})
	}
}

// A reviewer approves the head it read, and a push lands above it before its webhook arrives. Any
// push may change code, so the pass follows the new head alone, applies no push of its own (the
// push is left to its webhook), and the approval of the head before stands for that head alone.
func TestThePassFollowsAPushAboveAnApprovalAsANewHead(t *testing.T) {
	read := time.Date(2026, 10, 26, 20, 0, 0, 0, time.UTC)
	pool := isolatedOutboxPool(t)
	seedCapture(t, pool, phase.Reviewing, "needs_review", record.PhaseRow{Issue: "CAPTURE-1", Role: claim.RoleReviewer, Claim: "review-claim"})
	w := requiredRuntime(pool, pullRequestStandIn(t, "pushed", read, &atomic.Int32{}).URL, quietLogger())
	w.readRequiredChecks(context.Background())
	_, pr := capturePhase(t, pool)
	if pr.HeadSHA != "pushed" {
		t.Fatalf("after the pass the record is at %q, want pushed", pr.HeadSHA)
	}
	if classify.ApprovalStands(*pr, "code") {
		t.Fatalf("an approval of code stands for the followed head pushed (pushes %+v), want it to stand for code alone", pr.Pushes)
	}
	if len(pr.Pushes) != 0 {
		t.Fatalf("the pass recorded pushes %+v, want none: a push is left to its webhook", pr.Pushes)
	}
}

// The pass lists its records once, when it starts, and a webhook can move one before the pass
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
	w := requiredRuntime(pool, pullRequestStandIn(t, "pushed", read, &atomic.Int32{}).URL, quietLogger())
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

// A pull request GitHub lists open on an issue's branch that the workflow does not record - here
// an opened older than the closed record's clock - is found again every pass. The pass applies it,
// and warns, once while nothing the workflow decides it by changes: its event id is GitHub's pull
// request, the issue's generation and the record it is judged against (intake.OpenedRead),
// so it writes no processed_events row every two minutes. Once the record changes (a new
// generation removes the closed pull request), the same pull request against the new record is a
// new fact, and the workflow records it.
func TestAnOpenedTheWorkflowDoesNotRecordIsAppliedOnceWhileNothingChanges(t *testing.T) {
	opened := time.Date(2026, 10, 26, 19, 30, 0, 0, time.UTC)
	pool := isolatedOutboxPool(t)
	seedDeclinedOpened(t, pool, opened)
	var logs syncBuffer
	var listed atomic.Int32
	w := requiredRuntime(pool, pullRequestStandIn(t, "handoff", opened, &listed).URL, slog.New(slog.NewTextHandler(&logs, nil)))
	warnings := func() int {
		return strings.Count(logs.String(), "GitHub lists an open pull request on an issue's branch that the daemon does not record open")
	}
	for range 3 {
		w.readRequiredChecks(context.Background())
	}
	if listed.Load() != 3 || warnings() != 1 || headReads(t, pool, "opened-read") != 1 {
		t.Fatalf("three passes listed the repository %d times, warned %d times and applied %d opened facts, want 3, 1 and 1",
			listed.Load(), warnings(), headReads(t, pool, "opened-read"))
	}
	if got, _ := capturePhase(t, pool); got != phase.Implementing {
		t.Fatalf("the issue is in %s, want implementing: the workflow records no late opened", got)
	}
	if _, err := pool.Exec(context.Background(), "delete from pull_requests where issue = 'CAPTURE-1'"); err != nil {
		t.Fatalf("remove the closed pull request as a new generation would: %v", err)
	}
	w.readRequiredChecks(context.Background())
	if got, pr := capturePhase(t, pool); got != phase.Testing || pr == nil || pr.Number != 86 {
		t.Fatalf("after the record changed the issue is in %s with %+v, want testing on #86", got, pr)
	}
	if got := headReads(t, pool, "opened-read"); got != 2 {
		t.Fatalf("the pass applied %d opened facts in all, want 2", got)
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

// A pass that does not list the repository learns nothing new about a declined opened, and the
// next one still skips it: the fact applied is in processed_events, whichever pass applied it, so
// a listing GitHub fails, and a pass GitHub's rate limit cuts short at the listing, leave the next
// pass skipping the same opened.
func TestAPassThatDoesNotListTheRepositoryKeepsTheDeclinedOpened(t *testing.T) {
	opened := time.Date(2026, 10, 26, 19, 30, 0, 0, time.UTC)
	for _, tc := range []struct {
		name   string
		status int
	}{
		{"a list GitHub fails", http.StatusBadGateway},
		{"a pass the rate limit cuts short", http.StatusTooManyRequests},
	} {
		t.Run(tc.name, func(t *testing.T) {
			pool := isolatedOutboxPool(t)
			seedDeclinedOpened(t, pool, opened)
			var listed, lists atomic.Int32
			github := pullRequestStandIn(t, "handoff", opened, &listed)
			server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				if r.URL.Path == "/repos/acme/widgets/pulls" && lists.Add(1) == 2 {
					http.Error(w, `{"message":"failed"}`, tc.status)
					return
				}
				github.Config.Handler.ServeHTTP(w, r)
			}))
			t.Cleanup(server.Close)
			w := requiredRuntime(pool, server.URL, quietLogger())
			for range 3 {
				w.readRequiredChecks(context.Background())
			}
			if lists.Load() != 3 || headReads(t, pool, "opened-read") != 1 {
				t.Fatalf("three passes, the second's listing failed, listed %d times and applied %d opened facts, want 3 and 1",
					lists.Load(), headReads(t, pool, "opened-read"))
			}
		})
	}
}

// The pass takes a listed pull request only when its head is an issue's branch of the repository
// itself, where Legion pushes the issue's work: one on another branch records nothing, and neither
// does one on a branch of the issue's name in a fork, or in a fork GitHub no longer has.
func TestThePassTakesOnlyAPullRequestOnTheIssuesBranch(t *testing.T) {
	pool := isolatedOutboxPool(t)
	seedUnrecorded(t, pool, "handoff")
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/repos/acme/widgets/pulls" {
			http.NotFound(w, r)
			return
		}
		w.Write([]byte(`[{"number":91,"state":"open","base":{"ref":"main"},"head":{"ref":"it/IT-1","sha":"other","repo":{"full_name":"acme/widgets"}},"updated_at":"2026-10-26T19:30:00Z"},` +
			`{"number":92,"state":"open","base":{"ref":"main"},"head":{"ref":"legion/CAPTURE-1","sha":"forked","repo":{"full_name":"someone/widgets"}},"updated_at":"2026-10-26T19:30:00Z"},` +
			`{"number":93,"state":"open","base":{"ref":"main"},"head":{"ref":"legion/CAPTURE-1","sha":"gone","repo":null},"updated_at":"2026-10-26T19:30:00Z"}]`))
	}))
	t.Cleanup(server.Close)
	requiredRuntime(pool, server.URL, quietLogger()).readRequiredChecks(context.Background())
	if got, pr := capturePhase(t, pool); got != phase.Implementing || pr != nil {
		t.Fatalf("after the pass the issue is in %s with %+v, want implementing with no pull request", got, pr)
	}
	if got := headReads(t, pool, "opened-read"); got != 0 {
		t.Fatalf("the pass applied %d opened facts, want none", got)
	}
}

// LateLifecycle lets a clock equal to the record's through, so a read can be no newer than a
// webhook that moved the record after the pass listed it. followHead then keeps the record: the
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
			w := requiredRuntime(pool, pullRequestStandIn(t, "older", tc.read, &atomic.Int32{}).URL, quietLogger())
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
// the issue in implementing with its pull request open on GitHub. The pass lists the repository's
// open pull requests and applies the opened the webhook would have carried for one on the branch of
// an issue with no open record, which records it, and moves the issue on once its implementer has
// completed; a pull request the record holds closed is applied as its reopen, and one it holds
// merged is declined by the workflow, as GitHub never reopens one. It names the repository as a
// webhook's fact does, the mapping's spelling, so the webhooks that follow find the record. One
// pass lists the repository once, whatever the issues.
func TestThePassRecordsAnOpenPullRequestWhoseOpenedHasNotArrived(t *testing.T) {
	opened := time.Date(2026, 10, 26, 19, 30, 0, 0, time.UTC)
	for _, tc := range []struct {
		name     string
		handoff  string
		recorded record.PullRequestState
		served   string
		want     phase.Phase
		state    record.PullRequestState
		repo     string
		opened   int
	}{
		{"the implementer completed", "handoff", "", "", phase.Testing, record.PullRequestOpen, "acme/widgets", 1},
		{"the implementer completed on a pull request recorded closed", "handoff", record.PullRequestClosed, "", phase.Testing, record.PullRequestOpen, "acme/widgets", 1},
		{"the mapping spells the repository otherwise", "handoff", "", "Acme/Widgets", phase.Testing, record.PullRequestOpen, "Acme/Widgets", 1},
		{"the implementer has not completed", "", "", "", phase.Implementing, record.PullRequestOpen, "acme/widgets", 1},
		{"the pull request is recorded open", "handoff", record.PullRequestOpen, "", phase.Implementing, record.PullRequestOpen, "acme/widgets", 0},
		{"the pull request is recorded merged", "handoff", record.PullRequestMerged, "", phase.Implementing, record.PullRequestMerged, "acme/widgets", 1},
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
			w := requiredRuntime(pool, pullRequestStandIn(t, "handoff", opened, &listed).URL, quietLogger())
			if tc.served != "" {
				w.repositories = newRepositories("CAPTURE")
				w.repositories.set([]mappedRepository{{Repo: ghrepo.MustParse(tc.served), Default: true}}, nil)
			}
			w.readRequiredChecks(context.Background())
			got, pr := capturePhase(t, pool)
			if got != tc.want {
				t.Fatalf("after the pass the issue is in %s, want %s", got, tc.want)
			}
			if listed.Load() != 1 {
				t.Fatalf("the pass listed the repository's open pull requests %d times, want once", listed.Load())
			}
			if pr == nil || pr.State != tc.state || pr.Repo != tc.repo || pr.Number != 86 || pr.HeadSHA != "handoff" || pr.Branch != "legion/CAPTURE-1" {
				t.Fatalf("after the pass the record is %+v, want #86 of %s %s on legion/CAPTURE-1 at handoff", pr, tc.repo, tc.state)
			}
			if tc.opened == 1 && tc.state == record.PullRequestOpen && !pr.HeadUpdatedAt.Equal(opened) {
				t.Fatalf("after the pass the record's clock is %v, want GitHub's %v", pr.HeadUpdatedAt, opened)
			}
			if got := headReads(t, pool, "opened-read"); got != tc.opened {
				t.Fatalf("the pass applied %d opened facts, want %d", got, tc.opened)
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

// passPull is pull request number of acme/widgets as GitHub answers it, on branch of acme/widgets
// itself at head, in state, last updated at updated, with more fields after them (`,"merged":true`).
func passPull(number int, branch, head, state string, updated time.Time, more string) string {
	return fmt.Sprintf(`{"number":%d,"state":%q,"html_url":"https://github.com/acme/widgets/pull/%d","body":"","base":{"ref":"main"},`+
		`"head":{"ref":%q,"sha":%q,"repo":{"full_name":"acme/widgets"}},"updated_at":%q,"mergeable":true%s}`,
		number, state, number, branch, head, updated.Format(time.RFC3339), more)
}

// lostStandIn is GitHub's REST API for acme/widgets as a pass reads it after a webhook was lost:
// each of pulls, by number, is what GET /pulls/{number} answers, none has a review, base main
// requires pr-checks-result and is unprotected, and open is the JSON array GET /pulls?state=open
// answers. Every request is counted in reads.
func lostStandIn(t *testing.T, pulls map[int]string, open string, reads *pathReads) *httptest.Server {
	t.Helper()
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		reads.add(r.URL.Path)
		if r.Header.Get("Authorization") != "Bearer outbox-token" {
			http.Error(w, `{"message":"Bad credentials"}`, http.StatusUnauthorized)
			return
		}
		if number, ok := strings.CutPrefix(r.URL.Path, "/repos/acme/widgets/pulls/"); ok {
			if n, err := strconv.Atoi(number); err == nil && pulls[n] != "" {
				w.Write([]byte(pulls[n]))
				return
			}
			if strings.HasSuffix(number, "/reviews") {
				w.Write([]byte(`[]`))
				return
			}
		}
		switch r.URL.Path {
		case "/repos/acme/widgets/pulls":
			w.Write([]byte(open))
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

// recordSnapshot is every row the workflow keeps of the issues it records - the issues, their
// phases, pull requests and production checks, and the outbox - as JSON, to tell whether a fact
// changed any of them.
func recordSnapshot(t *testing.T, pool *pgxpool.Pool) string {
	t.Helper()
	return tablesSnapshot(t, pool, "issues", "phases", "pull_requests", "production_checks", "outbox")
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

// lateWebhookChangesNothing applies fact as the webhook of the event the pass already applied from
// its read would carry it, arriving afterwards, and fails t when it changed any record.
func lateWebhookChangesNothing(t *testing.T, pool *pgxpool.Pool, w *workflowRuntime, fact intake.Fact) {
	t.Helper()
	before := recordSnapshot(t, pool)
	if _, err := intake.ApplyFact(context.Background(), pool, "github", "late-webhook", fact, w.handlers...); err != nil {
		t.Fatalf("apply the late webhook: %v", err)
	}
	if after := recordSnapshot(t, pool); after != before {
		t.Fatalf("the late webhook's %T changed the record:\nbefore %s\nafter  %s", fact, before, after)
	}
}

// lateWebhookPushDecidesNothing applies push as the webhook of a push the pass already rebuilt
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

// A lost opened in any phase is healed by one pass: the workflow records a pull request opened on
// an issue's branch whatever phase the issue is in, done included, and the pass's listing finds
// one GitHub shows open that the record lacks and applies it, leaving the phase where it is. The
// webhook arriving afterwards is a redelivery and changes nothing.
func TestThePassRecordsAnOpenedLostInAnyPhase(t *testing.T) {
	at := time.Date(2026, 10, 26, 20, 0, 0, 0, time.UTC)
	for _, tc := range []struct {
		phase  phase.Phase
		status string
	}{
		{phase.Planning, "in_progress"},
		{phase.Testing, "testing"},
		{phase.Done, "done"},
	} {
		t.Run(string(tc.phase), func(t *testing.T) {
			pool := isolatedOutboxPool(t)
			if err := pgx.BeginFunc(context.Background(), pool, func(tx pgx.Tx) error {
				return record.NewStore().PutIssue(context.Background(), tx, record.Issue{Key: "CAPTURE-1", Repo: "acme/widgets", DefaultBranch: "main", GitHubRepo: "acme/widgets",
					Tree: "CAPTURE-1", Project: "CAPTURE", Title: "root", Phase: tc.phase, Generation: 1, Status: tc.status, Rank: "U"})
			}); err != nil {
				t.Fatalf("seed the issue: %v", err)
			}
			pull := passPull(86, "legion/CAPTURE-1", "code", "open", at, "")
			w := requiredRuntime(pool, lostStandIn(t, map[int]string{86: pull}, "["+pull+"]", &pathReads{}).URL, quietLogger())
			w.readRequiredChecks(context.Background())
			got, pr := capturePhase(t, pool)
			if got != tc.phase || pr == nil || pr.State != record.PullRequestOpen || pr.Number != 86 || pr.HeadSHA != "code" || !pr.HeadUpdatedAt.Equal(at) {
				t.Fatalf("after the pass the issue is in %s with %+v, want %s with #86 open at code", got, pr, tc.phase)
			}
			lateWebhookChangesNothing(t, pool, w, intake.PullRequestOpened{Repo: "acme/widgets", Number: 86, Branch: "legion/CAPTURE-1", HeadSHA: "code",
				URL: "https://github.com/acme/widgets/pull/86", UpdatedAt: at})
		})
	}
}

// A lost reopen in reviewing is healed by one pass: the record holds the pull request closed, GitHub
// lists it open again, and the pass applies its reopen, which opens the record with what the
// pull request had carried. The reopened webhook arriving afterwards changes nothing.
func TestThePassRecordsAReopenLostInReviewing(t *testing.T) {
	at := time.Date(2026, 10, 26, 20, 0, 0, 0, time.UTC)
	pool := isolatedOutboxPool(t)
	seedCapture(t, pool, phase.Reviewing, "needs_review", record.PhaseRow{Issue: "CAPTURE-1", Role: claim.RoleReviewer, Claim: "review-claim"})
	setCapturePullRequest(t, pool, func(pr *record.PullRequest) {
		pr.State, pr.HeadUpdatedAt, pr.FixAttempts = record.PullRequestClosed, at.Add(-time.Hour), 2
	})
	pull := passPull(86, "legion/CAPTURE-1", "code", "open", at, "")
	w := requiredRuntime(pool, lostStandIn(t, map[int]string{86: pull}, "["+pull+"]", &pathReads{}).URL, quietLogger())
	w.readRequiredChecks(context.Background())
	got, pr := capturePhase(t, pool)
	if got != phase.Reviewing || pr == nil || pr.State != record.PullRequestOpen || pr.FixAttempts != 2 || !pr.HeadUpdatedAt.Equal(at) {
		t.Fatalf("after the pass the issue is in %s with %+v, want reviewing with #86 open again, its two fix attempts carried", got, pr)
	}
	lateWebhookChangesNothing(t, pool, w, intake.PullRequestOpened{Repo: "acme/widgets", Number: 86, Branch: "legion/CAPTURE-1", HeadSHA: "code",
		URL: "https://github.com/acme/widgets/pull/86", UpdatedAt: at, Reopened: true})
}

// A lost close without a merge is healed by one pass: GitHub shows the pull request the record
// holds open closed, and the pass applies the close, which records it closed and tells the
// architect once; it reads nothing else of that pull request. A second pass reads it no more, and
// the closed webhook arriving afterwards changes nothing.
func TestThePassAppliesACloseWithoutMergeLost(t *testing.T) {
	at := time.Date(2026, 10, 26, 20, 0, 0, 0, time.UTC)
	pool := isolatedOutboxPool(t)
	seedCapture(t, pool, phase.Testing, "testing", record.PhaseRow{Issue: "CAPTURE-1", Role: claim.RoleTester, Claim: "test-claim"})
	var reads pathReads
	w := requiredRuntime(pool, lostStandIn(t, map[int]string{86: passPull(86, "legion/CAPTURE-1", "code", "closed", at, `,"merged":false,"merged_at":null`)}, "[]", &reads).URL, quietLogger())
	for range 2 {
		w.readRequiredChecks(context.Background())
	}
	got, pr := capturePhase(t, pool)
	if got != phase.Testing || pr == nil || pr.State != record.PullRequestClosed || !pr.HeadUpdatedAt.Equal(at) {
		t.Fatalf("after the passes the issue is in %s with %+v, want testing with #86 closed at GitHub's clock", got, pr)
	}
	if reasons := noticeReasons(t, pool, "pr-closed-unmerged"); len(reasons) != 1 || !strings.Contains(reasons[0], "#86 closed without merging") {
		t.Fatalf("pr-closed-unmerged notices %q, want one naming #86", reasons)
	}
	if counts := reads.counts(); counts["/repos/acme/widgets/pulls/86"] != 1 || counts["/repos/acme/widgets/pulls/86/reviews"] != 0 || counts["/repos/acme/widgets/rules/branches/main"] != 0 {
		t.Fatalf("two passes read %v, want #86 read once and none of its reviews or base", counts)
	}
	lateWebhookChangesNothing(t, pool, w, intake.PullRequestClosed{Repo: "acme/widgets", Number: 86, HeadSHA: "code", UpdatedAt: at})
}

// A lost merge in awaiting_merge is healed by one pass: GitHub shows the child's pull request
// merged, and the pass applies the merge, which closes the child, tells its architect child-closed
// and opens the child's production check on the merge commit. The merged webhook arriving
// afterwards changes nothing.
func TestThePassAppliesAMergeLostInAwaitingMerge(t *testing.T) {
	at := time.Date(2026, 10, 26, 20, 0, 0, 0, time.UTC)
	pool := isolatedOutboxPool(t)
	records := record.NewStore()
	if err := pgx.BeginFunc(context.Background(), pool, func(tx pgx.Tx) error {
		ctx := context.Background()
		if err := records.PutIssue(ctx, tx, record.Issue{Key: "CAPTURE-1", Repo: "acme/widgets", DefaultBranch: "main", GitHubRepo: "acme/widgets", Tree: "CAPTURE-1", Project: "CAPTURE", Title: "root",
			Phase: phase.Implementing, Generation: 1, Status: "in_progress", Rank: "U"}); err != nil {
			return err
		}
		parent := "CAPTURE-1"
		if err := records.PutIssue(ctx, tx, record.Issue{Key: "CAPTURE-2", Repo: "acme/widgets", DefaultBranch: "main", GitHubRepo: "acme/widgets", Tree: "CAPTURE-1", Project: "CAPTURE", Title: "child",
			Parent: &parent, Phase: phase.AwaitingMerge, Generation: 1, Status: "retro", Rank: "V"}); err != nil {
			return err
		}
		if err := records.PutPhase(ctx, tx, record.PhaseRow{Issue: "CAPTURE-2", Role: claim.RoleMerger, Claim: "merge-claim", HandoffCommit: "code"}); err != nil {
			return err
		}
		return records.PutPullRequest(ctx, tx, record.PullRequest{State: record.PullRequestOpen, Issue: "CAPTURE-2", Repo: "acme/widgets", Number: 87, Branch: "legion/CAPTURE-2",
			HeadSHA: "code", Failing: []string{}, Required: []string{"pr-checks-result"}})
	}); err != nil {
		t.Fatalf("seed the tree: %v", err)
	}
	merged := passPull(87, "legion/CAPTURE-2", "code", "closed", at, `,"merged":true,"merged_at":"`+at.Format(time.RFC3339)+`","merge_commit_sha":"merge-87"`)
	w := requiredRuntime(pool, lostStandIn(t, map[int]string{87: merged}, "[]", &pathReads{}).URL, quietLogger())
	w.readRequiredChecks(context.Background())
	var child *record.Issue
	var check *record.ProductionCheck
	if err := pgx.BeginFunc(context.Background(), pool, func(tx pgx.Tx) error {
		var err error
		if child, err = records.Issue(context.Background(), tx, "CAPTURE-2"); err != nil {
			return err
		}
		check, err = records.ProductionCheck(context.Background(), tx, "CAPTURE-2")
		return err
	}); err != nil {
		t.Fatalf("read the child: %v", err)
	}
	if child.Phase != phase.Done {
		t.Fatalf("after the pass the child is in %s, want done", child.Phase)
	}
	if reasons := noticeReasons(t, pool, "child-closed"); len(reasons) != 1 || !strings.Contains(reasons[0], "CAPTURE-2") {
		t.Fatalf("child-closed notices %q, want one naming CAPTURE-2", reasons)
	}
	if check == nil || !check.Open() || check.Number != 87 || check.MergeSHA != "merge-87" {
		t.Fatalf("the child's production check is %+v, want it open on #87 at merge-87", check)
	}
	lateWebhookChangesNothing(t, pool, w, intake.PullRequestMerged{Repo: "acme/widgets", Number: 87, MergeSHA: "merge-87"})
}

// A lost merge in implementing is healed by one pass: the merge is recorded and the architect is
// told pr-merged, the issue staying where it is. The merged webhook arriving afterwards changes
// nothing.
func TestThePassAppliesAMergeLostInImplementing(t *testing.T) {
	at := time.Date(2026, 10, 26, 20, 0, 0, 0, time.UTC)
	pool := isolatedOutboxPool(t)
	seedCapture(t, pool, phase.Implementing, "in_progress")
	merged := passPull(86, "legion/CAPTURE-1", "code", "closed", at, `,"merged":true,"merged_at":"`+at.Format(time.RFC3339)+`","merge_commit_sha":"merge-86"`)
	w := requiredRuntime(pool, lostStandIn(t, map[int]string{86: merged}, "[]", &pathReads{}).URL, quietLogger())
	w.readRequiredChecks(context.Background())
	got, pr := capturePhase(t, pool)
	if got != phase.Implementing || pr == nil || pr.State != record.PullRequestMerged || pr.MergeSHA != "merge-86" {
		t.Fatalf("after the pass the issue is in %s with %+v, want implementing with #86 merged at merge-86", got, pr)
	}
	if reasons := noticeReasons(t, pool, "pr-merged"); len(reasons) != 1 || !strings.Contains(reasons[0], "#86 merged while CAPTURE-1 was in implementing") {
		t.Fatalf("pr-merged notices %q, want one for #86 merged in implementing", reasons)
	}
	lateWebhookChangesNothing(t, pool, w, intake.PullRequestMerged{Repo: "acme/widgets", Number: 86, MergeSHA: "merge-86"})
}

// An issue whose implementer completed, with GitHub listing no open pull request on its branch,
// waits in implementing for one nobody would otherwise be told is missing. The pass tells its
// architect once per handoff commit, naming the issue, the repository, the branch and the commit,
// however many passes find it so, and again for a completion at another commit. A listing GitHub
// fails tells nothing: it does not show the pull request missing. A pull request the record holds
// closed without merging is missing too; one it holds merged is not: its merge told the architect
// (pr-merged), and the implementer's completion moves the issue on it.
func TestThePassTellsTheArchitectOfAPullRequestMissingOncePerHandoff(t *testing.T) {
	t.Run("the listing shows none", func(t *testing.T) {
		pool := isolatedOutboxPool(t)
		seedUnrecorded(t, pool, "first")
		w := requiredRuntime(pool, lostStandIn(t, nil, "[]", &pathReads{}).URL, quietLogger())
		for range 3 {
			w.readRequiredChecks(context.Background())
		}
		reasons := noticeReasons(t, pool, "pull-request-missing")
		if len(reasons) != 1 {
			t.Fatalf("three passes told pull-request-missing %q, want once", reasons)
		}
		for _, want := range []string{"CAPTURE-1", "acme/widgets", "legion/CAPTURE-1", "first", "open the pull request"} {
			if !strings.Contains(reasons[0], want) {
				t.Fatalf("the notice %q does not name %q", reasons[0], want)
			}
		}
		if got, _ := capturePhase(t, pool); got != phase.Implementing {
			t.Fatalf("the issue is in %s, want implementing", got)
		}
		if _, err := pool.Exec(context.Background(), "update phases set handoff_commit = 'second' where issue = 'CAPTURE-1' and role = 'implementer'"); err != nil {
			t.Fatalf("complete again at another commit: %v", err)
		}
		for range 2 {
			w.readRequiredChecks(context.Background())
		}
		if reasons := noticeReasons(t, pool, "pull-request-missing"); len(reasons) != 2 || !strings.Contains(reasons[1]+reasons[0], "second") {
			t.Fatalf("after a completion at another commit the notices are %q, want a second one naming it", reasons)
		}
	})
	for _, tc := range []struct {
		state record.PullRequestState
		told  int
	}{
		{record.PullRequestClosed, 1},
		{record.PullRequestMerged, 0},
	} {
		t.Run("the record holds the pull request "+string(tc.state), func(t *testing.T) {
			pool := isolatedOutboxPool(t)
			seedUnrecorded(t, pool, "first")
			if err := pgx.BeginFunc(context.Background(), pool, func(tx pgx.Tx) error {
				return record.NewStore().PutPullRequest(context.Background(), tx, record.PullRequest{State: tc.state, Issue: "CAPTURE-1", Repo: "acme/widgets",
					Number: 86, Branch: "legion/CAPTURE-1", HeadSHA: "first", Failing: []string{}})
			}); err != nil {
				t.Fatalf("seed the %s pull request: %v", tc.state, err)
			}
			w := requiredRuntime(pool, lostStandIn(t, nil, "[]", &pathReads{}).URL, quietLogger())
			w.readRequiredChecks(context.Background())
			if reasons := noticeReasons(t, pool, "pull-request-missing"); len(reasons) != tc.told {
				t.Fatalf("a pass over a handoff with its pull request recorded %s told pull-request-missing %q, want %d notices", tc.state, reasons, tc.told)
			}
		})
	}
	t.Run("the listing fails", func(t *testing.T) {
		pool := isolatedOutboxPool(t)
		seedUnrecorded(t, pool, "first")
		server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
			http.Error(w, `{"message":"Server Error"}`, http.StatusBadGateway)
		}))
		t.Cleanup(server.Close)
		requiredRuntime(pool, server.URL, quietLogger()).readRequiredChecks(context.Background())
		if reasons := noticeReasons(t, pool, "pull-request-missing"); len(reasons) != 0 {
			t.Fatalf("a pass whose listing failed told %q, want nothing", reasons)
		}
	})
}

// seedPassIssue records issue, with pr when it is not nil, and phases.
func seedPassIssue(t *testing.T, pool *pgxpool.Pool, issue record.Issue, pr *record.PullRequest, phases ...record.PhaseRow) {
	t.Helper()
	records := record.NewStore()
	if err := pgx.BeginFunc(context.Background(), pool, func(tx pgx.Tx) error {
		ctx := context.Background()
		if err := records.PutIssue(ctx, tx, issue); err != nil {
			return err
		}
		if pr != nil {
			if err := records.PutPullRequest(ctx, tx, *pr); err != nil {
				return err
			}
		}
		for _, row := range phases {
			if err := records.PutPhase(ctx, tx, row); err != nil {
				return err
			}
		}
		return nil
	}); err != nil {
		t.Fatalf("seed %s: %v", issue.Key, err)
	}
}

// The pass's cost on the implement App's installation, whose hourly limit every pane's gh shares:
// per served repository one listing request per hundred open pull requests, whatever their
// branches and however many issues wait for one, and per recorded open pull request its own reads
// (the pull request and its reviews, and its base's rulesets and protection once per base); the
// branch's activity only for a head that moved with no recorded push ending at it; and the check
// runs of a head no settlement stands for, once each pass until one does. No request reads one
// issue's branch's pull requests, and none compares two commits. Two served repositories:
// acme/widgets with 150 open pull requests, two of them recorded open - #86 moved by a push whose
// webhook arrived, #87 moved by two pushes whose webhooks were lost, neither new head settled -
// and one an implementing issue's whose opened was lost, on the second page; sjawhar/legion with
// none, where an issue's implementer completed with none open. #86's head settles from its check
// runs, and #87's does not yet (a run still going), so a second pass reads #87's runs again and
// #86's no more. A rate-limit answer to a listing ends the pass: no repository after it is listed,
// and nobody is told a pull request is missing.
func TestThePassKeepsItsRequestBudget(t *testing.T) {
	at := time.Date(2026, 10, 26, 20, 0, 0, 0, time.UTC)
	for _, tc := range []struct {
		name    string
		limited bool
		want    map[string]int
	}{
		{"every read answered", false, map[string]int{
			"/repos/acme/widgets/pulls": 2, "/repos/sjawhar/legion/pulls": 1,
			"/repos/acme/widgets/pulls/86": 1, "/repos/acme/widgets/pulls/86/reviews": 1, "/repos/acme/widgets/pulls/87": 1, "/repos/acme/widgets/pulls/87/reviews": 1,
			"/repos/acme/widgets/rules/branches/main": 1, "/repos/acme/widgets/branches/main": 1,
			"/repos/acme/widgets/activity":                  1,
			"/repos/acme/widgets/commits/code-2/check-runs": 1, "/repos/acme/widgets/commits/second-3/check-runs": 1,
		}},
		{"the listing's second page rate limited", true, map[string]int{
			"/repos/acme/widgets/pulls":    2,
			"/repos/acme/widgets/pulls/86": 1, "/repos/acme/widgets/pulls/86/reviews": 1, "/repos/acme/widgets/pulls/87": 1, "/repos/acme/widgets/pulls/87/reviews": 1,
			"/repos/acme/widgets/rules/branches/main": 1, "/repos/acme/widgets/branches/main": 1,
			"/repos/acme/widgets/activity":                  1,
			"/repos/acme/widgets/commits/code-2/check-runs": 1, "/repos/acme/widgets/commits/second-3/check-runs": 1,
		}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			pool := isolatedOutboxPool(t)
			seedCapture(t, pool, phase.Testing, "testing", record.PhaseRow{Issue: "CAPTURE-1", Role: claim.RoleTester, Claim: "test-claim"})
			// #86's push webhook arrived and its synchronize did not.
			setCapturePullRequest(t, pool, func(pr *record.PullRequest) { pr.Pushes = []record.ClassifiedPush{{SHA: "code-2", Before: "code"}} })
			issue := func(key, repo string) record.Issue {
				return record.Issue{Key: key, Repo: repo, DefaultBranch: "main", GitHubRepo: repo, Tree: key, Project: "CAPTURE", Title: key,
					Phase: phase.Implementing, Generation: 1, Status: "in_progress", Rank: "V" + key}
			}
			second := issue("CAPTURE-2", "acme/widgets")
			second.Phase, second.Status = phase.Testing, "testing"
			seedPassIssue(t, pool, second, &record.PullRequest{State: record.PullRequestOpen, Issue: "CAPTURE-2", Repo: "acme/widgets", Number: 87, Branch: "legion/CAPTURE-2",
				HeadSHA: "second", CheckedHead: "second", Failing: []string{}, CheckRuns: []record.AttemptRun{{Name: "pr-checks-result", ID: 1}}, Generation: 1, Snapshot: "settled",
				Required: []string{"pr-checks-result"}, Mergeability: record.MergeabilityMergeable})
			seedPassIssue(t, pool, issue("CAPTURE-3", "acme/widgets"), nil, record.PhaseRow{Issue: "CAPTURE-3", Role: claim.RoleImplementer, Claim: "implement-3", HandoffCommit: "third"})
			seedPassIssue(t, pool, issue("CAPTURE-4", "sjawhar/legion"), nil, record.PhaseRow{Issue: "CAPTURE-4", Role: claim.RoleImplementer, Claim: "implement-4", HandoffCommit: "fourth"})
			open := []string{passPull(86, "legion/CAPTURE-1", "code-2", "open", at, ""), passPull(87, "legion/CAPTURE-2", "second-3", "open", at, ""),
				passPull(90, "legion/OTHER-5", "other", "open", at, "")}
			for n := range 146 {
				open = append(open, passPull(1000+n, "feature/"+strconv.Itoa(n), "f"+strconv.Itoa(n), "open", at, ""))
			}
			open = append(open, passPull(88, "legion/CAPTURE-3", "third", "open", at, ""))
			// #88 is recorded by the first pass's listing, so only the second pass reads it.
			pulls := map[int]string{86: open[0], 87: open[1], 88: open[len(open)-1]}
			var reads pathReads
			var server *httptest.Server
			server = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				reads.add(r.URL.Path)
				if r.Header.Get("Authorization") != "Bearer outbox-token" || r.URL.Query().Get("head") != "" {
					t.Errorf("read %s, want no read of one branch, on the implement App's token", r.URL)
					http.NotFound(w, r)
					return
				}
				if number, ok := strings.CutPrefix(r.URL.Path, "/repos/acme/widgets/pulls/"); ok {
					if n, err := strconv.Atoi(number); err == nil && pulls[n] != "" {
						w.Write([]byte(pulls[n]))
						return
					}
					if strings.HasSuffix(number, "/reviews") {
						w.Write([]byte(`[]`))
						return
					}
				}
				switch r.URL.Path {
				case "/repos/acme/widgets/pulls":
					if r.URL.Query().Get("state") != "open" || r.URL.Query().Get("per_page") != "100" {
						t.Errorf("listed %s, want the open pull requests a hundred to a page", r.URL)
					}
					if r.URL.Query().Get("page") != "2" {
						w.Header().Set("Link", `<`+server.URL+`/repos/acme/widgets/pulls?state=open&per_page=100&page=2>; rel="next"`)
						w.Write([]byte("[" + strings.Join(open[:100], ",") + "]"))
						return
					}
					if tc.limited {
						w.Header().Set("X-RateLimit-Remaining", "0")
						http.Error(w, `{"message":"API rate limit exceeded"}`, http.StatusForbidden)
						return
					}
					w.Write([]byte("[" + strings.Join(open[100:], ",") + "]"))
				case "/repos/sjawhar/legion/pulls":
					w.Write([]byte(`[]`))
				case "/repos/acme/widgets/activity":
					if r.URL.Query().Get("ref") != "refs/heads/legion/CAPTURE-2" || r.URL.Query().Get("per_page") != "100" {
						t.Errorf("read the activity %s, want legion/CAPTURE-2's, a hundred to a page", r.URL)
					}
					w.Write([]byte("[" + activityEntry("legion/CAPTURE-2", "push", "second-2", "second-3", implementerLogin, at.Add(-time.Minute)) + "," +
						activityEntry("legion/CAPTURE-2", "push", "second", "second-2", implementerLogin, at.Add(-2*time.Minute)) + "," +
						activityEntry("legion/CAPTURE-2", "push", "first", "second", implementerLogin, at.Add(-time.Hour)) + "]"))
				case "/repos/acme/widgets/rules/branches/main":
					w.Write([]byte(`[{"type":"required_status_checks","parameters":{"required_status_checks":[{"context":"pr-checks-result"}]}}]`))
				case "/repos/acme/widgets/branches/main":
					w.Write([]byte(`{"name":"main","protected":false}`))
				case "/repos/acme/widgets/commits/code-2/check-runs":
					w.Write([]byte(`{"total_count":1,"check_runs":[` + checkRunJSON(31, "pr-checks-result", "success", time.Now().Add(-time.Hour), "15368") + `]}`))
				case "/repos/acme/widgets/commits/second-3/check-runs":
					w.Write([]byte(`{"total_count":1,"check_runs":[` + checkRunJSON(32, "pr-checks-result", "", time.Time{}, "15368") + `]}`))
				case "/repos/acme/widgets/commits/third/check-runs":
					w.Write([]byte(`{"total_count":0,"check_runs":[]}`))
				default:
					t.Errorf("read %s, which the pass has no need of", r.URL)
					http.NotFound(w, r)
				}
			}))
			t.Cleanup(server.Close)
			w := requiredRuntime(pool, server.URL, quietLogger())
			w.repositories.set([]mappedRepository{{Repo: ghrepo.MustParse("acme/widgets"), Default: true}, {Repo: ghrepo.MustParse("sjawhar/legion")}}, nil)
			w.readRequiredChecks(context.Background())
			if got := reads.counts(); fmt.Sprint(got) != fmt.Sprint(tc.want) {
				t.Fatalf("one pass read %v, want %v", got, tc.want)
			}
			var third *record.PullRequest
			if err := pgx.BeginFunc(context.Background(), pool, func(tx pgx.Tx) error {
				var err error
				third, err = record.NewStore().PullRequest(context.Background(), tx, "CAPTURE-3")
				return err
			}); err != nil {
				t.Fatalf("read CAPTURE-3's pull request: %v", err)
			}
			missing := noticeReasons(t, pool, "pull-request-missing")
			if tc.limited {
				if third != nil || len(missing) != 0 {
					t.Fatalf("a pass whose listing was rate limited recorded %+v and told %q, want nothing", third, missing)
				}
				return
			}
			if third == nil || third.State != record.PullRequestOpen || third.Number != 88 {
				t.Fatalf("CAPTURE-3's pull request is %+v, want #88 open from the listing's second page", third)
			}
			if len(missing) != 1 || !strings.Contains(missing[0], "CAPTURE-4") || !strings.Contains(missing[0], "sjawhar/legion") {
				t.Fatalf("pull-request-missing notices %q, want one for CAPTURE-4 on sjawhar/legion", missing)
			}
			w.readRequiredChecks(context.Background())
			if counts := reads.counts(); counts["/repos/acme/widgets/commits/code-2/check-runs"] != 1 || counts["/repos/acme/widgets/commits/second-3/check-runs"] != 2 {
				t.Fatalf("two passes read #86's settled head's check runs %d times and #87's unsettled head's %d, want once and twice",
					counts["/repos/acme/widgets/commits/code-2/check-runs"], counts["/repos/acme/widgets/commits/second-3/check-runs"])
			}
		})
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

// activityGitHub is GitHub's REST API for acme/widgets as a pass reads it after push webhooks of
// legion/CAPTURE-1 were lost: pull request 86 open on that branch at head, last updated at
// updated, with no reviews, on base main, which requires pr-checks-result and is unprotected; and
// the branch's activity, newest first, is activity, or GitHub answers activityStatus when it is
// set. Every request is counted in reads.
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
		pull := passPull(86, "legion/CAPTURE-1", g.head, "open", g.updated, "")
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

// reviewAppRuntime is requiredRuntime whose workflow knows the review App by reviewAppLogin, as a
// workflow boot always does.
func reviewAppRuntime(pool *pgxpool.Pool, api string, log *slog.Logger) *workflowRuntime {
	w := requiredRuntime(pool, api, log)
	w.engine = workflow.New(record.NewStore(), workflow.Config{Project: "CAPTURE", Repositories: testRepositories, ReviewAppLogin: reviewAppLogin}, log)
	w.handlers, w.reviewAppLogin = []intake.Handler{w.engine}, reviewAppLogin
	return w
}

// webhookPush is the intake.Push the push webhook of legion/CAPTURE-1 from before to after carries
// (intake's decodePush): its two commits and its pusher.
func webhookPush(before, after, pusher string) intake.Push {
	return intake.Push{Repo: "acme/widgets", Branch: "legion/CAPTURE-1", Before: before, After: after, Pusher: pusher}
}

// The review App pushes the tester's red tests twice, and both push webhooks are lost, with the
// synchronize. The pass reads the branch's activity, follows it back from GitHub's head to the
// recorded one, and applies each push, oldest first, as its webhook would have carried it, then
// the synchronize. The move is the review App's, so the record counts no fix attempt and the red
// the tests leave is planned, as the webhooks would have left them. The push webhooks arriving
// afterwards decide nothing.
func TestThePassRebuildsTheReviewAppsRedTestPushesLost(t *testing.T) {
	at := time.Date(2026, 10, 26, 20, 0, 0, 0, time.UTC)
	pool := isolatedOutboxPool(t)
	seedCapture(t, pool, phase.Testing, "testing", record.PhaseRow{Issue: "CAPTURE-1", Role: claim.RoleTester, Claim: "test-claim"})
	github := &activityGitHub{}
	github.moved("more-red-tests", at,
		activityEntry("legion/CAPTURE-1", "push", "red-tests", "more-red-tests", reviewAppLogin, at.Add(-time.Minute)),
		activityEntry("legion/CAPTURE-1", "push", "code", "red-tests", reviewAppLogin, at.Add(-2*time.Minute)),
		activityEntry("legion/CAPTURE-1", "push", "base", "code", implementerLogin, at.Add(-time.Hour)))
	w := reviewAppRuntime(pool, github.serve(t).URL, quietLogger())
	w.readRequiredChecks(context.Background())
	_, pr := capturePhase(t, pool)
	if pr.HeadSHA != "more-red-tests" || pr.Run != nil {
		t.Fatalf("after the pass the record is at %q with a head run %+v, want more-red-tests judged with its pushes", pr.HeadSHA, pr.Run)
	}
	if !pr.PlannedRed || pr.FixAttempts != 0 || pr.HeadCounted != "" {
		t.Fatalf("after the pass plannedRed=%t fixAttempts=%d headCounted=%q, want the review App's red planned and no attempt counted (pushes %+v)",
			pr.PlannedRed, pr.FixAttempts, pr.HeadCounted, pr.Pushes)
	}
	if got := headReads(t, pool, "push-read"); got != 2 {
		t.Fatalf("the pass applied %d push facts, want 2", got)
	}
	if counts := github.reads.counts(); counts["/repos/acme/widgets/activity"] != 1 {
		t.Fatalf("the pass read %v, want the branch's activity once", counts)
	}
	lateWebhookPushDecidesNothing(t, pool, w, webhookPush("code", "red-tests", reviewAppLogin))
	lateWebhookPushDecidesNothing(t, pool, w, webhookPush("red-tests", "more-red-tests", reviewAppLogin))
}

// The implementer pushes its fix onto the review App's planned red, and the push webhook is lost
// with the synchronize. The pass rebuilds the push from the branch's activity: the implementer's
// code clears the planned mark without counting the planned red. The fix's own red then stands
// unplanned, so the implementer's next push, lost too, is a counted fix attempt. The fix's push
// webhook arriving afterwards decides nothing.
func TestThePassRebuildsTheImplementersFixPushLost(t *testing.T) {
	at := time.Date(2026, 10, 26, 20, 0, 0, 0, time.UTC)
	pool := isolatedOutboxPool(t)
	seedCapture(t, pool, phase.Implementing, "in_progress")
	setCapturePullRequest(t, pool, func(pr *record.PullRequest) {
		pr.HeadSHA, pr.CheckedHead, pr.Failing, pr.PlannedRed = "red-tests", "red-tests", []string{"pr-checks-result"}, true
		pr.CheckRuns, pr.Generation, pr.Snapshot = []record.AttemptRun{{Name: "pr-checks-result", ID: 3}}, 2, "red-tests-red"
		pr.Moves = []record.Move{{Before: "code", SHA: "red-tests"}}
	})
	github := &activityGitHub{}
	fix := activityEntry("legion/CAPTURE-1", "push", "red-tests", "fix", implementerLogin, at.Add(-time.Minute))
	tests := activityEntry("legion/CAPTURE-1", "push", "code", "red-tests", reviewAppLogin, at.Add(-time.Hour))
	github.moved("fix", at, fix, tests)
	w := reviewAppRuntime(pool, github.serve(t).URL, quietLogger())
	w.readRequiredChecks(context.Background())
	_, pr := capturePhase(t, pool)
	if pr.HeadSHA != "fix" || pr.Run != nil || pr.PlannedRed || pr.FixAttempts != 0 {
		t.Fatalf("after the pass head %q plannedRed=%t fixAttempts=%d run %+v, want fix with the planned mark cleared, no attempt counted and no head run",
			pr.HeadSHA, pr.PlannedRed, pr.FixAttempts, pr.Run)
	}
	lateWebhookPushDecidesNothing(t, pool, w, webhookPush("red-tests", "fix", implementerLogin))

	setCapturePullRequest(t, pool, func(pr *record.PullRequest) {
		pr.CheckedHead, pr.Failing = "fix", []string{"pr-checks-result"}
		pr.CheckRuns, pr.Generation, pr.Snapshot = []record.AttemptRun{{Name: "pr-checks-result", ID: 4}}, 3, "fix-red"
	})
	github.moved("fix-2", at.Add(time.Minute), activityEntry("legion/CAPTURE-1", "push", "fix", "fix-2", implementerLogin, at.Add(30*time.Second)), fix, tests)
	w.readRequiredChecks(context.Background())
	_, pr = capturePhase(t, pool)
	if pr.HeadSHA != "fix-2" || pr.FixAttempts != 1 || pr.HeadCounted != "fix-2" || pr.PlannedRed {
		t.Fatalf("after the next pass head %q fixAttempts=%d headCounted=%q plannedRed=%t, want fix-2 counted as one fix attempt against the fix's red",
			pr.HeadSHA, pr.FixAttempts, pr.HeadCounted, pr.PlannedRed)
	}
}

// The implementer pushes onto a red head and the push webhook is lost with the synchronize,
// whether the branch's activity calls the move a push or a force push. The pass rebuilds it from
// that activity: any push may change code, so it is a counted fix attempt, and an approval of the
// head it replaced does not stand for the head it left. Either way the head is judged with its
// push, and the push webhook arriving afterwards decides nothing.
func TestThePassRebuildsAPushLostWhateverTheActivityCallsIt(t *testing.T) {
	at := time.Date(2026, 10, 26, 20, 0, 0, 0, time.UTC)
	for _, kind := range []string{"push", "force_push"} {
		t.Run(kind, func(t *testing.T) {
			pool := isolatedOutboxPool(t)
			seedCapture(t, pool, phase.Implementing, "in_progress")
			setCapturePullRequest(t, pool, func(pr *record.PullRequest) { pr.Failing = []string{"pr-checks-result"} })
			github := &activityGitHub{}
			github.moved("rewritten", at, activityEntry("legion/CAPTURE-1", kind, "code", "rewritten", implementerLogin, at.Add(-time.Minute)))
			w := reviewAppRuntime(pool, github.serve(t).URL, quietLogger())
			w.readRequiredChecks(context.Background())
			_, pr := capturePhase(t, pool)
			if pr.HeadSHA != "rewritten" || pr.Run != nil || pr.FixAttempts != 1 {
				t.Fatalf("after the pass head %q fixAttempts=%d run %+v, want rewritten judged with its push, one attempt counted", pr.HeadSHA, pr.FixAttempts, pr.Run)
			}
			if classify.ApprovalStands(*pr, "code") {
				t.Fatalf("an approval of code stands for the head the %s left, want it to stand for code alone (pushes %+v)", kind, pr.Pushes)
			}
			lateWebhookPushDecidesNothing(t, pool, w, webhookPush("code", "rewritten", implementerLogin))
		})
	}
}

// When the branch's activity does not lead back from GitHub's head to the recorded one - the
// recorded head is not on the page, the way back is no push (the branch was deleted and pushed
// again), or GitHub fails the read - the pass applies no push, logs a WARN saying why, and applies
// the synchronize as it did before it read the activity.
func TestThePassAppliesNoPushWhenTheActivityCannotBeFollowed(t *testing.T) {
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
			w.readRequiredChecks(context.Background())
			_, pr := capturePhase(t, pool)
			if pr.HeadSHA != "pushed" || headReads(t, pool, "head-read") != 1 {
				t.Fatalf("after the pass the record is at %q, want pushed by the synchronize", pr.HeadSHA)
			}
			if got := headReads(t, pool, "push-read"); got != 0 || len(pr.Pushes) != 0 {
				t.Fatalf("the pass applied %d push facts and the record holds %+v, want none", got, pr.Pushes)
			}
			warned := false
			for _, line := range strings.Split(logged.String(), "\n") {
				warned = warned || (strings.Contains(line, `"level":"WARN"`) && strings.Contains(line, "the pass cannot rebuild from GitHub") && strings.Contains(line, `"head":"pushed"`))
			}
			if !warned {
				t.Fatalf("the pass logged %s, want a WARN that it cannot rebuild the pushes", logged.String())
			}
		})
	}
}
