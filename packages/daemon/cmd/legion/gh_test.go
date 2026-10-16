package main

import (
	"bytes"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
)

// ghHelp is the FLAGS part of the real gh 2.98 help of the commands ghTargetOf's table reads,
// parsed by the same parser `legion gh` runs over the real gh's own --help.
var ghHelp = map[string]string{
	"api": `FLAGS
      --cache duration           Cache the response
  -F, --field key=value          Add a typed parameter
  -H, --header key:value         Add a HTTP request header
      --hostname string          The GitHub hostname
  -i, --include                  Include HTTP response status line
      --input file               The file to use as body
  -q, --jq string                Query to select values
  -X, --method string            The HTTP method
      --paginate                 Make additional HTTP requests
  -p, --preview strings          Opt into GitHub API previews
  -f, --raw-field key=value      Add a string parameter
      --silent                   Do not print the response body
  -t, --template string          Format JSON output

INHERITED FLAGS
  --help   Show help for command
`,
	"pr view": `FLAGS
  -c, --comments          View pull request comments
  -q, --jq expression     Filter JSON output using a jq expression
      --json fields       Output JSON with the specified fields
  -t, --template string   Format JSON output using a Go template
  -w, --web               Open a pull request in the browser

INHERITED FLAGS
      --help                     Show help for command
  -R, --repo [HOST/]OWNER/REPO   Select another repository
`,
	"pr create": `FLAGS
  -b, --body string         Body for the pull request
  -d, --draft               Mark pull request as a draft
  -f, --fill                Use commit info for title and body
  -t, --title string        Title for the pull request

INHERITED FLAGS
      --help                     Show help for command
  -R, --repo [HOST/]OWNER/REPO   Select another repository
`,
	"pr list": `FLAGS
  -L, --limit int         Maximum number of items to fetch

INHERITED FLAGS
      --help                     Show help for command
  -R, --repo [HOST/]OWNER/REPO   Select another repository
`,
	"repo view": `FLAGS
  -b, --branch string     View a specific branch of the repository
  -q, --jq expression     Filter JSON output using a jq expression
      --json fields       Output JSON with the specified fields
  -w, --web               Open a repository in the browser

INHERITED FLAGS
  --help   Show help for command
`,
}

func fakeGhFlags(t *testing.T) func(path []string) (ghFlags, error) {
	return func(path []string) (ghFlags, error) {
		help, ok := ghHelp[strings.Join(path, " ")]
		if !ok {
			t.Fatalf("ghTargetOf asked for the flags of %q, which the test has no help for", path)
		}
		return parseGhFlags(help)
	}
}

// ghTargetOf reads the owner of the repository a gh command acts on as gh itself resolves it, with
// each option's arity read from gh's help: for `gh api`, the owner of a repos/<owner>/ endpoint
// (GH_REPO only fills its {owner} placeholder); for any other command, a github.com URL given as
// its first positional selector, then -R/--repo, then GH_REPO; for `gh repo <sub>`, its repository
// argument, then -R, then GH_REPO. A command that names none is not named, and gh acts on its
// working directory's repository, the issue's. A selector that names no GitHub login, or a host
// other than github.com, is refused.
func TestGhTargetOfFollowsGhsSelectorPrecedenceAndOptionArity(t *testing.T) {
	acme := map[string]string{"GH_REPO": "acme/widgets"}
	for _, tc := range []struct {
		argv  []string
		env   map[string]string
		owner string
		named bool
	}{
		{argv: []string{"api", "repos/acme/widgets/pulls"}, owner: "acme", named: true},
		{argv: []string{"api", "repos/sjawhar/legion"}, env: acme, owner: "sjawhar", named: true},
		{argv: []string{"api", "-X", "GET", "/repos/Acme/widgets/branches/main"}, owner: "acme", named: true},
		{argv: []string{"api", "--method", "GET", "repos/sjawhar/legion-smoke", "--jq", ".full_name"}, owner: "sjawhar", named: true},
		{argv: []string{"api", "-H", "Accept: application/json", "https://api.github.com/repos/sjawhar/legion"}, owner: "sjawhar", named: true},
		{argv: []string{"api", "repos/{owner}/{repo}/pulls"}, env: acme, owner: "acme", named: true},
		{argv: []string{"api", "repos/{owner}/{repo}/pulls"}},
		{argv: []string{"api", "graphql", "-f", "query={viewer{login}}"}},
		{argv: []string{"api", "user"}, env: acme},
		{argv: []string{"pr", "view", "1", "-R", "sjawhar/legion", "--json", "number"}, env: acme, owner: "sjawhar", named: true},
		{argv: []string{"pr", "view", "https://github.com/sjawhar/legion/pull/1", "-R", "acme/widgets", "--json", "number"}, owner: "sjawhar", named: true},
		{argv: []string{"pr", "view", "--json", "url", "https://github.com/Sjawhar/legion/pull/2"}, owner: "sjawhar", named: true},
		{argv: []string{"pr", "view", "-wR", "sjawhar/legion", "3"}, owner: "sjawhar", named: true},
		{argv: []string{"pr", "view", "3", "-Rsjawhar/legion"}, owner: "sjawhar", named: true},
		{argv: []string{"pr", "view", "3", "--repo=github.com/sjawhar/legion"}, owner: "sjawhar", named: true},
		{argv: []string{"pr", "-R", "sjawhar/legion", "view", "1"}, owner: "sjawhar", named: true},
		{argv: []string{"pr", "create", "-R", "acme/widgets", "--body", "https://github.com/sjawhar/legion/pull/1", "--fill"}, owner: "acme", named: true},
		{argv: []string{"pr", "create", "-b", "https://github.com/sjawhar/legion/pull/1", "-f"}},
		{argv: []string{"pr", "list"}},
		{argv: []string{"pr", "list", "-L", "5"}, env: map[string]string{"GH_REPO": "github.com/Acme/w"}, owner: "acme", named: true},
		{argv: []string{"repo", "view", "--json", "nameWithOwner", "sjawhar/legion"}, env: acme, owner: "sjawhar", named: true},
		{argv: []string{"repo", "view", "https://github.com/sjawhar/legion"}, owner: "sjawhar", named: true},
		{argv: []string{"repo", "view"}, env: acme, owner: "acme", named: true},
		{argv: []string{"repo", "view"}},
		{argv: []string{"repo", "clone", "Acme/widgets", "--", "--depth=1"}, owner: "acme", named: true},
		{argv: []string{"repo", "list", "sjawhar"}, owner: "sjawhar", named: true},
		{argv: []string{"release", "list", "-R", "sjawhar/legion"}, env: acme, owner: "sjawhar", named: true},
		{argv: []string{"auth", "status"}},
		{argv: []string{"--help"}},
		{argv: nil},
	} {
		owner, named, err := ghTargetOf(tc.argv, tc.env, fakeGhFlags(t))
		if err != nil || owner != tc.owner || named != tc.named {
			t.Errorf("ghTargetOf(%q, %v) = %q, %t, %v; want %q, %t", tc.argv, tc.env, owner, named, err, tc.owner, tc.named)
		}
	}
	for _, tc := range []struct {
		argv []string
		env  map[string]string
		want string
	}{
		{argv: []string{"pr", "view", "1", "-R", "gitlab.com/acme/widgets"}, want: `"gitlab.com"`},
		{argv: []string{"api", "repos/../x/y"}, want: `".."`},
		{argv: []string{"pr", "list"}, env: map[string]string{"GH_REPO": "../x/y"}, want: `".."`},
	} {
		if _, _, err := ghTargetOf(tc.argv, tc.env, fakeGhFlags(t)); err == nil || !strings.Contains(err.Error(), tc.want) {
			t.Errorf("ghTargetOf(%q, %v) = %v, want a refusal naming %s", tc.argv, tc.env, err, tc.want)
		}
	}
}

// parseGhFlags reads which options take a value from gh's own help, a placeholder word after the
// option's name, and refuses a help that lists no --help, which is no gh command's help.
func TestParseGhFlagsReadsEachOptionsArity(t *testing.T) {
	flags, err := parseGhFlags(ghHelp["pr view"])
	if err != nil {
		t.Fatal(err)
	}
	for name, want := range map[string]bool{"jq": true, "json": true, "template": true, "repo": true, "comments": false, "web": false, "help": false} {
		if flags.long[name] != want {
			t.Errorf("--%s takes a value: %t, want %t", name, flags.long[name], want)
		}
	}
	for name, want := range map[rune]bool{'q': true, 't': true, 'R': true, 'c': false, 'w': false} {
		if flags.short[name] != want {
			t.Errorf("-%c takes a value: %t, want %t", name, flags.short[name], want)
		}
	}
	if _, err := parseGhFlags("unknown command \"nope\" for \"gh\"\n"); err == nil {
		t.Error("parseGhFlags of a help with no --help = nil, want a refusal")
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
// owner's (the GH_CONFIG_DIR it was given) when it names none. A command naming an owner the bundle
// holds no directory for is refused, exit 2, naming the owner, and the real gh never runs. Each of
// the three variables it needs is required, and an unset one is named with exit 2; --help with no
// real gh to hand it to is legion's usage.
func TestLegionGhExecsTheRealGhWithTheOwnersDirectory(t *testing.T) {
	root, defaultDir := gitCredentialBundle(t)
	stub := filepath.Join(t.TempDir(), "gh")
	// The stub answers `legion gh`'s own help reads (`<command> --help`) with an option list, and
	// every other run, `gh --help` included, with its argv and GH_CONFIG_DIR.
	script := "#!/bin/sh\ncase \"$#:$*\" in\n1:*) printf '%s|%s|%s\\n' \"$0\" \"$GH_CONFIG_DIR\" \"$*\" ;;\n" +
		"*--help) printf '%s\\n' FLAGS '  -q, --jq string   Query' '      --fill   Use' '      --help   Show help' '  -R, --repo [HOST/]OWNER/REPO   Select' ;;\n" +
		"*) printf '%s|%s|%s\\n' \"$0\" \"$GH_CONFIG_DIR\" \"$*\" ;;\nesac\n"
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

	for _, args := range [][]string{{"pr", "view", "https://github.com/other/thing/pull/1"}, {"api", "repos/Other/thing"}} {
		code, stdout, stderr := legionGh(t, env, args...)
		if code != 2 || stdout != "" || !strings.Contains(stderr, `"other" is not an owner of this project`) {
			t.Errorf("legion gh %q = exit %d, stdout %q, stderr %q; want exit 2 refusing other and no gh run", args, code, stdout, stderr)
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
