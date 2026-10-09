package main

import (
	"bytes"
	"context"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/sjawhar/legion/daemon/internal/phase"
)

// A merger's READY names a head a human merges, which GitHub merges only once every check the base
// branch requires has succeeded there. A head whose push skipped CI when it should not have reports
// none of them, so the completion refuses READY naming the head and the check, and nothing reaches
// the daemon; a required check still running, or one that failed, is refused the same way.
//
// A pull request that conflicts with its base (mergeable_state "dirty") gets no pull_request run,
// so a required check with no result on its head is refused naming the conflict rather than a
// skipped push. The conflict changes only that text, never which heads are refused: every row is
// posted or refused by its checks alone, whatever its mergeable_state, and a conflicting head
// whose required checks all succeeded is posted.
func TestHandoffCompleteReadyRefusesAHeadWithoutItsRequiredChecksGreen(t *testing.T) {
	const (
		ciGreen      = `{"id":1,"name":"ci","status":"completed","conclusion":"success"}`
		ciSkipped    = `{"id":1,"name":"ci","status":"completed","conclusion":"skipped"}`
		ciRunning    = `{"id":1,"name":"ci","status":"in_progress","conclusion":null}`
		legacyGreen  = `{"context":"legacy","state":"success"}`
		legacyFailed = `{"context":"legacy","state":"failure"}`
		skippedPush  = `: its push may have skipped CI`
		conflict     = `: the pull request conflicts with main, and GitHub starts no pull_request CI`
	)
	for _, tc := range []struct {
		name, checkRun, status, mergeableState, refusal string
	}{
		{"every required check green", ciGreen, legacyGreen, "clean", ""},
		{"a required check that ended skipped counts", ciSkipped, legacyGreen, "clean", ""},
		{"every required check green on a conflicting pull request", ciGreen, legacyGreen, "dirty", ""},
		{"a head whose push skipped CI", "", "", "blocked", `head c0de00000000 of pull request #42 has no result for the required check "ci"` + skippedPush},
		{"a head whose mergeability GitHub has not computed", "", "", "unknown", `head c0de00000000 of pull request #42 has no result for the required check "ci"` + skippedPush},
		{"a head of a conflicting pull request", "", "", "dirty", `head c0de00000000 of pull request #42 has no result for the required check "ci"` + conflict},
		{"a conflicting pull request missing one required check", ciGreen, "", "dirty", `head c0de00000000 of pull request #42 has no result for the required check "legacy"` + conflict},
		{"a required check still running", ciRunning, legacyGreen, "blocked", `the required check "ci" is still running on head c0de00000000`},
		{"a required check still running on a conflicting pull request", ciRunning, legacyGreen, "dirty", `the required check "ci" is still running on head c0de00000000`},
		{"a required status that failed", ciGreen, legacyFailed, "blocked", `the required check "legacy" ended failure on head c0de00000000`},
		{"a required status that failed on a conflicting pull request", ciGreen, legacyFailed, "dirty", `the required check "legacy" ended failure on head c0de00000000`},
	} {
		t.Run(tc.name, func(t *testing.T) {
			workspace := t.TempDir()
			t.Setenv("LEGION_ROLE", "merger")
			fakeHandoffJJ(t, "beef")
			bodies := handoffDaemon(t, phase.Merging)
			readyGitHub(t, tc.checkRun, tc.status, tc.mergeableState)
			var out, errb bytes.Buffer
			code := run(context.Background(), []string{"legion", "handoff", "complete", "--workspace", workspace, "--summary", "gate facts hold", "--ready"}, &out, &errb)
			if tc.refusal == "" {
				if code != 0 || len(*bodies) != 1 {
					t.Fatalf("READY = %d, daemon read %v, stderr %q; want it posted", code, *bodies, errb.String())
				}
				return
			}
			if code != 1 || len(*bodies) != 0 {
				t.Fatalf("READY = %d, daemon read %v, stderr %q; want it refused and nothing posted", code, *bodies, errb.String())
			}
			if !strings.Contains(errb.String(), "READY refused: "+tc.refusal) {
				t.Fatalf("READY refused with stderr %q; want the refusal to name %q", errb.String(), tc.refusal)
			}
		})
	}
}

// A merger's READY names the head a human merges, and a squash merge commits that head merged into
// the default branch: the issue's own handoffs, .legion/<issue>/, must be gone from it (retro's last
// commit removes them; dispatch://LEGION-605). READY reads the head's tree on GitHub and is refused,
// naming the head and the directory, while it still holds .legion/<issue>/, whether or not the base
// branch requires any check, and posted once GitHub answers that the head has none. A read GitHub
// fails leaves the head unknown, and READY is refused as GitHub's failure to retry, never sending the
// issue back to retro. A pull request a person already merged is posted unread: its head can no
// longer change, and the workflow takes it on to the production check only on that READY.
func TestHandoffCompleteReadyRefusesAHeadThatStillCarriesTheIssuesHandoffs(t *testing.T) {
	const (
		head          = "c0de0000000000000000000000000000000000ff"
		listing       = `[{"name":"implement.json","path":".legion/THIS-1/implement.json","type":"file"}]`
		notFound      = `{"message":"Not Found","status":"404"}`
		requiresCI    = `[{"type":"required_status_checks","parameters":{"required_status_checks":[{"context":"ci"}]}}]`
		stillCarries  = `head c0de00000000 of pull request #42 still carries .legion/THIS-1/`
		unprotected   = `{"name":"main","protected":false}`
		readRefused   = `GitHub's read of .legion/THIS-1/ at head c0de00000000 of pull request #42 failed, so whether the head still carries it is unknown: complete again`
		serverFailure = `{"message":"Server Error"}`
	)
	for _, tc := range []struct {
		name, rules  string
		merged       bool
		handoffs     int
		handoffsBody string
		refusal      string
	}{
		{"a head that still carries them", requiresCI, false, http.StatusOK, listing, stillCarries},
		{"a head that still carries them, on a base requiring no check", `[]`, false, http.StatusOK, listing, stillCarries},
		{"a head retro removed them from", requiresCI, false, http.StatusNotFound, notFound, ""},
		{"a head whose tree GitHub fails to read", requiresCI, false, http.StatusInternalServerError, serverFailure, readRefused},
		{"a pull request a person merged while its head still carried them", requiresCI, true, http.StatusOK, listing, ""},
	} {
		t.Run(tc.name, func(t *testing.T) {
			repo := "/repos/acme/widgets"
			server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				w.Header().Set("Content-Type", "application/json")
				switch r.URL.Path {
				case repo + "/pulls/42":
					_, _ = fmt.Fprintf(w, `{"head":{"sha":"%s"},"base":{"ref":"main"},"mergeable_state":"clean","merged":%t}`, head, tc.merged)
				case repo + "/contents/.legion/THIS-1":
					if tc.merged {
						t.Errorf("READY read the handoffs of a merged pull request's head")
					}
					if ref := r.URL.Query().Get("ref"); ref != head {
						t.Errorf("the handoffs were read at ref %q, want the pull request's head %s", ref, head)
					}
					w.WriteHeader(tc.handoffs)
					_, _ = w.Write([]byte(tc.handoffsBody))
				case repo + "/rules/branches/main":
					if tc.merged {
						t.Errorf("READY read the required checks of a merged pull request")
					}
					_, _ = w.Write([]byte(tc.rules))
				case repo + "/branches/main":
					_, _ = w.Write([]byte(unprotected))
				case repo + "/commits/" + head + "/check-runs":
					_, _ = w.Write([]byte(`{"total_count":1,"check_runs":[{"id":1,"name":"ci","status":"completed","conclusion":"success"}]}`))
				case repo + "/commits/" + head + "/status":
					_, _ = w.Write([]byte(`{"statuses":[]}`))
				default:
					http.NotFound(w, r)
				}
			}))
			t.Cleanup(server.Close)
			workspace := t.TempDir()
			t.Setenv("LEGION_ROLE", "merger")
			fakeHandoffJJ(t, "beef")
			bodies := handoffDaemon(t, phase.Merging)
			t.Setenv("LEGION_GITHUB_API_URL", server.URL)
			var out, errb bytes.Buffer
			code := run(context.Background(), []string{"legion", "handoff", "complete", "--workspace", workspace, "--summary", "gate facts hold", "--ready"}, &out, &errb)
			if tc.refusal == "" {
				if code != 0 || len(*bodies) != 1 {
					t.Fatalf("READY = %d, daemon read %v, stderr %q; want it posted", code, *bodies, errb.String())
				}
				if want := "pull request #42 is already merged"; tc.merged && !strings.Contains(out.String(), want) {
					t.Fatalf("READY of a merged pull request said %q; want it to say %q", out.String(), want)
				}
				return
			}
			if code != 1 || len(*bodies) != 0 || !strings.Contains(errb.String(), "READY refused: ") || !strings.Contains(errb.String(), tc.refusal) {
				t.Fatalf("READY = %d, daemon read %v, stderr %q; want it refused naming %q and nothing posted", code, *bodies, errb.String(), tc.refusal)
			}
		})
	}
}

// A ruleset can also require a workflow to succeed (rule type workflows), which GitHub judges by
// that workflow's run for the pull request's head. The rule shapes are a live repository's: a
// workflows rule for its review workflow, a required_status_checks rule for one aggregator check,
// a pull_request rule requiring no approval, and a branch protection requiring nothing. With the
// required check green, READY waits on the review workflow's latest pull_request run on the head:
// refused while it failed, still runs, or never ran, and posted once it succeeded. A workflow
// another repository defines (an organization ruleset can require one) never matches a run here,
// whatever the head ran, so READY is refused naming that repository rather than a skipped push.
func TestHandoffCompleteReadyJudgesARequiredWorkflowByItsRunOnTheHead(t *testing.T) {
	const (
		head   = "c0de0000000000000000000000000000000000ff"
		review = `{"id":%d,"name":"Review PR #42","path":".github/workflows/claude-pr-review.yml","event":"pull_request","status":"%s","conclusion":%s,"repository":{"id":%d}}`
		rules  = `[{"type":"workflows","parameters":{"do_not_enforce_on_create":true,"workflows":[{"repository_id":4242,"path":".github/workflows/claude-pr-review.yml","ref":"refs/heads/main"}]}},` +
			`{"type":"deletion","parameters":null},` +
			`{"type":"pull_request","parameters":{"required_approving_review_count":0,"dismiss_stale_reviews_on_push":true,"require_code_owner_review":false}},` +
			`{"type":"required_status_checks","parameters":{"strict_required_status_checks_policy":false,"required_status_checks":[{"context":"pr-checks-result","integration_id":15368}]}}]`
	)
	reviewRun := func(id int, status, conclusion string) string {
		return fmt.Sprintf(review, id, status, conclusion, 4242)
	}
	for _, tc := range []struct {
		name string
		runs []string
		// base is the id of the pull request's base repository.
		base    int
		refusal string
	}{
		{"while the required workflow failed", []string{reviewRun(7, "completed", `"failure"`)}, 4242, `the required workflow ".github/workflows/claude-pr-review.yml" ended failure on head c0de00000000`},
		{"while the required workflow still runs", []string{reviewRun(7, "in_progress", "null")}, 4242, `the required workflow ".github/workflows/claude-pr-review.yml" is still running on head c0de00000000`},
		{"while the head has no run of the required workflow", nil, 4242, `head c0de00000000 of pull request #42 has no run of the required workflow ".github/workflows/claude-pr-review.yml": its push may have skipped CI`},
		{"while another repository defines the required workflow", []string{fmt.Sprintf(review, 7, "completed", `"success"`, 5151)}, 5151,
			`the required workflow ".github/workflows/claude-pr-review.yml" is defined in repository 4242, not in pull request #42's own (5151)`},
		{"once the required workflow succeeded", []string{reviewRun(7, "completed", `"success"`)}, 4242, ""},
		{"once a later run of the required workflow succeeded", []string{reviewRun(7, "completed", `"failure"`), reviewRun(9, "completed", `"success"`)}, 4242, ""},
	} {
		t.Run(tc.name, func(t *testing.T) {
			repo := "/repos/acme/widgets"
			server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				w.Header().Set("Content-Type", "application/json")
				switch r.URL.Path {
				case repo + "/pulls/42":
					_, _ = fmt.Fprintf(w, `{"head":{"sha":"%s"},"base":{"ref":"main","repo":{"id":%d}},"mergeable_state":"blocked"}`, head, tc.base)
				case repo + "/rules/branches/main":
					_, _ = w.Write([]byte(rules))
				case repo + "/branches/main":
					_, _ = w.Write([]byte(`{"name":"main","protected":false}`))
				case repo + "/commits/" + head + "/check-runs":
					_, _ = w.Write([]byte(`{"total_count":1,"check_runs":[{"id":1,"name":"pr-checks-result","status":"completed","conclusion":"success"}]}`))
				case repo + "/commits/" + head + "/status":
					_, _ = w.Write([]byte(`{"statuses":[]}`))
				case repo + "/actions/runs":
					if r.URL.Query().Get("head_sha") != head {
						http.NotFound(w, r)
						return
					}
					_, _ = fmt.Fprintf(w, `{"total_count":%d,"workflow_runs":[%s]}`, len(tc.runs), strings.Join(tc.runs, ","))
				default:
					http.NotFound(w, r)
				}
			}))
			t.Cleanup(server.Close)
			workspace := t.TempDir()
			t.Setenv("LEGION_ROLE", "merger")
			fakeHandoffJJ(t, "beef")
			bodies := handoffDaemon(t, phase.Merging)
			t.Setenv("LEGION_GITHUB_API_URL", server.URL)
			var out, errb bytes.Buffer
			code := run(context.Background(), []string{"legion", "handoff", "complete", "--workspace", workspace, "--summary", "gate facts hold", "--ready"}, &out, &errb)
			if tc.refusal == "" {
				if code != 0 || len(*bodies) != 1 {
					t.Fatalf("READY = %d, daemon read %v, stderr %q; want it posted", code, *bodies, errb.String())
				}
				return
			}
			if code != 1 || len(*bodies) != 0 || !strings.Contains(errb.String(), "READY refused: "+tc.refusal) {
				t.Fatalf("READY = %d, daemon read %v, stderr %q; want it refused naming %q and nothing posted", code, *bodies, errb.String(), tc.refusal)
			}
		})
	}
}

// A private repository whose plan has no rulesets answers the rulesets read 403, "make this
// repository public to enable this feature" (docs/solutions/legion/controller-gate-2-required-checks-live-reads.md).
// It can define no ruleset, so none requires a check there, and READY rests on the branch's
// protection alone: posted when that requires nothing, refused when it requires a check the head
// lacks. Only that answer means no rulesets: a rulesets read that fails otherwise (another 403,
// such as a token that lost access, or a server error) leaves the required checks unknown, and
// READY is refused naming the read.
func TestHandoffCompleteReadyOnARepositoryWhosePlanHasNoRulesets(t *testing.T) {
	const planAnswer = `{"message":"Upgrade to GitHub Pro or make this repository public to enable this feature.","status":"403"}`
	unprotected := `{"name":"main","protected":false}`
	for _, tc := range []struct {
		name, branch string
		rulesStatus  int
		rulesBody    string
		posted       bool
		refusal      string
	}{
		{"and no branch protection", unprotected, http.StatusForbidden, planAnswer, true, ""},
		{"and branch protection requiring a check the head lacks", `{"name":"main","protected":true,"protection":{"required_status_checks":{"contexts":["legacy"]}}}`, http.StatusForbidden, planAnswer, false, `has no result for the required check "legacy"`},
		{"but the read is refused for another reason", unprotected, http.StatusForbidden, `{"message":"Resource not accessible by integration","status":"403"}`, false, "GET /rules/branches/main with 403"},
		{"but the read fails", unprotected, http.StatusInternalServerError, `{"message":"Server Error"}`, false, "GET /rules/branches/main with 500"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			repo := "/repos/acme/widgets"
			server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				w.Header().Set("Content-Type", "application/json")
				switch r.URL.Path {
				case repo + "/pulls/42":
					_, _ = w.Write([]byte(`{"head":{"sha":"c0de0000000000000000000000000000000000ff"},"base":{"ref":"main"}}`))
				case repo + "/rules/branches/main":
					w.WriteHeader(tc.rulesStatus)
					_, _ = w.Write([]byte(tc.rulesBody))
				case repo + "/branches/main":
					_, _ = w.Write([]byte(tc.branch))
				case repo + "/commits/c0de0000000000000000000000000000000000ff/check-runs":
					_, _ = w.Write([]byte(`{"total_count":0,"check_runs":[]}`))
				case repo + "/commits/c0de0000000000000000000000000000000000ff/status":
					_, _ = w.Write([]byte(`{"statuses":[]}`))
				default:
					http.NotFound(w, r)
				}
			}))
			t.Cleanup(server.Close)
			workspace := t.TempDir()
			t.Setenv("LEGION_ROLE", "merger")
			fakeHandoffJJ(t, "beef")
			bodies := handoffDaemon(t, phase.Merging)
			t.Setenv("LEGION_GITHUB_API_URL", server.URL)
			var out, errb bytes.Buffer
			code := run(context.Background(), []string{"legion", "handoff", "complete", "--workspace", workspace, "--summary", "gate facts hold", "--ready"}, &out, &errb)
			if tc.posted {
				// A base branch requiring no check has nothing to refuse, and READY says so rather
				// than reading like a head whose every required check was read and passed.
				if code != 0 || len(*bodies) != 1 || !strings.Contains(out.String(), `no check is required on "main" of acme/widgets`) {
					t.Fatalf("READY = %d, daemon read %v, stdout %q, stderr %q; want it posted, saying it read no checks", code, *bodies, out.String(), errb.String())
				}
				return
			}
			if code != 1 || len(*bodies) != 0 || !strings.Contains(errb.String(), tc.refusal) {
				t.Fatalf("READY = %d, daemon read %v, stderr %q; want a refusal naming %q", code, *bodies, errb.String(), tc.refusal)
			}
		})
	}
}

// A pull_request concurrency group cancels an earlier, still-queued run once a newer push's run
// for the same head's workflow starts (a `pull_request` `edited`/`synchronize` re-trigger after a
// concurrency-group cancellation, LEGION's own pr-title.yaml among them): GitHub's check-runs API
// documents no ordering for the list, unlike the combined-status endpoint, so the cancelled run
// can list after the later run that actually succeeded. READY must still see the later run's
// success rather than the cancelled run a naive "last one wins" read would keep.
func TestHandoffCompleteReadyKeepsTheNewestCheckRunWhenGitHubListsAnOlderCancelledDuplicateLast(t *testing.T) {
	workspace := t.TempDir()
	t.Setenv("LEGION_ROLE", "merger")
	fakeHandoffJJ(t, "beef")
	bodies := handoffDaemon(t, phase.Merging)
	// id 5 (the later run, success) listed before id 2 (the earlier run a concurrency-group
	// cancellation superseded) - the one ordering a "keep whichever is last" read gets wrong.
	readyGitHub(t,
		`{"id":5,"name":"ci","status":"completed","conclusion":"success"},`+
			`{"id":2,"name":"ci","status":"completed","conclusion":"cancelled"}`,
		`{"context":"legacy","state":"success"}`, "clean")
	var out, errb bytes.Buffer
	code := run(context.Background(), []string{"legion", "handoff", "complete", "--workspace", workspace, "--summary", "gate facts hold", "--ready"}, &out, &errb)
	if code != 0 || len(*bodies) != 1 {
		t.Fatalf("READY = %d, daemon read %v, stderr %q; want it posted since the newest \"ci\" run (id 5) succeeded", code, *bodies, errb.String())
	}
}
