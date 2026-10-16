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
// 2.98.0 (testdata/gh-help/VERSION) in a fresh configuration, one file per path, its words joined
// by "_", and gh's own help (`gh --help`) as gh.txt.
const ghHelpDir = "testdata/gh-help"

// savedGhHelp answers gh's help of path from ghHelpDir as the real gh does: a path it has no file
// for prints the help of the nearest parent it has (gh's answer to a subcommand it does not know),
// and one with no such parent at all is an error (gh's `unknown command`, exit 1).
func savedGhHelp(path []string) (string, error) {
	if len(path) == 0 {
		data, err := os.ReadFile(filepath.Join(ghHelpDir, "gh.txt"))
		return string(data), err
	}
	for p := path; len(p) > 0; p = p[:len(p)-1] {
		data, err := os.ReadFile(filepath.Join(ghHelpDir, strings.Join(p, "_")+".txt"))
		if err == nil {
			return string(data), nil
		}
	}
	return "", fmt.Errorf("unknown command %q for \"gh\"", path[0])
}

// editedGhHelp answers gh's help as savedGhHelp does, with old replaced by new in the help of
// each path edits names (its words joined by spaces, "" for gh's own): the help a gh with other
// aliases or extensions, or a later gh spelling its usages otherwise, prints.
func editedGhHelp(edits map[string][2]string) func([]string) (string, error) {
	return func(path []string) (string, error) {
		text, err := savedGhHelp(path)
		if edit, ok := edits[strings.Join(path, " ")]; ok && err == nil {
			if !strings.Contains(text, edit[0]) {
				return "", fmt.Errorf("the saved help of %q holds no %q to edit", path, edit[0])
			}
			text = strings.Replace(text, edit[0], edit[1], 1)
		}
		return text, err
	}
}

// The help of a gh with an extension, `hello`, and an alias of its own under pr, `pr pv2`, beside
// gh's default alias co; and of a gh whose usage of pr view spells its URL <URL>, as no gh this
// was read from does.
var (
	ghWithAnExtensionAndANestedAlias = editedGhHelp(map[string][2]string{
		"":   {"ALIAS COMMANDS\n", "EXTENSION COMMANDS\n  hello:         Extension hello\n\nALIAS COMMANDS\n"},
		"pr": {"\nFLAGS\n", "\nALIAS COMMANDS\n  pv2:           Alias for \"pr view -R sjawhar/legion\"\n\nFLAGS\n"},
	})
	ghSpellingItsURLOtherwise = editedGhHelp(map[string][2]string{
		"pr view": {"<url>", "<URL>"},
	})
)

// parseGhHelp reads gh's own help: a group by its COMMANDS section, with the gh aliases and
// extensions it lists; a leaf's canonical path, a cobra alias its ALIASES section lists; every
// option's arity, pflag's optional value, a shorthand taken by another option than --help; and the
// arguments its USAGE names, each of a kind ghSlotKinds knows or none. It refuses a help it cannot
// read whole, and the help a gh alias prints, its command's, which lists no such alias.
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
	kind := func(slot ghSlot) string {
		kind, unknown := slot.kind()
		if unknown != "" {
			return "unknown <" + unknown + ">"
		}
		return kind
	}
	for _, group := range [][]string{{}, {"pr"}, {"repo"}, {"repo", "deploy-key"}, {"search"}} {
		if cmd := read(group...); !cmd.group || !slices.Equal(cmd.path, group) {
			t.Errorf("gh %s's help reads as %+v, want the group %q", strings.Join(group, " "), cmd, group)
		}
	}
	if gh := read(); !gh.ghAliases["co"] || gh.ghAliases["pr"] || len(gh.extensions) != 0 {
		t.Errorf("gh's own help lists the aliases %v and the extensions %v, want gh's default alias co alone", gh.ghAliases, gh.extensions)
	}
	view := read("pr", "view")
	if _, h := view.shorts['h']; view.group || !view.flags["json"].value || view.flags["web"].value || !view.readsGHRepo() || view.shorts['R'] != "repo" || h {
		t.Errorf("gh pr view reads as %+v", view)
	}
	if len(view.slots) != 1 || kind(view.slots[0]) != "url" {
		t.Errorf("gh pr view's arguments read as %+v, want one URL-capable slot", view.slots)
	}
	if ls := read("pr", "ls"); !slices.Equal(ls.path, []string{"pr", "list"}) || ls.group {
		t.Errorf("gh pr ls reads as %+v, want its command, pr list, whose help lists the alias", ls)
	}
	if add := read("repo", "deploy-key", "add"); !add.flags["title"].value || add.group {
		t.Errorf("gh repo deploy-key add reads as %+v, want --title taking a value", add)
	}
	if clone := read("repo", "clone"); !clone.passthrough || len(clone.slots) != 2 || kind(clone.slots[0]) != "repository" || kind(clone.slots[1]) != "" {
		t.Errorf("gh repo clone reads as %+v, want a repository slot, a directory and a -- passthrough", clone)
	}
	if transfer := read("issue", "transfer"); len(transfer.slots) != 2 || kind(transfer.slots[1]) != "second-repository" {
		t.Errorf("gh issue transfer reads as %+v, want its destination a second repository", transfer.slots)
	}
	if browse := read("browse"); !browse.flags["commit"].value || !browse.flags["commit"].optional || browse.shorts['c'] != "commit" || kind(browse.slots[0]) != "" {
		t.Errorf("gh browse reads as %+v, want -c, --commit taking a value only after = and an argument naming no repository", browse)
	}
	if status := read("auth", "status"); status.shorts['h'] != "hostname" {
		t.Errorf("gh auth status's -h reads as %q, want --hostname", status.shorts['h'])
	}
	if search := read("search", "issues"); search.readsGHRepo() || kind(search.slots[0]) != "query" {
		t.Errorf("gh search issues reads as %+v, want a query and no inherited -R", search)
	}
	if set := read("secret", "set"); set.flags["repos"].placeholder != "repositories" || !set.flags["org"].value || kind(set.slots[0]) != "" {
		t.Errorf("gh secret set reads as %+v, want --repos repositories, --org taking a value and a secret's name", set)
	}
	drift, err := ghSpellingItsURLOtherwise([]string{"pr", "view"})
	if err != nil {
		t.Fatal(err)
	}
	if cmd, err := parseGhHelp([]string{"pr", "view"}, drift); err != nil || kind(cmd.slots[0]) != "unknown <URL>" {
		t.Errorf("pr view's help spelling <URL> reads as %+v, %v; want its argument of a kind this does not know", cmd.slots, err)
	}

	co, err := savedGhHelp([]string{"co"})
	if err != nil {
		t.Fatal(err)
	}
	prView := []string{"pr", "view"}
	for _, tc := range []struct {
		name string
		path []string
		help string
		want string
	}{
		{"no usage", prView, "FLAGS\n  --help   Show help for command\n", "names no usage"},
		{"no --help", prView, "USAGE\n  gh pr view [flags]\n\nFLAGS\n  -w, --web   Open\n", "lists no --help"},
		{"an option line it cannot read", prView, "USAGE\n  gh pr view [flags]\n\nFLAGS\n  --help   Show help\n  -w --web   Open\n", "cannot read the options"},
		{"a parent's help", prView, "USAGE\n  gh pr <command> [flags]\n", "gh has no command"},
		{"gh co's, pr checkout's, which lists no alias co", []string{"co"}, co, "is a gh alias"},
	} {
		if _, err := parseGhHelp(tc.path, tc.help); err == nil || !strings.Contains(err.Error(), tc.want) {
			t.Errorf("parseGhHelp of %s = %v, want an error containing %q", tc.name, err, tc.want)
		}
	}
	if count, err := parseGhHelp(prView, "USAGE\n  gh pr view [flags]\n\nFLAGS\n  -v, --verbose count   More\n\nINHERITED FLAGS\n  --help   Show help\n"); err != nil || !count.flags["verbose"].uncertain {
		t.Errorf("a count option reads as %+v, %v; want an option whose arity its help does not say", count.flags, err)
	}
}

// Every argument gh's usages name, across every command its reference lists, is of a kind
// ghSlotKinds knows, but for the few ghTargetOf refuses on purpose: what `gh copilot` and `gh
// extension exec` hand another program, codespace arguments of no fixed shape, and gh api's
// endpoint, which it reads itself. So the pinned gh refuses no command for an argument this does
// not know, and a later gh's new spelling is refused rather than read as naming nothing.
func TestEveryArgumentGhsReferenceNamesIsOfAKnownKind(t *testing.T) {
	data, err := os.ReadFile(filepath.Join(ghHelpDir, "reference.txt"))
	if err != nil {
		t.Fatal(err)
	}
	refused := map[string]bool{"args": true, "command": true, "flags": true, "...": true, "public": true, "private": true, "org": true, "endpoint": true}
	for name := range refused {
		if _, known := ghSlotKinds[name]; known {
			t.Errorf("ghSlotKinds knows <%s>, which ghTargetOf refuses on purpose", name)
		}
	}
	leaves := 0
	for _, line := range strings.Split(string(data), "\n") {
		words := strings.Fields(strings.Trim(strings.TrimSpace(strings.TrimLeft(line, "#")), "`"))
		if !strings.HasPrefix(line, "#") || len(words) < 2 || words[0] != "gh" {
			continue
		}
		n := 1
		for n < len(words) && ghCommandWord.MatchString(words[n]) {
			n++
		}
		args := strings.Join(words[n:], " ")
		if ghUsageNamesSubcommands(args) {
			continue
		}
		leaves++
		slots, _ := ghSlots(args)
		for _, slot := range slots {
			for _, name := range slot.names {
				if _, known := ghSlotKinds[name]; !known && !refused[name] {
					t.Errorf("`%s` names <%s>, of no kind ghSlotKinds knows", strings.Join(words, " "), name)
				}
			}
		}
	}
	if leaves < 150 {
		t.Errorf("gh's reference lists %d commands this reads, want every one of its at least 150", leaves)
	}
}

// ghTargetOf reads the owner a gh command targets from every selector gh's own help names, and
// fails closed: an argument it cannot place is refused, naming it, and so is a command naming two
// owners; only a command naming no repository at all is not named, and a command gh only prints
// the help of, its last --help true, sends nothing. GH_REPO names the owner only when nothing
// explicit does, and only for a leaf that reads it.
func TestGhTargetOfFailsClosed(t *testing.T) {
	acme := map[string]string{"GH_REPO": "acme/widgets"}
	for _, tc := range []struct {
		argv  []string
		env   map[string]string
		help  func([]string) (string, error) // savedGhHelp when nil
		owner string                         // "" with no refusal: not named
		err   string                         // a refusal containing this
	}{
		// The first re-review's reproductions.
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
		// The second re-review's reproductions. --help is a boolean, the last of its values
		// deciding, before the command's name or after; only true is help.
		{argv: []string{"api", "repos/sjawhar/legion", "--help=false"}, owner: "sjawhar"},
		{argv: []string{"pr", "view", "1", "-R", "sjawhar/legion", "--help=false"}, owner: "sjawhar"},
		{argv: []string{"--help", "pr", "view", "1", "-R", "sjawhar/legion", "--help=false"}, owner: "sjawhar"},
		{argv: []string{"pr", "--help", "view", "1", "-R", "sjawhar/legion", "--help=0"}, owner: "sjawhar"},
		{argv: []string{"pr", "view", "1", "-R", "sjawhar/legion", "--help=false", "--help"}},
		{argv: []string{"pr", "view", "1", "-R", "sjawhar/legion", "--help=false", "-h"}},
		{argv: []string{"pr", "view", "1", "-R", "sjawhar/legion", "--help=maybe"}, err: `"--help=maybe" gives --help a value that is no boolean`},
		{argv: []string{"--help=false", "api", "repos/sjawhar/legion"}, owner: "sjawhar"},
		// A name in --repos is a repository of the --org.
		{argv: []string{"secret", "set", "TOKEN", "--org", "sjawhar", "--repos", "legion", "--body", "dummy", "-R", "sjawhar/legion"}, owner: "sjawhar"},
		{argv: []string{"secret", "set", "TOKEN", "-o", "sjawhar", "-r", "legion,smoke", "--body", "dummy"}, owner: "sjawhar"},
		{argv: []string{"secret", "set", "TOKEN", "--org", "sjawhar", "--repos", "legion,acme/widgets", "--body", "dummy"}, err: "more than one owner"},
		{argv: []string{"secret", "set", "TOKEN", "--user", "--repos", "legion", "--body", "dummy"}, err: "no --org says whose"},
		// pflag's optional value after a shorthand: -c takes its default, and the rest of the
		// word is more shorthands.
		{argv: []string{"browse", "-c", "-R", "sjawhar/legion", "--no-browser"}, owner: "sjawhar"},
		{argv: []string{"browse", "-cnR", "sjawhar/legion"}, owner: "sjawhar"},
		{argv: []string{"browse", "-c=abc", "-R", "sjawhar/legion"}, owner: "sjawhar"},
		{argv: []string{"browse", "-c", "abc"}},
		// A gh alias, which may carry a repository of its own, and an extension, which reads gh's
		// token itself, are refused before their help is read, at any depth, and so is the Copilot
		// CLI gh runs.
		{argv: []string{"co", "https://github.com/sjawhar/legion/pull/1"}, err: `"co" is a gh alias`},
		{argv: []string{"pr", "pv2", "1"}, help: ghWithAnExtensionAndANestedAlias, err: `"pv2" is a gh alias`},
		{argv: []string{"hello", "-R", "sjawhar/legion"}, help: ghWithAnExtensionAndANestedAlias, err: `"hello" is a gh extension`},
		{argv: []string{"pr", "view", "1", "-R", "sjawhar/legion"}, help: ghWithAnExtensionAndANestedAlias, owner: "sjawhar"},
		{argv: []string{"pr", "ls", "-R", "sjawhar/legion"}, owner: "sjawhar"},
		{argv: []string{"copilot"}, err: "`gh copilot` runs another program"},
		// A search query names its owner in plain qualifiers, one to an argument, or not at all.
		{argv: []string{"search", "issues", "repo:sjawhar/legion", "is:open"}, owner: "sjawhar"},
		{argv: []string{"search", "issues", "bug OR crash"}, env: acme},
		{argv: []string{"search", "issues", "(repo:sjawhar/legion)"}, err: `"(repo:sjawhar/legion)" is a search qualifier this cannot read for certain`},
		{argv: []string{"search", "issues", "repo:sjawhar/legion is:open"}, err: "is a search qualifier this cannot read for certain"},
		{argv: []string{"search", "issues", `label:"help wanted"`}, err: "is a search qualifier this cannot read for certain"},
		{argv: []string{"search", "issues", "repo:sjawhar/legion", "OR", "bug"}, err: `"OR" joins the query's`},
		{argv: []string{"search", "issues", "(", "repo:sjawhar/legion", ")"}, err: `"(" joins the query's`},
		{argv: []string{"search", "issues", "--", "bug", "-repo:acme/widgets"}, err: `"-repo:acme/widgets" excludes an owner`},
		{argv: []string{"search", "issues", "bug", "-repo:acme/widgets"}, err: `"-repo:acme/widgets" names -r`},
		{argv: []string{"api", "search/issues?q=repo:sjawhar/legion+is:open"}, owner: "sjawhar"},
		{argv: []string{"api", "-X", "GET", "search/issues", "-f", "q=repo:sjawhar/legion is:open"}, owner: "sjawhar"},
		{argv: []string{"api", "search/issues?q=%28repo:sjawhar/legion%29"}, err: "is a search qualifier this cannot read for certain"},
		{argv: []string{"api", "-X", "GET", "search/issues", "-F", "q=@query.txt"}, err: "reads the search query from a file"},
		// gh sends its token only to GH_HOST, and GH_TOKEN or GITHUB_TOKEN in place of any file's.
		{argv: []string{"api", "repos/sjawhar/legion"}, env: map[string]string{"GH_HOST": "ghe.example.com"}, err: `GH_HOST is "ghe.example.com"`},
		{argv: []string{"api", "repos/sjawhar/legion"}, env: map[string]string{"GH_HOST": "GitHub.com"}, owner: "sjawhar"},
		{argv: []string{"pr", "list"}, env: map[string]string{"GH_TOKEN": "ghp_x"}, err: "GH_TOKEN is set"},
		{argv: []string{"pr", "list"}, env: map[string]string{"GITHUB_TOKEN": "ghp_x"}, err: "GITHUB_TOKEN is set"},
		// An argument the leaf's usage names in a way this does not know, or has no place for.
		{argv: []string{"pr", "view", "https://github.com/sjawhar/legion/pull/1"}, help: ghSpellingItsURLOtherwise, err: "<URL>"},
		{argv: []string{"pr", "view", "1"}, help: ghSpellingItsURLOtherwise, err: "<URL>"},
		{argv: []string{"pr", "view", "1", "2"}, err: `"2" is an argument`},
		{argv: []string{"pr", "list", "sjawhar/legion"}, err: `"sjawhar/legion" is an argument`},
		{argv: []string{"api", "repos/acme/widgets", "repos/sjawhar/legion"}, err: `"repos/sjawhar/legion" is an argument`},
		// A URL is one in any case, as gh parses it.
		{argv: []string{"pr", "view", "HTTPS://GITHUB.COM/sjawhar/legion/pull/1"}, owner: "sjawhar"},
		{argv: []string{"api", "HTTPS://API.GITHUB.COM/repos/sjawhar/legion"}, owner: "sjawhar"},
		{argv: []string{"api", "REPOS/sjawhar/legion"}, owner: "sjawhar"},
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
		{argv: []string{"-h", "pr", "view", "1", "-R", "acme/w"}},
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
		{argv: []string{"pr", "-R", "sjawhar/legion", "view", "1"}, owner: "sjawhar"},
		{argv: []string{"pr", "create", "--fill", "-R", "github.com/Acme/widgets"}, owner: "acme"},
		{argv: []string{"repo", "clone", "sjawhar/legion", "--", "--depth=1", "-R", "acme/w"}, owner: "sjawhar"},
		{argv: []string{"repo", "create", "sjawhar/new", "--private"}, owner: "sjawhar"},
		{argv: []string{"repo", "create", "newrepo", "--private"}},
		{argv: []string{"repo", "list", "Sjawhar"}, owner: "sjawhar"},
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
		{argv: []string{"pr", "view", "1", "-R="}, err: `"=" names no owner`},
		{argv: []string{"search", "issues", "--owner", "acme", "repo:sjawhar/x"}, err: "more than one owner"},
		{argv: []string{"issue", "transfer", "3", "sjawhar/legion"}, err: "names a second repository"},
		{argv: []string{"label", "clone", "acme/widgets", "-R", "sjawhar/legion"}, err: "more than one owner"},
		{argv: []string{"secret", "set", "TOKEN", "--org", "acme", "-R", "sjawhar/x"}, err: "more than one owner"},
		{argv: []string{"auth", "status", "-h", "ghe.example.com"}, err: "another host"},
	} {
		help := tc.help
		if help == nil {
			help = savedGhHelp
		}
		owner, named, err := ghTargetOf(tc.argv, tc.env, help)
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
// -R, or, for gh api, which has none, the endpoint; for a gh alias, the command it stands for.
func TestGhTargetOfRefusalsSayHowToNameTheRepository(t *testing.T) {
	for argv, want := range map[string]string{
		"pr view 1 --bogus":              "pass -R <owner>/<repo>",
		"api repositories/12345":         "name it in the endpoint, repos/<owner>/<repo>/…",
		"pr --json n view 1":             "pass -R <owner>/<repo>",
		"co 1":                           "run the gh command it stands for, with -R <owner>/<repo>",
		"copilot":                        "run gh's own commands, with -R <owner>/<repo>",
		"search issues (repo:acme/w)":    "pass -R <owner>/<repo>",
		"api search/issues?q=(repo:a/b)": "name it in the endpoint",
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
// owner's (the GH_CONFIG_DIR it was given) when it names none, reading --help=false as no help. It
// reads each command from the real gh's own help. A command naming an owner the bundle holds no
// directory for is refused, exit 2, naming the owner, and so are one it cannot read and a gh
// alias; the real gh never runs. Each of the three variables it needs is required, and an unset
// one is named with exit 2; --help with no real gh to hand it to is legion's usage.
func TestLegionGhExecsTheRealGhWithTheOwnersDirectory(t *testing.T) {
	root, defaultDir := gitCredentialBundle(t)
	help, err := filepath.Abs(ghHelpDir)
	if err != nil {
		t.Fatal(err)
	}
	// The stub answers `[<command>] --help` with the saved help of that command, gh's own for none,
	// as the real gh would, and every other run with its argv and GH_CONFIG_DIR.
	stub := filepath.Join(t.TempDir(), "gh")
	script := "#!/bin/sh\nfor last; do :; done\n" +
		"if [ \"$last\" = --help ]; then\n" +
		"  path=$(printf '%s ' \"$@\" | sed -e 's/--help $//' -e 's/ $//' -e 's/ /_/g')\n" +
		"  exec cat '" + help + "'/\"${path:-gh}\".txt\nfi\n" +
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
		{[]string{"pr", "view", "1", "-R", "acme/widgets", "--help=false"}, filepath.Join(root, "acme")},
		{[]string{"api", "repos/sjawhar/legion-smoke", "--help=false", "-q", "."}, filepath.Join(root, "sjawhar")},
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
		{[]string{"co", "1"}, `"co" is a gh alias`},
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
