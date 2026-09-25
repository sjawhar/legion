package prompts

import (
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"
	"time"

	"github.com/sjawhar/legion/daemon/internal/claim"
)

// A broken role sequence drops the shared core, uses a root prompt for a child, or makes the Go
// daemon's instructions win before the shared role instructions. The prompt paths are what the
// tmux runtime gives OMP in one --append-system-prompt word, so their order is the contract.
func TestComposeOrdersSharedRolePartsBeforeTheGoDaemonPart(t *testing.T) {
	rolesDir := completeRolesDir(t)
	stateDir := t.TempDir()
	composer, err := New(rolesDir, stateDir)
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	// Every operation the Go tool gives an architect besides register_gate
	// (packages/pi-envoy/src/legion/go-tools.ts), which a prompt that never names it leaves unused.
	architectOperations := []string{"`release_children`", "`park_child`", "`rerun_child`", "`request_backward_move`", "`retry_or_escalate`", "`sign_off`", "`read_record`"}

	for _, tc := range []struct {
		name     string
		role     claim.Role
		isRoot   bool
		shared   []string
		goPart   string
		contains []string
	}{
		{"root architect", claim.RoleArchitect, true, []string{"architect-root.md"}, "architect-root.md", append([]string{"Do not schedule a phase or call `spawn_worker`", "`register_gate`"}, architectOperations...)},
		{"sub-architect", claim.RoleArchitect, false, []string{"architect.md"}, "architect.md", append([]string{"Do not schedule a phase or call `spawn_worker`", "`register_gate` refuses a child issue"}, architectOperations...)},
		{"planner", claim.RolePlanner, false, []string{"core/common.md", "core/planner.md", "mechanics/headless.md", "planner.md"}, "planner.md", []string{`op: "handoff_complete"`}},
		{"implementer", claim.RoleImplementer, false, []string{"core/common.md", "core/implementer.md", "mechanics/headless.md", "implementer.md"}, "implementer.md", []string{`op: "handoff_complete"`}},
		{"tester", claim.RoleTester, false, []string{"core/common.md", "core/tester.md", "mechanics/headless.md", "tester.md"}, "tester.md", []string{`op: "handoff_complete"`, `verdict: "pass"`, `verdict: "fail"`}},
		{"reviewer", claim.RoleReviewer, false, []string{"core/common.md", "core/reviewer.md", "mechanics/headless.md", "reviewer.md"}, "reviewer.md", []string{`op: "handoff_complete"`}},
		{"merger", claim.RoleMerger, false, []string{"mechanics/headless.md", "merger.md"}, "merger.md", []string{`op: "handoff_complete"`, "`ready: true`"}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			parts, err := composer.Compose(tc.role, tc.isRoot)
			if err != nil {
				t.Fatalf("Compose: %v", err)
			}
			want := make([]string, 0, len(tc.shared)+1)
			for _, part := range tc.shared {
				want = append(want, filepath.Join(rolesDir, part))
			}
			want = append(want, filepath.Join(stateDir, "prompts", "go", tc.goPart))
			if !reflect.DeepEqual(parts.RolePromptPaths, want) {
				t.Fatalf("RolePromptPaths = %q, want %q", parts.RolePromptPaths, want)
			}
			body, err := os.ReadFile(parts.RolePromptPaths[len(parts.RolePromptPaths)-1])
			if err != nil {
				t.Fatalf("read Go daemon part: %v", err)
			}
			text := string(body)
			for _, requirement := range append([]string{
				"This Go daemon advances phases",
				"re-read your issue record with `legion state`",
				"notifications.legion.<project>.<issue>",
			}, tc.contains...) {
				if !strings.Contains(text, requirement) {
					t.Errorf("Go daemon part %s omits %q", tc.goPart, requirement)
				}
			}
		})
	}
}

// A daemon must fail before any worker is launched when the source role-prompt directory is
// incomplete; otherwise a production launch reports an opaque shell-level cat failure.
func TestNewRefusesEveryMissingSharedRolePrompt(t *testing.T) {
	rolesDir := t.TempDir()
	_, err := New(rolesDir, t.TempDir())
	if err == nil {
		t.Fatal("New succeeded with no shared role prompt files")
	}
	if !strings.Contains(err.Error(), rolesDir) {
		t.Errorf("New error = %q, want directory %q", err, rolesDir)
	}
	for _, name := range []string{
		"architect-root.md", "controller-root.md", "architect.md", "planner.md", "implementer.md",
		"tester.md", "reviewer.md", "merger.md", "core/common.md", "core/planner.md",
		"core/implementer.md", "core/tester.md", "core/reviewer.md", "core/oracle.md",
		"mechanics/headless.md", "mechanics/interactive.md",
	} {
		if !strings.Contains(err.Error(), name) {
			t.Errorf("New error = %q, want missing file %q", err, name)
		}
	}
}

// A state directory outlives the daemon binary that wrote its Go parts, so a boot of a newer daemon
// finds the older daemon's words there. A part whose content is not the running binary's is
// rewritten: a prompt that contradicts the daemon's own behaviour (who posts READY) is the defect,
// and nothing treats these files as the operator's to edit (the operator's text is `instructions`).
// A part already holding the running binary's content is left as it is, so an ordinary restart
// writes nothing.
func TestNewRewritesAGoPartTheRunningDaemonDidNotWrite(t *testing.T) {
	rolesDir := completeRolesDir(t)
	stateDir := t.TempDir()
	if _, err := New(rolesDir, stateDir); err != nil {
		t.Fatalf("first New: %v", err)
	}
	stale := filepath.Join(stateDir, "prompts", "go", "merger.md")
	current := filepath.Join(stateDir, "prompts", "go", "tester.md")
	if err := os.WriteFile(stale, []byte("an older daemon's merger prompt\n"), 0o600); err != nil {
		t.Fatalf("write the older part: %v", err)
	}
	written := time.Date(2026, 1, 2, 3, 4, 5, 0, time.UTC)
	if err := os.Chtimes(current, written, written); err != nil {
		t.Fatalf("date the current part: %v", err)
	}
	if _, err := New(rolesDir, stateDir); err != nil {
		t.Fatalf("second New: %v", err)
	}
	for _, name := range []string{"merger.md", "tester.md"} {
		want, err := goParts.ReadFile("go/" + name)
		if err != nil {
			t.Fatalf("read embedded %s: %v", name, err)
		}
		got, err := os.ReadFile(filepath.Join(stateDir, "prompts", "go", name))
		if err != nil {
			t.Fatalf("read %s: %v", name, err)
		}
		if string(got) != string(want) {
			t.Errorf("%s after the second New = %q, want the running daemon's part", name, got)
		}
	}
	if info, err := os.Stat(current); err != nil || !info.ModTime().Equal(written) {
		t.Errorf("the part already holding the running daemon's content was written again: %v %v", info.ModTime(), err)
	}
}

func completeRolesDir(t *testing.T) string {
	t.Helper()
	dir := t.TempDir()
	for _, name := range []string{
		"architect-root.md", "controller-root.md", "architect.md", "planner.md", "implementer.md",
		"tester.md", "reviewer.md", "merger.md", "core/common.md", "core/planner.md",
		"core/implementer.md", "core/tester.md", "core/reviewer.md", "core/oracle.md",
		"mechanics/headless.md", "mechanics/interactive.md",
	} {
		path := filepath.Join(dir, name)
		if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
			t.Fatalf("make prompt directory: %v", err)
		}
		if err := os.WriteFile(path, []byte("# "+name+"\n"), 0o600); err != nil {
			t.Fatalf("write prompt: %v", err)
		}
	}
	return dir
}
