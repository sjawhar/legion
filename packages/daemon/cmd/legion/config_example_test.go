package main

import (
	"bytes"
	"context"
	"maps"
	"os"
	"path/filepath"
	"runtime"
	"slices"
	"strings"
	"testing"

	configpkg "github.com/sjawhar/legion/daemon/internal/config"
)

// The legion.yaml example the repository ships (deploy/kubernetes/daemon/legion.yaml.example, the
// docs site's configuration reference) passes `legion start --check-config` once the files it
// names exist, as its header says: a loader change that would refuse it fails here rather than on
// an operator's first check. The example names no Postgres; the environment does, as its comment
// offers.
func TestShippedLegionYAMLExamplePassesCheckConfig(t *testing.T) {
	checkShippedExample(t, func(example string) string { return example }, nil)
}

// The example's commented role_config_files, uncommented with the operator-roles volume and mount
// it points to, passes the same check, and names the three roles' overlays.
func TestShippedLegionYAMLExamplesRoleConfigFilesPassCheckConfig(t *testing.T) {
	config := checkShippedExample(t, func(example string) string {
		return uncommentExample(t, example,
			"# role_config_files:",
			"#   implementer: [/etc/legion-operator-roles/fallback.yml]",
			"#   tester: [/etc/legion-operator-roles/fallback.yml]",
			"#   merger: [/etc/legion-operator-roles/fallback.yml]",
			"        # - name: operator-roles",
			"        #   config_map: {name: legion-operator-roles}",
			"        # - {volume: operator-roles, mount_path: /etc/legion-operator-roles}",
		)
	}, nil)
	cfg, err := configpkg.LoadForValidation(config, nil)
	if err != nil {
		t.Fatal(err)
	}
	if len(cfg.RoleConfigFiles) != 3 {
		t.Errorf("RoleConfigFiles = %v, want the implementer's, the tester's and the merger's", cfg.RoleConfigFiles)
	}
}

// The example's commented memory block, uncommented with the operator-memory volume and mount it
// points to, passes the same check, and names the project's mode and overlay. The block's
// `instructions` stays commented: boot reads that file from the daemon's own disk, and the example
// names a path only an operator has.
func TestShippedLegionYAMLExamplesMemoryPassesCheckConfig(t *testing.T) {
	config := checkShippedExample(t, func(example string) string {
		example = uncommentExample(t, example,
			"  # WIDGETS:",
			"  #   memory:",
			"  #     mode: on",
			"  #     config_files: [/etc/legion-memory/memory.yml]",
			"        # - name: operator-memory",
			"        #   config_map: {name: legion-operator-memory}",
			"        # - {volume: operator-memory, mount_path: /etc/legion-memory}",
		)
		// The example's two forms of the project entry are alternatives: the empty one it ships
		// active, and the commented one that carries the optional keys. An operator turning memory
		// on moves to the second, so this comments the first out, after the uncommenting above, to
		// which a commented `WIDGETS: {}` would otherwise be the first `# WIDGETS:` line.
		return strings.Replace(example, "\n  WIDGETS: {}", "\n  # WIDGETS: {}", 1)
	}, nil)
	cfg, err := configpkg.LoadForValidation(config, nil)
	if err != nil {
		t.Fatal(err)
	}
	memory := cfg.Projects["WIDGETS"].Memory
	if memory.Mode != configpkg.MemoryOn || !slices.Equal(memory.ConfigFiles, []string{"/etc/legion-memory/memory.yml"}) {
		t.Errorf("the WIDGETS memory block = %+v, want mode on with the operator-memory overlay alone", memory)
	}
	if memory.InstructionsPath != "" {
		t.Errorf("the WIDGETS memory instructions = %q, want none: the example leaves that key commented", memory.InstructionsPath)
	}
}

// uncommentExample is the example with each named line uncommented, each line the start of one of
// the example's own commented lines: the file comments a block out line by line, so turning it on
// is one `# ` removed per line, and a line the example does not hold is the test's own mistake.
func uncommentExample(t *testing.T, example string, lines ...string) string {
	t.Helper()
	for _, line := range lines {
		at := strings.Index(example, "\n"+line)
		if at < 0 {
			t.Fatalf("the example has no line starting %q", line)
		}
		uncommented := strings.Replace(line, "# ", "", 1)
		example = example[:at+1] + uncommented + example[at+1+len(line):]
	}
	return example
}

// checkShippedExample runs `legion start --check-config` on the shipped example as edit leaves
// it, beside the files it names and each of extra (a name under the configuration's own directory,
// or an absolute path), and answers the file's path.
func checkShippedExample(t *testing.T, edit func(string) string, extra map[string]string) string {
	t.Helper()
	legionState(t)
	t.Setenv("LEGION_POSTGRES_DSN", "postgres://legion:legion@127.0.0.1:1/legion")
	_, source, _, ok := runtime.Caller(0)
	if !ok {
		t.Fatal("locate command test source")
	}
	example, err := os.ReadFile(filepath.Join(filepath.Dir(source), "../../../../deploy/kubernetes/daemon/legion.yaml.example"))
	if err != nil {
		t.Fatal(err)
	}
	dir := t.TempDir()
	config := filepath.Join(dir, "legion.yaml")
	files := map[string]string{
		config:           edit(string(example)),
		"operator-token": "operator\n",
		"dispatch-token": "dispatch\n",
		"envoy-token":    "envoy\n",
		// The context the example names, and nothing the check would dial.
		"kubeconfig": `apiVersion: v1
kind: Config
clusters: [{name: example, cluster: {server: "https://192.0.2.20:6443"}}]
users: [{name: legion-daemon, user: {token: placeholder}}]
contexts: [{name: legion-daemon, context: {cluster: example, user: legion-daemon, namespace: legion}}]
current-context: legion-daemon
`,
	}
	maps.Copy(files, extra)
	for name, contents := range files {
		if !filepath.IsAbs(name) {
			name = filepath.Join(dir, name)
		}
		if err := os.WriteFile(name, []byte(contents), 0o600); err != nil {
			t.Fatal(err)
		}
	}

	var out, errb bytes.Buffer
	code := run(context.Background(), []string{"legion", "start", "--check-config", "--config", config}, &out, &errb)

	want := "Config OK: project=WIDGETS\n" + defaultCacheLine +
		"plugins: none — set runtime.kubernetes.plugins or run a release build; a daemon that knows none relaunches no process on another plugin contract\n"
	if code != 0 || out.String() != want {
		t.Fatalf("legion start --check-config on the shipped example = %d, stdout %q, stderr %q; want 0 and %q", code, out.String(), errb.String(), want)
	}
	return config
}

// The example's commented `answers` block, uncommented, passes the check too, on the postgres
// session store the example already names: its gh found by LEGION_GH_PATH and its conversation
// store's URL file read, and its placeholder api_key_command, which sh could not run, never run —
// a key is read for each model call, never by the check.
func TestTheShippedExamplesAnswersBlockPassesCheckConfig(t *testing.T) {
	gh := filepath.Join(t.TempDir(), "gh")
	if err := os.WriteFile(gh, []byte("\x7fELF\x02\x01\x01\x00"), 0o755); err != nil {
		t.Fatal(err)
	}
	t.Setenv("LEGION_GH_PATH", gh)
	config := checkShippedExample(t, uncommentAnswers, map[string]string{
		"conversations-dsn": "postgres://legion-daemon:placeholder@conversations.internal.example:5432/omp_sessions\n",
		"anthropic-key":     "placeholder\n",
	})
	cfg, err := configpkg.LoadForValidation(config, func(string) string { return "postgres://legion:legion@127.0.0.1:1/legion" })
	if err != nil {
		t.Fatal(err)
	}
	if cfg.Answers == nil || len(cfg.Answers.Providers) != 2 || len(cfg.Answers.Prices) != 2 || cfg.Answers.ConversationStore.DSNFile == "" {
		t.Fatalf("the uncommented answers block settled as %+v, want both providers, both prices and the store's file", cfg.Answers)
	}
}

// uncommentAnswers is the example with its whole commented `answers` block, which runs from the
// `# answers:` line to the blank line after it, uncommented: the block is commented line by line,
// so turning it on is one `# ` removed per line. A line whose text is itself a comment stays one,
// since what is left of it begins with spaces and a `#`: the block's own notes, and the `# user:`
// line, the alternative to `dsn_file`.
func uncommentAnswers(example string) string {
	start := strings.Index(example, "\n# answers:")
	if start < 0 {
		return example
	}
	end := strings.Index(example[start+1:], "\n\n")
	if end < 0 {
		return example
	}
	var block strings.Builder
	for _, line := range strings.SplitAfter(example[start+1:start+1+end+1], "\n") {
		block.WriteString(strings.TrimPrefix(line, "# "))
	}
	return example[:start+1] + block.String() + example[start+1+end+1:]
}
