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

// binary is an executable in dir that reads as a compiled program, as the real gh is: an ELF
// header, no `#!` line.
func binary(t *testing.T, dir, name string) string {
	t.Helper()
	path := filepath.Join(dir, name)
	if err := os.WriteFile(path, []byte("\x7fELF\x02\x01\x01\x00"), 0o755); err != nil {
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

// Boot resolves the binaries Legion itself runs or hands a pane — git, jj and the real gh a pane's
// `legion gh` runs — once, each by its LEGION_<TOOL>_PATH override or on the daemon's PATH, and
// names every missing one with its override.
func TestResolveToolsPrefersTheOverrideAndNamesEveryMissingTool(t *testing.T) {
	onPath, elsewhere := t.TempDir(), t.TempDir()
	executable(t, onPath, "git")
	pinned := executable(t, elsewhere, "git")
	env := map[string]string{"PATH": onPath, "LEGION_GIT_PATH": pinned}
	lookup := func(name string) (string, bool) { v, ok := env[name]; return v, ok }

	_, err := resolveTools(lookup)
	if err == nil || !strings.Contains(err.Error(), "jj (set LEGION_JJ_PATH to an absolute executable path)") ||
		!strings.Contains(err.Error(), "gh (set LEGION_GH_PATH to an absolute executable path)") || strings.Contains(err.Error(), "git (") {
		t.Fatalf("resolveTools with no jj or gh = %v, want only jj and gh named with their overrides", err)
	}
	fakeJJ(t, onPath, currentJJ)
	gh := executable(t, elsewhere, "gh")
	env["LEGION_GH_PATH"] = gh
	tools, err := resolveTools(lookup)
	if err != nil {
		t.Fatalf("resolveTools: %v", err)
	}
	if tools["git"] != pinned || tools["jj"] != filepath.Join(onPath, "jj") || tools["gh"] != gh || len(tools) != 3 {
		t.Fatalf("resolveTools = %v, want the override for git and gh and PATH for jj", tools)
	}
	env["LEGION_JJ_PATH"] = "jj"
	if _, err := resolveTools(lookup); err == nil || !strings.Contains(err.Error(), "LEGION_JJ_PATH") {
		t.Fatalf("resolveTools with a relative override = %v, want it refused", err)
	}
}

// The gh boot finds on PATH is every pane's real gh (LEGION_GH_PATH), which `legion gh` runs with
// the owner's gh directory. The real gh is a compiled program, so a script heading PATH is a
// wrapper — a machine's own gh shim, which routes to a token of its own, or Legion's own `legion
// gh` wrapper, which would run `legion gh` again — and boot refuses it, naming it and
// LEGION_GH_PATH, rather than taking the compiled gh behind it. LEGION_GH_PATH is the operator's
// own word, so a script it names is taken.
func TestResolveToolsRefusesAScriptAsTheRealGh(t *testing.T) {
	shims, bin := t.TempDir(), t.TempDir()
	binary(t, bin, "git")
	fakeJJ(t, bin, currentJJ)
	real := binary(t, bin, "gh")
	shim := filepath.Join(shims, "gh")
	if err := os.WriteFile(shim, []byte("#!/bin/bash\nexec knives gh -- \"$@\"\n"), 0o755); err != nil {
		t.Fatal(err)
	}
	env := map[string]string{"PATH": shims + ":" + bin}
	lookup := func(name string) (string, bool) { v, ok := env[name]; return v, ok }

	_, err := resolveTools(lookup)
	if err == nil || !strings.Contains(err.Error(), "the first gh on PATH, "+shim+", is a script") || !strings.Contains(err.Error(), "LEGION_GH_PATH") {
		t.Fatalf("resolveTools with a script first on PATH = %v, want it refused naming %s and LEGION_GH_PATH", err, shim)
	}
	env["PATH"] = bin + ":" + shims
	if tools, err := resolveTools(lookup); err != nil || tools["gh"] != real {
		t.Fatalf("resolveTools with the compiled gh first on PATH = %v, %v; want %s", tools, err, real)
	}
	env["PATH"], env["LEGION_GH_PATH"] = shims+":"+bin, shim
	if tools, err := resolveTools(lookup); err != nil || tools["gh"] != shim {
		t.Fatalf("resolveTools with LEGION_GH_PATH naming a script = %v, %v; want %s", tools, err, shim)
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
			binary(t, onPath, "gh")
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
