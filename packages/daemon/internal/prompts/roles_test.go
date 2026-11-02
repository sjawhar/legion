package prompts

import (
	"embed"
	"io/fs"
	"os"
	"path/filepath"
	"regexp"
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
	reusableParts      = append(slices.Clone(modeNeutral), "mechanics/headless.md")
	headlessOnly       = []string{
		"LEGION_", "GH_CONFIG_DIR", "handoff_", "envoy_publish",
		"roleToken", "spawn_worker", "legion-worker",
		// Naming a task agent is headless mechanics: the daemon's load probe checks that a pane's Oh
		// My Pi finds every agent a role prompt names and runs it on its own model, and nothing
		// checks an interactive session. The needle is the boot gate's own form (promptrefs), so a
		// dispatch that carries other arguments is caught too.
		`agent="`,
	}
	repoSpecific = []string{"Inspect", "inspect_ai", "inspect_", "Hawk", "middleman", "Taiga"}
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
	for _, file := range reusableParts {
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
	if _, err := fs.Stat(roleParts, "roles/core/merger.md"); err == nil {
		t.Error("roles/core/merger.md exists; the merger composes no core")
	}
}

func TestSharedRulesSitInCommonOnceAndRoleRulesInTheirCores(t *testing.T) {
	const (
		readSource       = "read the code that already does the nearest thing"
		noDefer          = "Nothing needed for correctness is deferred"
		fastChecks       = "run the repository's fast local checks"
		testerRunsTheApp = "read CI for everything static"
		redTest          = "the test itself is not theirs to change"
		dontModify       = "make the tester's red test pass; do not modify it"
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
	// The implementer runs the repository's fast local checks before a push; the tester runs the
	// application and reads CI for the static lanes (dispatch://LEGION-631), so the rule to rerun
	// them is the implementer's alone.
	contains("core/implementer.md", fastChecks, true)
	contains("core/tester.md", fastChecks, false)
	contains("core/tester.md", testerRunsTheApp, true)
	contains("core/tester.md", redTest, true)
	contains("core/implementer.md", dontModify, true)
	for _, role := range []string{"planner", "reviewer", "oracle"} {
		contains("core/"+role+".md", dontModify, false)
	}
	for _, role := range []string{"planner", "implementer", "reviewer", "oracle"} {
		contains("core/"+role+".md", redTest, false)
		contains("core/"+role+".md", testerRunsTheApp, false)
	}
}

// A finding outside the tree takes one of two paths (skill://legion-worker/references/findings.md):
// a clear defect goes to the controller, and a finding that needs a decision is an ask. The headless
// fragment composed into the same planner prompt says the same, so the core may not say "always an ask".
func TestPlannerKeepsFindingsAndHumanToDoAsksOutsideDesignQuestions(t *testing.T) {
	planner := rolePart(t, "core/planner.md")
	for _, rule := range []string{
		"Read the repository's `STRATEGY.md` when it has one",
		"A finding outside the tree is never an issue you file.",
		"A clear defect in code this tree touches or depends on is fixed in this tree's own pull request.",
		"Any other clear defect goes to whoever coordinates the project's work, with its evidence, and a finding that needs a decision is an ask.",
		"A to-do only a person can do is always an ask.",
	} {
		if !strings.Contains(planner, rule) {
			t.Errorf("core/planner.md does not contain %q", rule)
		}
	}
	if strings.Contains(planner, "A finding outside the tree, and a to-do only a person can do, is always an ask.") {
		t.Error("core/planner.md still makes every finding outside the tree an ask, which contradicts the headless fragment's clear-defect path")
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
	parts := slices.Clone(reusableParts)
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
		if !slices.Equal(agents[agent], []string{"roles/planner.md"}) {
			t.Errorf("task agent %s is dispatched by %q, want roles/planner.md alone", agent, agents[agent])
		}
	}
	for agent, files := range agents {
		if _, err := os.Stat(filepath.Join("..", "..", "..", "pi-legion", "agents", agent+".md")); err != nil {
			t.Errorf("task agent %s, dispatched by %q, is not shipped in the plugin's agents/: %v", agent, files, err)
		}
	}
}

// The daemon submits the merge itself once it accepts READY, and Legion enforces no GitHub
// restriction the repository itself does not: a part that says Legion never merges, shared or the
// Go daemon's, contradicts what the merger is told the daemon does. The phrases, in any case,
// are a pattern rather than spelled out, so a grep for them over this directory finds only prompt
// text.
func TestNoPartSaysLegionNeverMerges(t *testing.T) {
	saysLegionNeverMerges := regexp.MustCompile(`(?i)never merg(es)|merg(e|es) nothing`)
	for dir, parts := range map[string]embed.FS{"roles": roleParts, "go": goParts} {
		err := eachPart(parts, dir, func(name string, body []byte) error {
			if phrase := saysLegionNeverMerges.Find(body); phrase != nil {
				t.Errorf("%s/%s says %q", dir, name, phrase)
			}
			return nil
		})
		if err != nil {
			t.Fatalf("read the %s parts: %v", dir, err)
		}
	}
}

// Background work is on for every role: Legion writes no Oh My Pi settings overlay, so
// a long bash call becomes a background job and a task subagent runs as one, and a role reads a
// result with the `wait` tool. Nothing takes a turn away from a role either: a CI red
// that ends a phase starts the implementer at once and tells the role by one message, which stops
// nothing. So no part may say a takeover cancels a turn or tell a role to keep its work in
// the foreground, and a part that dispatches a task subagent names the tool that reads its result.
//
// Both halves are read over the markdown parts alone, and both are exact. The call shape appears
// in prompts_test.go and in this file, which are not prompt text, and the English word "wait" is
// in many parts — go/implementer.md forbids waiting for CI — so the needle is the backticked
// token. Each retired sentence is joined from its two halves, as TestNoPartSaysLegionNeverMerges
// keeps its phrases a pattern, so that a grep for one over this directory finds prompt text and
// never this test. The parts that dispatch are listed here rather than counted, so a part that
// stopped dispatching fails as loudly as one that started: a reader that found nothing would
// otherwise pass while checking nothing. The skills half of this rule, over skills/ and the
// plugin the Claude Code roles load, is packages/pi-legion/src/skills-guard.test.ts.
func TestNoPartKeepsItsWorkInTheForegroundAndEveryDispatchNamesWait(t *testing.T) {
	retired := []string{
		"interrupts" + " your turn",
		"tool calls" + " are cancelled",
		"interrupted" + " its turn",
		"run none" + " in the background",
		"one foreground" + " command",
		"blocking" + ": true",
	}
	var dispatching []string
	for _, dir := range []string{"go", "roles"} {
		parts := goParts
		if dir == "roles" {
			parts = roleParts
		}
		err := eachPart(parts, dir, func(name string, body []byte) error {
			if !strings.HasSuffix(name, ".md") {
				return nil
			}
			part, text := dir+"/"+name, string(body)
			for _, phrase := range retired {
				if strings.Contains(text, phrase) {
					t.Errorf("%s says %q, which does not hold: nothing interrupts a turn and background work is on", part, phrase)
				}
			}
			if strings.Contains(text, `task(agent="`) {
				dispatching = append(dispatching, part)
				if !strings.Contains(text, "`wait`") {
					t.Errorf("%s dispatches a task subagent and never names `wait`, the tool that reads a job's result", part)
				}
			}
			return nil
		})
		if err != nil {
			t.Fatalf("read the %s parts: %v", dir, err)
		}
	}
	want := []string{"go/claude-mechanics.md", "roles/implementer.md", "roles/planner.md"}
	if !slices.Equal(dispatching, want) {
		t.Errorf("the parts that dispatch a task subagent are %q, want %q", dispatching, want)
	}
}
