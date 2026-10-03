package prompts

import (
	"fmt"
	"os"
	"path/filepath"
	"reflect"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/sjawhar/legion/daemon/internal/claim"
	"github.com/sjawhar/legion/daemon/internal/promptrefs"
	"github.com/sjawhar/legion/daemon/internal/record"
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
	// (packages/pi-envoy/src/legion/tools.ts), which a prompt that never names it leaves unused.
	architectOperations := []string{"`release_children`", "`park_child`", "`rerun_child`", "`request_backward_move`", "`retry_or_escalate`", "`sign_off`", "`read_record`"}

	for _, tc := range []struct {
		name     string
		role     claim.Role
		isRoot   bool
		shared   []string
		goParts  []string
		contains []string
	}{
		{"root architect", claim.RoleArchitect, true, []string{"architect-root.md"}, []string{"architect-root.md", "architect-common.md"}, append([]string{"Do not schedule a phase or call `spawn_worker`", "`register_gate`", "end the tree with `close_root`", "Every notice about an issue you own arrives on your own role topic"}, architectOperations...)},
		{"sub-architect", claim.RoleArchitect, false, []string{"architect.md"}, []string{"architect.md", "architect-common.md"}, append([]string{"Do not schedule a phase or call `spawn_worker`", "`register_gate` refuses a child issue", "Every notice about an issue you own arrives on your own role topic"}, architectOperations...)},
		{"planner", claim.RolePlanner, false, []string{"core/common.md", "core/planner.md", "mechanics/headless.md", "planner.md"}, []string{"planner.md", "worker-common.md"}, []string{`op: "handoff_complete"`, "push it with `legion push`", "whose `verdict` is `\"changes_requested\"`"}},
		{"implementer", claim.RoleImplementer, false, []string{"core/common.md", "core/implementer.md", "mechanics/headless.md", "implementer.md"}, []string{"implementer.md", "worker-common.md"}, []string{`op: "handoff_complete"`}},
		{"tester", claim.RoleTester, false, []string{"core/common.md", "core/tester.md", "mechanics/headless.md", "tester.md"}, []string{"tester.md", "worker-common.md"}, []string{`op: "handoff_complete"`, `verdict: "pass"`, `verdict: "fail"`}},
		{"reviewer", claim.RoleReviewer, false, []string{"core/common.md", "core/reviewer.md", "mechanics/headless.md", "reviewer.md"}, []string{"reviewer.md", "worker-common.md"}, []string{`op: "handoff_complete"`,
			"submit `APPROVE` on a clean head even though it carries `.legion/`", "`pullRequest.head` is the commit you pushed"}},
		// The packet limit the merger is told is the one the handoff route enforces.
		{"merger", claim.RoleMerger, false, []string{"mechanics/headless.md", "merger.md"}, []string{"merger.md", "worker-common.md"}, []string{`op: "handoff_complete"`, "`ready: true`", "refuses READY unless every check the base branch requires", fmt.Sprintf("at most %d characters", record.MessagePostLimit)}},
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
			// The runtimes join the parts byte for byte ($(cat …)), so the role's part and the shared
			// one must meet at one blank line, as the paragraphs within a part do.
			var text strings.Builder
			for i, path := range parts.RolePromptPaths[len(tc.shared):] {
				body, err := os.ReadFile(path)
				if err != nil {
					t.Fatalf("read Go daemon part: %v", err)
				}
				if joined := text.String(); i > 0 {
					gap := len(joined) - len(strings.TrimRight(joined, "\n")) + len(body) - len(strings.TrimLeft(string(body), "\n"))
					if gap != 2 {
						t.Errorf("Go daemon parts %q join %s with %d newlines, want one blank line", tc.goParts, path, gap)
					}
				}
				text.Write(body)
			}
			for _, requirement := range append([]string{
				"This Go daemon advances phases",
				"re-read your issue record with `legion state`",
			}, tc.contains...) {
				if !strings.Contains(text.String(), requirement) {
					t.Errorf("Go daemon parts %q omit %q", tc.goParts, requirement)
				}
			}
		})
	}
}

// The planner this daemon composes runs the gap analyst before it drafts and the plan reviewer
// after (LEGION-421), each a task agent the plugin ships in agents/, where Oh My Pi finds it in a
// pane and in a pod; an agent missing there is one the boot gate refuses by name. Only the shared
// headless residue dispatches them: the core is also composed with the interactive fragment, whose
// subagent dispatches nothing, and the Go daemon's own parts leave the checks to the shared text.
// The boot gate reads the same references (RoleReferences).
func TestTheComposedPlannerDispatchesItsPlanChecksToShippedAgents(t *testing.T) {
	plugin := filepath.Join("..", "..", "..", "pi-envoy")
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
		if err := dispatched.File(stateDir, path, "state"); err != nil {
			t.Fatalf("references of %s: %v", path, err)
		}
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
		if _, err := os.Stat(filepath.Join(plugin, "agents", agent+".md")); err != nil {
			t.Errorf("task agent %s, dispatched by %q, is not shipped in the plugin's agents/: %v", agent, files, err)
		}
	}
	gated := RoleReferences()
	for _, agent := range want {
		if files := gated[promptrefs.TaskAgents][agent]; !slices.Equal(files, []string{"roles/planner.md"}) {
			t.Errorf("the boot gate's references name task agent %s from %q, want roles/planner.md", agent, files)
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
