package tmux

import (
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"

	"github.com/sjawhar/legion/daemon/internal/runtime/shellprefix"
)

// Oh My Pi's bash tool sources the operator's rc file and snapshots the PATH it leaves, so an rc
// that prepends its own directories (this devbox's dotfiles shims, with their own gh) lands them
// ahead of worker-bin. The prefix Oh My Pi runs before every command, `${PI_SHELL_PREFIX} <command>`
// in its persistent shell, puts this daemon's two directories back in front, and running it again
// leaves PATH as it was.
func TestShellPrefixResolvesLegionsGhAndLegionAheadOfAnRcsDirectories(t *testing.T) {
	stateDir := filepath.Join(t.TempDir(), "state dir")
	if err := InstallWorkerBin(stateDir, "/opt/legion"); err != nil {
		t.Fatalf("InstallWorkerBin: %v", err)
	}
	rc := t.TempDir()
	for _, name := range []string{"gh", "legion"} {
		if err := os.WriteFile(filepath.Join(rc, name), []byte("#!/bin/sh\nexit 97\n"), 0o700); err != nil {
			t.Fatal(err)
		}
	}
	snapshot := strings.Join([]string{rc, workerBinDir(stateDir), legionBinDir(stateDir), "/usr/bin", "/bin"}, ":")
	prefix := shellprefix.For(workerBinDir(stateDir), legionBinDir(stateDir))
	script := "PATH=" + shellLiteral(snapshot) + "\n" +
		prefix + " command -v gh\n" +
		prefix + " command -v legion\n" +
		"before=$PATH\n" +
		prefix + ` test "$PATH" = "$before" && echo stable`
	out, err := exec.Command("bash", "-c", script).CombinedOutput()
	if err != nil {
		t.Fatalf("bash: %v: %s", err, out)
	}
	want := filepath.Join(workerBinDir(stateDir), "gh") + "\n" + filepath.Join(legionBinDir(stateDir), "legion") + "\nstable\n"
	if string(out) != want {
		t.Fatalf("resolved\n%s\nwant\n%s", out, want)
	}
}

func TestInstallWorkerBinInstallsPrivateExecutableGhShim(t *testing.T) {
	stateDir := t.TempDir()
	if err := InstallWorkerBin(stateDir, "/opt/legion"); err != nil {
		t.Fatalf("InstallWorkerBin: %v", err)
	}
	bin := filepath.Join(stateDir, "worker-bin")
	if info, err := os.Stat(bin); err != nil {
		t.Fatalf("worker-bin: %v", err)
	} else if info.Mode().Perm() != 0o700 {
		t.Fatalf("worker-bin mode = %o, want 0700", info.Mode().Perm())
	}
	shim := filepath.Join(bin, "gh")
	contents, err := os.ReadFile(shim)
	if err != nil {
		t.Fatalf("gh shim: %v", err)
	}
	if info, err := os.Stat(shim); err != nil {
		t.Fatalf("gh shim stat: %v", err)
	} else if info.Mode().Perm() != 0o700 {
		t.Fatalf("gh shim mode = %o, want 0700", info.Mode().Perm())
	}
	want := "#!/bin/sh\nPATH=${PATH#'" + bin + ":'}\nexport PATH\nexec legion gh -- \"$@\"\n"
	if string(contents) != want {
		t.Fatalf("gh shim = %q, want %q", contents, want)
	}
}

func TestPaneEnvironmentPutsWorkerBinAndTheLauncherFirstExactlyOnce(t *testing.T) {
	stateDir := "/var/lib/legion"
	workerBin := filepath.Join(stateDir, "worker-bin")
	env := PaneEnvironment([]string{
		"PATH=" + workerBin + ":/usr/local/bin:" + filepath.Join(stateDir, "bin") + ":" + workerBin + ":/usr/bin",
	}, stateDir)
	if got, want := env["PATH"], workerBin+":"+filepath.Join(stateDir, "bin")+":/usr/local/bin:/usr/bin"; got != want {
		t.Fatalf("PATH = %q, want %q", got, want)
	}
	if strings.Count(env["PATH"], workerBin) != 1 {
		t.Fatalf("PATH = %q carries worker-bin more than once", env["PATH"])
	}
}
