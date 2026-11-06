package main

import (
	"bytes"
	"maps"
	"os"
	"os/exec"
	"path/filepath"
	"slices"
	"strings"
	"testing"

	"github.com/sjawhar/legion/daemon/internal/runtime/workerbin"
)

// ghHelpDir holds the real gh's help of each command `legion gh` and internal/ghroute's tests
// read, saved from gh 2.98.0 (testdata/gh-help/VERSION), one file per path, its words joined by
// "_", and gh's own help (`gh --help`) as gh.txt.
const ghHelpDir = "testdata/gh-help"

// legionGh runs this test binary as `legion gh args…` in its own process, since a run execs the
// real gh in place of the process, with env (and nothing of this process's environment but PATH),
// in a working directory that is no git repository.
func legionGh(t *testing.T, env map[string]string, args ...string) (code int, stdout, stderr string) {
	t.Helper()
	return legionGhIn(t, t.TempDir(), env, "", args...)
}

// ghWrapperDir is a directory holding a pane's `gh` wrapper, a script running this test binary as
// `legion gh`, for the head of a test's PATH.
func ghWrapperDir(t *testing.T) string {
	t.Helper()
	dir := t.TempDir()
	if err := os.WriteFile(filepath.Join(dir, "gh"), []byte("#!/bin/sh\nexec '"+os.Args[0]+"' gh \"$@\"\n"), 0o700); err != nil {
		t.Fatal(err)
	}
	return dir
}

// legionGhIn runs `legion gh args…` as legionGh does, in the working directory dir with stdin,
// its PATH this process's behind a `gh` wrapper (ghWrapperDir) unless env names one.
func legionGhIn(t *testing.T, dir string, env map[string]string, stdin string, args ...string) (code int, stdout, stderr string) {
	t.Helper()
	cmd := exec.Command(os.Args[0], append([]string{"gh"}, args...)...)
	cmd.Dir = dir
	cmd.Env = []string{testMainEnv + "=1", "PATH=" + ghWrapperDir(t) + ":" + os.Getenv("PATH"), "GIT_CONFIG_NOSYSTEM=1", "GIT_CONFIG_GLOBAL=" + os.DevNull}
	for name, value := range env {
		cmd.Env = append(cmd.Env, name+"="+value)
	}
	var out, errb bytes.Buffer
	cmd.Stdin, cmd.Stdout, cmd.Stderr = strings.NewReader(stdin), &out, &errb
	err := cmd.Run()
	if exit, ok := err.(*exec.ExitError); ok {
		return exit.ExitCode(), out.String(), errb.String()
	}
	if err != nil {
		t.Fatalf("legion gh %v: %v", args, err)
	}
	return 0, out.String(), errb.String()
}

// `legion gh` execs the real gh (LEGION_GH_PATH) as `gh`, with its arguments untouched and
// GH_CONFIG_DIR pointed at the directory of the owner the command names, and at the issue's
// owner's (the GH_CONFIG_DIR it was given) when it names none, reading --help=false as no help. It
// reads each command from the real gh's own help. The real gh runs with GH_PATH naming the `gh`
// wrapper first on PATH, so the git gh runs asks `legion gh auth git-credential`, which answers by
// the path's owner as `legion git-credential` does; with useHttpPath on for github.com, and gh's
// update check and prompts off. A command naming an owner the bundle holds no directory for is
// refused, exit 2, naming the owner, and so are one it cannot read, a gh alias, and any command
// when PATH's first gh is the real gh; the real gh never runs. In a clone of an owner's
// repository, a command naming none runs as that owner's. Each of the three variables it needs is
// required, and an unset one is named with exit 2; --help with no real gh to hand it to is
// legion's usage.
func TestLegionGhExecsTheRealGhWithTheOwnersDirectory(t *testing.T) {
	root, defaultDir := gitCredentialBundle(t)
	help, err := filepath.Abs(ghHelpDir)
	if err != nil {
		t.Fatal(err)
	}
	// The stub answers `[<command>] --help` with the saved help of that command, gh's own for none,
	// as the real gh would, and every other run with its argv and GH_CONFIG_DIR, then the rest of
	// the environment `legion gh` gives it.
	stub := filepath.Join(t.TempDir(), "gh")
	script := "#!/bin/sh\nfor last; do :; done\n" +
		"if [ \"$last\" = --help ]; then\n" +
		"  path=$(printf '%s ' \"$@\" | sed -e 's/--help $//' -e 's/ $//' -e 's/ /_/g')\n" +
		"  exec cat '" + help + "'/\"${path:-gh}\".txt\nfi\n" +
		"printf '%s|%s|%s\\n' \"$0\" \"$GH_CONFIG_DIR\" \"$*\"\n" +
		"printf 'env %s|%s|%s=%s|%s|%s\\n' \"$GH_PATH\" \"$GIT_CONFIG_COUNT\" \"$GIT_CONFIG_KEY_0\" \"$GIT_CONFIG_VALUE_0\" \"$GH_NO_UPDATE_NOTIFIER\" \"$GH_PROMPT_DISABLED\"\n"
	if err := os.WriteFile(stub, []byte(script), 0o700); err != nil {
		t.Fatal(err)
	}
	wrappers := ghWrapperDir(t)
	env := map[string]string{"LEGION_GH_PATH": stub, "GH_CONFIG_DIR": defaultDir, "LEGION_GH_OWNERS_DIR": root, "PATH": wrappers + ":" + os.Getenv("PATH")}
	wantEnv := "env " + filepath.Join(wrappers, "gh") + "|1|credential.https://github.com.useHttpPath=true|1|1\n"
	for _, tc := range []struct {
		args []string
		dir  string
	}{
		{[]string{"api", "repos/sjawhar/legion-smoke", "--jq", ".full_name"}, filepath.Join(root, "sjawhar")},
		{[]string{"pr", "create", "-R", "acme/widgets", "--fill"}, filepath.Join(root, "acme")},
		{[]string{"pr", "list"}, defaultDir},
		{[]string{"pr", "view", "1", "-R", "acme/widgets", "--help=false"}, filepath.Join(root, "acme")},
		{[]string{"api", "repos/sjawhar/legion-smoke", "--help=false", "-q", "."}, filepath.Join(root, "sjawhar")},
	} {
		code, stdout, stderr := legionGh(t, env, tc.args...)
		want := stub + "|" + tc.dir + "|" + strings.Join(tc.args, " ") + "\n" + wantEnv
		if code != 0 || stdout != want {
			t.Errorf("legion gh %q = exit %d, stdout %q, stderr %q; want exit 0 and %q", tc.args, code, stdout, stderr, want)
		}
	}

	for _, tc := range []struct {
		args []string
		want string
	}{
		{[]string{"pr", "view", "https://github.com/other/thing/pull/1"}, `"other" is not an owner of this project`},
		{[]string{"api", "repos/Other/thing"}, `"other" is not an owner of this project`},
		{[]string{"pr", "view", "1", "--bogus"}, `"--bogus" is no option`},
		{[]string{"issue", "view", "1"}, "cannot read `gh issue view`"},
		{[]string{"co", "1"}, `"co" is a gh alias`},
	} {
		code, stdout, stderr := legionGh(t, env, tc.args...)
		if code != 2 || stdout != "" || !strings.Contains(stderr, tc.want) {
			t.Errorf("legion gh %q = exit %d, stdout %q, stderr %q; want exit 2 containing %q and no gh run", tc.args, code, stdout, stderr, tc.want)
		}
	}

	// gh's own git asks the wrapper, which answers with the token of the owner in git's path.
	for path, token := range map[string]string{"sjawhar/legion-smoke.git": "ghs_sjawhar_read", "acme/widgets.git": "ghs_acme_write"} {
		code, stdout, stderr := legionGhIn(t, t.TempDir(), env, "protocol=https\nhost=github.com\npath="+path+"\n\n", "auth", "git-credential", "get")
		if want := "username=x-access-token\npassword=" + token + "\n"; code != 0 || stdout != want {
			t.Errorf("legion gh auth git-credential get for %s = exit %d, stdout %q, stderr %q; want %q", path, code, stdout, stderr, want)
		}
	}
	if code, stdout, _ := legionGhIn(t, t.TempDir(), env, "protocol=https\nhost=github.com\n\n", "auth", "git-credential", "get"); code == 0 || stdout != "" {
		t.Errorf("legion gh auth git-credential get with no path = exit %d, stdout %q; want no answer", code, stdout)
	}
	// With the real gh first on PATH, gh's own git would answer with GH_CONFIG_DIR's token.
	unwrapped := map[string]string{"PATH": filepath.Dir(stub) + ":" + os.Getenv("PATH")}
	maps.Copy(unwrapped, map[string]string{"LEGION_GH_PATH": stub, "GH_CONFIG_DIR": defaultDir, "LEGION_GH_OWNERS_DIR": root})
	if code, stdout, stderr := legionGh(t, unwrapped, "pr", "list"); code != 2 || stdout != "" || !strings.Contains(stderr, "is not the wrapper that runs `legion gh`") {
		t.Errorf("legion gh with the real gh first on PATH = exit %d, stdout %q, stderr %q; want exit 2 naming it", code, stdout, stderr)
	}
	// Nor may another wrapper of the machine's own lead PATH: gh would make it its git helper.
	shims := t.TempDir()
	if err := os.WriteFile(filepath.Join(shims, "gh"), []byte("#!/bin/sh\nGH_TOKEN=$(cat ~/.token) exec '"+stub+"' \"$@\"\n"), 0o700); err != nil {
		t.Fatal(err)
	}
	shimmed := map[string]string{"PATH": shims + ":" + wrappers + ":" + os.Getenv("PATH")}
	maps.Copy(shimmed, map[string]string{"LEGION_GH_PATH": stub, "GH_CONFIG_DIR": defaultDir, "LEGION_GH_OWNERS_DIR": root})
	if code, stdout, stderr := legionGh(t, shimmed, "pr", "list"); code != 2 || stdout != "" || !strings.Contains(stderr, "is not the wrapper that runs `legion gh`") {
		t.Errorf("legion gh with another gh wrapper first on PATH = exit %d, stdout %q, stderr %q; want exit 2 naming it", code, stdout, stderr)
	}

	// In a clone of sjawhar's repository, a command naming none acts on sjawhar's repository.
	clone := t.TempDir()
	for _, args := range [][]string{{"init", "-q"}, {"remote", "add", "origin", "https://github.com/sjawhar/legion-smoke"}} {
		git := exec.Command("git", args...)
		git.Dir, git.Env = clone, []string{"PATH=" + os.Getenv("PATH"), "GIT_CONFIG_NOSYSTEM=1", "GIT_CONFIG_GLOBAL=" + os.DevNull}
		if out, err := git.CombinedOutput(); err != nil {
			t.Fatalf("git %v: %v: %s", args, err, out)
		}
	}
	for _, args := range [][]string{{"pr", "list"}, {"pr", "list", "-R", "sjawhar/legion-smoke"}} {
		if code, stdout, stderr := legionGhIn(t, clone, env, "", args...); code != 0 || !strings.HasPrefix(stdout, stub+"|"+filepath.Join(root, "sjawhar")+"|") {
			t.Errorf("legion gh %q in that clone = exit %d, stdout %q, stderr %q; want sjawhar's directory", args, code, stdout, stderr)
		}
	}
	if code, stdout, stderr := legionGhIn(t, clone, env, "", "pr", "list", "-R", "acme/widgets"); code != 0 || !strings.HasPrefix(stdout, stub+"|"+filepath.Join(root, "acme")+"|") {
		t.Errorf("legion gh pr list -R acme/widgets in that clone = exit %d, stdout %q, stderr %q; want acme's directory", code, stdout, stderr)
	}

	for _, missing := range []string{"LEGION_GH_PATH", "GH_CONFIG_DIR", "LEGION_GH_OWNERS_DIR"} {
		partial := map[string]string{}
		for name, value := range env {
			if name != missing {
				partial[name] = value
			}
		}
		code, stdout, stderr := legionGh(t, partial, "pr", "list")
		if code != 2 || stdout != "" || !strings.Contains(stderr, missing+" is unset") {
			t.Errorf("legion gh without %s = exit %d, stdout %q, stderr %q; want exit 2 naming it", missing, code, stdout, stderr)
		}
	}

	code, _, stderr := legionGh(t, map[string]string{}, "--help")
	if code != 0 || !usageOf(stderr, "gh") {
		t.Errorf("legion gh --help with no LEGION_GH_PATH = exit %d, stderr %q; want exit 0 and its usage", code, stderr)
	}
}

// The `gh` each runtime puts first on PATH, the image's packages/daemon/docker/gh and the script a
// tmux pane's launcher directory holds (workerbin), is one `legion gh` accepts as its wrapper
// (ghWrapperScript), so the git gh runs names it as its helper; a machine's own wrapper is not.
func TestTheRuntimesGhWrappersAreOnesLegionGhAccepts(t *testing.T) {
	image, err := os.ReadFile(filepath.Join("..", "..", "docker", "gh"))
	if err != nil {
		t.Fatal(err)
	}
	root := t.TempDir()
	if err := workerbin.Install(root, "/opt/legion state/legion"); err != nil {
		t.Fatal(err)
	}
	pane, err := os.ReadFile(filepath.Join(workerbin.LauncherDir(root), "gh"))
	if err != nil {
		t.Fatal(err)
	}
	for name, script := range map[string][]byte{"the image's": image, "a tmux pane's": pane} {
		if !ghWrapperScript.Match(script) {
			t.Errorf("%s gh wrapper %q is not one legion gh accepts", name, script)
		}
	}
	for _, script := range []string{"#!/bin/sh\nexec \"/opt/legion state/legion\" gh \"$@\"\n", "#!/bin/sh\nexec '/opt/legion'\\''s/legion' gh \"$@\"\n", "#!/bin/sh\nexec /opt/legion/bin/legion gh \"$@\"\n"} {
		if !ghWrapperScript.MatchString(script) {
			t.Errorf("legion gh refuses %q as its wrapper", script)
		}
	}
	for _, script := range []string{
		"#!/bin/sh\nGH_TOKEN=x exec /usr/bin/gh \"$@\"\n", "#!/bin/sh\nexec /usr/bin/gh \"$@\"\n", "\x7fELF",
		"#!/bin/sh\nexec \"$HOME/legion\" gh \"$@\"\n", "#!/bin/sh\nexec $(cat /x) gh \"$@\"\n", "#!/bin/sh\nexec `cat /x` gh \"$@\"\n",
		// Two commands, a redirect, a tilde and a glob: shell, not one literal path.
		"#!/bin/sh\nexec /x|/bin/sh gh \"$@\"\n", "#!/bin/sh\nexec /x&/bin/sh gh \"$@\"\n", "#!/bin/sh\nexec /x;/bin/sh gh \"$@\"\n",
		"#!/bin/sh\nexec /x>/dev/null gh \"$@\"\n", "#!/bin/sh\nexec ~/legion gh \"$@\"\n", "#!/bin/sh\nexec /opt/*/legion gh \"$@\"\n",
		// An option to the shell's exec, and a path relative to wherever gh runs.
		"#!/bin/sh\nexec -a gh \"$@\"\n", "#!/bin/sh\nexec '-a' gh \"$@\"\n", "#!/bin/sh\nexec \"-a\" gh \"$@\"\n",
		"#!/bin/sh\nexec legion gh \"$@\"\n", "#!/bin/sh\nexec 'bin/legion' gh \"$@\"\n",
	} {
		if ghWrapperScript.MatchString(script) {
			t.Errorf("legion gh accepts %q as its wrapper", script)
		}
	}

	// The whole script is read, to the bound: a wrapper at the bound passes, and one a byte past it
	// is refused by the bound alone, its first ghWrapperMaxBytes+1 bytes a whole wrapper, whatever
	// follows; the pattern's anchors refuse anything that goes on after a wrapper within it.
	wrapperOf := func(size int) string {
		head, tail := "#!/bin/sh\nexec '/", "' gh \"$@\"\n"
		return head + strings.Repeat("a", size-len(head)-len(tail)) + tail
	}
	atBound, pastBound := wrapperOf(ghWrapperMaxBytes), wrapperOf(ghWrapperMaxBytes+1)
	if !ghWrapperScript.MatchString(pastBound) {
		t.Fatalf("the pattern refuses a wrapper of %d bytes, so the bound is not what refuses it", len(pastBound))
	}
	for name, tc := range map[string]struct {
		script string
		want   bool
	}{
		"a wrapper at the bound":              {atBound, true},
		"a wrapper a byte past it":            {pastBound, false},
		"a wrapper a byte past it, then more": {pastBound + "rm -rf ~\n", false},
		"a wrapper at the bound, then more":   {atBound + "rm -rf ~\n", false},
		"a short wrapper, then more":          {"#!/bin/sh\nexec '/opt/legion/bin/legion' gh \"$@\"\nrm -rf ~\n", false},
		"a wrapper of a short path":           {"#!/bin/sh\nexec '/opt/legion/bin/legion' gh \"$@\"\n", true},
	} {
		if got, err := isGhWrapper(strings.NewReader(tc.script)); err != nil || got != tc.want {
			t.Errorf("isGhWrapper(%s, %d bytes) = %t, %v; want %t", name, len(tc.script), got, err, tc.want)
		}
	}
}

// The git gh runs reads credential.useHttpPath from GIT_CONFIG_COUNT's entries, added after any
// the environment already holds and in place of a stale entry at the next index; a count that is
// no number is refused.
func TestWithGitHubUseHTTPPathAddsToGitsConfiguration(t *testing.T) {
	got, err := withGitHubUseHTTPPath([]string{"A=1", "GIT_CONFIG_COUNT=1", "GIT_CONFIG_KEY_0=core.x", "GIT_CONFIG_VALUE_0=y", "GIT_CONFIG_KEY_1=stale", "GIT_CONFIG_VALUE_1=stale"})
	want := []string{"A=1", "GIT_CONFIG_KEY_0=core.x", "GIT_CONFIG_VALUE_0=y", "GIT_CONFIG_KEY_1=credential.https://github.com.useHttpPath", "GIT_CONFIG_VALUE_1=true", "GIT_CONFIG_COUNT=2"}
	if err != nil || !slices.Equal(got, want) {
		t.Errorf("withGitHubUseHTTPPath = %q, %v; want %q", got, err, want)
	}
	if got, err := withGitHubUseHTTPPath([]string{"A=1"}); err != nil || !slices.Equal(got, []string{"A=1", "GIT_CONFIG_KEY_0=credential.https://github.com.useHttpPath", "GIT_CONFIG_VALUE_0=true", "GIT_CONFIG_COUNT=1"}) {
		t.Errorf("withGitHubUseHTTPPath with no count = %q, %v", got, err)
	}
	if _, err := withGitHubUseHTTPPath([]string{"GIT_CONFIG_COUNT=x"}); err == nil || !strings.Contains(err.Error(), "no count") {
		t.Errorf("withGitHubUseHTTPPath with GIT_CONFIG_COUNT=x = %v, want a refusal", err)
	}
}
