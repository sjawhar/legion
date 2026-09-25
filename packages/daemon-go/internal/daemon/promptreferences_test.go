package daemon

import (
	"maps"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"

	"github.com/sjawhar/legion/daemon/internal/promptrefs"
)

// testPlugin lays out a pi-legion-envoy under dir as the image unpacks one: a manifest declaring
// contract 3 and a skills directory, the legion.ts load marker, and files, each named relative to
// the plugin's root.
func testPlugin(t *testing.T, dir string, files map[string]string) string {
	t.Helper()
	root := filepath.Join(dir, "pi-legion-envoy")
	all := map[string]string{
		"package.json": `{"name":"@sjawhar/pi-legion-envoy","version":"0.0.0-test","legion":{"goDaemonApiVersion":3},` +
			`"omp":{"extensions":["dist/legion.js"],"skills":["dist/skills"]}}`,
		filepath.Join("dist", "legion.js"): "globalThis[Symbol.for(\"legion.pi-envoy.legion-loaded\")] = import.meta.url;\n" +
			"export default function () {}\n",
	}
	maps.Copy(all, files)
	for name, content := range all {
		path := filepath.Join(root, name)
		mkdir(t, filepath.Dir(path))
		if err := os.WriteFile(path, []byte(content), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	return root
}

// referencePlugin is a testPlugin whose one skill loads itself and dispatches
// thermonuclear-deep-review and the bundled scout. agent puts thermonuclear-deep-review's
// definition, which loads its rubric skill, in the plugin's agents/ directory; rubric ships that
// skill.
func referencePlugin(t *testing.T, dir string, agent, rubric bool) string {
	t.Helper()
	files := map[string]string{
		filepath.Join("dist", "skills", "legion-worker", "SKILL.md"): "---\nname: legion-worker\ndescription: test\n---\n" +
			"Read `skill://legion-worker/references/x.md`. Run `task(agent=\"thermonuclear-deep-review\")`, then `task(agent=\"scout\")`.\n",
	}
	if agent {
		files[filepath.Join("agents", "thermonuclear-deep-review.md")] = "---\nname: thermonuclear-deep-review\ndescription: test\n---\n" +
			"Load `skill://thermonuclear-deep-review` and use its rubric.\n"
	}
	if rubric {
		files[filepath.Join("dist", "skills", "thermonuclear-deep-review", "SKILL.md")] = "---\nname: thermonuclear-deep-review\ndescription: test\n---\nThe rubric.\n"
	}
	return testPlugin(t, dir, files)
}

// The names the gate resolves are every task agent and skill a Legion prompt names: in the
// plugin's shipped skills and agent definitions and in the role prompts the daemon hands each pane,
// each named with the files that name it.
func TestPromptReferencesReadTheSkillsTheAgentsAndTheRolePrompts(t *testing.T) {
	dir := t.TempDir()
	root := referencePlugin(t, dir, true, false)
	roles := filepath.Join(dir, "roles")
	// LEGION_ROLE_PROMPTS_DIR may name a link to the directory, which the walk must follow.
	link := filepath.Join(dir, "roles-link")
	if err := os.Symlink(roles, link); err != nil {
		t.Fatal(err)
	}
	for name, content := range map[string]string{
		filepath.Join("core", "planner.md"): "Consult `task(agent=\"oracle\")` on a hard tradeoff.\n",
		"reviewer.md":                       "Run `task(agent=\"thermonuclear-deep-review\")` after `skill://ce-simplify-code`.\n",
		"tester.md":                         "Follow skill://legion-worker.\n",
		"notes.txt":                         "task(agent=\"not-a-prompt\") skill://not-a-prompt\n",
	} {
		path := filepath.Join(roles, name)
		mkdir(t, filepath.Dir(path))
		if err := os.WriteFile(path, []byte(content), 0o644); err != nil {
			t.Fatal(err)
		}
	}

	worker := filepath.Join("dist", "skills", "legion-worker", "SKILL.md")
	want := promptrefs.Names{
		promptrefs.TaskAgents: {
			"oracle":                    {filepath.Join("roles", "core", "planner.md")},
			"scout":                     {worker},
			"thermonuclear-deep-review": {worker, filepath.Join("roles", "reviewer.md")},
		},
		promptrefs.Skills: {
			"ce-simplify-code":          {filepath.Join("roles", "reviewer.md")},
			"legion-worker":             {worker, filepath.Join("roles", "tester.md")},
			"thermonuclear-deep-review": {filepath.Join("agents", "thermonuclear-deep-review.md")},
		},
	}
	manifest := filepath.Join(root, "package.json")
	plugin, err := readPluginManifest(manifest, "the test", 3)
	if err != nil {
		t.Fatal(err)
	}
	for _, rolesDir := range []string{roles, link} {
		names, err := promptReferences(manifest, plugin.skills)
		if err != nil {
			t.Fatal(err)
		}
		references, err := promptrefs.Roles(rolesDir)
		if err != nil {
			t.Fatal(err)
		}
		names.Merge(references)
		for _, words := range promptKinds {
			if !maps.EqualFunc(names[words.kind], want[words.kind], slices.Equal) {
				t.Errorf("the gate's names over %s: %ss = %v, want %v", rolesDir, words.noun, names[words.kind], want[words.kind])
			}
		}
	}
}

// The manifest's `omp.skills` names the directories whose skills dispatch agents, so a manifest the
// gate cannot read them from is refused rather than checked as shipping none; absent or null, it
// ships none.
func TestReadPluginManifestRefusesSkillsItCannotRead(t *testing.T) {
	for _, testCase := range []struct {
		name, omp, want string
		skills          []string
	}{
		{name: "skills listed", omp: `,"omp":{"skills":["dist/skills"]}`, skills: []string{"dist/skills"}},
		{name: "no omp", omp: ""},
		{name: "a null omp", omp: `,"omp":null`},
		{name: "null skills", omp: `,"omp":{"skills":null}`},
		{name: "an omp that is not an object", omp: `,"omp":"dist/skills"`, want: "has an `omp` that is not an object"},
		{name: "skills that are not a list", omp: `,"omp":{"skills":"dist/skills"}`, want: "has an `omp.skills` that is not a list of directories"},
		{name: "an entry that is not a directory name", omp: `,"omp":{"skills":[3]}`, want: "lists a skills entry 3 that is not a directory name"},
	} {
		t.Run(testCase.name, func(t *testing.T) {
			manifest := filepath.Join(t.TempDir(), "package.json")
			if err := os.WriteFile(manifest, []byte(`{"version":"1.0.0","legion":{"goDaemonApiVersion":3}`+testCase.omp+`}`), 0o644); err != nil {
				t.Fatal(err)
			}

			plugin, err := readPluginManifest(manifest, "the test", 3)

			if testCase.want != "" {
				if err == nil || !strings.Contains(err.Error(), testCase.want) {
					t.Fatalf("readPluginManifest = %v, want a refusal saying %q", err, testCase.want)
				}
				return
			}
			if err != nil || !slices.Equal(plugin.skills, testCase.skills) {
				t.Fatalf("readPluginManifest = %v skills %v, want skills %v", err, plugin.skills, testCase.skills)
			}
		})
	}
}

// The gate reads the load probe's answer on the agents' models line by line: resolved passes; each
// unresolved agent is one JSON object, refused naming the agent, the files that dispatch it, its
// model and why; a line it cannot read is refused as such; and no answer at all is refused naming
// the likely cause.
func TestAgentModelRefusalReadsTheProbesAnswer(t *testing.T) {
	agents := map[string][]string{"oracle": {"dist/skills/legion-oracle/SKILL.md"}}
	for _, testCase := range []struct {
		name, output string
		want         []string
	}{
		{name: "resolved", output: "LEGION_PROMPT_AGENTS=resolved\nLEGION_AGENT_MODELS=resolved\n"},
		{name: "unresolved", output: `LEGION_AGENT_MODEL_UNRESOLVED={"agent":"oracle","model":"@oracle","why":"role oracle is not configured"}` + "\n",
			want: []string{"in a pane of the default profile, cannot run task agent oracle (dispatched by dist/skills/legion-oracle/SKILL.md) on its model @oracle: role oracle is not configured"}},
		{name: "unreadable", output: "LEGION_AGENT_MODEL_UNRESOLVED=oracle @oracle role oracle is not configured\n",
			want: []string{"which the gate cannot read"}},
		{name: "no answer", output: "LEGION_PROMPT_AGENTS=resolved\n",
			want: []string{"gave no answer on the models of the task agents Legion's prompts name (oracle)", "session_shutdown handler", "2 s"}},
	} {
		t.Run(testCase.name, func(t *testing.T) {
			err := agentModelRefusal(testCase.output, agents, "in a pane of the default profile")

			if len(testCase.want) == 0 {
				if err != nil {
					t.Fatalf("agentModelRefusal = %v, want a pass", err)
				}
				return
			}
			for _, want := range testCase.want {
				if err == nil || !strings.Contains(err.Error(), want) {
					t.Errorf("agentModelRefusal = %v, want it to say %q", err, want)
				}
			}
		})
	}
}

// A probe that could not discover the task agents registers no model check, so no answer on their
// models can come: the refusal is the discovery's alone, and names no slow key as a cause.
func TestAnAgentDiscoveryThatFailedIsTheWholeRefusal(t *testing.T) {
	check := promptCheck{names: promptrefs.New()}
	check.names[promptrefs.TaskAgents]["oracle"] = []string{"dist/skills/legion-oracle/SKILL.md"}

	err := check.refusal("LEGION_PLUGIN_LOADED=yes\nLEGION_PROMPT_AGENTS_UNRESOLVABLE=Cannot find module task/discovery\n", "in a pane of the default profile")

	if err == nil || !strings.Contains(err.Error(), "could not resolve task agents for the load probe (Cannot find module task/discovery)") {
		t.Fatalf("refusal = %v, want the discovery's refusal", err)
	}
	if strings.Contains(err.Error(), "gave no answer") {
		t.Errorf("refusal = %v, want no word on the models, which the probe never checked", err)
	}
}
