package main

import (
	"bytes"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
)

// OwnerOf reads the owner of the repository a gh command acts on, lowercased, from the places a gh
// command names one, in order: GH_REPO, -R/--repo, a `gh api` path under repos/, a github.com URL,
// and `gh repo <sub> <OWNER/REPO>`; a command that names none is "".
func TestOwnerOfReadsTheRepositoryAGhCommandNames(t *testing.T) {
	for _, tc := range []struct {
		argv []string
		env  map[string]string
		want string
	}{
		{argv: []string{"pr", "list", "-R", "acme/widgets"}, want: "acme"},
		{argv: []string{"pr", "list", "-Racme/widgets"}, want: "acme"},
		{argv: []string{"pr", "list", "-R=acme/widgets"}, want: "acme"},
		{argv: []string{"pr", "list", "--repo", "acme/widgets"}, want: "acme"},
		{argv: []string{"pr", "view", "3", "--repo=github.com/Acme/W"}, want: "acme"},
		{argv: []string{"pr", "view", "--repo", "https://github.com/Acme/widgets"}, want: "acme"},
		{argv: []string{"api", "repos/acme/widgets/pulls"}, want: "acme"},
		{argv: []string{"api", "-X", "GET", "/repos/Acme/widgets/branches/main"}, want: "acme"},
		{argv: []string{"api", "repos/sjawhar/legion-smoke", "--jq", ".full_name"}, want: "sjawhar"},
		{argv: []string{"pr", "view", "https://github.com/acme/widgets/pull/3", "--json", "state"}, want: "acme"},
		{argv: []string{"api", "graphql", "-f", "query={viewer{login}}"}, want: ""},
		{argv: []string{"repo", "view", "Acme/widgets"}, want: "acme"},
		{argv: []string{"repo", "clone", "acme/widgets", "--", "--depth=1"}, want: "acme"},
		{argv: []string{"repo", "view"}, want: ""},
		{argv: []string{"pr", "create", "--fill"}, want: ""},
		{argv: []string{"auth", "status"}, want: ""},
		{argv: nil, want: ""},
		{argv: []string{"pr", "list"}, env: map[string]string{"GH_REPO": "acme/w"}, want: "acme"},
		{argv: []string{"pr", "list"}, env: map[string]string{"GH_REPO": "github.com/Acme/w"}, want: "acme"},
		{argv: []string{"pr", "list", "-R", "sjawhar/legion"}, env: map[string]string{"GH_REPO": "acme/w"}, want: "acme"},
		{argv: []string{"api", "repos/acme/widgets", "-R", "sjawhar/legion"}, want: "sjawhar"},
	} {
		if got := OwnerOf(tc.argv, tc.env); got != tc.want {
			t.Errorf("OwnerOf(%q, %v) = %q, want %q", tc.argv, tc.env, got, tc.want)
		}
	}
}

// legionGh runs this test binary as `legion gh args…` in its own process, since a run execs the
// real gh in place of the process, with env (and nothing of this process's environment but PATH).
func legionGh(t *testing.T, env map[string]string, args ...string) (code int, stdout, stderr string) {
	t.Helper()
	cmd := exec.Command(os.Args[0], append([]string{"gh"}, args...)...)
	cmd.Env = []string{testMainEnv + "=1", "PATH=" + os.Getenv("PATH")}
	for name, value := range env {
		cmd.Env = append(cmd.Env, name+"="+value)
	}
	var out, errb bytes.Buffer
	cmd.Stdout, cmd.Stderr = &out, &errb
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
// GH_CONFIG_DIR pointed at the directory of the owner the command names when the bundle holds that
// owner, and at the issue's owner's (the GH_CONFIG_DIR it was given) otherwise. Each of the three
// variables it needs is required, and an unset one is named with exit 2; --help with no real gh to
// hand it to is legion's usage.
func TestLegionGhExecsTheRealGhWithTheOwnersDirectory(t *testing.T) {
	root, defaultDir := gitCredentialBundle(t)
	stub := filepath.Join(t.TempDir(), "gh")
	if err := os.WriteFile(stub, []byte("#!/bin/sh\nprintf '%s|%s|%s\\n' \"$0\" \"$GH_CONFIG_DIR\" \"$*\"\n"), 0o700); err != nil {
		t.Fatal(err)
	}
	env := map[string]string{"LEGION_GH_PATH": stub, "GH_CONFIG_DIR": defaultDir, "LEGION_GH_OWNERS_DIR": root}
	for _, tc := range []struct {
		args []string
		dir  string
	}{
		{[]string{"api", "repos/sjawhar/legion-smoke", "--jq", ".full_name"}, filepath.Join(root, "sjawhar")},
		{[]string{"pr", "create", "-R", "acme/widgets", "--fill"}, filepath.Join(root, "acme")},
		{[]string{"pr", "view", "https://github.com/other/thing/pull/1"}, defaultDir},
		{[]string{"pr", "list"}, defaultDir},
		{[]string{"--help"}, defaultDir},
	} {
		code, stdout, stderr := legionGh(t, env, tc.args...)
		want := stub + "|" + tc.dir + "|" + strings.Join(tc.args, " ") + "\n"
		if code != 0 || stdout != want {
			t.Errorf("legion gh %q = exit %d, stdout %q, stderr %q; want exit 0 and %q", tc.args, code, stdout, stderr, want)
		}
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
