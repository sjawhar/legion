package daemon

import (
	"bytes"
	"context"
	"log/slog"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
)

// The load probe resolves every task agent and skill Legion's prompts name through the real Oh My
// Pi's own agent and skill discovery, on the lane the probed Oh My Pi loads the plugin by: a pod's
// one explicit root with discovery off, or a pane's installed plugins. Only the real binary shows
// that the probe's imports of that discovery work and see what the launch sees, the profile's skill
// settings included, since a session drops a skill they disable or ignore. The profile's one model
// is an operator's provider nothing listens on, so no credential the machine carries decides the
// run (no probe here makes a model call).
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
			agentDir := filepath.Join(home, ".omp", "profiles", "legion", "agent")
			mkdir(t, agentDir)
			for name, content := range map[string]string{
				"models.yml": "providers:\n  operator:\n    baseUrl: http://127.0.0.1:9\n    auth: apiKey\n    api: anthropic-messages\n" +
					"    apiKey: unused\n    models:\n      - id: probe-test\n        name: Probe test\n",
				"config.yml": "modelRoles:\n  default: operator/probe-test\n",
			} {
				if err := os.WriteFile(filepath.Join(agentDir, name), []byte(content), 0o600); err != nil {
					t.Fatal(err)
				}
			}
			environ := []string{"HOME=" + home, "OMP_PROFILE=legion", "PATH=/usr/local/bin:/usr/bin:/bin", "AWS_EC2_METADATA_DISABLED=true"}
			if testCase.settings != "" {
				config, err := os.OpenFile(filepath.Join(agentDir, "config.yml"), os.O_APPEND|os.O_WRONLY, 0)
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
			for _, pair := range environ {
				name, value, _ := strings.Cut(pair, "=")
				env[name] = value
			}
			probe := ImageProbe{Omp: omp, Contract: 3, Env: env, WorkDir: dir, Log: slog.New(slog.NewTextHandler(&bytes.Buffer{}, nil))}
			if testCase.onPod {
				probe.PluginRoot = root
			} else {
				install := exec.Command(omp, "plugin", "install", root)
				install.Env = environ
				if out, err := install.CombinedOutput(); err != nil {
					t.Fatalf("omp plugin install: %v\n%s", err, out)
				}
			}

			err := ProbeImage(context.Background(), probe)

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
