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
// contract 3 and a skills directory, the legion.ts load marker, and one skill dispatching
// thermonuclear-deep-review and the bundled scout. ships puts thermonuclear-deep-review's
// definition in the plugin's agents/ directory.
func testPlugin(t *testing.T, dir string, ships bool) string {
	t.Helper()
	root := filepath.Join(dir, "pi-legion-envoy")
	files := map[string]string{
		"package.json": `{"name":"@sjawhar/pi-legion-envoy","version":"0.0.0-test","legion":{"goDaemonApiVersion":3},` +
			`"omp":{"extensions":["dist/legion.js"],"skills":["dist/skills"]}}`,
		filepath.Join("dist", "legion.js"): "globalThis[Symbol.for(\"legion.pi-envoy.legion-loaded\")] = import.meta.url;\n" +
			"export default function () {}\n",
		filepath.Join("dist", "skills", "legion-worker", "SKILL.md"): "---\nname: legion-worker\ndescription: test\n---\n" +
			"Run `task(agent=\"thermonuclear-deep-review\")`, then `task(agent=\"scout\")`.\n",
	}
	if ships {
		files[filepath.Join("agents", "thermonuclear-deep-review.md")] = "---\nname: thermonuclear-deep-review\ndescription: test\n---\nReview.\n"
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

// The load probe resolves every task agent the plugin's skills dispatch through the real Oh My
// Pi's own agent discovery, on the lane the probed Oh My Pi loads the plugin by: a pod's one
// explicit root with discovery off, or a pane's installed plugins. Only the real binary shows that
// the probe's import of that discovery works and sees what the launch sees. The profile is routed
// through a gateway nothing listens on, so no credential the machine carries decides the run
// (no probe here makes a model call).
func TestThePromptAgentProbeOnTheRealOhMyPi(t *testing.T) {
	omp := os.Getenv("LEGION_TEST_OMP")
	switch {
	case omp == "" && os.Getenv("GITHUB_ACTIONS") == "true":
		t.Fatal("LEGION_TEST_OMP is unset on GitHub Actions: name the pinned Oh My Pi binary (the daemon-go job installs it)")
	case omp == "":
		t.Skip("LEGION_TEST_OMP names no Oh My Pi binary")
	}
	for _, testCase := range []struct {
		name         string
		ships, onPod bool
		refusal      string
	}{
		{name: "a pod, the plugin shipping the agent", ships: true, onPod: true},
		{name: "a pod, the plugin without it", onPod: true,
			refusal: "finds no task agent thermonuclear-deep-review (dispatched by dist/skills/legion-worker/SKILL.md)"},
		{name: "a pane, the installed plugin shipping the agent", ships: true},
		{name: "a pane, the installed plugin without it",
			refusal: "finds no task agent thermonuclear-deep-review (dispatched by dist/skills/legion-worker/SKILL.md)"},
	} {
		t.Run(testCase.name, func(t *testing.T) {
			dir := t.TempDir()
			home := filepath.Join(dir, "home")
			mkdir(t, home)
			root := testPlugin(t, dir, testCase.ships)
			token := filepath.Join(dir, "token")
			if err := os.WriteFile(token, []byte("unused\n"), 0o600); err != nil {
				t.Fatal(err)
			}
			installed, err := modelroute.InstallKeyedBy([]string{"HOME=" + home, "OMP_PROFILE=legion",
				modelroute.EnvURL + "=http://127.0.0.1:9", "PATH=/usr/local/bin:/usr/bin:/bin"}, token)
			if err != nil {
				t.Fatal(err)
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
			}
		})
	}
}

// The agents the gate resolves are every task agent a Legion prompt dispatches: the plugin's
// shipped skills and the role prompts the daemon hands each pane, each agent named with the files
// that dispatch it.
func TestPromptAgentsReadsTheSkillsAndTheRolePrompts(t *testing.T) {
	dir := t.TempDir()
	root := testPlugin(t, dir, false)
	roles := filepath.Join(dir, "roles")
	for name, content := range map[string]string{
		filepath.Join("core", "planner.md"): "Consult `task(agent=\"oracle\")` on a hard tradeoff.\n",
		"reviewer.md":                       "Run `task(agent=\"thermonuclear-deep-review\")` at the head.\n",
		"notes.txt":                         "task(agent=\"not-a-prompt\")\n",
	} {
		path := filepath.Join(roles, name)
		mkdir(t, filepath.Dir(path))
		if err := os.WriteFile(path, []byte(content), 0o644); err != nil {
			t.Fatal(err)
		}
	}

	named, err := promptAgents(filepath.Join(root, "package.json"), roles)

	if err != nil {
		t.Fatal(err)
	}
	want := map[string][]string{
		"oracle":                    {filepath.Join("roles", "core", "planner.md")},
		"scout":                     {filepath.Join("dist", "skills", "legion-worker", "SKILL.md")},
		"thermonuclear-deep-review": {filepath.Join("dist", "skills", "legion-worker", "SKILL.md"), filepath.Join("roles", "reviewer.md")},
	}
	if !maps.EqualFunc(named, want, slices.Equal) {
		t.Errorf("promptAgents = %v, want %v", named, want)
	}
}
