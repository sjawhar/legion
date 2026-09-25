package daemon

import (
	"bytes"
	"context"
	"github.com/sjawhar/legion/daemon/internal/bootprobe"
	"github.com/sjawhar/legion/daemon/internal/testomp"
	"log/slog"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"

	"github.com/sjawhar/legion/daemon/internal/promptrefs"
)

// agentModelPlugin lays out a pi-legion-envoy under dir whose one skill dispatches three task agents
// the plugin ships: oracle declaring its model as `@oracle`, reviewer as the list `["@review"]`, and
// plain declaring none.
func agentModelPlugin(t *testing.T, dir string) string {
	t.Helper()
	return testPlugin(t, dir, map[string]string{
		filepath.Join("dist", "skills", "legion-worker", "SKILL.md"): "---\nname: legion-worker\ndescription: test\n---\n" +
			"Run `task(agent=\"oracle\")`, then `task(agent=\"reviewer\")`, then `task(agent=\"plain\")`.\n",
		filepath.Join("agents", "oracle.md"):   "---\nname: oracle\ndescription: test\nmodel: \"@oracle\"\n---\nConsult.\n",
		filepath.Join("agents", "reviewer.md"): "---\nname: reviewer\ndescription: test\nmodel: [\"@review\"]\n---\nReview.\n",
		filepath.Join("agents", "plain.md"):    "---\nname: plain\ndescription: test\n---\nHelp.\n",
	})
}

// The load probe resolves the model of every task agent Legion's prompts dispatch as the task tool
// resolves a subagent's, and the gate refuses, naming the agent, the files that dispatch it, and
// why, each way the tool would run the agent on another model or none: its role not configured, a
// model no available provider serves, a model whose key does not work while the parent's does (the
// tool's silent fallback to the parent's model), and a model whose key does not work at all. A key
// the environment supplies, as a worker's shim exports one from the providers Secret, counts. A
// build-time probe skips the check, and a key command slower than the 2 s Oh My Pi gives the
// probe's shutdown handler still answers. The operator's override counts only when it expands to a model,
// as the task tool takes it, and an agent that declares no model runs on the session's own and is
// not judged. The profile's providers listen nowhere, so no model is called and no credential the
// machine carries decides the run.
func TestTheAgentModelCheckOnTheRealOhMyPi(t *testing.T) {
	omp := testomp.Binary(t)
	const models = "providers:\n" +
		"  fake:\n    baseUrl: http://127.0.0.1:9\n    auth: apiKey\n    api: anthropic-messages\n    apiKey: static-key\n" +
		"    models:\n      - id: m1\n        name: M1\n" +
		"  badkey:\n    baseUrl: http://127.0.0.1:9\n    auth: apiKey\n    api: anthropic-messages\n    apiKey: \"!exit 1\"\n" +
		"    models:\n      - id: m3\n        name: M3\n" +
		"  slowkey:\n    baseUrl: http://127.0.0.1:9\n    auth: apiKey\n    api: anthropic-messages\n    apiKey: \"!sleep 3; echo k\"\n" +
		"    models:\n      - id: m4\n        name: M4\n"
	const dispatched = "(dispatched by dist/skills/legion-worker/SKILL.md)"
	for _, testCase := range []struct {
		name  string
		roles string
		// config is the rest of the profile's config.yml, after modelRoles.
		config string
		env    map[string]string
		pane   bool
		skip   bool
		want   []string
	}{
		{name: "every agent's role configured", roles: "  default: fake/m1\n  review: fake/m1\n  oracle: fake/m1\n"},
		{name: "a role not configured", roles: "  default: fake/m1\n  review: fake/m1\n",
			want: []string{"task agent oracle " + dispatched, "@oracle", "role oracle is not configured"}},
		{name: "a role not configured, in a pane", roles: "  default: fake/m1\n  review: fake/m1\n", pane: true,
			want: []string{"in a pane of", "task agent oracle " + dispatched, "role oracle is not configured"}},
		{name: "a key that fails while the parent's works", roles: "  default: fake/m1\n  review: fake/m1\n  oracle: badkey/m3\n",
			want: []string{"task agent oracle " + dispatched, "badkey/m3 has no working credentials, so Oh My Pi runs it on the parent's model fake/m1"}},
		{name: "every role on a key that fails", roles: "  default: badkey/m3\n  review: badkey/m3\n  oracle: badkey/m3\n",
			want: []string{"task agent oracle " + dispatched, "task agent reviewer " + dispatched, "its model badkey/m3 has no working credentials"}},
		{name: "a provider with no key", roles: "  default: fake/m1\n  review: fake/m1\n  oracle: anthropic/claude-sonnet-4-5\n",
			want: []string{"task agent oracle " + dispatched, "no available model matches anthropic/claude-sonnet-4-5"}},
		{name: "that provider's key from the environment", roles: "  default: fake/m1\n  review: fake/m1\n  oracle: anthropic/claude-sonnet-4-5\n",
			env: map[string]string{"ANTHROPIC_API_KEY": "sk-test-not-a-key"}},
		{name: "a build-time probe", roles: "  default: fake/m1\n", skip: true},
		{name: "an empty override", roles: "  default: fake/m1\n  review: fake/m1\n", config: "task:\n  agentModelOverrides:\n    oracle: \"\"\n",
			want: []string{"task agent oracle " + dispatched, "@oracle", "role oracle is not configured"}},
		{name: "an override that expands to a model", roles: "  default: fake/m1\n  review: fake/m1\n",
			config: "task:\n  agentModelOverrides:\n    oracle: fake/m1\n"},
		{name: "an agent that declares no model, with no default role", roles: "  review: fake/m1\n  oracle: fake/m1\n"},
		// The answer comes from a session_shutdown handler Oh My Pi abandons after 2 s; at the pin the
		// process outlives it, so a key slower than that still answers. A pin that ends the process
		// with the handler would refuse this boot.
		{name: "a key command slower than the shutdown handler's 2 s", roles: "  default: fake/m1\n  review: fake/m1\n  oracle: slowkey/m4\n"},
	} {
		t.Run(testCase.name, func(t *testing.T) {
			dir := t.TempDir()
			home := filepath.Join(dir, "home")
			agent := filepath.Join(home, ".omp", "profiles", "legion", "agent")
			mkdir(t, agent)
			for name, content := range map[string]string{"models.yml": models, "config.yml": "modelRoles:\n" + testCase.roles + testCase.config} {
				if err := os.WriteFile(filepath.Join(agent, name), []byte(content), 0o600); err != nil {
					t.Fatal(err)
				}
			}
			root := agentModelPlugin(t, dir)
			env := map[string]string{"HOME": home, "OMP_PROFILE": "legion", "PATH": "/usr/local/bin:/usr/bin:/bin", "AWS_EC2_METADATA_DISABLED": "true"}
			for name, value := range testCase.env {
				env[name] = value
			}
			log := slog.New(slog.NewTextHandler(&bytes.Buffer{}, nil))
			var err error
			if testCase.pane {
				install := exec.Command(omp, "plugin", "install", root)
				for name, value := range env {
					install.Env = append(install.Env, name+"="+value)
				}
				if out, err := install.CombinedOutput(); err != nil {
					t.Fatalf("omp plugin install: %v\n%s", err, out)
				}
				// A pane loads the plugin through discovery, as the daemon's boot gate on tmux
				// probes it.
				err = pluginGate{env: env, workDir: dir, invocation: omp, timeout: defaultProbeTimeout, retry: bootprobe.Image,
					contract: 3, skipAgentModels: testCase.skip, log: log}.verify(context.Background())
			} else {
				err = ProbeImage(context.Background(), ImageProbe{Omp: omp, Contract: 3, Env: env, WorkDir: dir, PluginRoot: root,
					SkipAgentModels: testCase.skip, RoleReferences: promptrefs.New(), Log: log})
			}

			if len(testCase.want) == 0 {
				if err != nil {
					t.Fatalf("ProbeImage = %v, want every probe to pass", err)
				}
				return
			}
			if err == nil {
				t.Fatalf("ProbeImage passed, want a refusal saying %q", testCase.want)
			}
			for _, want := range testCase.want {
				if !strings.Contains(err.Error(), want) {
					t.Errorf("ProbeImage = %v, want it to say %q", err, want)
				}
			}
		})
	}
}

// A Sandbox pod runs on the role prompts the daemon inlines from its own directory, not the image's
// copy, so the image's probe resolves what the daemon's prompts name (promptrefs.Roles): an
// agent only the daemon's copy dispatches, which the image's plugin lacks, is refused naming the
// daemon's prompt file, though the image's own roles never name it.
func TestTheImageProbeResolvesTheAgentsTheDaemonsPromptsName(t *testing.T) {
	omp := testomp.Binary(t)
	dir := t.TempDir()
	home := filepath.Join(dir, "home")
	agent := filepath.Join(home, ".omp", "profiles", "legion", "agent")
	mkdir(t, agent)
	for name, content := range map[string]string{
		"models.yml": "providers:\n  fake:\n    baseUrl: http://127.0.0.1:9\n    auth: apiKey\n    api: anthropic-messages\n    apiKey: static-key\n" +
			"    models:\n      - id: m1\n        name: M1\n",
		"config.yml": "modelRoles:\n  default: fake/m1\n  review: fake/m1\n  oracle: fake/m1\n",
	} {
		if err := os.WriteFile(filepath.Join(agent, name), []byte(content), 0o600); err != nil {
			t.Fatal(err)
		}
	}
	root := agentModelPlugin(t, dir)
	roles := func(name, prompt string) string {
		rolesDir := filepath.Join(dir, name)
		mkdir(t, filepath.Join(rolesDir, "core"))
		if err := os.WriteFile(filepath.Join(rolesDir, "core", "planner.md"), []byte(prompt), 0o644); err != nil {
			t.Fatal(err)
		}
		return rolesDir
	}
	image := roles("image-roles", "Consult `task(agent=\"oracle\")`.\n")
	daemonCopy := roles("daemon-roles", "Consult `task(agent=\"oracle\")`, then `task(agent=\"daemon-only\")`.\n")
	imageReferences, err := promptrefs.Roles(image)
	if err != nil {
		t.Fatal(err)
	}
	references, err := promptrefs.Roles(daemonCopy)
	if err != nil {
		t.Fatal(err)
	}
	env := map[string]string{"HOME": home, "OMP_PROFILE": "legion", "PATH": "/usr/local/bin:/usr/bin:/bin", "AWS_EC2_METADATA_DISABLED": "true"}
	probe := func(references promptrefs.Names) error {
		return ProbeImage(context.Background(), ImageProbe{Omp: omp, Contract: 3, Env: env, WorkDir: dir, PluginRoot: root,
			RoleReferences: references, Log: slog.New(slog.NewTextHandler(&bytes.Buffer{}, nil))})
	}
	if err := probe(imageReferences); err != nil {
		t.Fatalf("ProbeImage on the image's own roles = %v, want a pass: they name only agents the plugin ships", err)
	}
	err = probe(references)
	if err == nil || !strings.Contains(err.Error(), "task agent daemon-only (dispatched by roles/core/planner.md)") {
		t.Fatalf("ProbeImage on the daemon's references = %v, want the refusal naming daemon-only and the daemon's prompt", err)
	}
}
