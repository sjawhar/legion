package ghconfig

import (
	"bytes"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

// Hosts renders exactly the file gh >= 2.40 writes for one account, and TokenFromHosts reads the
// token back out of it: the daemon writes one and the `legion` CLI reads the other, so the shape
// is a contract between them.
func TestHostsRoundTripsThroughTokenFromHosts(t *testing.T) {
	const token = "ghs_roundtrip_token"
	want := "github.com:\n" +
		"    oauth_token: ghs_roundtrip_token\n" +
		"    user: x-access-token\n" +
		"    git_protocol: https\n" +
		"    users:\n" +
		"        x-access-token:\n" +
		"            oauth_token: ghs_roundtrip_token\n"
	hosts := Hosts(token)
	if hosts != want {
		t.Fatalf("Hosts(%q) =\n%s\nwant\n%s", token, hosts, want)
	}
	got, err := TokenFromHosts([]byte(hosts))
	if err != nil {
		t.Fatalf("TokenFromHosts(Hosts(token)): %v", err)
	}
	if got != token {
		t.Fatalf("TokenFromHosts(Hosts(token)) = %q, want %q", got, token)
	}
}

// A hosts.yml gh wrote before 2.40 has no `users` block; the token still sits at
// github.com.oauth_token, and TokenFromHosts reads it from there.
func TestTokenFromHostsReadsGhsPre240Shape(t *testing.T) {
	legacy := "github.com:\n" +
		"    oauth_token: ghs_legacy_token\n" +
		"    user: x-access-token\n" +
		"    git_protocol: https\n"
	got, err := TokenFromHosts([]byte(legacy))
	if err != nil {
		t.Fatalf("TokenFromHosts(pre-2.40 hosts.yml): %v", err)
	}
	if got != "ghs_legacy_token" {
		t.Fatalf("TokenFromHosts(pre-2.40 hosts.yml) = %q, want ghs_legacy_token", got)
	}
}

// A hosts.yml with no token for github.com is refused naming the key it lacks, whether the file
// has a github.com entry without one or nothing at all.
func TestTokenFromHostsNamesTheMissingKey(t *testing.T) {
	for name, data := range map[string]string{
		"no oauth_token":    "github.com:\n    user: x-access-token\n    git_protocol: https\n",
		"blank oauth_token": "github.com:\n    oauth_token: \"\"\n",
		"empty file":        "",
	} {
		t.Run(name, func(t *testing.T) {
			got, err := TokenFromHosts([]byte(data))
			if err == nil || !strings.Contains(err.Error(), "oauth_token") {
				t.Fatalf("TokenFromHosts(%q) = %q, %v; want an error naming oauth_token", data, got, err)
			}
		})
	}
	if _, err := TokenFromHosts([]byte("github.com: [not a mapping\n")); err == nil {
		t.Fatal("TokenFromHosts(malformed YAML) = nil error, want a parse error")
	}
}

// Render is the two files plus what the daemon's log line needs: the App label and the expiry,
// carried through untouched.
func TestRenderCarriesTheLeaseThrough(t *testing.T) {
	expires := time.Date(2026, 10, 8, 5, 0, 0, 0, time.UTC)
	rendered := Render("ghs_rendered_token", "implement", expires)
	if rendered.Hosts != Hosts("ghs_rendered_token") {
		t.Errorf("Rendered.Hosts =\n%s\nwant Hosts(token)", rendered.Hosts)
	}
	if rendered.Config != Config {
		t.Errorf("Rendered.Config = %q, want %q", rendered.Config, Config)
	}
	if rendered.App != "implement" {
		t.Errorf("Rendered.App = %q, want implement", rendered.App)
	}
	if !rendered.ExpiresAt.Equal(expires) {
		t.Errorf("Rendered.ExpiresAt = %v, want %v", rendered.ExpiresAt, expires)
	}
}

// Write brings a role's GH_CONFIG_DIR to a render: a fresh directory is made and chmod'ed 0700 with
// both files written and changed reported true, a second call with the same render writes nothing
// and reports changed false, a different token rewrites hosts.yml alone and reports changed true,
// and a directory that already exists 0755 is chmod'ed to 0700 regardless of whether hosts.yml
// changes.
func TestWriteBringsTheDirectoryToTheRender(t *testing.T) {
	assertMode := func(t *testing.T, dir string) {
		t.Helper()
		info, err := os.Stat(dir)
		if err != nil || info.Mode().Perm() != 0o700 {
			t.Fatalf("Stat(%q) = %v, %v; want mode 0700", dir, info, err)
		}
	}
	assertNoStrayTemp := func(t *testing.T, dir string) {
		t.Helper()
		entries, err := os.ReadDir(dir)
		if err != nil {
			t.Fatal(err)
		}
		for _, entry := range entries {
			if strings.HasPrefix(entry.Name(), "."+HostsFile+"-") {
				t.Errorf("ReadDir(%q) left a stray temp file %q", dir, entry.Name())
			}
		}
	}

	dir := t.TempDir()
	rendered := Render("ghs_first_token", "implement", time.Time{})
	changed, err := Write(dir, rendered)
	if err != nil {
		t.Fatal(err)
	}
	if !changed {
		t.Error("Write on a fresh directory: changed = false, want true")
	}
	assertMode(t, dir)
	assertNoStrayTemp(t, dir)
	hostsPath, configPath := filepath.Join(dir, HostsFile), filepath.Join(dir, ConfigFile)
	hosts, err := os.ReadFile(hostsPath)
	if err != nil || string(hosts) != rendered.Hosts {
		t.Fatalf("ReadFile(hosts.yml) = %q, %v; want %q, nil", hosts, err, rendered.Hosts)
	}
	config, err := os.ReadFile(configPath)
	if err != nil || string(config) != rendered.Config {
		t.Fatalf("ReadFile(config.yml) = %q, %v; want %q, nil", config, err, rendered.Config)
	}

	changed, err = Write(dir, rendered)
	if err != nil {
		t.Fatal(err)
	}
	if changed {
		t.Error("Write with an unchanged render: changed = true, want false")
	}
	assertNoStrayTemp(t, dir)
	if hosts, err := os.ReadFile(hostsPath); err != nil || string(hosts) != rendered.Hosts {
		t.Fatalf("ReadFile(hosts.yml) after an unchanged Write = %q, %v; want %q, nil", hosts, err, rendered.Hosts)
	}

	other := Render("ghs_second_token", "implement", time.Time{})
	changed, err = Write(dir, other)
	if err != nil {
		t.Fatal(err)
	}
	if !changed {
		t.Error("Write with a different token: changed = false, want true")
	}
	if hosts, err := os.ReadFile(hostsPath); err != nil || string(hosts) != other.Hosts {
		t.Fatalf("ReadFile(hosts.yml) after a changed Write = %q, %v; want %q, nil", hosts, err, other.Hosts)
	}
	if config, err := os.ReadFile(configPath); err != nil || string(config) != rendered.Config {
		t.Fatalf("ReadFile(config.yml) after a hosts.yml-only change = %q, %v; want unchanged %q, nil", config, err, rendered.Config)
	}

	looseDir := filepath.Join(t.TempDir(), "loose")
	if err := os.Mkdir(looseDir, 0o755); err != nil {
		t.Fatal(err)
	}
	if _, err := Write(looseDir, rendered); err != nil {
		t.Fatal(err)
	}
	assertMode(t, looseDir)
}

// ghBinary is the real GitHub CLI to prove the rendered files against: the `gh` on PATH, unless
// it is a Legion worker pane's shim (`<state_dir>/worker-bin/gh`, which runs `legion gh`), in
// which case the CLI it stands in front of at /usr/local/bin/gh. A machine with neither skips.
func ghBinary(t *testing.T) string {
	t.Helper()
	path, err := exec.LookPath("gh")
	if err == nil && filepath.Base(filepath.Dir(path)) != "worker-bin" {
		return path
	}
	if path, err := exec.LookPath("/usr/local/bin/gh"); err == nil {
		return path
	}
	t.Skip("no real gh on this machine: the gh-backed test needs one to prove the rendered files against")
	return ""
}

// The real gh reads the rendered files as its own: `gh auth token` prints the token, its
// git-credential helper answers git with the installation-token username and the token, and a
// `store` from git changes nothing in a directory gh may not write — which is what a pod's
// read-only Secret volume is.
func TestGhReadsTheRenderedFilesWithoutWriting(t *testing.T) {
	gh := ghBinary(t)
	const token = "ghs_test_token"
	root := t.TempDir()
	dir := filepath.Join(root, "gh")
	if err := os.Mkdir(dir, 0o750); err != nil {
		t.Fatal(err)
	}
	// TempDir's cleanup removes the directory, which it cannot do while it is read-only; cleanups
	// run last-registered first, so this one runs before TempDir's.
	t.Cleanup(func() { _ = os.Chmod(dir, 0o750) })
	files := map[string]string{HostsFile: Hosts(token), ConfigFile: Config}
	for name, content := range files {
		if err := os.WriteFile(filepath.Join(dir, name), []byte(content), 0o440); err != nil {
			t.Fatal(err)
		}
	}
	if err := os.Chmod(dir, 0o550); err != nil {
		t.Fatal(err)
	}
	home := filepath.Join(root, "home")
	if err := os.Mkdir(home, 0o750); err != nil {
		t.Fatal(err)
	}
	// Nothing of the running environment reaches gh: a GH_TOKEN or GH_HOST would outrank the
	// files, and gh ignores an empty value exactly as an unset one.
	env := []string{
		"PATH=" + os.Getenv("PATH"),
		"HOME=" + home,
		"XDG_CONFIG_HOME=" + filepath.Join(home, ".config"),
		"XDG_STATE_HOME=" + filepath.Join(home, ".local", "state"),
		"XDG_DATA_HOME=" + filepath.Join(home, ".local", "share"),
		"XDG_CACHE_HOME=" + filepath.Join(home, ".cache"),
		"GH_CONFIG_DIR=" + dir,
		"GH_TOKEN=",
		"GITHUB_TOKEN=",
		"GH_HOST=",
		"GH_NO_UPDATE_NOTIFIER=1",
	}
	run := func(stdin string, args ...string) string {
		t.Helper()
		cmd := exec.Command(gh, args...)
		cmd.Env = env
		cmd.Stdin = strings.NewReader(stdin)
		var stderr bytes.Buffer
		cmd.Stderr = &stderr
		out, err := cmd.Output()
		if err != nil {
			t.Fatalf("gh %s: %v\nstderr:\n%s", strings.Join(args, " "), err, stderr.String())
		}
		return string(out)
	}

	if got := run("", "auth", "token"); got != token+"\n" {
		t.Fatalf("gh auth token = %q, want %q", got, token+"\n")
	}

	answer := run("protocol=https\nhost=github.com\n\n", "auth", "git-credential", "get")
	for _, want := range []string{"username=" + GitUser, "password=" + token} {
		if !strings.Contains(answer, want) {
			t.Errorf("gh auth git-credential get answered\n%s\nwant a %q line", answer, want)
		}
	}

	run("protocol=https\nhost=github.com\nusername="+GitUser+"\npassword="+token+"\n\n", "auth", "git-credential", "store")
	for name, want := range files {
		got, err := os.ReadFile(filepath.Join(dir, name))
		if err != nil {
			t.Fatal(err)
		}
		if string(got) != want {
			t.Errorf("%s after gh ran =\n%s\nwant it unchanged:\n%s", name, got, want)
		}
	}
	entries, err := os.ReadDir(dir)
	if err != nil {
		t.Fatal(err)
	}
	if len(entries) != len(files) {
		t.Errorf("GH_CONFIG_DIR holds %d entries after gh ran, want the %d written", len(entries), len(files))
	}
}
