package workerbin

import (
	"os"
	"os/exec"
	"path/filepath"
	"slices"
	"strings"
	"testing"

	"github.com/sjawhar/legion/daemon/internal/runtime/shellprefix"
)

// Oh My Pi's bash tool sources the operator's rc file and snapshots the PATH it leaves, so an rc
// that prepends its own directories (this devbox's dotfiles shims, with their own legion and gh)
// lands them ahead of the launcher directory. The prefix Oh My Pi runs before every command,
// `${PI_SHELL_PREFIX} <command>` in its persistent shell, puts the installed directory back in
// front, so legion and gh both resolve to its scripts, and running it again leaves PATH as it was.
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
	want := filepath.Join(LauncherDir(root), "legion") + "\n" + filepath.Join(LauncherDir(root), "gh") + "\nstable\n"
	if string(out) != want {
		t.Fatalf("resolved\n%s\nwant\n%s", out, want)
	}
}

// The worker image's gh (packages/daemon/docker/gh, COPY'd to /opt/legion/bin/gh) is the script
// Install writes for the image's legion, so a pod's gh and a pane's are one wrapper.
func TestTheImagesGhIsTheWrapperInstallWrites(t *testing.T) {
	root := t.TempDir()
	if err := Install(root, "/opt/legion/bin/legion"); err != nil {
		t.Fatalf("Install: %v", err)
	}
	want, err := os.ReadFile(filepath.Join(LauncherDir(root), "gh"))
	if err != nil {
		t.Fatal(err)
	}
	image, err := os.ReadFile(filepath.Join("..", "..", "..", "docker", "gh"))
	if err != nil {
		t.Fatal(err)
	}
	if string(image) != string(want) {
		t.Fatalf("packages/daemon/docker/gh = %q, want the wrapper Install writes for /opt/legion/bin/legion, %q", image, want)
	}
}

// Install puts two private 0700 scripts in a 0700 bin under root and nothing else: the launcher,
// which execs the installing legion, and gh, which execs its `legion gh` (the wrapper that picks
// the owner's gh directory).
func TestInstallInstallsThePrivateLauncherAndTheGhWrapper(t *testing.T) {
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
	for name, want := range map[string]string{
		"legion": "#!/bin/sh\nexec '/opt/legion' \"$@\"\n",
		"gh":     "#!/bin/sh\nexec '/opt/legion' gh \"$@\"\n",
	} {
		script := filepath.Join(bin, name)
		contents, err := os.ReadFile(script)
		if err != nil {
			t.Fatalf("%s: %v", name, err)
		}
		if info, err := os.Stat(script); err != nil {
			t.Fatalf("%s stat: %v", name, err)
		} else if info.Mode().Perm() != 0o700 {
			t.Fatalf("%s mode = %o, want 0700", name, info.Mode().Perm())
		}
		if string(contents) != want {
			t.Fatalf("%s = %q, want %q", name, contents, want)
		}
	}
	for dir, want := range map[string][]string{root: {"bin"}, bin: {"gh", "legion"}} {
		entries, err := os.ReadDir(dir)
		if err != nil {
			t.Fatal(err)
		}
		names := make([]string, 0, len(entries))
		for _, entry := range entries {
			names = append(names, entry.Name())
		}
		if !slices.Equal(names, want) {
			t.Fatalf("%s holds %q after Install, want %q", dir, names, want)
		}
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
