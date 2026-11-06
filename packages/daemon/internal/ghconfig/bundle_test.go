package ghconfig

import (
	"os"
	"os/exec"
	"path/filepath"
	"slices"
	"strings"
	"testing"
	"time"
)

func twoOwnerBundle(acmeToken string) Bundle {
	expires := time.Date(2026, 10, 16, 18, 0, 0, 0, time.UTC)
	return Bundle{
		Default: "acme",
		Dirs: map[string]Rendered{
			"acme":    Render(acmeToken, "implement", expires),
			"sjawhar": Render("ghs_sjawhar_read", "implement", expires),
		},
	}
}

// WriteBundle writes each owner's two gh files under <root>/<owner>, reports the owners whose
// hosts.yml changed (all of them on the first write, sorted), then only the owner whose token
// turned, and removes every other entry of root: an owner dropped from the mapping, a file of a
// directory written before bundles, and anything else, such as a file naming a default owner, that
// is no owner's directory, since nothing under the root but the gh files is the daemon's.
func TestWriteBundleWritesOneDirectoryPerOwnerAndRemovesTheRest(t *testing.T) {
	root := filepath.Join(t.TempDir(), "gh")
	if err := os.MkdirAll(root, 0o700); err != nil {
		t.Fatal(err)
	}
	for name, body := range map[string]string{HostsFile: Hosts("ghs_stale"), ".default": "sjawhar"} {
		if err := os.WriteFile(filepath.Join(root, name), []byte(body), 0o600); err != nil {
			t.Fatal(err)
		}
	}
	changed, err := WriteBundle(root, twoOwnerBundle("ghs_acme_write"))
	if err != nil {
		t.Fatalf("first WriteBundle: %v", err)
	}
	if want := []string{"acme", "sjawhar"}; !slices.Equal(changed, want) {
		t.Fatalf("first WriteBundle changed %q, want %q", changed, want)
	}
	for owner, token := range map[string]string{"acme": "ghs_acme_write", "sjawhar": "ghs_sjawhar_read"} {
		dir := Dir(root, owner)
		if dir != filepath.Join(root, owner) {
			t.Fatalf("Dir(%q) = %q, want %q", owner, dir, filepath.Join(root, owner))
		}
		hosts, err := os.ReadFile(filepath.Join(dir, HostsFile))
		if err != nil {
			t.Fatal(err)
		}
		if got, err := TokenFromHosts(hosts); err != nil || got != token {
			t.Fatalf("%s's hosts.yml token = %q, %v, want %q", owner, got, err, token)
		}
		if config, err := os.ReadFile(filepath.Join(dir, ConfigFile)); err != nil || string(config) != Config {
			t.Fatalf("%s's config.yml = %q, %v, want %q", owner, config, err, Config)
		}
	}
	for _, stray := range []string{HostsFile, ".default"} {
		if _, err := os.Stat(filepath.Join(root, stray)); !os.IsNotExist(err) {
			t.Fatalf("the stray %s at the root survived the write: %v", stray, err)
		}
	}
	if info, err := os.Stat(root); err != nil || info.Mode().Perm() != 0o700 {
		t.Fatalf("root = %v, %v, want a 0700 directory", info, err)
	}

	changed, err = WriteBundle(root, twoOwnerBundle("ghs_acme_turned"))
	if err != nil {
		t.Fatalf("second WriteBundle: %v", err)
	}
	if want := []string{"acme"}; !slices.Equal(changed, want) {
		t.Fatalf("second WriteBundle changed %q, want %q", changed, want)
	}

	alone := twoOwnerBundle("ghs_acme_turned")
	delete(alone.Dirs, "sjawhar")
	changed, err = WriteBundle(root, alone)
	if err != nil {
		t.Fatalf("third WriteBundle: %v", err)
	}
	if len(changed) != 0 {
		t.Fatalf("third WriteBundle changed %q, want nothing", changed)
	}
	entries, err := os.ReadDir(root)
	if err != nil {
		t.Fatal(err)
	}
	names := make([]string, 0, len(entries))
	for _, entry := range entries {
		names = append(names, entry.Name())
	}
	if want := []string{"acme"}; !slices.Equal(names, want) {
		t.Fatalf("root holds %q after sjawhar left the bundle, want %q", names, want)
	}
}

// A bundle whose directories name repositories is written two levels deep: the owner's own
// hosts.yml and config.yml at `<root>/<owner>`, and each repository's pair at
// `<root>/<owner>/<repo>`. WriteBundle cleans both levels, so a repository the mapping no longer
// serves is left holding no stale token, while the owner's own two files stay where they are.
func TestWriteBundleWritesAndPrunesRepositoryDirectories(t *testing.T) {
	expires := time.Date(2026, 10, 16, 18, 0, 0, 0, time.UTC)
	root := filepath.Join(t.TempDir(), "gh")
	bundle := twoOwnerBundle("ghs_acme_write")
	bundle.Dirs["acme/widgets"] = Render("ghs_acme_widgets", "implement", expires)
	bundle.Dirs["acme/gadgets"] = Render("ghs_acme_gadgets", "implement", expires)
	changed, err := WriteBundle(root, bundle)
	if err != nil {
		t.Fatalf("WriteBundle: %v", err)
	}
	if want := []string{"acme", "acme/gadgets", "acme/widgets", "sjawhar"}; !slices.Equal(changed, want) {
		t.Fatalf("WriteBundle changed %q, want %q", changed, want)
	}
	for name, token := range map[string]string{
		"acme": "ghs_acme_write", "acme/widgets": "ghs_acme_widgets", "acme/gadgets": "ghs_acme_gadgets",
		"sjawhar": "ghs_sjawhar_read",
	} {
		hosts, err := os.ReadFile(filepath.Join(Dir(root, name), HostsFile))
		if err != nil {
			t.Fatal(err)
		}
		if got, err := TokenFromHosts(hosts); err != nil || got != token {
			t.Fatalf("%s's hosts.yml token = %q, %v, want %q", name, got, err, token)
		}
	}

	// gadgets leaves the mapping: its directory goes, widgets' stays, and the owner's own two
	// files are untouched.
	delete(bundle.Dirs, "acme/gadgets")
	if _, err := WriteBundle(root, bundle); err != nil {
		t.Fatalf("second WriteBundle: %v", err)
	}
	if _, err := os.Stat(Dir(root, "acme/gadgets")); !os.IsNotExist(err) {
		t.Fatalf("the directory of a repository the bundle no longer holds survived: %v", err)
	}
	entries, err := os.ReadDir(Dir(root, "acme"))
	if err != nil {
		t.Fatal(err)
	}
	names := make([]string, 0, len(entries))
	for _, entry := range entries {
		names = append(names, entry.Name())
	}
	slices.Sort(names)
	if want := []string{ConfigFile, HostsFile, "widgets"}; !slices.Equal(names, want) {
		t.Fatalf("acme's directory holds %q, want %q", names, want)
	}
}

// IsDirName takes the two shapes a bundle directory has and nothing else; IsOwner stays the
// stricter rule for the callers that mean an owner, the bundle's Default among them. A repository
// named like one of gh's own two files is no directory either: the owner's hosts.yml and
// config.yml sit where that directory would be made, and WriteBundle would fail every directory
// of the bundle over the one repository (appauth's bundleDirs leaves it out instead).
func TestIsDirNameTakesAnOwnerOrOneOfItsRepositories(t *testing.T) {
	for name, want := range map[string]bool{
		"acme": true, "acme-labs": true, "acme/widgets": true, "acme/gadgets.io": true, "acme/x_1-2": true,
		"acme/hosts.yaml": true, "acme/my-hosts.yml": true,
		"": false, "Acme": false, "acme_labs": false, "-acme": false, "acme/": false, "/widgets": false,
		"acme/Widgets": false, "acme/.": false, "acme/..": false, "acme/a/b": false, "../x": false,
		"acme/" + HostsFile: false, "acme/" + ConfigFile: false,
	} {
		if got := IsDirName(name); got != want {
			t.Errorf("IsDirName(%q) = %t, want %t", name, got, want)
		}
	}
	for name, want := range map[string]bool{"acme": true, "acme/widgets": false, "Acme": false} {
		if got := IsOwner(name); got != want {
			t.Errorf("IsOwner(%q) = %t, want %t", name, got, want)
		}
	}
}

// Validate refuses a bundle whose default is missing, not lowercased, absent from its
// directories or naming a repository rather than an owner — GH_CONFIG_DIR always names an
// owner's directory — and one with a directory that is neither a lowercased GitHub login nor a
// lowercased `<owner>/<repo>` under one, since a directory name is a path under the bundle's
// root; WriteBundle writes nothing for a bundle Validate refuses.
func TestValidateRefusesABundleThatCannotNameItsDirectories(t *testing.T) {
	good := twoOwnerBundle("ghs_acme")
	if err := good.Validate(); err != nil {
		t.Fatalf("Validate(two-owner bundle) = %v, want nil", err)
	}
	for _, tc := range []struct {
		name   string
		bundle func() Bundle
		want   string
	}{
		{"no default", func() Bundle { b := twoOwnerBundle("t"); b.Default = ""; return b }, "names no default owner"},
		{"default not lowercased", func() Bundle { b := twoOwnerBundle("t"); b.Default = "Acme"; return b }, `"Acme"`},
		{"unknown default", func() Bundle { b := twoOwnerBundle("t"); b.Default = "other"; return b }, `default owner "other"`},
		{"owner that walks out of root", func() Bundle {
			b := twoOwnerBundle("t")
			b.Dirs["../x"] = b.Dirs["sjawhar"]
			return b
		}, `"../x"`},
		{"owner not lowercased", func() Bundle {
			b := twoOwnerBundle("t")
			b.Dirs["Sjawhar"] = b.Dirs["sjawhar"]
			return b
		}, `"Sjawhar"`},
		{"a default naming a repository rather than an owner", func() Bundle {
			b := twoOwnerBundle("t")
			b.Dirs["acme/widgets"] = b.Dirs["sjawhar"]
			b.Default = "acme/widgets"
			return b
		}, `"acme/widgets"`},
		{"a repository name that walks out of its owner", func() Bundle {
			b := twoOwnerBundle("t")
			b.Dirs["acme/.."] = b.Dirs["sjawhar"]
			return b
		}, `"acme/.."`},
		{"a repository name not lowercased", func() Bundle {
			b := twoOwnerBundle("t")
			b.Dirs["acme/Widgets"] = b.Dirs["sjawhar"]
			return b
		}, `"acme/Widgets"`},
		{"a directory nested two deep", func() Bundle {
			b := twoOwnerBundle("t")
			b.Dirs["acme/widgets/deep"] = b.Dirs["sjawhar"]
			return b
		}, `"acme/widgets/deep"`},
	} {
		t.Run(tc.name, func(t *testing.T) {
			bundle := tc.bundle()
			err := bundle.Validate()
			if err == nil || !strings.Contains(err.Error(), tc.want) {
				t.Fatalf("Validate() = %v, want an error containing %q", err, tc.want)
			}
			root := filepath.Join(t.TempDir(), "gh")
			if _, err := WriteBundle(root, bundle); err == nil {
				t.Fatal("WriteBundle wrote a bundle Validate refuses")
			}
			if _, err := os.Stat(root); !os.IsNotExist(err) {
				t.Fatalf("WriteBundle of a refused bundle left %s: %v", root, err)
			}
		})
	}
}

// DirNames is the bundle's directories, sorted, which the runtimes' log lines name. An owner
// sorts before its own repositories' directories, each of which begins with the owner's name.
func TestDirNamesIsSorted(t *testing.T) {
	if got, want := twoOwnerBundle("t").DirNames(), []string{"acme", "sjawhar"}; !slices.Equal(got, want) {
		t.Fatalf("DirNames() = %q, want %q", got, want)
	}
	nested := twoOwnerBundle("t")
	nested.Dirs["acme/widgets"] = nested.Dirs["sjawhar"]
	if got, want := nested.DirNames(), []string{"acme", "acme/widgets", "sjawhar"}; !slices.Equal(got, want) {
		t.Fatalf("DirNames() = %q, want %q", got, want)
	}
}

// The real gh's git-credential helper answers git from one owner's directory of a written bundle
// with that owner's token, the same check TestGhReadsTheRenderedFilesWithoutWriting makes of one
// directory.
func TestGhAnswersGitFromAnOwnersDirectory(t *testing.T) {
	gh := ghBinary(t)
	root := filepath.Join(t.TempDir(), "gh")
	if _, err := WriteBundle(root, twoOwnerBundle("ghs_acme_write")); err != nil {
		t.Fatal(err)
	}
	home := t.TempDir()
	for owner, token := range map[string]string{"acme": "ghs_acme_write", "sjawhar": "ghs_sjawhar_read"} {
		cmd := exec.Command(gh, "auth", "git-credential", "get")
		cmd.Env = []string{
			"PATH=" + os.Getenv("PATH"),
			"HOME=" + home,
			"XDG_CONFIG_HOME=" + filepath.Join(home, ".config"),
			"XDG_STATE_HOME=" + filepath.Join(home, ".local", "state"),
			"XDG_DATA_HOME=" + filepath.Join(home, ".local", "share"),
			"XDG_CACHE_HOME=" + filepath.Join(home, ".cache"),
			"GH_CONFIG_DIR=" + Dir(root, owner),
			"GH_TOKEN=",
			"GITHUB_TOKEN=",
			"GH_HOST=",
			"GH_NO_UPDATE_NOTIFIER=1",
		}
		cmd.Stdin = strings.NewReader("protocol=https\nhost=github.com\n\n")
		out, err := cmd.Output()
		if err != nil {
			t.Fatalf("gh auth git-credential get for %s: %v", owner, err)
		}
		if !strings.Contains(string(out), "password="+token+"\n") {
			t.Fatalf("gh auth git-credential get for %s answered\n%s\nwant password=%s", owner, out, token)
		}
	}
}
