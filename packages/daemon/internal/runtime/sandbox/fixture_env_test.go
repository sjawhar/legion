package sandbox

import (
	"io/fs"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"

	"github.com/sjawhar/legion/daemon/internal/fixtureenv"
	"github.com/sjawhar/legion/daemon/internal/fixturegit"
)

// This package's TestMain takes every variable that names another repository, remote or credential
// out of the test process (fixtureenv.MustGuard), and internal/fixtureenv's own
// TestEveryGuardingPackagesTestMainGuardsItsProcess holds that it still does. Neither covers the
// one way a fixture command can still be moved onto another repository: a test that plants a
// variable back with t.Setenv, which runs long after the guard.
// TestLiveSetupIgnoresTheRoleEnvironmentGitVariables does exactly that, planting the maps
// setupRig.poisons returns, GIT_DIR and GIT_COMMON_DIR among them, to prove the setup script
// under test ignores the role's environment, and GIT_DIR outranks the directory a command runs
// in. The spec's own run, with no Env on setupRig.git, had the rig's git write a credential.helper
// value and a ref into a repository that was not its own while the package's guard assertion
// stayed green: the defect this test holds closed. So the test plants the variables that reach
// furthest and then drives the rig's two writing shapes, a `git init` and a
// `git -C <clone> config`, past the plant; measured with git 2.47.3, those two write the value
// and no ref.
func TestFixtureCommandsIgnoreAPlantedRepository(t *testing.T) {
	realGit, err := exec.LookPath("git")
	if err != nil {
		t.Fatal(err)
	}
	dir := t.TempDir()
	home, target := filepath.Join(dir, "home"), filepath.Join(dir, "target")
	if err := os.Mkdir(home, 0o700); err != nil {
		t.Fatal(err)
	}
	// The planting target is made by a command that already has a clean environment, so what the
	// test measures is what the rig's commands do, never what made the target.
	fixturegit.Isolated(t, home).Run(dir, append([]string{realGit}, fixturegit.InitWorkTree(target, "")[1:]...)...)
	// Everything a pod's environment carries that would move a git away from the directory it runs
	// in: the repository and working tree themselves, and a command-scope credential helper. The
	// plant goes on after newSetupRig below, which sets GIT_CONFIG_COUNT to 0 for its own purposes
	// and would leave the block dead; t.Setenv restores each when the test ends.
	plant := func() {
		t.Helper()
		t.Setenv("GIT_DIR", filepath.Join(target, ".git"))
		t.Setenv("GIT_WORK_TREE", target)
		t.Setenv("GIT_CONFIG_COUNT", "1")
		t.Setenv("GIT_CONFIG_KEY_0", "credential.helper")
		t.Setenv("GIT_CONFIG_VALUE_0", "!planted-credential-helper")
	}

	// The target's fingerprint is the three things a stray git would change: its configuration, its
	// refs and the files of it. Each reading command is isolated too, so the reading never writes
	// and never follows the plant somewhere else.
	fingerprint := func() map[string]string {
		t.Helper()
		env := fixtureenv.Isolated(home)
		read := func(args ...string) string {
			command := exec.Command(realGit, args...)
			command.Dir = target
			command.Env = env
			output, err := command.Output()
			if err != nil {
				t.Fatalf("git %q in the planting target: %v", args, err)
			}
			return string(output)
		}
		var files []string
		if err := filepath.WalkDir(target, func(path string, entry fs.DirEntry, err error) error {
			if err != nil {
				return err
			}
			relative, err := filepath.Rel(target, path)
			if err != nil {
				return err
			}
			files = append(files, relative)
			return nil
		}); err != nil {
			t.Fatalf("walk the planting target: %v", err)
		}
		return map[string]string{
			"configuration": read("config", "--list", "--local"),
			"refs":          read("for-each-ref"),
			"files":         strings.Join(files, "\n"),
		}
	}
	before := fingerprint()

	const helper = "!rig-credential-helper"
	r := newSetupRig(t, private)
	plant()
	clone := r.plainClone()
	r.git("-C", clone, "config", "credential.helper", helper)

	after := fingerprint()
	for name, want := range before {
		if got := after[name]; got != want {
			t.Errorf("the rig wrote into the planted repository: its %s is now\n%s\nwant the one it had before the rig ran\n%s", name, got, want)
		}
	}
	// The other half of the proof: the rig's work has to have landed somewhere, and that somewhere is
	// the rig's own clone. A fixture that followed the plant leaves this clone without a repository
	// at all, since its `git init` reinitialised the planted one instead.
	if _, err := os.Stat(filepath.Join(clone, ".git")); err != nil {
		t.Fatalf("the rig's clone holds no repository of its own: %v", err)
	}
	value := exec.Command(realGit, "config", "--local", "credential.helper")
	value.Dir = clone
	value.Env = fixtureenv.Isolated(home)
	output, err := value.Output()
	if err != nil {
		t.Fatalf("read credential.helper from the rig's clone: %v", err)
	}
	if got := strings.TrimSpace(string(output)); got != helper {
		t.Errorf("the rig's clone's credential.helper = %q, want %q", got, helper)
	}
}
