package daemon

import (
	"context"
	"fmt"
	"net/http"
	"net/http/httptest"
	"os"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/sjawhar/legion/daemon/internal/claim"
	"github.com/sjawhar/legion/daemon/internal/intake"
	"github.com/sjawhar/legion/daemon/internal/phase"
	"github.com/sjawhar/legion/daemon/internal/record"
	"github.com/sjawhar/legion/daemon/internal/workflow"
)

// reviewReadApp is the review App's bot login in these tests.
const reviewReadApp = "legion-reviewer[bot]"

// listedReview is one review as GitHub's GET /pulls/{number}/reviews lists it: submitted reviews
// carry submitted_at, a pending one (a draft) carries none.
func listedReview(id int64, login, state, commit, submitted string) string {
	when := ""
	if submitted != "" {
		when = fmt.Sprintf(`,"submitted_at":%q`, submitted)
	}
	return fmt.Sprintf(`{"id":%d,"user":{"login":%q},"body":"review %d","state":%q,"commit_id":%q%s}`, id, login, id, state, commit, when)
}

// reviewsStandIn is GitHub's REST API for acme/widgets as the required-checks pass reads it: pull
// request 86 on base main, whose rulesets require pr-checks-result and whose branch protection
// requires nothing, whose reviews are what reviews answers when asked (a JSON array), each read
// counted in reads, and on which a-writer has write access and an-outsider read access.
func reviewsStandIn(t *testing.T, reviews func() string, reads *atomic.Int64) *httptest.Server {
	t.Helper()
	return reviewsStandInWith(t, reviews, reads, func() string { return "read" }, &atomic.Int64{})
}

// reviewsStandInWith is reviewsStandIn on which an-outsider's permission is what outsider answers
// when asked, each read of it counted in outsiderReads.
func reviewsStandInWith(t *testing.T, reviews func() string, reads *atomic.Int64, outsider func() string, outsiderReads *atomic.Int64) *httptest.Server {
	t.Helper()
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Header.Get("Authorization") != "Bearer outbox-token" {
			http.Error(w, `{"message":"Bad credentials"}`, http.StatusUnauthorized)
			return
		}
		switch r.URL.Path {
		case "/repos/acme/widgets/pulls/86":
			w.Write([]byte(`{"number":86,"base":{"ref":"main"},"mergeable":true}`))
		case "/repos/acme/widgets/pulls/86/reviews":
			reads.Add(1)
			w.Write([]byte(reviews()))
		case "/repos/acme/widgets/rules/branches/main":
			w.Write([]byte(`[{"type":"required_status_checks","parameters":{"required_status_checks":[{"context":"pr-checks-result"}]}}]`))
		case "/repos/acme/widgets/branches/main":
			w.Write([]byte(`{"name":"main","protected":false}`))
		case "/repos/acme/widgets/collaborators/a-writer/permission":
			w.Write([]byte(`{"permission":"write","user":{"login":"a-writer"}}`))
		case "/repos/acme/widgets/collaborators/an-outsider/permission":
			outsiderReads.Add(1)
			w.Write([]byte(`{"permission":"` + outsider() + `","user":{"login":"an-outsider"}}`))
		default:
			http.NotFound(w, r)
		}
	}))
	t.Cleanup(server.Close)
	return server
}

// reviewReadRuntime is a workflow runtime over pool whose GitHub reads go to api, with the review
// App's login set on the engine and the runtime as a boot sets both.
func reviewReadRuntime(pool *pgxpool.Pool, api string) *workflowRuntime {
	engine := workflow.New(record.NewStore(), workflow.Config{Project: "CAPTURE", ReviewRoundCap: 10, ReviewAppLogin: reviewReadApp}, quietLogger())
	return &workflowRuntime{
		pool: pool, records: projectRecords{Store: record.NewStore(), project: "CAPTURE"}, engine: engine, handlers: []intake.Handler{engine},
		tokens: outboxTokens{}, dispatchProject: "CAPTURE", bootID: "test-boot", log: quietLogger(), githubAPI: api, reviewAppLogin: reviewReadApp,
	}
}

// completedReviewer is CAPTURE-1's reviewer having completed its round with commit, no review
// recorded as deciding it.
func completedReviewer(commit string) record.PhaseRow {
	return record.PhaseRow{Issue: "CAPTURE-1", Role: claim.RoleReviewer, Claim: "review-claim", HandoffCommit: commit, LastHandoff: commit,
		Summary: "reviewed", CompletedAt: time.Date(2026, 10, 8, 22, 16, 6, 0, time.UTC)}
}

// reviewerDecision is the state of CAPTURE-1's recorded review decision, "" for none, and the
// implementer's count of returns to implementing.
func reviewerDecision(t *testing.T, pool *pgxpool.Pool) (string, int) {
	t.Helper()
	var state string
	var rounds int
	if err := pool.QueryRow(context.Background(), `select coalesce((select decision->>'state' from phases where issue = 'CAPTURE-1' and role = 'reviewer'), ''),
		(select rounds from phases where issue = 'CAPTURE-1' and role = 'implementer')`).Scan(&state, &rounds); err != nil {
		t.Fatalf("read CAPTURE-1's review decision: %v", err)
	}
	return state, rounds
}

// A review whose pull_request_review webhook never reached the daemon still decides its round: the
// required-checks pass reads the pull request's reviews and applies the newest deciding one as the
// webhook would have, by the same rules - the review App's, or an account's with write access to
// the repository, ordered by when it was submitted rather than where
// GitHub lists it. The reviewer completed its round first, as when a delivery is lost: before the
// pass, the round sits in reviewing with its decision unread on GitHub. A comment, a dismissed
// review, a pending draft and an outsider's approval decide nothing, as they decide nothing by
// webhook. A round held from reviewing records the decision for the retry to end it on.
func TestAReviewWhoseWebhookNeverArrivedDecidesItsRoundOnThePass(t *testing.T) {
	const (
		early = "2026-10-08T22:15:40Z"
		late  = "2026-10-08T22:15:52Z"
	)
	for _, tc := range []struct {
		name     string
		held     bool
		reviews  []string
		want     phase.Phase
		decision string
	}{
		{name: "the review App's approval of the green head", reviews: []string{listedReview(5463503611, reviewReadApp, "APPROVED", "code", early)},
			want: phase.Retro, decision: "approved"},
		{name: "the review App's request for changes", reviews: []string{listedReview(5463503611, reviewReadApp, "CHANGES_REQUESTED", "code", early)},
			want: phase.Implementing, decision: "changes_requested"},
		{name: "a writer's approval", reviews: []string{listedReview(5463503611, "a-writer", "APPROVED", "code", early)},
			want: phase.Retro, decision: "approved"},
		{name: "an outsider's approval decides nothing", reviews: []string{listedReview(5463503611, "an-outsider", "APPROVED", "code", early)},
			want: phase.Reviewing},
		{name: "a comment, a dismissed approval and a draft decide nothing", reviews: []string{
			listedReview(5463503609, reviewReadApp, "DISMISSED", "code", early),
			listedReview(5463503610, reviewReadApp, "COMMENTED", "code", late),
			listedReview(5463503611, reviewReadApp, "PENDING", "code", ""),
		}, want: phase.Reviewing},
		{name: "the newest submitted decides, wherever GitHub lists it", reviews: []string{
			listedReview(5463503610, reviewReadApp, "CHANGES_REQUESTED", "code", late),
			listedReview(5463503611, reviewReadApp, "APPROVED", "code", early),
		}, want: phase.Implementing, decision: "changes_requested"},
		{name: "held from reviewing", held: true, reviews: []string{listedReview(5463503611, reviewReadApp, "APPROVED", "code", early)},
			want: phase.Held, decision: "approved"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			pool := isolatedOutboxPool(t)
			seedCapture(t, pool, phase.Reviewing, "needs_review", completedReviewer("review-1"))
			if tc.held {
				holdFromReviewing(t, pool)
			}
			var reads atomic.Int64
			api := reviewsStandIn(t, func() string { return "[" + strings.Join(tc.reviews, ",") + "]" }, &reads).URL
			reviewReadRuntime(pool, api).readRequiredChecks(context.Background())
			if got, _ := capturePhase(t, pool); got != tc.want {
				t.Fatalf("after the pass the issue is in %s, want %s", got, tc.want)
			}
			if got, _ := reviewerDecision(t, pool); got != tc.decision {
				t.Fatalf("the round's recorded decision = %q, want %q", got, tc.decision)
			}
			if got := reads.Load(); got != 1 {
				t.Fatalf("the pass read the pull request's reviews %d times, want once", got)
			}
		})
	}
}

// holdFromReviewing puts CAPTURE-1 in held, held from reviewing.
func holdFromReviewing(t *testing.T, pool *pgxpool.Pool) {
	t.Helper()
	records := record.NewStore()
	if err := pgx.BeginFunc(context.Background(), pool, func(tx pgx.Tx) error {
		issue, err := records.Issue(context.Background(), tx, "CAPTURE-1")
		if err != nil {
			return err
		}
		issue.Phase, issue.Hold = phase.Held, &record.Hold{From: phase.Reviewing}
		return records.PutIssue(context.Background(), tx, *issue)
	}); err != nil {
		t.Fatalf("hold CAPTURE-1 from reviewing: %v", err)
	}
}

// The pass decodes GitHub's own answer, not just the stand-in's shape:
// testdata/legion-smoke-pull-550-reviews.json is GET /repos/sjawhar/legion-smoke/pulls/550/reviews
// as GitHub answered it, formatted, with the review App's comments and its request for changes at
// an earlier head, two other Apps' comments, and the review App's approval of the head last. With
// the pull request's head at that approval's commit and green, the approval ends the round, its
// head recorded as the commit GitHub names.
func TestTheReviewsReadDecodesGitHubsOwnAnswer(t *testing.T) {
	const head = "a2cd06879ab30d0e8fbeca82b2970826a2c3b6b3"
	answer, err := os.ReadFile("testdata/legion-smoke-pull-550-reviews.json")
	if err != nil {
		t.Fatal(err)
	}
	pool := isolatedOutboxPool(t)
	seedCapture(t, pool, phase.Reviewing, "needs_review", completedReviewer("review-1"))
	records := record.NewStore()
	if err := pgx.BeginFunc(context.Background(), pool, func(tx pgx.Tx) error {
		pr, err := records.PullRequest(context.Background(), tx, "CAPTURE-1")
		if err != nil {
			return err
		}
		pr.HeadSHA, pr.CheckedHead = head, head
		return records.PutPullRequest(context.Background(), tx, *pr)
	}); err != nil {
		t.Fatalf("move CAPTURE-1's head to the approved commit: %v", err)
	}
	var reads atomic.Int64
	reviewReadRuntime(pool, reviewsStandIn(t, func() string { return string(answer) }, &reads).URL).readRequiredChecks(context.Background())
	if got, _ := capturePhase(t, pool); got != phase.Retro {
		t.Fatalf("after the pass the issue is in %s, want retro", got)
	}
	var state, approved string
	if err := pool.QueryRow(context.Background(), "select decision->>'state', decision->>'head' from phases where issue = 'CAPTURE-1' and role = 'reviewer'").Scan(&state, &approved); err != nil {
		t.Fatal(err)
	}
	if state != "approved" || approved != head {
		t.Fatalf("the recorded decision is %s of %s, want approved of %s", state, approved, head)
	}
}

// One review decision delivered both by its webhook and by the pass's read of GitHub acts once,
// whichever comes first: the pull request's newest deciding review (record.PullRequest.ReviewSeen)
// orders both copies alike, since both carry GitHub's review id and submission time. The first
// copy ends round 1; the second, arriving while the work is back in implementing, then again once
// round 2 is open and its reviewer has completed, decides nothing and counts no round - the
// webhook's second copy is a new Envoy envelope, as GitHub's redelivery is, so the daemon's
// processed events do not catch it. The pass reads the reviews in implementing too, and finds
// nothing newer to apply; round 2's own review, once GitHub lists it, still ends round 2.
func TestAReviewDecisionDeliveredTwiceActsOnce(t *testing.T) {
	round1 := listedReview(11, reviewReadApp, "CHANGES_REQUESTED", "code", "2026-10-08T22:15:40Z")
	webhook := intake.PullRequestReview{Repo: "acme/widgets", Number: 86, ID: 11, SubmittedAt: time.Date(2026, 10, 8, 22, 15, 40, 0, time.UTC),
		State: "changes_requested", CommitID: "code", HeadSHA: "code", Author: reviewReadApp, Body: "review 11"}
	for _, passFirst := range []bool{false, true} {
		t.Run(fmt.Sprintf("the pass first: %t", passFirst), func(t *testing.T) {
			pool := isolatedOutboxPool(t)
			ctx := context.Background()
			seedCapture(t, pool, phase.Reviewing, "needs_review", completedReviewer("review-1"))
			listed := []string{round1}
			var reads atomic.Int64
			w := reviewReadRuntime(pool, reviewsStandIn(t, func() string { return "[" + strings.Join(listed, ",") + "]" }, &reads).URL)
			apply := func(id string, fact intake.Fact) {
				t.Helper()
				if result, err := intake.ApplyFact(ctx, pool, "github", id, fact, w.handlers...); err != nil || result.Refusal != nil || result.Duplicate {
					t.Fatalf("apply %s = %+v, %v", id, result, err)
				}
			}
			deliver := func(first bool, copy string) {
				t.Helper()
				if first == passFirst {
					w.readRequiredChecks(ctx)
					return
				}
				apply("delivery-11-"+copy, webhook)
			}
			expect := func(step string, want phase.Phase, decision string, rounds int) {
				t.Helper()
				gotPhase, _ := capturePhase(t, pool)
				gotDecision, gotRounds := reviewerDecision(t, pool)
				if gotPhase != want || gotDecision != decision || gotRounds != rounds {
					t.Fatalf("after %s the issue is in %s with decision %q and %d rounds, want %s with %q and %d", step, gotPhase, gotDecision, gotRounds, want, decision, rounds)
				}
			}

			deliver(true, "first")
			expect("the first copy", phase.Implementing, "changes_requested", 1)
			readsBefore := reads.Load()
			deliver(false, "while implementing")
			expect("the second copy while implementing", phase.Implementing, "changes_requested", 1)
			if !passFirst && reads.Load() == readsBefore {
				t.Fatalf("the pass in implementing read no reviews")
			}

			apply("implementer-round-2", intake.HandoffComplete{Generation: 1, Issue: "CAPTURE-1", Role: claim.RoleImplementer, Claim: "implement-claim", Summary: "fixed", Commit: "round-2"})
			apply("tester-round-2", intake.HandoffComplete{Generation: 1, Issue: "CAPTURE-1", Role: claim.RoleTester, Claim: "test-claim", Summary: "tested", Verdict: "pass", Commit: "test-2"})
			apply("reviewer-round-2", intake.HandoffComplete{Generation: 1, Issue: "CAPTURE-1", Role: claim.RoleReviewer, Claim: "review-claim", Summary: "reviewed", Commit: "review-2"})
			expect("round 2's completion", phase.Reviewing, "", 1)
			apply("delivery-11-in-round-2", webhook)
			w.readRequiredChecks(ctx)
			expect("round 1's review delivered again in round 2", phase.Reviewing, "", 1)

			listed = append(listed, listedReview(12, reviewReadApp, "APPROVED", "code", "2026-10-08T23:24:06Z"))
			w.readRequiredChecks(ctx)
			expect("round 2's own review", phase.Retro, "approved", 1)
		})
	}
}

// A deciding review submitted while no round is open, whose webhook never arrived, decides no later
// round: the pass reads it while the work is in testing, or back in implementing after CI's red
// ended a round inside the two minutes, and the workflow records it as the pull request's newest
// deciding review and decides nothing with it, as it does a webhook's out-of-round review. The
// round that opens afterwards starts with no decision, its reviewer completes before its own review
// arrives, and the round is stuck undecided - told to the architect, no round counted - rather than
// sent back on the earlier request for changes, or ended in retro on the earlier approval of the
// head. A later pass finds nothing newer to apply.
func TestADecidingReviewOutsideARoundDecidesNoLaterRound(t *testing.T) {
	const stale = "2026-10-08T22:15:40Z"
	for _, tc := range []struct {
		from   phase.Phase
		status string
		state  string
	}{
		{phase.Testing, "testing", "CHANGES_REQUESTED"},
		{phase.Testing, "testing", "APPROVED"},
		{phase.Implementing, "in_progress", "CHANGES_REQUESTED"},
		{phase.Implementing, "in_progress", "APPROVED"},
	} {
		t.Run(string(tc.from)+" "+tc.state, func(t *testing.T) {
			pool := isolatedOutboxPool(t)
			ctx := context.Background()
			seedCapture(t, pool, tc.from, tc.status, record.PhaseRow{Issue: "CAPTURE-1", Role: claim.RoleReviewer, Claim: "review-claim"})
			var reads atomic.Int64
			w := reviewReadRuntime(pool, reviewsStandIn(t, func() string {
				return "[" + listedReview(21, "a-writer", tc.state, "code", stale) + "]"
			}, &reads).URL)
			apply := func(id string, fact intake.Fact) {
				t.Helper()
				if result, err := intake.ApplyFact(ctx, pool, "test", id, fact, w.handlers...); err != nil || result.Refusal != nil || result.Duplicate {
					t.Fatalf("apply %s = %+v, %v", id, result, err)
				}
			}
			expect := func(step string, want phase.Phase) {
				t.Helper()
				gotPhase, _ := capturePhase(t, pool)
				gotDecision, gotRounds := reviewerDecision(t, pool)
				if gotPhase != want || gotDecision != "" || gotRounds != 0 {
					t.Fatalf("after %s the issue is in %s with decision %q and %d rounds, want %s with none and 0", step, gotPhase, gotDecision, gotRounds, want)
				}
			}

			w.readRequiredChecks(ctx)
			expect("the pass in "+string(tc.from), tc.from)
			if tc.from == phase.Implementing {
				apply("implementer", intake.HandoffComplete{Generation: 1, Issue: "CAPTURE-1", Role: claim.RoleImplementer, Claim: "implement-claim", Summary: "fixed", Commit: "round-1"})
			}
			apply("tester", intake.HandoffComplete{Generation: 1, Issue: "CAPTURE-1", Role: claim.RoleTester, Claim: "test-claim", Summary: "tested", Verdict: "pass", Commit: "test-1"})
			apply("reviewer", intake.HandoffComplete{Generation: 1, Issue: "CAPTURE-1", Role: claim.RoleReviewer, Claim: "review-claim", Summary: "reviewed", Commit: "review-1"})
			w.readRequiredChecks(ctx)
			expect("the reviewer's completion and the next pass", phase.Reviewing)
			stuck := noticeReasons(t, pool, "review-stuck")
			if len(stuck) != 1 || !strings.Contains(stuck[0], "with no review that decides it") {
				t.Fatalf("review-stuck notices %q, want one saying no review decides the round", stuck)
			}
			if got := reads.Load(); got != 2 {
				t.Fatalf("the two passes read the reviews %d times, want twice: once in %s, once in reviewing", got, tc.from)
			}
		})
	}
}

// The same holds between two rounds, after the pass has already applied a decision: round 1's
// request for changes sends the work back, and while it is in implementing a writer submits a
// request for changes, or an approval of the head, whose webhook never arrives. The pass in
// implementing records it and decides nothing. Round 2 opens with no decision and its reviewer
// completes, and the round stays open and undecided with one round counted, rather than going back
// to implementing a second time or on to retro on that review.
func TestADecidingReviewBetweenRoundsDecidesNoLaterRound(t *testing.T) {
	round1 := listedReview(11, reviewReadApp, "CHANGES_REQUESTED", "code", "2026-10-08T22:15:40Z")
	for _, state := range []string{"CHANGES_REQUESTED", "APPROVED"} {
		t.Run(state, func(t *testing.T) {
			pool := isolatedOutboxPool(t)
			ctx := context.Background()
			seedCapture(t, pool, phase.Reviewing, "needs_review", completedReviewer("review-1"))
			listed := []string{round1}
			var reads atomic.Int64
			w := reviewReadRuntime(pool, reviewsStandIn(t, func() string { return "[" + strings.Join(listed, ",") + "]" }, &reads).URL)
			apply := func(id string, fact intake.Fact) {
				t.Helper()
				if result, err := intake.ApplyFact(ctx, pool, "test", id, fact, w.handlers...); err != nil || result.Refusal != nil || result.Duplicate {
					t.Fatalf("apply %s = %+v, %v", id, result, err)
				}
			}
			expect := func(step string, want phase.Phase, decision string, rounds int) {
				t.Helper()
				gotPhase, _ := capturePhase(t, pool)
				gotDecision, gotRounds := reviewerDecision(t, pool)
				if gotPhase != want || gotDecision != decision || gotRounds != rounds {
					t.Fatalf("after %s the issue is in %s with decision %q and %d rounds, want %s with %q and %d", step, gotPhase, gotDecision, gotRounds, want, decision, rounds)
				}
			}

			w.readRequiredChecks(ctx)
			expect("round 1's review", phase.Implementing, "changes_requested", 1)
			listed = append(listed, listedReview(21, "a-writer", state, "code", "2026-10-08T22:40:00Z"))
			w.readRequiredChecks(ctx)
			expect("the pass in implementing", phase.Implementing, "changes_requested", 1)
			apply("implementer-round-2", intake.HandoffComplete{Generation: 1, Issue: "CAPTURE-1", Role: claim.RoleImplementer, Claim: "implement-claim", Summary: "fixed", Commit: "round-2"})
			apply("tester-round-2", intake.HandoffComplete{Generation: 1, Issue: "CAPTURE-1", Role: claim.RoleTester, Claim: "test-claim", Summary: "tested", Verdict: "pass", Commit: "test-2"})
			apply("reviewer-round-2", intake.HandoffComplete{Generation: 1, Issue: "CAPTURE-1", Role: claim.RoleReviewer, Claim: "review-claim", Summary: "reviewed", Commit: "review-2"})
			w.readRequiredChecks(ctx)
			expect("round 2's completion and the next pass", phase.Reviewing, "", 1)
		})
	}
}

// Within one daemon process the read judges whether a review's author may decide once, as a
// webhook's review is judged once when it is delivered: an outsider's approval of the head is
// refused on the first pass, and after GitHub starts giving that account write access - past the
// permission answer's own window - the next pass neither asks about it again nor lets the approval
// end the round. Its webhook, judged at delivery, would not have ended it either.
func TestTheReadJudgesEachReviewOnce(t *testing.T) {
	pool := isolatedOutboxPool(t)
	seedCapture(t, pool, phase.Reviewing, "needs_review", completedReviewer("review-1"))
	var reads, outsiderReads atomic.Int64
	var promoted atomic.Bool
	api := reviewsStandInWith(t, func() string {
		return "[" + listedReview(31, "an-outsider", "APPROVED", "code", "2026-10-08T22:15:40Z") + "]"
	}, &reads, func() string {
		if promoted.Load() {
			return "write"
		}
		return "read"
	}, &outsiderReads).URL
	w := reviewReadRuntime(pool, api)
	w.permissionTTL = time.Nanosecond
	w.readRequiredChecks(context.Background())
	if got, _ := capturePhase(t, pool); got != phase.Reviewing || outsiderReads.Load() != 1 {
		t.Fatalf("after the first pass the issue is in %s and the outsider's permission was read %d times, want reviewing and once", got, outsiderReads.Load())
	}
	promoted.Store(true)
	time.Sleep(time.Millisecond)
	w.readRequiredChecks(context.Background())
	if got, _ := capturePhase(t, pool); got != phase.Reviewing {
		t.Fatalf("after the account was given write access the issue is in %s, want reviewing: its approval was judged when the account could not write", got)
	}
	if got, _ := reviewerDecision(t, pool); got != "" || outsiderReads.Load() != 1 || reads.Load() != 2 {
		t.Fatalf("the round's decision is %q, the outsider's permission was read %d times and the reviews %d; want none, once and twice", got, outsiderReads.Load(), reads.Load())
	}
}

// GitHub's rate limit on the reviews read ends the pass there, as it ends the pass on any of its
// reads: every later read on the implement App's installation meets the same limit, and the panes
// share it. The stand-in answers the first pull request's own read and refuses its reviews read; a
// second open pull request is recorded, and nothing of either is read after the refusal.
func TestARateLimitOnTheReviewsReadEndsThePass(t *testing.T) {
	pool := isolatedOutboxPool(t)
	seedCapture(t, pool, phase.Reviewing, "needs_review", completedReviewer("review-1"))
	if err := pgx.BeginFunc(context.Background(), pool, func(tx pgx.Tx) error {
		records := record.NewStore()
		if err := records.PutIssue(context.Background(), tx, record.Issue{Key: "CAPTURE-2", Tree: "CAPTURE-2", Project: "CAPTURE", Title: "second",
			Phase: phase.Reviewing, Generation: 1, Status: "needs_review", Rank: "V"}); err != nil {
			return err
		}
		return records.PutPullRequest(context.Background(), tx, record.PullRequest{State: record.PullRequestOpen, Issue: "CAPTURE-2",
			Repo: "acme/widgets", Number: 87, Branch: "legion/CAPTURE-2", HeadSHA: "second"})
	}); err != nil {
		t.Fatalf("seed a second open pull request: %v", err)
	}
	var paths []string
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		paths = append(paths, r.URL.Path)
		switch r.URL.Path {
		case "/repos/acme/widgets/pulls/86", "/repos/acme/widgets/pulls/87":
			w.Write([]byte(`{"base":{"ref":"main"},"mergeable":true}`))
		default:
			w.Header().Set("X-RateLimit-Remaining", "0")
			http.Error(w, `{"message":"API rate limit exceeded"}`, http.StatusForbidden)
		}
	}))
	t.Cleanup(server.Close)
	reviewReadRuntime(pool, server.URL).readRequiredChecks(context.Background())
	if len(paths) != 2 || paths[1] != "/repos/acme/widgets/pulls/"+strings.TrimPrefix(paths[0], "/repos/acme/widgets/pulls/")+"/reviews" {
		t.Fatalf("the pass read %q, want one pull request and then its reviews, and nothing after the rate limit", paths)
	}
}
