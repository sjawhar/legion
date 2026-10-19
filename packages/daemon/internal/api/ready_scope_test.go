package api

import (
	"fmt"
	"net/http"
	"strings"
	"testing"
	"time"

	"github.com/sjawhar/legion/daemon/internal/claim"
	"github.com/sjawhar/legion/daemon/internal/phase"
	"github.com/sjawhar/legion/daemon/internal/record"
)

// smokeRetroCompare is GitHub's compare of the approved head of sjawhar/legion-smoke#589
// (7733416e7) with the head the daemon merged (ea3e449ed), its issue key spelled LEGION-208 as this
// harness's issue is: retro's two learnings added under docs/solutions/, and its last commit's
// removal of the issue's four handoffs.
const smokeRetroCompare = `{"status":"ahead","ahead_by":2,"files":[` +
	`{"filename":".legion/LEGION-208/implement.json","status":"removed"},` +
	`{"filename":".legion/LEGION-208/plan.json","status":"removed"},` +
	`{"filename":".legion/LEGION-208/review.json","status":"removed"},` +
	`{"filename":".legion/LEGION-208/test.json","status":"removed"},` +
	`{"filename":"docs/solutions/legion/github-heads-checks-and-app-identity-LEGION-208.md","status":"added"},` +
	`{"filename":"docs/solutions/legion/proving-a-change-with-no-runtime-LEGION-208.md","status":"added"}]}`

// After the reviewer approves a head, retro's commits sit above it: its learnings under
// docs/solutions/, and its last commit's removal of the issue's own .legion/<issue>/. READY reads
// GitHub's compare of the approved head with READY's head, as the implement App for the
// repository's owner, and publishes a READY whose change since the approval is only those, as
// legion-smoke#589's was, or none at all.
func TestHandoffCompleteReadyAcceptsOnlyRetrosChangesAboveTheApprovedHead(t *testing.T) {
	for _, tc := range []struct{ name, compare string }{
		{"legion-smoke#589's retro: two learnings added and the issue's handoffs removed", smokeRetroCompare},
		{"a READY at the approved head itself", `{"status":"identical","files":[]}`},
	} {
		t.Run(tc.name, func(t *testing.T) {
			github := newHandoffGitHub(t)
			github.set(func(g *handoffGitHub) { g.compare = tc.compare })
			h, facts, merger := newReadyHarness(t, github)
			if code, body := ready(t, h, merger); code != http.StatusOK || body != "" {
				t.Fatalf("READY = %d %q; want it published", code, body)
			}
			if got := facts.recorded(); len(got) != 1 {
				t.Fatalf("handoff facts = %#v, want the one READY", got)
			}
			if got := github.compares(); len(got) != 1 || got[0] != approvedSHA+"..."+headSHA {
				t.Fatalf("READY compared %v, want the approved head with READY's head, %s...%s", got, approvedSHA, headSHA)
			}
			github.mu.Lock()
			defer github.mu.Unlock()
			for _, bearer := range github.bearers {
				if bearer != "Bearer implement-token-for-acme" {
					t.Fatalf("GitHub was read with %q, want the implement App's token for acme, the repository's owner, alone", bearer)
				}
			}
		})
	}
}

// Anything else above the approved head is a change no review approved, and READY is refused with
// READY_UNREVIEWED_CHANGE, naming the paths, so the merger sends the issue back to review: a change
// to the product, a path that only resembles retro's (a sibling of docs/solutions/, another
// issue's handoffs), and a rename whose old name is outside them. A compare GitHub may have cut
// short leaves a path unread, and a head the approved one is not an ancestor of (a rewrite, or a
// head behind the approval) is not the approved code plus anything, so each is refused the same
// way, as is an approved head GitHub does not know. Nothing is recorded for any of them.
func TestHandoffCompleteReadyRefusesAChangeAboveTheApprovedHeadNoReviewApproved(t *testing.T) {
	compare := func(status string, files ...string) string {
		return fmt.Sprintf(`{"status":%q,"files":[%s]}`, status, strings.Join(files, ","))
	}
	file := func(name string) string { return fmt.Sprintf(`{"filename":%q,"status":"modified"}`, name) }
	retro := []string{file("docs/solutions/legion/retro-LEGION-208.md"), `{"filename":".legion/LEGION-208/review.json","status":"removed"}`}
	many := make([]string, 300)
	for i := range many {
		many[i] = file(fmt.Sprintf("docs/solutions/legion/note-%03d.md", i))
	}
	const code = "READY_UNREVIEWED_CHANGE: "
	for _, tc := range []struct {
		name, compare string
		status        int
		want          []string
	}{
		{name: "a product change after the approval", compare: compare("ahead", append(retro, file("src/main.go"))...),
			want: []string{code, "src/main.go", approvedSHA[:12], "reviewing"}},
		{name: "a sibling of docs/solutions/", compare: compare("ahead", append(retro, file("docs/solutions-old/note.md"))...),
			want: []string{code, "docs/solutions-old/note.md"}},
		{name: "another issue's handoffs", compare: compare("ahead", append(retro, `{"filename":".legion/OTHER-1/plan.json","status":"removed"}`)...),
			want: []string{code, ".legion/OTHER-1/plan.json"}},
		{name: "the handoffs directory's own name as a file", compare: compare("ahead", file(".legion/LEGION-208")),
			want: []string{code, ".legion/LEGION-208"}},
		{name: "a rename out of the product into docs/solutions/", compare: compare("ahead",
			`{"filename":"docs/solutions/legion/moved.md","previous_filename":"src/moved.md","status":"renamed"}`),
			want: []string{code, "src/moved.md"}},
		{name: "a compare GitHub may have cut short", compare: compare("ahead", many...),
			want: []string{code, "300", "unread"}},
		{name: "a compare with no file list", compare: `{"status":"ahead"}`,
			want: []string{code, "no file list"}},
		{name: "a head the approved head is not an ancestor of", compare: compare("diverged", retro...),
			want: []string{code, "diverged", "not an ancestor"}},
		{name: "a head behind the approved head", compare: compare("behind"),
			want: []string{code, "behind", "not an ancestor"}},
		{name: "an approved head GitHub does not know", compare: `{"message":"Not Found","status":"404"}`, status: http.StatusNotFound,
			want: []string{code, "does not know", approvedSHA[:12]}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			github := newHandoffGitHub(t)
			github.set(func(g *handoffGitHub) {
				g.compare = tc.compare
				if tc.status != 0 {
					g.fail["compare/"] = tc.status
				}
			})
			h, facts, merger := newReadyHarness(t, github)
			status, body := ready(t, h, merger)
			if status != http.StatusConflict || !strings.HasPrefix(body, tc.want[0]) {
				t.Fatalf("READY = %d %q; want %q", status, body, tc.want[0])
			}
			for _, want := range tc.want[1:] {
				if !strings.Contains(body, want) {
					t.Errorf("READY's refusal %q does not name %q", body, want)
				}
			}
			for _, retroPath := range []string{"docs/solutions/legion/retro-LEGION-208.md", ".legion/LEGION-208/review.json"} {
				if strings.Contains(body, retroPath) {
					t.Errorf("READY's refusal %q names retro's own %s", body, retroPath)
				}
			}
			if got := facts.recorded(); len(got) != 0 {
				t.Fatalf("handoff facts = %#v, want none for a refused READY", got)
			}
		})
	}
}

// The approved head is the review round's recorded decision. A merger's task carries no approved
// head when the round recorded none (workflow's withApprovedHead) — a re-admission cleared it, or
// the round's decision was a request for changes — and its prompt says not to complete then; READY
// is refused READY_NOT_APPROVED all the same, before GitHub's compare is asked for, so a merger that
// skips that step merges nothing no review approved.
func TestHandoffCompleteReadyRefusesARoundWithNoRecordedApproval(t *testing.T) {
	for _, tc := range []struct {
		name     string
		decision *record.ReviewDecision
	}{
		{"no decision recorded", nil},
		{"a request for changes", &record.ReviewDecision{State: "changes_requested", Head: approvedSHA}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			github := newHandoffGitHub(t)
			h, facts := newHandoffHarness(t, github, nil, nil)
			seedIssueAt(t, h, "LEGION-208", phase.Merging)
			seedPullRequest(t, h, "LEGION-208", headSHA)
			seedReviewDecision(t, h, "LEGION-208", tc.decision)
			merger := newLiveClaim(t, h, "LEGION-208", claim.RoleMerger)
			code, body := ready(t, h, merger)
			if code != http.StatusConflict || !strings.HasPrefix(body, "READY_NOT_APPROVED: ") || !strings.Contains(body, "reviewing") {
				t.Fatalf("READY with no recorded approval = %d %q", code, body)
			}
			if got := github.compares(); len(got) != 0 {
				t.Fatalf("READY compared %v with no approved head, want no compare", got)
			}
			if got := facts.recorded(); len(got) != 0 {
				t.Fatalf("handoff facts = %#v, want none", got)
			}
		})
	}
}

// A READY a person merges gets the same check: its by-hand note keeps the daemon from submitting the
// merge, not the person from merging a change no review approved.
func TestHandoffCompleteReadyHoldsAByHandPullRequestToItsApprovedHead(t *testing.T) {
	github := byHandGitHub(t, `[{"filename":"migrations/versions/0001_init.py"}]`)
	github.set(func(g *handoffGitHub) {
		g.compare = `{"status":"ahead","files":[{"filename":"migrations/versions/0002_more.py","status":"added"}]}`
	})
	h, facts, merger := newReadyHarness(t, github)
	code, body := ready(t, h, merger)
	if code != http.StatusConflict || !strings.HasPrefix(body, "READY_UNREVIEWED_CHANGE: ") || !strings.Contains(body, "migrations/versions/0002_more.py") {
		t.Fatalf("a by-hand READY above an unreviewed change = %d %q", code, body)
	}
	if got := facts.recorded(); len(got) != 0 {
		t.Fatalf("handoff facts = %#v, want none", got)
	}
}

// READY compares the approved head with the head GitHub shows, which READY's commit names, never
// the head the record holds: the record follows GitHub only by webhook, so it can still hold an
// earlier, retro-only push while GitHub's head already carries a product change above it. Comparing
// the recorded head would read retro's learnings alone and accept a change no review approved.
func TestHandoffCompleteReadyComparesGitHubsHeadNotALaggingRecord(t *testing.T) {
	const retroOnly = "e7200000000000000000000000000000000000ff"
	recordedAt, githubAt := time.Date(2026, 10, 12, 10, 0, 0, 0, time.UTC), time.Date(2026, 10, 12, 10, 5, 0, 0, time.UTC)
	github := newHandoffGitHub(t)
	github.set(func(g *handoffGitHub) {
		g.pullUpdatedAt = githubAt
		g.compare = `{"status":"ahead","files":[{"filename":"docs/solutions/legion/retro-LEGION-208.md","status":"added"},{"filename":"src/main.go","status":"modified"}]}`
		g.compareAnswers = map[string]string{approvedSHA + "..." + retroOnly: retroCompare}
	})
	h, facts, merger := newReadyHarness(t, github)
	seedPullRequestAt(t, h, "LEGION-208", retroOnly, recordedAt)
	code, body := ready(t, h, merger)
	if code != http.StatusConflict || !strings.HasPrefix(body, "READY_UNREVIEWED_CHANGE: ") || !strings.Contains(body, "src/main.go") {
		t.Fatalf("READY while the record lags at a retro-only head = %d %q; want READY_UNREVIEWED_CHANGE naming src/main.go", code, body)
	}
	if got := github.compares(); len(got) != 1 || got[0] != approvedSHA+"..."+headSHA {
		t.Fatalf("READY compared %v, want GitHub's head %s, not the record's %s", got, headSHA, retroOnly)
	}
	if got := facts.recorded(); len(got) != 0 {
		t.Fatalf("handoff facts = %#v, want none", got)
	}
}

// GitHub failing the compare is GitHub's failure, to retry, not the head's.
func TestHandoffCompleteReadyRefusesWhenGitHubFailsTheCompare(t *testing.T) {
	github := newHandoffGitHub(t)
	github.set(func(g *handoffGitHub) { g.fail["compare/"] = http.StatusInternalServerError })
	h, facts, merger := newReadyHarness(t, github)
	code, body := ready(t, h, merger)
	if code != http.StatusBadGateway || !strings.HasPrefix(body, "GITHUB_READ_FAILED: ") || !strings.Contains(body, "complete again") {
		t.Fatalf("READY while GitHub fails the compare = %d %q", code, body)
	}
	if got := facts.recorded(); len(got) != 0 {
		t.Fatalf("handoff facts = %#v, want none", got)
	}
}

// A pull request a person already merged is published unread, as before: its head can no longer
// change, and no compare is asked for.
func TestHandoffCompleteReadyComparesNothingForAMergedPullRequest(t *testing.T) {
	github := newHandoffGitHub(t)
	github.set(func(g *handoffGitHub) {
		g.merged = true
		g.compare = `{"status":"ahead","files":[{"filename":"src/main.go","status":"modified"}]}`
	})
	h, _, merger := newReadyHarness(t, github)
	if code, body := ready(t, h, merger); code != http.StatusOK || !strings.Contains(body, "already merged") {
		t.Fatalf("READY of a merged pull request = %d %q", code, body)
	}
	if got := github.compares(); len(got) != 0 {
		t.Fatalf("READY compared %v for a merged pull request, want nothing", got)
	}
}
