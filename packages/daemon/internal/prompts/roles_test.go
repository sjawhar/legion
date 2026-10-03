package prompts

import (
	"io/fs"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"

	"github.com/sjawhar/legion/daemon/internal/claim"
	"github.com/sjawhar/legion/daemon/internal/promptrefs"
)

// The role prompt parts Compose joins. A core and the interactive fragment are also composed for an
// interactive session, whose subagent runs no Legion mechanics, so they stay mode-neutral; the
// headless fragment carries every phase worker's shared mechanics once.
var (
	phaseRoles         = []string{"planner", "implementer", "tester", "reviewer", "merger"}
	cores              = []string{"planner", "implementer", "tester", "reviewer", "oracle"}
	composedWithCommon = []string{"planner", "implementer", "tester", "reviewer"}
	modeNeutral        = append(append([]string{"core/common.md"}, coreFiles()...), "mechanics/interactive.md")
	headlessOnly       = []string{
		"LEGION_", "legion gh", "legion handoff", "handoff_", "legion threads", "envoy_publish", ".legion/",
		"roleToken", "spawn_worker", "legion-worker",
		// A task subagent the interactive fragment starts dispatches none of its own. The needle is
		// the boot gate's own form (promptrefs), so a dispatch that carries other arguments is caught
		// too.
		`agent="`,
	}
	repoSpecific = []string{"Inspect", "inspect_ai", "inspect_", "Hawk", "middleman", "Taiga", "agent-c", "trajectory"}
)

func coreFiles() []string {
	files := make([]string, 0, len(cores))
	for _, role := range cores {
		files = append(files, "core/"+role+".md")
	}
	return files
}

// rolePart is one embedded role prompt part, by its path under roles/.
func rolePart(t *testing.T, name string) string {
	t.Helper()
	body, err := roleParts.ReadFile("roles/" + name)
	if err != nil {
		t.Fatalf("read role prompt part %s: %v", name, err)
	}
	return string(body)
}

func TestCoresAndTheInteractiveFragmentCarryNoHeadlessMechanics(t *testing.T) {
	for _, file := range modeNeutral {
		text := rolePart(t, file)
		for _, needle := range headlessOnly {
			if strings.Contains(text, needle) {
				t.Errorf("%s contains %q", file, needle)
			}
		}
	}
}

func TestNoReusablePartNamesARepositoryOrLibrary(t *testing.T) {
	for _, file := range append(slices.Clone(modeNeutral), "mechanics/headless.md") {
		text := rolePart(t, file)
		for _, needle := range repoSpecific {
			if strings.Contains(text, needle) {
				t.Errorf("%s contains %q", file, needle)
			}
		}
	}
}

// Every role a claim can be on has its own part, and none is blank: a blank part boots a worker with
// no instructions at all. The merger composes no core.
func TestEveryRoleHasItsPartAndTheMergerHasNoCore(t *testing.T) {
	for _, role := range claim.Roles {
		if strings.TrimSpace(rolePart(t, string(role)+".md")) == "" {
			t.Errorf("roles/%s.md is blank", role)
		}
	}
	for _, file := range append(slices.Clone(modeNeutral), "mechanics/headless.md") {
		rolePart(t, file)
	}
	if _, err := fs.Stat(roleParts, "roles/core/merger.md"); err == nil {
		t.Error("roles/core/merger.md exists; the merger composes no core")
	}
}

func TestSharedRulesSitInCommonOnceAndRoleRulesInTheirCores(t *testing.T) {
	const (
		readSource = "read the code that already does the nearest thing"
		noDefer    = "Nothing needed for correctness is deferred"
		fastChecks = "run the repository's fast local checks"
		redTest    = "the test itself is not theirs to change"
		dontModify = "make the tester's red test pass; do not modify it"
	)
	contains := func(file, needle string, want bool) {
		t.Helper()
		if strings.Contains(rolePart(t, file), needle) != want {
			t.Errorf("%s contains %q = %t, want %t", file, needle, !want, want)
		}
	}
	contains("core/common.md", readSource, true)
	contains("core/common.md", noDefer, true)
	// The four roles that compose common.md do not repeat it; the oracle composes alone and keeps
	// the read-first rule itself, never the hardening ledger (it changes nothing).
	for _, role := range composedWithCommon {
		contains("core/"+role+".md", readSource, false)
		contains("core/"+role+".md", noDefer, false)
	}
	contains("core/oracle.md", readSource, true)
	contains("core/oracle.md", noDefer, false)
	for _, role := range []string{"implementer", "tester"} {
		contains("core/"+role+".md", fastChecks, true)
	}
	contains("core/tester.md", redTest, true)
	contains("core/implementer.md", dontModify, true)
	for _, role := range []string{"planner", "reviewer", "oracle"} {
		contains("core/"+role+".md", dontModify, false)
	}
	for _, role := range []string{"planner", "implementer", "reviewer", "oracle"} {
		contains("core/"+role+".md", redTest, false)
	}
}

func TestTheHeadlessFragmentHoldsTheWorkersSharedRules(t *testing.T) {
	headless := rolePart(t, "mechanics/headless.md")
	interactive := rolePart(t, "mechanics/interactive.md")
	// The interactive fragment states its own one-line version of the skills-first preamble.
	const preamble = "find this repository's skills"
	if !strings.Contains(headless, preamble) || strings.Contains(interactive, preamble) {
		t.Errorf("the skills-first preamble %q is in the headless fragment %t and the interactive one %t, want only the headless",
			preamble, strings.Contains(headless, preamble), strings.Contains(interactive, preamble))
	}
	for _, role := range cores {
		if strings.Contains(rolePart(t, "core/"+role+".md"), preamble) {
			t.Errorf("core/%s.md repeats the skills-first preamble", role)
		}
	}
	shared := []string{
		"## Step one: find this repository's skills",
		"create another workspace",
		"Read and follow `skill://legion-worker` before acting.",
		`op: "handoff_complete"`,
		"When your phase is done, stay in this session afterwards:",
		"roleToken",
	}
	for _, rule := range shared {
		if !strings.Contains(headless, rule) {
			t.Errorf("mechanics/headless.md omits %q", rule)
		}
	}
	for _, role := range phaseRoles {
		residue := rolePart(t, role+".md")
		for _, rule := range append(slices.Clone(shared), preamble) {
			if strings.Contains(residue, rule) {
				t.Errorf("%s.md repeats the headless fragment's %q", role, rule)
			}
		}
	}
}

// Each phase worker's residue is headed by its role, and only the roles whose plan names skills for
// them are told to read those.
func TestEachResidueNamesItsRoleAndOnlyTheSkilledRolesReadRequiredSkills(t *testing.T) {
	const requiredSkills = "Then read the plan handoff's `requiredSkills` for your role and follow those too."
	skilled := []string{"implementer", "reviewer", "tester"}
	for _, role := range phaseRoles {
		residue := rolePart(t, role+".md")
		if heading := "# Legion " + strings.ToUpper(role[:1]) + role[1:]; !strings.Contains(residue, heading) {
			t.Errorf("%s.md omits its heading %q", role, heading)
		}
		if got, want := strings.Contains(residue, requiredSkills), slices.Contains(skilled, role); got != want {
			t.Errorf("%s.md tells it to read requiredSkills = %t, want %t", role, got, want)
		}
	}
}

// The runtimes join the parts byte for byte, so each starts with a heading and ends with one
// newline.
func TestEveryPartStartsWithAHeadingAndEndsWithOneNewline(t *testing.T) {
	parts := append(slices.Clone(modeNeutral), "mechanics/headless.md")
	for _, role := range phaseRoles {
		parts = append(parts, role+".md")
	}
	for _, file := range parts {
		text := rolePart(t, file)
		if !strings.HasPrefix(text, "#") {
			t.Errorf("%s does not start with a heading", file)
		}
		if !strings.HasSuffix(text, "\n") || strings.HasSuffix(text, "\n\n") {
			t.Errorf("%s does not end with exactly one newline", file)
		}
	}
}

// Every task agent a role prompt dispatches, in the form the boot gate resolves, is shipped in the
// plugin's agents/, where Oh My Pi finds it in a pane and in a pod.
func TestEveryTaskAgentARolePromptDispatchesIsShipped(t *testing.T) {
	agents := RoleReferences()[promptrefs.TaskAgents]
	// A reader that finds nothing would pass below while checking nothing.
	for _, agent := range []string{"plan-gap-analyst", "plan-reviewer"} {
		if !slices.Contains(agents[agent], "roles/planner.md") {
			t.Errorf("task agent %s is dispatched by %q, want roles/planner.md among them", agent, agents[agent])
		}
	}
	for agent, files := range agents {
		if _, err := os.Stat(filepath.Join("..", "..", "..", "pi-envoy", "agents", agent+".md")); err != nil {
			t.Errorf("task agent %s, dispatched by %q, is not shipped in the plugin's agents/: %v", agent, files, err)
		}
	}
}
