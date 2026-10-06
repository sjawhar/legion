package daemon

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
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

// Boot resolves the binaries Legion itself runs once, each by its LEGION_<TOOL>_PATH override or
// on the daemon's PATH, and names every missing one with its override.
func TestResolveToolsPrefersTheOverrideAndNamesEveryMissingTool(t *testing.T) {
	onPath, elsewhere := t.TempDir(), t.TempDir()
	executable(t, onPath, "gh")
	executable(t, onPath, "git")
	pinned := executable(t, elsewhere, "gh")
	env := map[string]string{"PATH": onPath, "LEGION_GH_PATH": pinned}
	lookup := func(name string) (string, bool) { v, ok := env[name]; return v, ok }

	_, err := resolveTools(lookup)
	if err == nil || !strings.Contains(err.Error(), "jj (set LEGION_JJ_PATH to an absolute executable path)") || strings.Contains(err.Error(), "gh (") {
		t.Fatalf("resolveTools with no jj = %v, want only jj named with its override", err)
	}
	fakeJJ(t, onPath, currentJJ)
	tools, err := resolveTools(lookup)
	if err != nil {
		t.Fatalf("resolveTools: %v", err)
	}
	if tools["gh"] != pinned || tools["git"] != filepath.Join(onPath, "git") || tools["jj"] != filepath.Join(onPath, "jj") {
		t.Fatalf("resolveTools = %v, want the override for gh and PATH for git and jj", tools)
	}
	env["LEGION_GIT_PATH"] = "git"
	if _, err := resolveTools(lookup); err == nil || !strings.Contains(err.Error(), "LEGION_GIT_PATH") {
		t.Fatalf("resolveTools with a relative override = %v, want it refused", err)
	}
}

// A daemon started from inside a Legion pane inherits that pane's worker-bin, whose gh is the shim
// that runs `legion gh`, first on its PATH. Its panes are told the real gh, never the shim, which
// their own `legion gh` would run and so reach `legion gh` again.
func TestResolveToolsSkipsAnInheritedPanesWorkerBin(t *testing.T) {
	pane, real := t.TempDir(), t.TempDir()
	workerBin := filepath.Join(pane, "worker-bin")
	if err := os.Mkdir(workerBin, 0o700); err != nil {
		t.Fatal(err)
	}
	executable(t, workerBin, "gh")
	executable(t, real, "gh")
	executable(t, real, "git")
	fakeJJ(t, real, currentJJ)
	env := map[string]string{"PATH": workerBin + string(filepath.ListSeparator) + real}
	tools, err := resolveTools(func(name string) (string, bool) { v, ok := env[name]; return v, ok })
	if err != nil {
		t.Fatalf("resolveTools: %v", err)
	}
	if want := filepath.Join(real, "gh"); tools["gh"] != want {
		t.Fatalf("resolveTools with a pane's worker-bin first on PATH = gh %s, want the real %s", tools["gh"], want)
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
			executable(t, onPath, "gh")
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
