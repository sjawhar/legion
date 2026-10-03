package main

import (
	"bytes"
	"context"
	"os"
	"path/filepath"
	"runtime"
	"testing"
)

// The legion.yaml example the repository ships (deploy/kubernetes/daemon/legion.yaml.example, the
// docs site's configuration reference) passes `legion start --check-config` once the files it
// names exist, as its header says: a loader change that would refuse it fails here rather than on
// an operator's first check. The example names no Postgres; the environment does, as its comment
// offers.
func TestShippedLegionYAMLExamplePassesCheckConfig(t *testing.T) {
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
	for name, contents := range map[string]string{
		config:           string(example),
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
	} {
		if !filepath.IsAbs(name) {
			name = filepath.Join(dir, name)
		}
		if err := os.WriteFile(name, []byte(contents), 0o600); err != nil {
			t.Fatal(err)
		}
	}

	var out, errb bytes.Buffer
	code := run(context.Background(), []string{"legion", "start", "--check-config", "--config", config}, &out, &errb)

	if code != 0 || out.String() != "Config OK: project=WIDGETS\n" {
		t.Fatalf("legion start --check-config on the shipped example = %d, stdout %q, stderr %q; want 0 and Config OK", code, out.String(), errb.String())
	}
}
