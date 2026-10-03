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
	executable(t, onPath, "jj")
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
	for _, tool := range []string{"gh", "git", "jj"} {
		executable(t, real, tool)
	}
	env := map[string]string{"PATH": workerBin + string(filepath.ListSeparator) + real}
	tools, err := resolveTools(func(name string) (string, bool) { v, ok := env[name]; return v, ok })
	if err != nil {
		t.Fatalf("resolveTools: %v", err)
	}
	if want := filepath.Join(real, "gh"); tools["gh"] != want {
		t.Fatalf("resolveTools with a pane's worker-bin first on PATH = gh %s, want the real %s", tools["gh"], want)
	}
}
