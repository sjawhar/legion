package daemon

import (
	"context"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/sjawhar/legion/daemon/internal/phase"
	"github.com/sjawhar/legion/daemon/internal/record"
)

// compareStandIn is reviewsStandIn, listing no reviews, that also answers GitHub's compare of the
// approved head code with the recorded head handoff: answer when status is 200, GitHub's error
// otherwise. Each compare read is counted in compares.
func compareStandIn(t *testing.T, status int, answer string, compares *atomic.Int64) *httptest.Server {
	t.Helper()
	reviews := reviewsStandIn(t, func() string { return "[]" }, &atomic.Int64{})
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/repos/acme/widgets/compare/code...handoff" {
			reviews.Config.Handler.ServeHTTP(w, r)
			return
		}
		compares.Add(1)
		if status != http.StatusOK {
			http.Error(w, `{"message":"compare failed"}`, status)
			return
		}
		w.Write([]byte(answer))
	}))
	t.Cleanup(server.Close)
	return server
}

// seedApprovalBelowHead records CAPTURE-1 in reviewing, its reviewer completed with an approval of
// head code, and its pull request at head handoff with no push recorded between the two: the
// synchronize that moved the head arrived and the push that names what changed did not.
func seedApprovalBelowHead(t *testing.T, pool *pgxpool.Pool, approved string) {
	t.Helper()
	reviewer := completedReviewer("review-1")
	reviewer.Decision = &record.ReviewDecision{State: "approved", Head: approved}
	seedCapture(t, pool, phase.Reviewing, "needs_review", reviewer)
	setCapturePullRequest(t, pool, func(pr *record.PullRequest) { pr.HeadSHA = "handoff" })
}

// An approval of a head the pull request has since moved past stands when every commit above it
// changed only the issue's own handoffs, .legion/CAPTURE-1/, even when the push webhook that would
// say so never arrived: the pass reads GitHub's compare of the approved head with the recorded
// one, records the handoff-only push it shows, and the round ends approved. Anything else leaves
// the approval set aside, as it is today: a path outside .legion/CAPTURE-1/ (code, another issue's
// handoffs, a rename out of the code), a head the approved one is not an ancestor of, a list GitHub
// may have cut short or did not give, and a compare GitHub refused or failed. GitHub's answer for
// two fixed heads never changes, so a second pass asks again only after a failed read.
func TestThePassCarriesAnApprovalAcrossCommitsThatChangeOnlyTheIssuesHandoffs(t *testing.T) {
	file := func(name string) string { return fmt.Sprintf(`{"filename":%q,"status":"modified"}`, name) }
	compare := func(status string, files ...string) string {
		return fmt.Sprintf(`{"status":%q,"ahead_by":1,"behind_by":0,"files":[%s]}`, status, strings.Join(files, ","))
	}
	review := file(".legion/CAPTURE-1/review.json")
	// GitHub's compare lists at most 300 changed files (compareFilesCap).
	many := make([]string, 300)
	for i := range many {
		many[i] = file(fmt.Sprintf(".legion/CAPTURE-1/x%d.json", i))
	}
	for _, tc := range []struct {
		name    string
		status  int
		answer  string
		carried bool
	}{
		{name: "only the issue's own handoffs", status: http.StatusOK, answer: compare("ahead", review, file(".legion/CAPTURE-1/test.json")), carried: true},
		{name: "a handoff removed", status: http.StatusOK, answer: compare("ahead", `{"filename":".legion/CAPTURE-1/plan.json","status":"removed"}`), carried: true},
		{name: "a handoff and code", status: http.StatusOK, answer: compare("ahead", review, file("src/widget.go"))},
		{name: "another issue's handoffs", status: http.StatusOK, answer: compare("ahead", review, file(".legion/CAPTURE-2/plan.json"))},
		{name: "the handoffs directory's own name as a file", status: http.StatusOK, answer: compare("ahead", file(".legion/CAPTURE-1"))},
		{name: "a rename out of the code into the handoffs", status: http.StatusOK,
			answer: compare("ahead", `{"filename":".legion/CAPTURE-1/moved.go","previous_filename":"src/moved.go","status":"renamed"}`)},
		{name: "a head the approved head is not an ancestor of", status: http.StatusOK, answer: compare("diverged", review)},
		{name: "a head behind the approved head", status: http.StatusOK, answer: compare("behind")},
		{name: "a list GitHub may have cut short", status: http.StatusOK, answer: compare("ahead", many...)},
		{name: "no file list", status: http.StatusOK, answer: `{"status":"ahead"}`},
		{name: "an empty file list", status: http.StatusOK, answer: compare("ahead")},
		{name: "an approved head GitHub does not know", status: http.StatusNotFound},
		{name: "a compare GitHub failed", status: http.StatusInternalServerError},
	} {
		t.Run(tc.name, func(t *testing.T) {
			pool := isolatedOutboxPool(t)
			seedApprovalBelowHead(t, pool, "code")
			var compares atomic.Int64
			api := compareStandIn(t, tc.status, tc.answer, &compares).URL
			w := reviewReadRuntime(pool, api)
			w.readRequiredChecks(context.Background())
			if got := compares.Load(); got != 1 {
				t.Fatalf("the pass compared the approved head with the recorded head %d times, want once", got)
			}
			w.readRequiredChecks(context.Background())
			again := int64(1)
			if tc.status == http.StatusInternalServerError {
				again = 2
			}
			if got := compares.Load(); got != again {
				t.Fatalf("after a second pass the compares number %d, want %d", got, again)
			}
			got, pr := capturePhase(t, pool)
			want, pushes := phase.Reviewing, []record.ClassifiedPush(nil)
			if tc.carried {
				want, pushes = phase.Accepting, []record.ClassifiedPush{{SHA: "handoff", Before: "code", HandoffOnly: true}}
			}
			if got != want {
				t.Fatalf("after the pass the issue is in %s, want %s", got, want)
			}
			if fmt.Sprint(pr.Pushes) != fmt.Sprint(pushes) {
				t.Fatalf("after the pass the pushes recorded are %+v, want %+v", pr.Pushes, pushes)
			}
		})
	}
}

// The pass compares only while an approval is set aside: an approval of the recorded head itself
// stands with no read, and so does one a recorded handoff-only push already carries; a round with
// no approval, or an issue not in reviewing, has no approval for a compare to carry.
func TestThePassComparesOnlyForAnApprovalSetAside(t *testing.T) {
	for _, tc := range []struct {
		name string
		seed func(t *testing.T, pool *pgxpool.Pool)
	}{
		{name: "an approval of the head", seed: func(t *testing.T, pool *pgxpool.Pool) { seedApprovalBelowHead(t, pool, "handoff") }},
		{name: "an approval a recorded push carries", seed: func(t *testing.T, pool *pgxpool.Pool) {
			seedApprovalBelowHead(t, pool, "code")
			setCapturePullRequest(t, pool, func(pr *record.PullRequest) {
				pr.Pushes = []record.ClassifiedPush{{SHA: "handoff", Before: "code", HandoffOnly: true}}
			})
		}},
		{name: "a request for changes", seed: func(t *testing.T, pool *pgxpool.Pool) {
			reviewer := completedReviewer("review-1")
			reviewer.Decision = &record.ReviewDecision{State: "changes_requested", Head: "code"}
			seedCapture(t, pool, phase.Reviewing, "needs_review", reviewer)
			setCapturePullRequest(t, pool, func(pr *record.PullRequest) { pr.HeadSHA = "handoff" })
		}},
		{name: "an issue not in reviewing", seed: func(t *testing.T, pool *pgxpool.Pool) {
			reviewer := completedReviewer("review-1")
			reviewer.Decision = &record.ReviewDecision{State: "approved", Head: "code"}
			seedCapture(t, pool, phase.Testing, "in_progress", reviewer)
			setCapturePullRequest(t, pool, func(pr *record.PullRequest) { pr.HeadSHA = "handoff" })
		}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			pool := isolatedOutboxPool(t)
			tc.seed(t, pool)
			var compares atomic.Int64
			api := compareStandIn(t, http.StatusOK, `{"status":"ahead","files":[{"filename":".legion/CAPTURE-1/review.json"}]}`, &compares).URL
			reviewReadRuntime(pool, api).readRequiredChecks(context.Background())
			if got := compares.Load(); got != 0 {
				t.Fatalf("the pass compared %d times, want none", got)
			}
		})
	}
}

// The record can lag GitHub by more than one head: the reviewer approved approved, a head the
// record never reached, and pushed its handoff to handoff, while the record still holds code. The
// pass follows the head from code to handoff (followHead), which records the push from code, but
// no recorded push starts at approved, so the approval is still set aside; the pass then compares
// approved with handoff and carries the approval when only the issue's handoffs changed between
// them, and leaves it set aside when anything else did.
func TestThePassCarriesAnApprovalOfAHeadBetweenTheRecordedAndGitHubsHead(t *testing.T) {
	for _, tc := range []struct {
		name    string
		above   string
		want    phase.Phase
		carried bool
	}{
		{name: "only the issue's handoffs above the approved head", above: `{"filename":".legion/CAPTURE-1/review.json"}`, want: phase.Accepting, carried: true},
		{name: "code above the approved head", above: `{"filename":".legion/CAPTURE-1/review.json"},{"filename":"src/widget.go"}`, want: phase.Reviewing},
	} {
		t.Run(tc.name, func(t *testing.T) {
			pool := isolatedOutboxPool(t)
			reviewer := completedReviewer("review-1")
			reviewer.Decision = &record.ReviewDecision{State: "approved", Head: "approved"}
			seedCapture(t, pool, phase.Reviewing, "needs_review", reviewer)
			compares := map[string]string{
				"code...handoff":     `{"status":"ahead","files":[{"filename":".legion/CAPTURE-1/test.json"},{"filename":".legion/CAPTURE-1/review.json"}]}`,
				"approved...handoff": `{"status":"ahead","files":[` + tc.above + `]}`,
			}
			reviews := reviewsStandIn(t, func() string { return "[]" }, &atomic.Int64{})
			server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				if basehead, ok := strings.CutPrefix(r.URL.Path, "/repos/acme/widgets/compare/"); ok {
					w.Write([]byte(compares[basehead]))
					return
				}
				if r.URL.Path == "/repos/acme/widgets/pulls/86" {
					w.Write([]byte(`{"number":86,"state":"open","base":{"ref":"main"},"head":{"ref":"legion/CAPTURE-1","sha":"handoff"},"updated_at":"2026-10-26T22:42:00Z","mergeable":true}`))
					return
				}
				reviews.Config.Handler.ServeHTTP(w, r)
			}))
			t.Cleanup(server.Close)
			reviewReadRuntime(pool, server.URL).readRequiredChecks(context.Background())
			got, pr := capturePhase(t, pool)
			if got != tc.want {
				t.Fatalf("after the pass the issue is in %s, want %s (pushes %+v)", got, tc.want, pr.Pushes)
			}
			if pr.HeadSHA != "handoff" {
				t.Fatalf("after the pass the record is at %q, want handoff", pr.HeadSHA)
			}
			carried := false
			for _, push := range pr.Pushes {
				carried = carried || (push.Before == "approved" && push.SHA == "handoff" && push.HandoffOnly)
			}
			if carried != tc.carried {
				t.Fatalf("a handoff-only push from approved to handoff is recorded: %t, want %t (pushes %+v)", carried, tc.carried, pr.Pushes)
			}
		})
	}
}

// GitHub's rate limit on the compare ends the pass there, as it ends the pass on any of its reads:
// nothing is read after the refusal, the approval stays set aside, and the next pass asks again.
func TestARateLimitOnTheCompareEndsThePass(t *testing.T) {
	pool := isolatedOutboxPool(t)
	seedApprovalBelowHead(t, pool, "code")
	var paths []string
	reviews := reviewsStandIn(t, func() string { return "[]" }, &atomic.Int64{})
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		paths = append(paths, r.URL.Path)
		if r.URL.Path != "/repos/acme/widgets/compare/code...handoff" {
			reviews.Config.Handler.ServeHTTP(w, r)
			return
		}
		w.Header().Set("X-RateLimit-Remaining", "0")
		http.Error(w, `{"message":"API rate limit exceeded"}`, http.StatusForbidden)
	}))
	t.Cleanup(server.Close)
	w := reviewReadRuntime(pool, server.URL)
	w.readRequiredChecks(context.Background())
	want := []string{"/repos/acme/widgets/pulls/86", "/repos/acme/widgets/pulls/86/reviews", "/repos/acme/widgets/compare/code...handoff"}
	if fmt.Sprint(paths) != fmt.Sprint(want) {
		t.Fatalf("the pass read %q, want %q and nothing after the rate limit", paths, want)
	}
	if got, _ := capturePhase(t, pool); got != phase.Reviewing {
		t.Fatalf("after the pass the issue is in %s, want reviewing", got)
	}
	w.readRequiredChecks(context.Background())
	if got := strings.Count(fmt.Sprint(paths), "/compare/"); got != 2 {
		t.Fatalf("after a second pass the compares number %d, want 2", got)
	}
}

// setCapturePullRequest rewrites CAPTURE-1's pull request record with change.
func setCapturePullRequest(t *testing.T, pool *pgxpool.Pool, change func(*record.PullRequest)) {
	t.Helper()
	records := record.NewStore()
	if err := pgx.BeginFunc(context.Background(), pool, func(tx pgx.Tx) error {
		pr, err := records.PullRequest(context.Background(), tx, "CAPTURE-1")
		if err != nil {
			return err
		}
		change(pr)
		return records.PutPullRequest(context.Background(), tx, *pr)
	}); err != nil {
		t.Fatalf("rewrite CAPTURE-1's pull request: %v", err)
	}
}
