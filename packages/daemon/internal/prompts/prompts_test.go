package prompts

import (
	"os"
	"path/filepath"
	"reflect"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/sjawhar/legion/daemon/internal/claim"
	"github.com/sjawhar/legion/daemon/internal/promptrefs"
)

// A broken role sequence drops the shared core, uses a root prompt for a child, or makes the Go
// daemon's instructions win before the shared role instructions. The prompt paths are what the
// tmux runtime gives OMP in one --append-system-prompt word, so their order is the contract. The
// Go daemon's text is the role's own part, then the part every architect or every phase worker
// shares.
func TestComposeOrdersSharedRolePartsBeforeTheGoDaemonParts(t *testing.T) {
	stateDir := t.TempDir()
	composer, err := New(stateDir)
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	// Every operation the legion tool gives an architect besides register_gate
	// (packages/pi-legion/src/tools.ts), which a prompt that never names it leaves unused.
	architectOperations := []string{"`release_children`", "`park_child`", "`rerun_child`", "`request_backward_move`", "`retry_or_escalate`", "`sign_off`", "`read_record`"}

	for _, tc := range []struct {
		name     string
		role     claim.Role
		isRoot   bool
		shared   []string
		goParts  []string
		contains []string
	}{
		// Every architect is told what a pull-request-missing notice asks of it, whatever its tree.
		{"root architect", claim.RoleArchitect, true, []string{"architect-root.md"}, []string{"architect-root.md", "architect-common.md"}, append([]string{"it chooses and starts the next role", "`register_gate`", "end the tree with `close_root`", "Every notice about an issue you own arrives on your own role topic", "to merge a pull request that deletes it", "A `pull-request-missing` notice"}, architectOperations...)},
		{"sub-architect", claim.RoleArchitect, false, []string{"architect.md"}, []string{"architect.md", "architect-common.md"}, append([]string{"it chooses and starts the next role", "`register_gate` refuses a child issue", "Every notice about an issue you own arrives on your own role topic", "to merge a pull request that deletes it", "A `pull-request-missing` notice"}, architectOperations...)},
		{"planner", claim.RolePlanner, false, []string{"core/common.md", "core/planner.md", "mechanics/headless.md", "planner.md"}, []string{"planner.md", "worker-common.md"}, []string{`op: "handoff_complete"`, "jj -R \"$LEGION_WORKSPACE\" git push --bookmark legion/<issue>", "ends its head's commit message with `skip-checks: true` as the last line, preceded by two empty lines", "$'\\n\\n\\nskip-checks: true'", "finds, with your pane's jj, the newest commit on the issue branch carrying `.legion/<issue>/<phase>.json`", "The daemon reads no handoff file and no branch head", "A refusal records nothing"}},
		// The implementer is told the completion of implementing the handoff route refuses with no
		// open pull request, and what to do about it.
		{"implementer", claim.RoleImplementer, false, []string{"core/common.md", "core/implementer.md", "mechanics/headless.md", "implementer.md"}, []string{"implementer.md", "worker-common.md"}, []string{`op: "handoff_complete"`,
			"the merger reads the head for that directory itself and merges nothing while it is there", "(`HANDOFF_NO_PULL_REQUEST`), recording nothing: open the pull request and complete again at the same commit"}},
		// The tester runs the application, not CI again (dispatch://LEGION-631).
		{"tester", claim.RoleTester, false, []string{"core/common.md", "core/tester.md", "mechanics/headless.md", "tester.md"}, []string{"tester.md", "worker-common.md"}, []string{`op: "handoff_complete"`, `verdict: "pass"`, `verdict: "fail"`,
			"read CI for everything static", "try to break it"}},
		{"reviewer", claim.RoleReviewer, false, []string{"core/common.md", "core/reviewer.md", "mechanics/headless.md", "reviewer.md"}, []string{"reviewer.md", "worker-common.md"}, []string{`op: "handoff_complete"`,
			"Retro's last commit removes that directory above your approved head", "`pullRequest.head` is the commit you pushed"}},
		// The merge the merger is told to run names the head GitHub must match, on a queue base and
		// off it, and carries the message it writes itself; its completion is an ordinary phase
		// completion, refused only on the grounds every phase worker's is (dispatch://INFRA-714, R2).
		{"merger", claim.RoleMerger, false, []string{"mechanics/headless.md", "merger.md"}, []string{"merger.md", "worker-common.md"}, []string{`op: "handoff_complete"`,
			"gh pr merge <n> --squash --match-head-commit <head> --subject '<pull request title> (#<n>)' --body ''",
			"enqueuePullRequest", "squash_merge_commit_message",
			"`HANDOFF_NOT_CURRENT_PHASE`", "`HANDOFF_STALE_GENERATION`", "`HANDOFF_NO_RUN`",
			"`HANDOFF_ALREADY_RECORDED`", "`TREE_LINGERING`"}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			parts, err := composer.Compose(tc.role, tc.isRoot)
			if err != nil {
				t.Fatalf("Compose: %v", err)
			}
			want := make([]string, 0, len(tc.shared)+len(tc.goParts))
			for _, part := range tc.shared {
				want = append(want, filepath.Join(stateDir, "prompts", "shared", part))
			}
			for _, part := range tc.goParts {
				want = append(want, filepath.Join(stateDir, "prompts", "go", part))
			}
			if !reflect.DeepEqual(parts.RolePromptPaths, want) {
				t.Fatalf("RolePromptPaths = %q, want %q", parts.RolePromptPaths, want)
			}
			// A pane reads each shared part from the snapshot by path, so the snapshot holds the
			// embedded part byte for byte.
			for _, part := range tc.shared {
				got, err := os.ReadFile(filepath.Join(stateDir, "prompts", "shared", part))
				if err != nil {
					t.Fatalf("read the snapshot's %s: %v", part, err)
				}
				if string(got) != rolePart(t, part) {
					t.Errorf("the snapshot's %s is not the embedded part", part)
				}
			}
			// The runtimes join the parts byte for byte ($(cat …)), so the role's Go daemon part and
			// the shared one must meet at one blank line, as the paragraphs within a part do. The
			// phrases each role is held to are read from the whole composed prompt, shared parts and
			// the Go daemon's alike.
			var text strings.Builder
			for i, path := range parts.RolePromptPaths {
				body, err := os.ReadFile(path)
				if err != nil {
					t.Fatalf("read part: %v", err)
				}
				if joined := text.String(); i > len(tc.shared) {
					gap := len(joined) - len(strings.TrimRight(joined, "\n")) + len(body) - len(strings.TrimLeft(string(body), "\n"))
					if gap != 2 {
						t.Errorf("Go daemon parts %q join %s with %d newlines, want one blank line", tc.goParts, path, gap)
					}
				}
				text.Write(body)
			}
			for _, requirement := range append([]string{
				"This Go daemon advances phases",
				"re-read your issue record with the `legion` tool's `read_record`",
			}, tc.contains...) {
				if !strings.Contains(text.String(), requirement) {
					t.Errorf("the composed %s prompt omits %q", tc.name, requirement)
				}
			}
		})
	}
}

// The planner this daemon composes runs the gap analyst before it drafts and the plan reviewer
// after (LEGION-421). Only the shared headless residue names them: the core is also composed with
// the interactive fragment, and no load probe checks the agents an interactive session names, and
// the Go daemon's own parts leave the checks to the shared text.
func TestTheComposedPlannerDispatchesItsPlanChecksFromTheHeadlessResidue(t *testing.T) {
	stateDir := t.TempDir()
	composer, err := New(stateDir)
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	parts, err := composer.Compose(claim.RolePlanner, false)
	if err != nil {
		t.Fatalf("Compose: %v", err)
	}
	var text strings.Builder
	dispatched := promptrefs.New()
	for _, path := range parts.RolePromptPaths {
		body, err := os.ReadFile(path)
		if err != nil {
			t.Fatalf("read %s: %v", path, err)
		}
		text.Write(body)
		rel, err := filepath.Rel(stateDir, path)
		if err != nil {
			t.Fatal(err)
		}
		dispatched.Text(filepath.Join("state", rel), body)
	}
	gap := strings.Index(text.String(), "`task(agent=\"plan-gap-analyst\")`")
	review := strings.Index(text.String(), "`task(agent=\"plan-reviewer\")`")
	if gap < 0 || review < gap {
		t.Errorf("the composed planner dispatches the gap analyst at %d and the plan reviewer at %d, want the analyst first", gap, review)
	}
	want := []string{"plan-gap-analyst", "plan-reviewer"}
	residue := []string{filepath.Join("state", "prompts", "shared", "planner.md")}
	for _, agent := range want {
		files, named := dispatched[promptrefs.TaskAgents][agent]
		if !named {
			t.Errorf("the composed planner does not dispatch task agent %s", agent)
			continue
		}
		if !slices.Equal(files, residue) {
			t.Errorf("task agent %s is dispatched by %q, want the headless residue %q alone", agent, files, residue)
		}
	}
}

// composedText is the whole text a role's composed prompt gives its pane: the parts Compose names,
// in the order the tmux runtime appends them (Parts.RolePromptPaths), read back from the snapshot
// and joined. A test that asks whether a prompt still says something retired asks it of this join,
// since which part holds the words is no part of the claim, and a compose or a read that fails
// leaves the question unanswered rather than unsaid, so both end the test (t.Fatalf).
func composedText(t *testing.T, composer *Composer, role claim.Role, isRoot bool) string {
	t.Helper()
	parts, err := composer.Compose(role, isRoot)
	if err != nil {
		t.Fatalf("Compose %s (root %t): %v", role, isRoot, err)
	}
	var text strings.Builder
	for _, path := range parts.RolePromptPaths {
		body, err := os.ReadFile(path)
		if err != nil {
			t.Fatalf("read %s: %v", path, err)
		}
		text.Write(body)
	}
	return text.String()
}

// No role's handoffs reach the default branch: retro's last commit removes the issue's own
// .legion/<issue>/ from the head that merges (dispatch://LEGION-605). No phase worker's composed
// prompt still carries the rule this replaced: that the approved and the merged head keep .legion/,
// and that the daemon strips the leftovers from the next issue's branch. The merger reads the head
// for the directory itself before it merges (dispatch://INFRA-714, R2), and no prompt holds the
// head that merges to the head a review approved: the repository's own rulesets decide that.
func TestNoComposedPromptKeepsTheIssuesHandoffsOnTheMergedHead(t *testing.T) {
	stateDir := t.TempDir()
	composer, err := New(stateDir)
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	retired := []string{"No role pushes a `.legion/` deletion", "No role removes `.legion/`", "nobody removes it before the merge", "strips whatever `.legion/`", "still carries `.legion/`",
		"Retro then commits only `docs/solutions/`", "no file changes above the approved head", "the same sha when retro added nothing", "Retro's `docs/solutions/` commit is never",
		"'~(docs/solutions | .legion/<issue>)'", "`READY_UNREVIEWED_CHANGE`", "`READY_NOT_APPROVED`", "Approved head:"}
	for _, role := range []claim.Role{claim.RolePlanner, claim.RoleImplementer, claim.RoleTester, claim.RoleReviewer, claim.RoleMerger} {
		text := composedText(t, composer, role, false)
		for _, phrase := range retired {
			if strings.Contains(text, phrase) {
				t.Errorf("the composed %s prompt still says %q", role, phrase)
			}
		}
		if want := "repos/{owner}/{repo}/contents/.legion/<issue>?ref=<head>"; role == claim.RoleMerger && !strings.Contains(text, want) {
			t.Errorf("the merger's prompt does not name %s, its own read of the head for the issue's handoffs", want)
		}
	}
}

// GitHub, under each repository's base-branch rules, decides whether a merge happens, and no
// prompt may tell an agent that Legion refuses one over a check (dispatch://INFRA-83). READY reads
// none of the head's checks and judges none, so no composed prompt — any phase worker's, and the
// architect's in both its shapes — still names the refusal that read them, the packet note that
// said which checks went unread, or a trailer the daemon turns away for the CI it leaves the head
// without.
func TestNoComposedPromptSaysLegionRefusesAMergeOverACheck(t *testing.T) {
	composer, err := New(t.TempDir())
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	retired := []string{"READY_CHECKS_NOT_GREEN", "no check is required on", "refused at READY"}
	for _, tc := range []struct {
		role   claim.Role
		isRoot bool
	}{
		{claim.RolePlanner, false}, {claim.RoleImplementer, false}, {claim.RoleTester, false},
		{claim.RoleReviewer, false}, {claim.RoleMerger, false},
		{claim.RoleArchitect, true}, {claim.RoleArchitect, false},
	} {
		text := composedText(t, composer, tc.role, tc.isRoot)
		for _, phrase := range retired {
			if strings.Contains(text, phrase) {
				t.Errorf("the composed %s prompt (root %t) still says %q", tc.role, tc.isRoot, phrase)
			}
		}
	}
}

// A state directory outlives the daemon binary that wrote its prompt snapshot, so a boot of a newer
// daemon finds the older daemon's words there. A part whose content is not the running binary's is
// rewritten, shared or the daemon's own: a prompt that contradicts the daemon's own behaviour (who
// posts READY) is the defect, and nothing treats these files as the operator's to edit (the
// operator's text is `instructions`). A part already holding the running binary's content is left
// as it is, so an ordinary restart writes nothing.
func TestNewRewritesAPartTheRunningDaemonDidNotWrite(t *testing.T) {
	stateDir := t.TempDir()
	if _, err := New(stateDir); err != nil {
		t.Fatalf("first New: %v", err)
	}
	snapshot := func(name string) string { return filepath.Join(stateDir, "prompts", name) }
	embedded := map[string]string{
		"go/merger.md":          "go/merger.md",
		"go/tester.md":          "go/tester.md",
		"shared/merger.md":      "roles/merger.md",
		"shared/core/tester.md": "roles/core/tester.md",
	}
	stale := []string{"go/merger.md", "shared/merger.md"}
	current := []string{"go/tester.md", "shared/core/tester.md"}
	for _, name := range stale {
		if err := os.WriteFile(snapshot(name), []byte("an older daemon's "+name+"\n"), 0o600); err != nil {
			t.Fatalf("write the older part: %v", err)
		}
	}
	written := time.Date(2026, 1, 2, 3, 4, 5, 0, time.UTC)
	for _, name := range current {
		if err := os.Chtimes(snapshot(name), written, written); err != nil {
			t.Fatalf("date the current part: %v", err)
		}
	}
	if _, err := New(stateDir); err != nil {
		t.Fatalf("second New: %v", err)
	}
	for name, source := range embedded {
		parts := goParts
		if strings.HasPrefix(source, "roles/") {
			parts = roleParts
		}
		want, err := parts.ReadFile(source)
		if err != nil {
			t.Fatalf("read embedded %s: %v", source, err)
		}
		got, err := os.ReadFile(snapshot(name))
		if err != nil {
			t.Fatalf("read %s: %v", name, err)
		}
		if string(got) != string(want) {
			t.Errorf("%s after the second New = %q, want the running daemon's part", name, got)
		}
	}
	for _, name := range current {
		if info, err := os.Stat(snapshot(name)); err != nil || !info.ModTime().Equal(written) {
			t.Errorf("%s, already holding the running daemon's content, was written again: %v", name, err)
		}
	}
}
