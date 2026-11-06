package main

import (
	"bytes"
	"context"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"testing"

	"github.com/sjawhar/legion/daemon/internal/workspace"
)

// runLegionHere runs `legion <args…>` in this process, as the other command tests run their own.
func runLegionHere(args []string) (code int, stdout, stderr string) {
	var out, errb bytes.Buffer
	code = run(context.Background(), append([]string{"legion"}, args...), &out, &errb)
	return code, out.String(), errb.String()
}

// skillsVolumeFixture is a skills volume as an issue pod's init container leaves it: a clone whose
// one plugin directory holds a file, and the four records beside it, the TREES record digesting
// that directory. It returns the volume root and the plugin's directory of the clone.
func skillsVolumeFixture(t *testing.T) (dir, plugin string) {
	t.Helper()
	dir = t.TempDir()
	plugin = filepath.Join(dir, "repo", "plugins", "probe-skills")
	if err := os.MkdirAll(filepath.Join(plugin, "skills"), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(plugin, "skills", "example.md"), []byte("A local skill.\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	digest, err := workspace.SkillsTreeDigest(plugin)
	if err != nil {
		t.Fatal(err)
	}
	for path, content := range map[string]string{
		workspace.SkillsCommitFile(dir):      "0123456789abcdef0123456789abcdef01234567\n",
		workspace.SkillsMarketplaceFile(dir): "probe-market\n",
		workspace.SkillsPluginsFile(dir):     "probe-skills\n",
		workspace.SkillsTreesFile(dir):       digest + "  probe-skills\n",
	} {
		if err := os.WriteFile(path, []byte(content), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	return dir, plugin
}

// ompListStub is a stand-in for the Oh My Pi the check asks where it installed each plugin: it
// answers `plugin list --json` with the shape omp answers (measured against omp 18.16 with a local
// marketplace of one plugin: `marketplace[].entries[].installPath` is the installed directory),
// and anything else with nothing. exit is its status for every call.
func ompListStub(t *testing.T, body string, exit int) string {
	t.Helper()
	path := filepath.Join(t.TempDir(), "omp")
	script := "#!/bin/sh\nif [ \"$1 $2 $3\" = \"plugin list --json\" ]; then\n  cat <<'JSON'\n" + body + "\nJSON\nfi\nexit " + strconv.Itoa(exit) + "\n"
	if err := os.WriteFile(path, []byte(script), 0o700); err != nil {
		t.Fatal(err)
	}
	return path
}

// listing is omp's `plugin list --json` naming one install of probe-skills at root.
func listing(root string) string {
	return `{"npm":[],"marketplace":[{"id":"probe-skills@probe-market","scope":"user","entries":[{"scope":"user","installPath":"` + root + `","version":"0.1.0"}]}]}`
}

// The passing arm: the directory Oh My Pi reports holds the bytes of the clone the pod's init
// container recorded, so the check exits 0 and says which plugins of which marketplace came from
// which commit.
func TestSkillsCheckPassesWhenTheInstalledTreeIsTheRecordedCommit(t *testing.T) {
	dir, plugin := skillsVolumeFixture(t)
	installed := filepath.Join(t.TempDir(), "installed")
	if err := os.CopyFS(installed, os.DirFS(plugin)); err != nil {
		t.Fatal(err)
	}
	code, stdout, stderr := runLegionHere([]string{"skills-check", "--dir", dir, "--omp", ompListStub(t, listing(installed), 0)})
	if code != 0 {
		t.Fatalf("exit %d, stdout %q, stderr %q; want 0", code, stdout, stderr)
	}
	for _, want := range []string{"probe-skills", "probe-market", "0123456789abcdef0123456789abcdef01234567"} {
		if !strings.Contains(stdout, want) {
			t.Errorf("stdout %q does not name %q", stdout, want)
		}
	}
}

// The failing arm, with the role's launch failing rather than its agent starting: a tree that
// differs from the recorded commit, a plugin Oh My Pi reports nowhere, an Oh My Pi that cannot be
// asked, and a volume whose records are missing. Each exits non-zero with one line on stderr.
func TestSkillsCheckRefusesAnInstallThatIsNotTheRecordedCommit(t *testing.T) {
	for name, tc := range map[string]struct {
		prepare func(t *testing.T, dir, plugin string) (ompPath string)
		want    string
	}{
		"a tree that differs": {
			func(t *testing.T, dir, plugin string) string {
				installed := filepath.Join(t.TempDir(), "installed")
				if err := os.CopyFS(installed, os.DirFS(plugin)); err != nil {
					t.Fatal(err)
				}
				if err := os.WriteFile(filepath.Join(installed, "skills", "example.md"), []byte("planted\n"), 0o600); err != nil {
					t.Fatal(err)
				}
				return ompListStub(t, listing(installed), 0)
			},
			"is not what the pod's skills clone recorded",
		},
		"a plugin installed nowhere": {
			func(t *testing.T, dir, plugin string) string {
				return ompListStub(t, `{"npm":[],"marketplace":[]}`, 0)
			},
			"is not installed: Oh My Pi reports no directory for it",
		},
		"an Oh My Pi that cannot be asked": {
			func(t *testing.T, dir, plugin string) string { return ompListStub(t, "", 3) },
			"plugin list --json exited 3",
		},
		"a volume with no records": {
			func(t *testing.T, dir, plugin string) string {
				if err := os.Remove(workspace.SkillsMarketplaceFile(dir)); err != nil {
					t.Fatal(err)
				}
				return ompListStub(t, listing(plugin), 0)
			},
			"read the skills record",
		},
		"a volume with no tree record": {
			func(t *testing.T, dir, plugin string) string {
				if err := os.Remove(workspace.SkillsTreesFile(dir)); err != nil {
					t.Fatal(err)
				}
				return ompListStub(t, listing(plugin), 0)
			},
			"read the skills record",
		},
	} {
		t.Run(name, func(t *testing.T) {
			dir, plugin := skillsVolumeFixture(t)
			ompPath := tc.prepare(t, dir, plugin)
			code, stdout, stderr := runLegionHere([]string{"skills-check", "--dir", dir, "--omp", ompPath})
			if code == 0 || !strings.Contains(stderr, tc.want) {
				t.Fatalf("exit %d, stdout %q, stderr %q; want non-zero naming %q", code, stdout, stderr, tc.want)
			}
			if stdout != "" {
				t.Errorf("a refused check wrote %q to stdout, which a role's wrapper keeps for the harness", stdout)
			}
		})
	}
}

// Both paths are checked before anything runs, so a wrapper that was rendered wrongly says so
// rather than asking Oh My Pi about a relative path.
func TestSkillsCheckRefusesARelativePath(t *testing.T) {
	dir, _ := skillsVolumeFixture(t)
	for _, tc := range []struct {
		args []string
		want string
	}{
		{[]string{"skills-check", "--dir", "skills", "--omp", "/opt/omp/bin/omp"}, `--dir must be an absolute path (got "skills")`},
		{[]string{"skills-check", "--dir", dir, "--omp", "omp"}, `--omp must be an absolute path (got "omp")`},
	} {
		code, _, stderr := runLegionHere(tc.args)
		if code == 0 || !strings.Contains(stderr, tc.want) {
			t.Errorf("exit %d, stderr %q; want non-zero naming %q", code, stderr, tc.want)
		}
	}
}
