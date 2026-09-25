package daemon

import (
	"bytes"
	"context"
	"log/slog"
	"maps"
	"os"
	"os/exec"
	"path/filepath"
	"slices"
	"strings"
	"testing"

	"github.com/sjawhar/legion/daemon/internal/modelroute"
)

// testPlugin lays out a pi-legion-envoy under dir as the image unpacks one: a manifest declaring
// contract 3 and a skills directory, the legion.ts load marker, and one skill that loads itself
// and dispatches thermonuclear-deep-review and the bundled scout. agent puts
// thermonuclear-deep-review's definition, which loads its rubric skill, in the plugin's agents/
// directory; rubric ships that skill.
func testPlugin(t *testing.T, dir string, agent, rubric bool) string {
	t.Helper()
	root := filepath.Join(dir, "pi-legion-envoy")
	files := map[string]string{
		"package.json": `{"name":"@sjawhar/pi-legion-envoy","version":"0.0.0-test","legion":{"goDaemonApiVersion":3},` +
			`"omp":{"extensions":["dist/legion.js"],"skills":["dist/skills"]}}`,
		filepath.Join("dist", "legion.js"): "globalThis[Symbol.for(\"legion.pi-envoy.legion-loaded\")] = import.meta.url;\n" +
			"export default function () {}\n",
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
	for name, content := range files {
		path := filepath.Join(root, name)
		mkdir(t, filepath.Dir(path))
		if err := os.WriteFile(path, []byte(content), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	return root
}

// The load probe resolves every task agent and skill Legion's prompts name through the real Oh My
// Pi's own agent and skill discovery, on the lane the probed Oh My Pi loads the plugin by: a pod's
// one explicit root with discovery off, or a pane's installed plugins. Only the real binary shows
// that the probe's imports of that discovery work and see what the launch sees, the profile's skill
// settings included, since a session drops a skill they disable or ignore. The profile is routed
// through a gateway nothing listens on, so no credential the machine carries decides the run
// (no probe here makes a model call).
func TestThePromptReferenceProbeOnTheRealOhMyPi(t *testing.T) {
	omp := os.Getenv("LEGION_TEST_OMP")
	switch {
	case omp == "" && os.Getenv("GITHUB_ACTIONS") == "true":
		t.Fatal("LEGION_TEST_OMP is unset on GitHub Actions: name the pinned Oh My Pi binary (the daemon-go job installs it)")
	case omp == "":
		t.Skip("LEGION_TEST_OMP names no Oh My Pi binary")
	}
	noAgent := "finds no task agent thermonuclear-deep-review (dispatched by dist/skills/legion-worker/SKILL.md)"
	noRubric := "finds no skill thermonuclear-deep-review (loaded by agents/thermonuclear-deep-review.md)"
	for _, testCase := range []struct {
		name                 string
		agent, rubric, onPod bool
		// settings is appended to the profile's config.yml, the settings the launch reads.
		settings string
		refusal  string
	}{
		{name: "a pod, the plugin shipping the agent and its rubric", agent: true, rubric: true, onPod: true},
		{name: "a pod, the plugin without the agent", rubric: true, onPod: true, refusal: noAgent},
		{name: "a pod, the plugin without the rubric", agent: true, onPod: true, refusal: noRubric},
		{name: "a pane, the installed plugin shipping the agent and its rubric", agent: true, rubric: true},
		{name: "a pane, the installed plugin without the agent", rubric: true, refusal: noAgent},
		{name: "a pane, the installed plugin without the rubric", agent: true, refusal: noRubric},
		{name: "a pod, a profile that disables the rubric", agent: true, rubric: true, onPod: true,
			settings: "disabledExtensions:\n  - skill:thermonuclear-deep-review\n", refusal: noRubric},
		{name: "a pane, a profile that ignores the rubric", agent: true, rubric: true,
			settings: "skills:\n  ignoredSkills:\n    - thermonuclear-deep-review\n", refusal: noRubric},
	} {
		t.Run(testCase.name, func(t *testing.T) {
			dir := t.TempDir()
			home := filepath.Join(dir, "home")
			mkdir(t, home)
			root := testPlugin(t, dir, testCase.agent, testCase.rubric)
			token := filepath.Join(dir, "token")
			if err := os.WriteFile(token, []byte("unused\n"), 0o600); err != nil {
				t.Fatal(err)
			}
			installed, err := modelroute.InstallKeyedBy([]string{"HOME=" + home, "OMP_PROFILE=legion",
				modelroute.EnvURL + "=http://127.0.0.1:9", "PATH=/usr/local/bin:/usr/bin:/bin"}, token)
			if err != nil {
				t.Fatal(err)
			}
			if testCase.settings != "" {
				config, err := os.OpenFile(filepath.Join(home, ".omp", "profiles", "legion", "agent", "config.yml"), os.O_APPEND|os.O_WRONLY, 0)
				if err != nil {
					t.Fatal(err)
				}
				if _, err := config.WriteString(testCase.settings); err != nil {
					t.Fatal(err)
				}
				if err := config.Close(); err != nil {
					t.Fatal(err)
				}
			}
			env := map[string]string{}
			for _, pair := range installed.Environ {
				name, value, _ := strings.Cut(pair, "=")
				env[name] = value
			}
			probe := ImageProbe{Omp: omp, Contract: 3, Env: env, WorkDir: dir, Log: slog.New(slog.NewTextHandler(&bytes.Buffer{}, nil))}
			if testCase.onPod {
				probe.PluginRoot = root
			} else {
				install := exec.Command(omp, "plugin", "install", root)
				install.Env = installed.Environ
				if out, err := install.CombinedOutput(); err != nil {
					t.Fatalf("omp plugin install: %v\n%s", err, out)
				}
			}

			err = ProbeImage(context.Background(), probe)

			switch {
			case testCase.refusal == "" && err != nil:
				t.Fatalf("ProbeImage = %v, want every probe to pass", err)
			case testCase.refusal != "" && (err == nil || !strings.Contains(err.Error(), testCase.refusal)):
				t.Fatalf("ProbeImage = %v, want the refusal to say %q", err, testCase.refusal)
			case testCase.refusal != "" && strings.Contains(err.Error(), "scout"):
				t.Errorf("ProbeImage = %v, which names scout, a bundled agent Oh My Pi finds", err)
			case testCase.refusal != "" && strings.Contains(err.Error(), "legion-worker (loaded by"):
				t.Errorf("ProbeImage = %v, which names legion-worker, a skill the plugin ships", err)
			}
		})
	}
}

// The names the gate resolves are every task agent and skill a Legion prompt names: in the
// plugin's shipped skills and agent definitions and in the role prompts the daemon hands each pane,
// each named with the files that name it.
func TestPromptReferencesReadTheSkillsTheAgentsAndTheRolePrompts(t *testing.T) {
	dir := t.TempDir()
	root := testPlugin(t, dir, true, false)
	roles := filepath.Join(dir, "roles")
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

	names, err := promptReferences(filepath.Join(root, "package.json"), roles)

	if err != nil {
		t.Fatal(err)
	}
	worker := filepath.Join("dist", "skills", "legion-worker", "SKILL.md")
	want := promptNames{
		{
			"oracle":                    {filepath.Join("roles", "core", "planner.md")},
			"scout":                     {worker},
			"thermonuclear-deep-review": {worker, filepath.Join("roles", "reviewer.md")},
		},
		{
			"ce-simplify-code":          {filepath.Join("roles", "reviewer.md")},
			"legion-worker":             {worker, filepath.Join("roles", "tester.md")},
			"thermonuclear-deep-review": {filepath.Join("agents", "thermonuclear-deep-review.md")},
		},
	}
	for i, kind := range promptKinds {
		if !maps.EqualFunc(names[i], want[i], slices.Equal) {
			t.Errorf("promptReferences' %ss = %v, want %v", kind.noun, names[i], want[i])
		}
	}
}
