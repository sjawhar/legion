package daemon

import (
	"log/slog"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/sjawhar/legion/daemon/internal/config"
)

func executable(t *testing.T, dir, name string) string {
	t.Helper()
	path := filepath.Join(dir, name)
	if err := os.WriteFile(path, []byte("#!/bin/sh\n"), 0o755); err != nil {
		t.Fatal(err)
	}
	return path
}

// currentJJ is what the worker image's jj prints for `jj --version`.
const currentJJ = "jj 0.45.1-sami.20260910-043938-bb5ffc8f23b1e2a9dac6fdf05b0540058a3a21d0"

// fakeJJ is an executable jj in dir whose `jj --version` prints version.
func fakeJJ(t *testing.T, dir, version string) string {
	t.Helper()
	path := filepath.Join(dir, "jj")
	if err := os.WriteFile(path, []byte("#!/bin/sh\nprintf '%s\\n' '"+version+"'\n"), 0o755); err != nil {
		t.Fatal(err)
	}
	return path
}

// Boot resolves the binaries Legion itself runs — git and jj, never gh — once, each by its
// LEGION_<TOOL>_PATH override or on the daemon's PATH, and names every missing one with its
// override.
func TestResolveToolsPrefersTheOverrideAndNamesEveryMissingTool(t *testing.T) {
	onPath, elsewhere := t.TempDir(), t.TempDir()
	executable(t, onPath, "git")
	pinned := executable(t, elsewhere, "git")
	env := map[string]string{"PATH": onPath, "LEGION_GIT_PATH": pinned}
	lookup := func(name string) (string, bool) { v, ok := env[name]; return v, ok }

	_, err := resolveTools(lookup)
	if err == nil || !strings.Contains(err.Error(), "jj (set LEGION_JJ_PATH to an absolute executable path)") || strings.Contains(err.Error(), "git (") {
		t.Fatalf("resolveTools with no jj = %v, want only jj named with its override", err)
	}
	fakeJJ(t, onPath, currentJJ)
	tools, err := resolveTools(lookup)
	if err != nil {
		t.Fatalf("resolveTools: %v", err)
	}
	if tools["git"] != pinned || tools["jj"] != filepath.Join(onPath, "jj") {
		t.Fatalf("resolveTools = %v, want the override for git and PATH for jj", tools)
	}
	if _, resolved := tools["gh"]; resolved || len(tools) != 2 {
		t.Fatalf("resolveTools = %v, want git and jj alone: a pane's gh is its PATH's", tools)
	}
	env["LEGION_JJ_PATH"] = "jj"
	if _, err := resolveTools(lookup); err == nil || !strings.Contains(err.Error(), "LEGION_JJ_PATH") {
		t.Fatalf("resolveTools with a relative override = %v, want it refused", err)
	}
}

// A configuration that puts a role on claude needs Claude Code, bun for its MCP server, and the
// built claude-envoy plugin: boot names every gap in one refusal, and takes each tool's override
// before PATH.
func TestResolveClaudeNamesEveryGap(t *testing.T) {
	onPath, plugin := t.TempDir(), t.TempDir()
	env := map[string]string{"PATH": onPath}
	lookup := func(name string) (string, bool) { v, ok := env[name]; return v, ok }
	resolve := func() (launchReads, error) { return resolveClaude(config.Config{}, lookup, nil) }
	_, err := resolve()
	want := "worker_backends runs a role on claude, and boot is missing bun (set LEGION_BUN_PATH to an absolute executable path), " +
		"claude (set LEGION_CLAUDE_PATH to an absolute executable path), the claude-envoy plugin (set LEGION_CLAUDE_PLUGIN_DIR to its package directory)"
	if err == nil || err.Error() != want {
		t.Fatalf("resolveClaude with nothing = %v\nwant %s", err, want)
	}
	executable(t, onPath, "bun")
	pinned := executable(t, t.TempDir(), "claude")
	env["LEGION_CLAUDE_PATH"], env["LEGION_CLAUDE_PLUGIN_DIR"] = pinned, plugin
	if _, err := resolve(); err == nil || !strings.Contains(err.Error(), "holds no dist/envoy-channel.js: build the plugin") {
		t.Fatalf("resolveClaude with an unbuilt plugin = %v", err)
	}
	for _, file := range []string{"dist/envoy-channel.js", "legion/.claude-plugin/plugin.json"} {
		if err := os.MkdirAll(filepath.Dir(filepath.Join(plugin, file)), 0o700); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(filepath.Join(plugin, file), nil, 0o600); err != nil {
			t.Fatal(err)
		}
	}
	read, err := resolve()
	if err != nil {
		t.Fatal(err)
	}
	var logged strings.Builder
	read.log(slog.New(slog.NewTextHandler(&logged, nil)))
	for _, want := range []string{"claude=" + pinned, "bun=" + filepath.Join(onPath, "bun"), "plugin=" + plugin} {
		if !strings.Contains(logged.String(), want) {
			t.Errorf("resolveClaude logged %q, want %s", logged.String(), want)
		}
	}
	if read.launcher.Marker() != " claude-bridge -- " || read.invocation != "" {
		t.Errorf("resolveClaude = marker %q, invocation %q", read.launcher.Marker(), read.invocation)
	}
	env["LEGION_CLAUDE_PLUGIN_DIR"] = "relative"
	if _, err := resolve(); err == nil || !strings.Contains(err.Error(), "LEGION_CLAUDE_PLUGIN_DIR is not an absolute path") {
		t.Errorf("a relative plugin dir = %v", err)
	}
}

// Provisioning relies on jj 0.38's configuration layout (minimumJJ), so boot refuses a jj older
// than 0.38, or one whose version it cannot read, naming what it found and LEGION_JJ_PATH, whether
// the jj came from the override or from PATH; 0.38 itself and later releases, a build's suffix
// included, pass.
func TestResolveToolsRefusesAJJOlderThanTheLayoutProvisioningReliesOn(t *testing.T) {
	for _, tc := range []struct {
		name, version string
		override      bool
		refused       []string
	}{
		{"jj 0.37.0 on PATH", "jj 0.37.0", false, []string{"is 0.37.0, older than 0.38", "LEGION_JJ_PATH"}},
		{"jj 0.37.0 at the override", "jj 0.37.0-abc123", true, []string{"is 0.37.0-abc123, older than 0.38", "LEGION_JJ_PATH"}},
		{"a version it cannot read", "jujutsu, probably", false, []string{`printed "jujutsu, probably"`, "LEGION_JJ_PATH"}},
		{"jj 0.38.0", "jj 0.38.0", false, nil},
		{"the worker image's jj", currentJJ, false, nil},
		{"jj 1.0.0 at the override", "jj 1.0.0", true, nil},
	} {
		t.Run(tc.name, func(t *testing.T) {
			onPath, elsewhere := t.TempDir(), t.TempDir()
			executable(t, onPath, "git")
			env := map[string]string{"PATH": onPath}
			if tc.override {
				env["LEGION_JJ_PATH"] = fakeJJ(t, elsewhere, tc.version)
			} else {
				fakeJJ(t, onPath, tc.version)
			}
			tools, err := resolveTools(func(name string) (string, bool) { v, ok := env[name]; return v, ok })
			if tc.refused == nil {
				if err != nil {
					t.Fatalf("resolveTools with %s = %v, want it accepted", tc.version, err)
				}
				if tools["jj"] == "" {
					t.Fatalf("resolveTools with %s resolved no jj: %v", tc.version, tools)
				}
				return
			}
			if err == nil {
				t.Fatalf("resolveTools with %s = %v, want it refused", tc.version, tools)
			}
			for _, want := range tc.refused {
				if !strings.Contains(err.Error(), want) {
					t.Errorf("resolveTools with %s = %v, want it to contain %q", tc.version, err, want)
				}
			}
		})
	}
}

// The jj --version boot runs starts without either raw NATS seed the daemon holds, as every
// command the daemon starts does (runtime.WithoutNATSSeeds).
func TestTheJJVersionCheckGetsNoNATSSeed(t *testing.T) {
	t.Setenv("NATS_NKEY_SEED", "pane-seed-value")
	t.Setenv("NATS_DAEMON_NKEY_SEED", "daemon-seed-value")
	dir := t.TempDir()
	dump := filepath.Join(dir, "env")
	jj := filepath.Join(dir, "jj")
	if err := os.WriteFile(jj, []byte("#!/bin/sh\nenv >"+dump+"\necho '"+currentJJ+"'\n"), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := checkJJVersion(jj); err != nil {
		t.Fatalf("checkJJVersion(%s) = %v, want it accepted", jj, err)
	}
	seen, err := os.ReadFile(dump)
	if err != nil {
		t.Fatalf("jj --version never ran: %v", err)
	}
	if strings.Contains(string(seen), "seed-value") || !strings.Contains(string(seen), "PATH=") {
		t.Errorf("jj --version's environment carries a NATS seed (or is not the daemon's):\n%s", seen)
	}
}
