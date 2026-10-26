package main

import (
	"bytes"
	"context"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"testing"

	"github.com/sjawhar/legion/daemon/internal/config"
)

// shippedExample is the legion.yaml example the repository ships
// (deploy/kubernetes/daemon/legion.yaml.example, the docs site's configuration reference).
func shippedExample(t *testing.T) string {
	t.Helper()
	_, source, _, ok := runtime.Caller(0)
	if !ok {
		t.Fatal("locate command test source")
	}
	example, err := os.ReadFile(filepath.Join(filepath.Dir(source), "../../../../deploy/kubernetes/daemon/legion.yaml.example"))
	if err != nil {
		t.Fatal(err)
	}
	return string(example)
}

// checkExample runs `legion start --check-config` on example, written beside the files it names
// (the three token files, the kubeconfig, and files' own), and fails the test unless the check
// passes with the example's own output. The example names no Postgres; the environment does, as
// its comment offers.
func checkExample(t *testing.T, example string, files map[string]string) {
	t.Helper()
	legionState(t)
	t.Setenv("LEGION_POSTGRES_DSN", "postgres://legion:legion@127.0.0.1:1/legion")
	dir := t.TempDir()
	config := filepath.Join(dir, "legion.yaml")
	contents := map[string]string{
		config:           example,
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
	for name, body := range files {
		contents[name] = body
	}
	for name, body := range contents {
		if !filepath.IsAbs(name) {
			name = filepath.Join(dir, name)
		}
		if err := os.WriteFile(name, []byte(body), 0o600); err != nil {
			t.Fatal(err)
		}
	}

	var out, errb bytes.Buffer
	code := run(context.Background(), []string{"legion", "start", "--check-config", "--config", config}, &out, &errb)

	want := "Config OK: project=WIDGETS\n" + defaultCacheLine +
		"plugins: none — set runtime.kubernetes.plugins or run a release build; a daemon that knows none relaunches no process on another plugin contract\n"
	if code != 0 || out.String() != want {
		t.Fatalf("legion start --check-config = %d, stdout %q, stderr %q; want 0 and %q", code, out.String(), errb.String(), want)
	}
}

// The shipped example passes `legion start --check-config` once the files it names exist, as its
// header says: a loader change that would refuse it fails here rather than on an operator's first
// check.
func TestShippedLegionYAMLExamplePassesCheckConfig(t *testing.T) {
	checkExample(t, shippedExample(t), nil)
}

// The example's commented `answers` block, uncommented on a postgres session store, passes the
// check too, its gh found by LEGION_GH_PATH and its conversation store's URL file read, and its
// placeholder api_key_command, which sh could not run, never run: a key is read for each model
// call, never by the check.
func TestTheShippedExamplesAnswersBlockPassesCheckConfig(t *testing.T) {
	example := shippedExample(t)
	start := strings.Index(example, "\n# answers:")
	end := strings.Index(example[start+1:], "\n\n")
	if start < 0 || end < 0 {
		t.Fatal("the shipped example has no commented answers block ending at a blank line")
	}
	var block strings.Builder
	for _, line := range strings.SplitAfter(example[start+1:start+1+end+1], "\n") {
		block.WriteString(strings.TrimPrefix(line, "# "))
	}
	const contextLine = "    context: legion-daemon\n"
	if !strings.Contains(example, contextLine) {
		t.Fatalf("the shipped example has no line %q to set the session store after", contextLine)
	}
	example = strings.Replace(example, contextLine, contextLine+"    session_store: postgres\n    session_dsn_secret: SESSION_DSN\n", 1) + "\n" + block.String()

	// The block is live, not a comment the uncommenting left behind: both providers and the store.
	path := filepath.Join(t.TempDir(), "legion.yaml")
	if err := os.WriteFile(path, []byte(example), 0o600); err != nil {
		t.Fatal(err)
	}
	cfg, err := config.LoadForValidation(path, func(string) string { return "postgres://legion:legion@127.0.0.1:1/legion" })
	if err != nil {
		t.Fatalf("LoadForValidation of the uncommented answers block: %v", err)
	}
	if cfg.Answers == nil || len(cfg.Answers.Providers) != 2 || len(cfg.Answers.Prices) != 2 || cfg.Answers.ConversationStore.DSNFile == "" {
		t.Fatalf("the uncommented answers block settled as %+v, want both providers, both prices and the store's file", cfg.Answers)
	}

	gh := filepath.Join(t.TempDir(), "gh")
	if err := os.WriteFile(gh, []byte("\x7fELF\x02\x01\x01\x00"), 0o755); err != nil {
		t.Fatal(err)
	}
	t.Setenv("LEGION_GH_PATH", gh)
	checkExample(t, example, map[string]string{
		"conversations-dsn": "postgres://legion-daemon:placeholder@conversations.internal.example:5432/omp_sessions\n",
	})
}
