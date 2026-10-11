package workerbin

import (
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"

	"github.com/sjawhar/legion/daemon/internal/runtime/shellprefix"
)

// Oh My Pi's bash tool sources the operator's rc file and snapshots the PATH it leaves, so an rc
// that prepends its own directories (this devbox's dotfiles shims, with their own legion) lands
// them ahead of the launcher directory. The prefix Oh My Pi runs before every command,
// `${PI_SHELL_PREFIX} <command>` in its persistent shell, puts the installed directory back in
// front, and running it again leaves PATH as it was. gh is not intercepted: the rc's gh, like any
// other gh on PATH, reads the agent's credential from GH_CONFIG_DIR.
func TestShellPrefixResolvesLegionsLauncherAheadOfAnRcsDirectories(t *testing.T) {
	root := filepath.Join(t.TempDir(), "state dir")
	if err := Install(root, "/opt/legion"); err != nil {
		t.Fatalf("Install: %v", err)
	}
	rc := t.TempDir()
	for _, name := range []string{"gh", "legion"} {
		if err := os.WriteFile(filepath.Join(rc, name), []byte("#!/bin/sh\nexit 97\n"), 0o700); err != nil {
			t.Fatal(err)
		}
	}
	snapshot := strings.Join([]string{rc, LauncherDir(root), "/usr/bin", "/bin"}, ":")
	prefix := shellprefix.For(LauncherDir(root))
	script := "PATH=" + shellprefix.Literal(snapshot) + "\n" +
		prefix + " command -v legion\n" +
		prefix + " command -v gh\n" +
		"before=$PATH\n" +
		prefix + ` test "$PATH" = "$before" && echo stable`
	out, err := exec.Command("bash", "-c", script).CombinedOutput()
	if err != nil {
		t.Fatalf("bash: %v: %s", err, out)
	}
	want := filepath.Join(LauncherDir(root), "legion") + "\n" + filepath.Join(rc, "gh") + "\nstable\n"
	if string(out) != want {
		t.Fatalf("resolved\n%s\nwant\n%s", out, want)
	}
}

// Install puts the launcher alone under root: a private 0700 script in a 0700 directory that execs
// the installing legion, and no gh interception directory beside it.
func TestInstallInstallsThePrivateLauncherAndNothingElse(t *testing.T) {
	root := t.TempDir()
	if err := Install(root, "/opt/legion"); err != nil {
		t.Fatalf("Install: %v", err)
	}
	bin := LauncherDir(root)
	if info, err := os.Stat(bin); err != nil {
		t.Fatalf("bin: %v", err)
	} else if info.Mode().Perm() != 0o700 {
		t.Fatalf("bin mode = %o, want 0700", info.Mode().Perm())
	}
	launcher := filepath.Join(bin, "legion")
	contents, err := os.ReadFile(launcher)
	if err != nil {
		t.Fatalf("launcher: %v", err)
	}
	if info, err := os.Stat(launcher); err != nil {
		t.Fatalf("launcher stat: %v", err)
	} else if info.Mode().Perm() != 0o700 {
		t.Fatalf("launcher mode = %o, want 0700", info.Mode().Perm())
	}
	if want := "#!/bin/sh\nexec '/opt/legion' \"$@\"\n"; string(contents) != want {
		t.Fatalf("launcher = %q, want %q", contents, want)
	}
	entries, err := os.ReadDir(root)
	if err != nil {
		t.Fatal(err)
	}
	if len(entries) != 1 || entries[0].Name() != "bin" {
		t.Fatalf("Install left %d entries under root, want bin alone", len(entries))
	}
	if err := Install(root, "legion"); err == nil || !strings.Contains(err.Error(), "not an absolute path") {
		t.Fatalf("Install with a relative executable = %v, want a refusal", err)
	}
}

// Path leads with root's bin exactly once, whatever PATH the daemon inherited: a daemon started
// from an agent's shell carries its predecessor's bin, and every other entry stays verbatim.
func TestPathLeadsWithTheLauncherDirectoryExactlyOnce(t *testing.T) {
	root := "/var/lib/legion"
	bin := LauncherDir(root)
	for path, want := range map[string]string{
		"/usr/local/bin:/usr/bin":                    bin + ":/usr/local/bin:/usr/bin",
		bin + ":/usr/local/bin:" + bin + ":/usr/bin": bin + ":/usr/local/bin:/usr/bin",
		"":               bin,
		"/usr/bin::/bin": bin + ":/usr/bin:/bin",
	} {
		if got := Path(path, root); got != want {
			t.Errorf("Path(%q) = %q, want %q", path, got, want)
		}
	}
}
