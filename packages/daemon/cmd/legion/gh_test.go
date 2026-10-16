package main

import (
	"bytes"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"slices"
	"strings"
	"testing"
)

// ghHelpDir holds the real gh's help of each command the resolver's tests read, saved from gh
// 2.98.0 (testdata/gh-help/VERSION), one file per path, its words joined by "_".
const ghHelpDir = "testdata/gh-help"

// savedGhHelp answers gh's help of path from ghHelpDir as the real gh does: a path it has no file
// for prints the help of the nearest parent it has (gh's answer to a subcommand it does not know),
// and one with no such parent at all is an error (gh's `unknown command`, exit 1).
func savedGhHelp(path []string) (string, error) {
	for p := path; len(p) > 0; p = p[:len(p)-1] {
		data, err := os.ReadFile(filepath.Join(ghHelpDir, strings.Join(p, "_")+".txt"))
		if err == nil {
			return string(data), nil
		}
	}
	return "", fmt.Errorf("unknown command %q for \"gh\"", path[0])
}

// parseGhHelp reads gh's own help: a group by its COMMANDS section, a leaf's canonical path (an
// alias's command), every option's arity, pflag's optional value, a shorthand taken by another
// option than --help, and the arguments its USAGE names; it refuses a help it cannot read whole.
func TestParseGhHelpReadsRealGhHelp(t *testing.T) {
	read := func(path ...string) ghCommand {
		t.Helper()
		text, err := savedGhHelp(path)
		if err != nil {
			t.Fatal(err)
		}
		cmd, err := parseGhHelp(path, text)
		if err != nil {
			t.Fatalf("parseGhHelp(%q): %v", path, err)
		}
		return cmd
	}
	for _, group := range [][]string{{"pr"}, {"repo"}, {"repo", "deploy-key"}, {"search"}} {
		if cmd := read(group...); !cmd.group || !slices.Equal(cmd.path, group) {
			t.Errorf("gh %s's help reads as %+v, want the group %q", strings.Join(group, " "), cmd, group)
		}
	}
	view := read("pr", "view")
	if view.group || !view.flags["json"].value || view.flags["web"].value || !view.readsGHRepo() || view.shorts['R'] != "repo" || view.shorts['h'] != "help" {
		t.Errorf("gh pr view reads as %+v", view)
	}
	if kinds := []string{view.slots[0].kind()}; len(view.slots) != 1 || kinds[0] != "url" {
		t.Errorf("gh pr view's arguments read as %+v, want one URL-capable slot", view.slots)
	}
	if co := read("co"); !slices.Equal(co.path, []string{"pr", "checkout"}) || co.group {
		t.Errorf("gh co reads as %+v, want its command, pr checkout", co)
	}
	if add := read("repo", "deploy-key", "add"); !add.flags["title"].value || add.group {
		t.Errorf("gh repo deploy-key add reads as %+v, want --title taking a value", add)
	}
	if clone := read("repo", "clone"); !clone.passthrough || len(clone.slots) != 2 || clone.slots[0].kind() != "repository" {
		t.Errorf("gh repo clone reads as %+v, want a repository slot and a -- passthrough", clone)
	}
	if transfer := read("issue", "transfer"); len(transfer.slots) != 2 || transfer.slots[1].kind() != "second-repository" {
		t.Errorf("gh issue transfer reads as %+v, want its destination a second repository", transfer.slots)
	}
	if commit := read("browse").flags["commit"]; !commit.value || !commit.optional {
		t.Errorf("gh browse --commit reads as %+v, want a value taken only after =", commit)
	}
	if status := read("auth", "status"); status.shorts['h'] != "hostname" {
		t.Errorf("gh auth status's -h reads as %q, want --hostname", status.shorts['h'])
	}
	if search := read("search", "issues"); search.readsGHRepo() || search.slots[0].kind() != "query" {
		t.Errorf("gh search issues reads as %+v, want a query and no inherited -R", search)
	}

	for _, tc := range []struct {
		name, help, want string
	}{
		{"no usage", "FLAGS\n  --help   Show help for command\n", "names no usage"},
		{"no --help", "USAGE\n  gh pr view [flags]\n\nFLAGS\n  -w, --web   Open\n", "lists no --help"},
		{"an option line it cannot read", "USAGE\n  gh pr view [flags]\n\nFLAGS\n  --help   Show help\n  -w --web   Open\n", "cannot read the options"},
		{"a parent's help", "USAGE\n  gh pr <command> [flags]\n", "gh has no command"},
	} {
		if _, err := parseGhHelp([]string{"pr", "view"}, tc.help); err == nil || !strings.Contains(err.Error(), tc.want) {
			t.Errorf("parseGhHelp of %s = %v, want an error containing %q", tc.name, err, tc.want)
		}
	}
}

// ghTargetOf reads the owner a gh command targets from every selector gh's own help names, and
// fails closed: an argument it cannot place is refused, naming it, and so is a command naming two
// owners; only a command naming no repository at all is not named. GH_REPO names the owner only
// when nothing explicit does, and only for a leaf that reads it.
func TestGhTargetOfFailsClosed(t *testing.T) {
	acme := map[string]string{"GH_REPO": "acme/widgets"}
	for _, tc := range []struct {
		argv  []string
		env   map[string]string
		owner string // "" with no refusal: not named
		err   string // a refusal containing this
	}{
		// The re-review's reproductions.
		{argv: []string{"-R", "sjawhar/legion", "pr", "view", "1", "--json", "title"}, owner: "sjawhar"},
		{argv: []string{"--repo=sjawhar/legion", "pr", "view", "1", "--json", "title"}, owner: "sjawhar"},
		{argv: []string{"api", "--", "repos/sjawhar/legion"}, owner: "sjawhar"},
		{argv: []string{"pr", "view", "--json", "title", "--", "https://github.com/sjawhar/legion/pull/1"}, owner: "sjawhar"},
		{argv: []string{"repo", "deploy-key", "add", "key.pub", "-R", "sjawhar/legion", "--title", "-Racme/widgets"}, owner: "sjawhar"},
		{argv: []string{"pr", "--json", "number", "view", "https://github.com/sjawhar/legion/pull/1"}, err: `"--json" comes before the command's name`},
		// The first review's reproductions.
		{argv: []string{"api", "repos/sjawhar/legion"}, env: acme, owner: "sjawhar"},
		{argv: []string{"pr", "view", "1", "-R", "sjawhar/legion", "--json", "title"}, env: acme, owner: "sjawhar"},
		{argv: []string{"pr", "view", "https://github.com/sjawhar/legion/pull/1", "-R", "acme/widgets"}, err: "more than one owner, acme (--repo acme/widgets) and sjawhar"},
		{argv: []string{"repo", "view", "--json", "nameWithOwner", "sjawhar/legion"}, env: acme, owner: "sjawhar"},
		// A command naming no repository, and GH_REPO where the leaf reads it.
		{argv: []string{"pr", "list"}},
		{argv: []string{"pr", "list"}, env: acme, owner: "acme"},
		{argv: []string{"api", "graphql", "-f", "query=q", "-F", "owner=Sjawhar", "-F", "name=legion"}, env: acme, owner: "sjawhar"},
		{argv: []string{"api", "graphql", "-f", "query=q", "-F", "o=sjawhar", "-F", "id=PRRT_x"}},
		{argv: []string{"api", "user"}, env: acme},
		{argv: []string{"api", "graphql", "-f", "query={viewer{login}}"}, env: acme},
		{argv: []string{"pr"}},
		{argv: nil},
		{argv: []string{"--help"}},
		{argv: []string{"pr", "view", "1", "--help"}},
		{argv: []string{"pr", "view", "1", "-R", "acme/w", "-h"}},
		{argv: []string{"api", "graphql", "-f", "query={viewer{login}}"}},
		{argv: []string{"api", "graphql", "-F", "owner={owner}", "-f", "query=q"}, env: acme, owner: "acme"},
		{argv: []string{"api", "repos/{owner}/{repo}/pulls"}},
		{argv: []string{"api", "repos/{owner}/{repo}/pulls"}, env: acme, owner: "acme"},
		{argv: []string{"auth", "status"}, env: acme},
		{argv: []string{"auth", "status", "-h", "github.com"}},
		{argv: []string{"browse", "--commit"}},
		{argv: []string{"search", "issues", "is:open"}, env: acme},
		// Explicit selectors.
		{argv: []string{"api", "-X", "GET", "https://api.github.com/orgs/Acme/teams"}, owner: "acme"},
		{argv: []string{"co", "https://github.com/sjawhar/legion/pull/1"}, owner: "sjawhar"},
		{argv: []string{"pr", "-R", "sjawhar/legion", "view", "1"}, owner: "sjawhar"},
		{argv: []string{"pr", "create", "--fill", "-R", "github.com/Acme/widgets"}, owner: "acme"},
		{argv: []string{"repo", "clone", "sjawhar/legion", "--", "--depth=1", "-R", "acme/w"}, owner: "sjawhar"},
		{argv: []string{"repo", "create", "sjawhar/new", "--private"}, owner: "sjawhar"},
		{argv: []string{"repo", "create", "newrepo", "--private"}},
		{argv: []string{"repo", "list", "Sjawhar"}, owner: "sjawhar"},
		{argv: []string{"search", "issues", "repo:sjawhar/legion is:open"}, owner: "sjawhar"},
		{argv: []string{"search", "issues", "--owner", "acme", "bug"}, owner: "acme"},
		{argv: []string{"issue", "transfer", "3", "sjawhar/legion", "-R", "sjawhar/other"}, owner: "sjawhar"},
		{argv: []string{"secret", "set", "TOKEN", "--org", "acme"}, owner: "acme"},
		// Refusals.
		{argv: []string{"pr", "view", "1", "--bogus"}, err: `"--bogus" is no option`},
		{argv: []string{"pr", "view", "1", "-Z"}, err: "names -Z"},
		{argv: []string{"pr", "nonexist"}, err: "gh has no command `gh pr nonexist`"},
		{argv: []string{"nonexist"}, err: "cannot read `gh nonexist`"},
		{argv: []string{"--bogus", "pr", "list"}, err: `"--bogus" comes before the command's name`},
		{argv: []string{"api", "repositories/12345"}, err: "by its id"},
		{argv: []string{"api", "https://ghe.example.com/api/v3/repos/acme/w"}, err: "another host"},
		{argv: []string{"api", "--hostname", "ghe.example.com", "repos/acme/w"}, err: "another host"},
		{argv: []string{"api", "repos/../x/y"}, err: `"..", which is no GitHub login`},
		{argv: []string{"pr", "list"}, env: map[string]string{"GH_REPO": "../x/y"}, err: `names another host`},
		{argv: []string{"pr", "list"}, env: map[string]string{"GH_REPO": "../y"}, err: "no GitHub login"},
		{argv: []string{"pr", "list"}, env: map[string]string{"GH_REPO": "widgets"}, err: "names no owner"},
		{argv: []string{"repo", "view", "widgets"}, err: "names no owner"},
		{argv: []string{"pr", "view", "1", "-R"}, err: "has no value"},
		{argv: []string{"search", "issues", "--owner", "acme", "repo:sjawhar/x"}, err: "more than one owner"},
		{argv: []string{"issue", "transfer", "3", "sjawhar/legion"}, err: "names a second repository"},
		{argv: []string{"label", "clone", "acme/widgets", "-R", "sjawhar/legion"}, err: "more than one owner"},
		{argv: []string{"secret", "set", "TOKEN", "--org", "acme", "-R", "sjawhar/x"}, err: "more than one owner"},
		{argv: []string{"auth", "status", "-h", "ghe.example.com"}, err: "another host"},
		{argv: []string{"browse", "-c", "abc"}, err: "shorthand takes a value"},
	} {
		owner, named, err := ghTargetOf(tc.argv, tc.env, savedGhHelp)
		switch {
		case tc.err != "":
			if err == nil || !strings.Contains(err.Error(), tc.err) {
				t.Errorf("ghTargetOf(%q, %v) = %q, %t, %v; want a refusal containing %q", tc.argv, tc.env, owner, named, err, tc.err)
			}
		case err != nil || owner != tc.owner || named != (tc.owner != ""):
			t.Errorf("ghTargetOf(%q, %v) = %q, %t, %v; want %q", tc.argv, tc.env, owner, named, err, tc.owner)
		}
	}
}

// Every refusal names the argument it could not place and how to name the repository instead:
// -R, or, for gh api, which has none, the endpoint.
func TestGhTargetOfRefusalsSayHowToNameTheRepository(t *testing.T) {
	for argv, want := range map[string]string{
		"pr view 1 --bogus":      "pass -R <owner>/<repo>",
		"api repositories/12345": "name it in the endpoint, repos/<owner>/<repo>/…",
		"pr --json n view 1":     "pass -R <owner>/<repo>",
	} {
		_, _, err := ghTargetOf(strings.Fields(argv), nil, savedGhHelp)
		if err == nil || !strings.Contains(err.Error(), want) || !strings.Contains(err.Error(), "sends no token") {
			t.Errorf("ghTargetOf(%q) = %v, want a refusal saying it sends no token and %q", argv, err, want)
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
// GH_CONFIG_DIR pointed at the directory of the owner the command names, and at the issue's
// owner's (the GH_CONFIG_DIR it was given) when it names none. It reads each command from the
// real gh's own help. A command naming an owner the bundle holds no directory for is refused, exit
// 2, naming the owner, and so is one it cannot read; the real gh never runs. Each of the three
// variables it needs is required, and an unset one is named with exit 2; --help with no real gh to
// hand it to is legion's usage.
func TestLegionGhExecsTheRealGhWithTheOwnersDirectory(t *testing.T) {
	root, defaultDir := gitCredentialBundle(t)
	help, err := filepath.Abs(ghHelpDir)
	if err != nil {
		t.Fatal(err)
	}
	// The stub answers `<command> --help` with the saved help of that command, as the real gh
	// would, and every other run with its argv and GH_CONFIG_DIR.
	stub := filepath.Join(t.TempDir(), "gh")
	script := "#!/bin/sh\nfor last; do :; done\n" +
		"if [ \"$#\" -gt 1 ] && [ \"$last\" = --help ]; then\n" +
		"  path=$(printf '%s ' \"$@\" | sed -e 's/ --help $//' -e 's/ /_/g')\n" +
		"  exec cat '" + help + "'/\"$path\".txt\nfi\n" +
		"printf '%s|%s|%s\\n' \"$0\" \"$GH_CONFIG_DIR\" \"$*\"\n"
	if err := os.WriteFile(stub, []byte(script), 0o700); err != nil {
		t.Fatal(err)
	}
	env := map[string]string{"LEGION_GH_PATH": stub, "GH_CONFIG_DIR": defaultDir, "LEGION_GH_OWNERS_DIR": root}
	for _, tc := range []struct {
		args []string
		dir  string
	}{
		{[]string{"api", "repos/sjawhar/legion-smoke", "--jq", ".full_name"}, filepath.Join(root, "sjawhar")},
		{[]string{"pr", "create", "-R", "acme/widgets", "--fill"}, filepath.Join(root, "acme")},
		{[]string{"pr", "list"}, defaultDir},
		{[]string{"--help"}, defaultDir},
	} {
		code, stdout, stderr := legionGh(t, env, tc.args...)
		want := stub + "|" + tc.dir + "|" + strings.Join(tc.args, " ") + "\n"
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
	} {
		code, stdout, stderr := legionGh(t, env, tc.args...)
		if code != 2 || stdout != "" || !strings.Contains(stderr, tc.want) {
			t.Errorf("legion gh %q = exit %d, stdout %q, stderr %q; want exit 2 containing %q and no gh run", tc.args, code, stdout, stderr, tc.want)
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
