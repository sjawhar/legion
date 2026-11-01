package main

import (
	"bytes"
	"context"
	"os"
	"path/filepath"
	"runtime"
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
	checkShippedExample(t, func(example string) string { return example })
}

// The example's commented role_config_files, uncommented with the operator-roles volume and mount
// it points to, passes the same check, and names the three roles' overlays.
func TestShippedLegionYAMLExamplesRoleConfigFilesPassCheckConfig(t *testing.T) {
	config := checkShippedExample(t, func(example string) string {
		for _, line := range []string{
			"# role_config_files:",
			"#   implementer: [/etc/legion-operator-roles/fallback.yml]",
			"#   tester: [/etc/legion-operator-roles/fallback.yml]",
			"#   merger: [/etc/legion-operator-roles/fallback.yml]",
			"        # - name: operator-roles",
			"        #   config_map: {name: legion-operator-roles}",
			"        # - {volume: operator-roles, mount_path: /etc/legion-operator-roles}",
		} {
			at := strings.Index(example, "\n"+line)
			if at < 0 {
				t.Fatalf("the example has no line starting %q", line)
			}
			uncommented := strings.Replace(line, "# ", "", 1)
			example = example[:at+1] + uncommented + example[at+1+len(line):]
		}
		return example
	})
	cfg, err := configpkg.LoadForValidation(config, nil)
	if err != nil {
		t.Fatal(err)
	}
	if len(cfg.RoleConfigFiles) != 3 {
		t.Errorf("RoleConfigFiles = %v, want the implementer's, the tester's and the merger's", cfg.RoleConfigFiles)
	}
}

// checkShippedExample runs `legion start --check-config` on the shipped example as edit leaves
// it, beside the files it names, and answers the file's path.
func checkShippedExample(t *testing.T, edit func(string) string) string {
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
	for name, contents := range map[string]string{
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

	want := "Config OK: project=WIDGETS\n" + defaultCacheLine +
		"plugins: none — set runtime.kubernetes.plugins or run a release build; a daemon that knows none relaunches no process on another plugin contract\n"
	if code != 0 || out.String() != want {
		t.Fatalf("legion start --check-config on the shipped example = %d, stdout %q, stderr %q; want 0 and %q", code, out.String(), errb.String(), want)
	}
	return config
}
