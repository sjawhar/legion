package daemon

import (
	"context"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/sjawhar/legion/daemon/internal/claim"
	"github.com/sjawhar/legion/daemon/internal/classify"
	"github.com/sjawhar/legion/daemon/internal/ghrepo"
	"github.com/sjawhar/legion/daemon/internal/phase"
	"github.com/sjawhar/legion/daemon/internal/record"
)

// pullRequestStandIn is GitHub's REST API for acme/widgets as the required-checks pass reads it:
// pull request 86 on base main, open on legion/CAPTURE-1 at head, last updated at updated, with
// no reviews, whose base requires pr-checks-result and is unprotected. Listed by its branch (GET
// /pulls?head=acme:legion/CAPTURE-1&state=open), it is the one open pull request there; each such
// list is counted in listed.
func pullRequestStandIn(t *testing.T, head string, updated time.Time, listed *atomic.Int32) *httptest.Server {
	t.Helper()
	pull := `{"number":86,"state":"open","html_url":"https://github.com/acme/widgets/pull/86","body":"Dispatch: CAPTURE-1",` +
		`"base":{"ref":"main"},"head":{"ref":"legion/CAPTURE-1","sha":"` + head + `"},"updated_at":"` + updated.Format(time.RFC3339) + `","mergeable":true}`
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Header.Get("Authorization") != "Bearer outbox-token" {
			http.Error(w, `{"message":"Bad credentials"}`, http.StatusUnauthorized)
			return
		}
		switch r.URL.Path {
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

// headReads is how many facts the pass applied from its own read of a pull request's head.
func headReads(t *testing.T, pool *pgxpool.Pool, kind string) int {
	t.Helper()
	var n int
	if err := pool.QueryRow(context.Background(),
		`select count(*) from processed_events where source = 'github' and event_id like $1`, kind+":CAPTURE:test-boot:%").Scan(&n); err != nil {
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
	_, pull, err := w.readPullRequest(context.Background(), *listed)
	if err != nil {
		t.Fatalf("read the pull request: %v", err)
	}
	got, err := w.followHead(context.Background(), *listed, pull)
	if err != nil || got.HeadSHA != "pushed" {
		t.Fatalf("followHead = %q, %v; want the record at pushed", got.HeadSHA, err)
	}
	if n := headReads(t, pool, "head-read"); n != 0 {
		t.Fatalf("followHead applied %d head facts, want none", n)
	}
}

// A pull request GitHub shows open on an issue's branch that the workflow does not record - here an
// opened older than the closed record's clock - is found again every pass. The pass applies it, and
// warns, once while nothing it decides by changes, so it writes no processed_events row every two
// minutes; once the record changes (a new generation removes the closed pull request), it applies
// it again, and the workflow records it.
func TestAnOpenedTheWorkflowDoesNotRecordIsAppliedOnceWhileNothingChanges(t *testing.T) {
	opened := time.Date(2026, 10, 26, 19, 30, 0, 0, time.UTC)
	pool := isolatedOutboxPool(t)
	seedDeclinedOpened(t, pool, opened)
	var logs syncBuffer
	var listed atomic.Int32
	w := requiredRuntime(pool, pullRequestStandIn(t, "handoff", opened, &listed).URL, slog.New(slog.NewTextHandler(&logs, nil)))
	warnings := func() int {
		return strings.Count(logs.String(), "GitHub holds an open pull request for an issue whose implementer completed")
	}
	for range 3 {
		w.readRequiredChecks(context.Background())
	}
	if listed.Load() != 3 || warnings() != 1 || headReads(t, pool, "opened-read") != 1 {
		t.Fatalf("three passes listed the branch %d times, warned %d times and applied %d opened facts, want 3, 1 and 1",
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

// A pass that does not read an issue's branch learns nothing about its declined opened, so it keeps
// what the pass before it learned: a list GitHub fails for that issue, and a pass GitHub's rate
// limit cuts short before it reaches the issue, leave the next pass skipping the same opened.
func TestAPassThatDoesNotReadTheBranchKeepsTheDeclinedOpened(t *testing.T) {
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
				t.Fatalf("three passes, the second's list failed, listed %d times and applied %d opened facts, want 3 and 1",
					lists.Load(), headReads(t, pool, "opened-read"))
			}
		})
	}
}

// The pass does not take the list's head filter on trust: a filter GitHub did not apply as meant
// would list other pull requests, so it takes one only when its head is the issue's branch, and one
// on another branch records nothing.
func TestThePassTakesOnlyAPullRequestOnTheIssuesBranch(t *testing.T) {
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
			_, pull, err := w.readPullRequest(context.Background(), *listed)
			if err != nil {
				t.Fatalf("read the pull request: %v", err)
			}
			got, err := w.followHead(context.Background(), *listed, pull)
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
// the issue in implementing with its pull request open on GitHub. The pass looks up the open pull
// request on the branch of each implementing issue with a recorded handoff and no open record, and
// applies the opened the webhook would have carried, which records it and moves the issue on; a
// pull request the record holds closed is applied as its reopen. It names the repository as a
// webhook's fact does, the mapping's spelling when the mapping serves it, so the webhooks that
// follow find the record. An issue whose implementer has not completed, or whose pull request is
// recorded open or merged, costs no read.
func TestThePassRecordsAnOpenPullRequestWhoseOpenedHasNotArrived(t *testing.T) {
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
			if listed.Load() != tc.listed {
				t.Fatalf("the pass listed the branch's pull requests %d times, want %d", listed.Load(), tc.listed)
			}
			if tc.listed == 0 {
				if (pr != nil) != (tc.recorded != "") {
					t.Fatalf("the pass left the record %+v, want it as seeded", pr)
				}
				return
			}
			if pr == nil || pr.State != record.PullRequestOpen || pr.Repo != tc.repo || pr.Number != 86 || pr.HeadSHA != "handoff" || pr.Branch != "legion/CAPTURE-1" || !pr.HeadUpdatedAt.Equal(opened) {
				t.Fatalf("after the pass the record is %+v, want #86 of %s open on legion/CAPTURE-1 at handoff, clock %v", pr, tc.repo, opened)
			}
			if got := headReads(t, pool, "opened-read"); got != 1 {
				t.Fatalf("the pass applied %d opened facts, want 1", got)
			}
		})
	}
}
