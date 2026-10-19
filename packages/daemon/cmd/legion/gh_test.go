package main

import (
	"bytes"
	"fmt"
	"maps"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"slices"
	"strings"
	"testing"

	"github.com/sjawhar/legion/daemon/internal/runtime/workerbin"
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

// parseGhHelp reads gh's own help: a group by its COMMANDS section, with every command and help
// topic it lists and the gh aliases and extensions among them; a leaf's canonical path, a cobra
// alias its ALIASES section lists; every option's arity, pflag's optional value, a shorthand taken
// by another option than --help, and what its value selects; and the arguments its USAGE names,
// each of a kind the tables know or none. It refuses a help it cannot read whole, and the help a gh
// alias prints, its command's, which lists no such alias.
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
	if gh := read(); !gh.listed["pr"] || !gh.listed["api"] || !gh.listed["copilot"] || !gh.listed["environment"] || gh.listed["stack"] || gh.listed["cs"] {
		t.Errorf("gh's own help lists %v, want its commands and help topics, and no official-extension stub or cobra alias", gh.listed)
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
	if view.flags["repo"].selects != "repository" || view.flags["json"].selects != "" || view.flags["web"].selects != "nothing" {
		t.Errorf("gh pr view's -R selects %q, --json %q and --web %q, want a repository, none of the table's, and nothing", view.flags["repo"].selects, view.flags["json"].selects, view.flags["web"].selects)
	}
	if edit := read("issue", "edit"); edit.flags["add-blocked-by"].selects != "url" || edit.flags["parent"].selects != "url" || edit.flags["title"].selects != "" {
		t.Errorf("gh issue edit's --add-blocked-by selects %q, --parent %q and --title %q, want an issue's URL, an issue's URL, and none of the table's", edit.flags["add-blocked-by"].selects, edit.flags["parent"].selects, edit.flags["title"].selects)
	}
	if create := read("repo", "create"); kind(create.slots[0]) != "new-repository" {
		t.Errorf("gh repo create's argument reads as %+v, want the repository it creates", create.slots)
	}
	if clone := read("repo", "clone"); !clone.passthrough || len(clone.slots) != 2 || kind(clone.slots[0]) != "repository" || kind(clone.slots[1]) != "" {
		t.Errorf("gh repo clone reads as %+v, want a repository slot, a directory and a -- passthrough", clone)
	}
	if transfer := read("issue", "transfer"); len(transfer.slots) != 2 || kind(transfer.slots[1]) != "second-repository" {
		t.Errorf("gh issue transfer reads as %+v, want its destination a second repository", transfer.slots)
	}
	if browse := read("browse"); !browse.flags["commit"].value || !browse.flags["commit"].optional || browse.shorts['c'] != "commit" || kind(browse.slots[0]) != "number" {
		t.Errorf("gh browse reads as %+v, want -c, --commit taking a value only after = and an argument naming a repository only as OWNER/REPO#N", browse)
	}
	if status := read("auth", "status"); status.shorts['h'] != "hostname" {
		t.Errorf("gh auth status's -h reads as %q, want --hostname", status.shorts['h'])
	}
	if edit := read("issue", "edit"); len(edit.slots) != 1 || kind(edit.slots[0]) != "url" || edit.slots[0].variadic || !edit.slots[0].takesMany() {
		t.Errorf("gh issue edit reads as %+v, want one URL-capable slot of one or more arguments spelled with no ...", edit.slots)
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
	if later, err := parseGhHelp(prView, "USAGE\n  gh pr view [flags]\n\nFLAGS\n  --mirror string   Read the pull request from this repository instead\n  --notes string   Notes\n\nINHERITED FLAGS\n  --help   Show help\n"); err != nil || later.flags["mirror"].selects != "unknown" || later.flags["notes"].selects != "" {
		t.Errorf("a later gh's options read as %+v, %v; want --mirror, which names a repository, unknown and --notes none of the table's", later.flags, err)
	}
}

// Every argument gh's usages name, across every command its reference lists, is of a kind the
// tables know, but for the few ghTargetOf refuses on purpose: what `gh copilot` and `gh extension
// exec` hand another program, codespace arguments of no fixed shape, and gh api's endpoint, which
// it reads itself. So the pinned gh refuses no command for an argument this does not know, and a
// later gh's new spelling is refused rather than read as naming nothing. Each name's cardinality
// holds too: a plural name (`{<numbers> | <urls>}`, one or more issues) is one the tables take as
// one or more arguments even with no `...`, and every slot taking more than one fits one argument
// more than the usage has slots, wherever the slot stands.
func TestEveryArgumentGhsReferenceNamesIsOfAKnownKind(t *testing.T) {
	refused := map[string]bool{"args": true, "command": true, "flags": true, "...": true, "public": true, "private": true, "org": true, "endpoint": true}
	for name := range refused {
		if _, known := ghSlotKinds[name]; known {
			t.Errorf("ghSlotKinds knows <%s>, which ghTargetOf refuses on purpose", name)
		}
	}
	// The one name gh spells ending in "s" that stands for one argument.
	singular := map[string]bool{"alias": true}
	leaves, many := 0, 0
	for _, usage := range ghReferenceUsages(t) {
		if ghUsageNamesSubcommands(usage.args) {
			continue
		}
		leaves++
		spelled := "gh " + strings.Join(usage.path, " ") + " " + usage.args
		slots, _ := ghSlots(usage.path, usage.args)
		for _, slot := range slots {
			for _, arg := range slot.args {
				switch {
				case !arg.known && !refused[arg.name]:
					t.Errorf("`%s` names <%s>, of no kind the tables know", spelled, arg.name)
				case arg.known && strings.HasSuffix(arg.name, "s") && !singular[arg.name] && !arg.kind.many:
					t.Errorf("`%s` names <%s>, a plural, which the tables take as one argument", spelled, arg.name)
				}
			}
			if slot.takesMany() {
				many++
				if placed, ok := (&ghCommand{slots: slots}).slotsFor(len(slots) + 1); !ok || !slices.ContainsFunc(placed, func(s ghSlot) bool { return slices.Equal(s.args, slot.args) }) {
					t.Errorf("`%s` takes one or more arguments in %v, and %d arguments do not fit its usage there: %v", spelled, slot.args, len(slots)+1, placed)
				}
			}
		}
	}
	if leaves < 150 || many < 10 {
		t.Errorf("gh's reference lists %d commands this reads and %d arguments taking more than one, want every one of its at least 150 and 10", leaves, many)
	}
}

// Every option of every command gh's reference lists whose help mentions a repository, an owner,
// an organization or a URL is one ghOptionKinds holds, as a selector or as naming nothing, so the
// pinned gh refuses no command for an option this does not know, and an option a later gh adds
// that names one is refused rather than read as naming nothing. No entry names nothing while its
// placeholder is a repository's, an owner's or an organization's.
func TestEveryOptionGhsReferenceNamesARepositoryWithIsInTheTable(t *testing.T) {
	options, mentioning := 0, 0
	for _, usage := range ghReferenceUsages(t) {
		for _, line := range usage.flags {
			cmd := ghCommand{flags: map[string]ghFlag{}, shorts: map[byte]string{}}
			if err := cmd.addFlag(line); err != nil {
				t.Fatalf("gh %s: %v", strings.Join(usage.path, " "), err)
			}
			for name, flag := range cmd.flags {
				options++
				if flag.value && (ghMentionsSelector.MatchString(flag.description) || ghMentionsSelector.MatchString(flag.placeholder)) {
					mentioning++
				}
				if kind := ghOptionKind(usage.path, name, flag); kind == "unknown" {
					t.Errorf("gh %s's --%s %s (%q) names a repository, an owner or a URL, and ghOptionKinds holds no kind for it", strings.Join(usage.path, " "), name, flag.placeholder, flag.description)
				}
			}
		}
	}
	for key, kind := range ghOptionKinds {
		if kind == "nothing" && regexp.MustCompile(`(?i)repo|owner|org`).MatchString(key[strings.LastIndex(key, " ")+1:]) {
			t.Errorf("ghOptionKinds names %q as naming nothing, while its placeholder names a repository or an owner", key)
		}
	}
	if options < 1000 || mentioning < 50 {
		t.Errorf("gh's reference lists %d options and %d naming a repository, an owner or a URL, want every one of its at least 1000 and 50", options, mentioning)
	}
}

// ghReferenceUsage is one command of gh's reference (testdata/gh-help/reference.txt): its path,
// the arguments its usage names after it, and its option lines.
type ghReferenceUsage struct {
	path  []string
	args  string
	flags []string
}

// ghReferenceUsages reads every command of gh's saved reference.
func ghReferenceUsages(t *testing.T) []ghReferenceUsage {
	t.Helper()
	data, err := os.ReadFile(filepath.Join(ghHelpDir, "reference.txt"))
	if err != nil {
		t.Fatal(err)
	}
	var usages []ghReferenceUsage
	for _, line := range strings.Split(string(data), "\n") {
		if strings.HasPrefix(line, "#") {
			words := strings.Fields(strings.Trim(strings.TrimSpace(strings.TrimLeft(line, "#")), "`"))
			if len(words) < 2 || words[0] != "gh" {
				continue
			}
			n := 1
			for n < len(words) && ghCommandWord.MatchString(words[n]) {
				n++
			}
			usages = append(usages, ghReferenceUsage{path: words[1:n], args: strings.Join(words[n:], " ")})
			continue
		}
		if len(usages) > 0 && ghFlagLine.MatchString(line) {
			usages[len(usages)-1].flags = append(usages[len(usages)-1].flags, line)
		}
	}
	return usages
}

// ghTargetOf reads the owner a gh command targets from every selector gh's own help names, and
// fails closed: an argument it cannot place is refused, naming it, and so is a command naming two
// owners; only a command naming no repository at all is not named, once the working directory's
// git remotes name no owner but the token's, and a command gh only prints the help or version of
// sends nothing. GH_REPO names the owner only when nothing explicit does, and only for a leaf that
// reads it.
func TestGhTargetOfFailsClosed(t *testing.T) {
	acme := map[string]string{"GH_REPO": "acme/widgets"}
	// A pane of the issue owner acme, whose GH_CONFIG_DIR is acme's directory of the bundle.
	pane := map[string]string{"GH_CONFIG_DIR": "/owners/acme", "LEGION_GH_OWNERS_DIR": "/owners"}
	paneWith := func(name, value string) map[string]string {
		env := map[string]string{name: value}
		maps.Copy(env, pane)
		return env
	}
	remotes := func(listed ...ghRemote) func() ([]ghRemote, error) {
		return func() ([]ghRemote, error) { return listed, nil }
	}
	acmeClone := ghRemote{name: "origin", url: "https://github.com/Acme/widgets.git"}
	sjawharClone := ghRemote{name: "origin", url: "https://github.com/sjawhar/legion"}
	// A gh whose pr view lists an option a later gh might add, naming a repository.
	ghWithARepositoryOption := editedGhHelp(map[string][2]string{
		"pr view": {"\nINHERITED FLAGS\n", "      --mirror string    Read the pull request from this repository instead\n\nINHERITED FLAGS\n"},
	})
	for _, tc := range []struct {
		argv    []string
		env     map[string]string
		help    func([]string) (string, error) // savedGhHelp when nil
		remotes func() ([]ghRemote, error)     // a working directory that is no git repository when nil
		owner   string                         // "" with no refusal: not named
		err     string                         // a refusal containing this
	}{
		// The final gates' at 96e1672cd, as round 4 reads them. A command naming no repository acts
		// on the working directory's: every github.com remote and gh-resolved setting names an
		// owner, and they must name one.
		{argv: []string{"pr", "view", "1"}, env: pane, remotes: remotes(acmeClone), owner: "acme"},
		{argv: []string{"pr", "view", "1"}, env: pane, remotes: remotes(acmeClone, ghRemote{name: "origin", resolved: "base"}), owner: "acme"},
		{argv: []string{"pr", "view", "1"}, env: pane, remotes: remotes(sjawharClone), owner: "sjawhar"},
		{argv: []string{"repo", "view"}, env: pane, remotes: remotes(sjawharClone), owner: "sjawhar"},
		{argv: []string{"api", "repos/{owner}/{repo}"}, env: pane, remotes: remotes(sjawharClone), owner: "sjawhar"},
		{argv: []string{"pr", "list"}, env: pane, remotes: remotes(ghRemote{name: "origin", url: "https://github.com/octocat/hello"}), owner: "octocat"},
		{argv: []string{"pr", "list"}, env: pane, remotes: remotes(acmeClone, ghRemote{name: "upstream", url: "git@github.com:sjawhar/legion.git"}), err: `more than one owner, acme (the working directory's remote "origin" (https://github.com/Acme/widgets.git)) and sjawhar (the working directory's remote "upstream" (git@github.com:sjawhar/legion.git))`},
		{argv: []string{"pr", "list"}, env: pane, remotes: remotes(acmeClone, ghRemote{name: "origin", resolved: "sjawhar/legion"}), err: "sjawhar (the working directory's remote.origin.gh-resolved sjawhar/legion)"},
		{argv: []string{"pr", "list"}, env: pane, remotes: remotes(acmeClone, ghRemote{name: "fork", url: "ssh://git@ssh.github.com:443/Sjawhar/legion"}), err: "more than one owner"},
		{argv: []string{"pr", "list"}, env: pane, remotes: remotes(acmeClone, ghRemote{name: "work", url: "git@gh-work:sjawhar/legion.git"}), err: "is an ssh remote whose host gh may read as github.com"},
		{argv: []string{"pr", "list"}, env: pane, remotes: remotes(acmeClone, ghRemote{name: "mirror", url: "https://gitlab.example.com/sjawhar/legion"}, ghRemote{name: "local", url: "/srv/legion"}), owner: "acme"},
		{argv: []string{"pr", "list"}, env: map[string]string{"GH_CONFIG_DIR": "/owners", "LEGION_GH_OWNERS_DIR": "/owners"}, remotes: remotes(acmeClone), owner: "acme"},
		{argv: []string{"pr", "list"}, env: pane, remotes: func() ([]ghRemote, error) { return nil, fmt.Errorf("bad config line 3") }, err: "cannot be read (bad config line 3)"},
		{argv: []string{"-R", "acme/widgets", "pr", "view", "1"}, env: pane, remotes: remotes(sjawharClone), owner: "acme"},
		{argv: []string{"pr", "list"}, env: paneWith("GH_REPO", "acme/widgets"), remotes: remotes(sjawharClone), owner: "acme"},
		{argv: []string{"pr", "view", "--help"}, env: pane, remotes: remotes(sjawharClone)},
		// Round 4's: a remote URL gh reads as github.com's whatever its scheme, as gh's own parser
		// reads it (deep 2, quality A, final B1).
		{argv: []string{"pr", "list"}, remotes: remotes(acmeClone, ghRemote{name: "upstream", url: "git+https://github.com/sjawhar/legion.git"}), err: "more than one owner"},
		{argv: []string{"pr", "list"}, remotes: remotes(acmeClone, ghRemote{name: "upstream", url: "ftp://github.com/sjawhar/legion"}), err: "more than one owner"},
		{argv: []string{"pr", "list"}, remotes: remotes(acmeClone, ghRemote{name: "upstream", url: "ftps://github.com/sjawhar/legion"}), err: "more than one owner"},
		{argv: []string{"pr", "list"}, remotes: remotes(acmeClone, ghRemote{name: "upstream", url: "file://github.com/sjawhar/legion"}), err: "more than one owner"},
		{argv: []string{"pr", "list"}, remotes: remotes(acmeClone, ghRemote{name: "upstream", url: "github.com://sjawhar/legion"}), err: "more than one owner"},
		{argv: []string{"pr", "list"}, remotes: remotes(acmeClone, ghRemote{name: "upstream", url: "git@github.com/sjawhar:legion"}), err: "more than one owner"},
		{argv: []string{"pr", "list"}, remotes: remotes(ghRemote{name: "origin", url: "git+https://github.com/sjawhar/legion"}), owner: "sjawhar"},
		{argv: []string{"pr", "list"}, remotes: remotes(acmeClone, ghRemote{name: "upstream", url: "GIT+HTTPS://github.com/sjawhar/legion"}), err: "is an ssh remote whose host gh may read as github.com"},
		{argv: []string{"pr", "list"}, remotes: remotes(acmeClone, ghRemote{name: "upstream", url: "https://github.com/%zz/legion"}), err: "is a URL this cannot read as gh does"},
		{argv: []string{"pr", "list"}, remotes: remotes(acmeClone, ghRemote{name: "upstream", url: "https://github.com/sjawhar/legion/extra"}), err: "names no OWNER/REPO"},
		// Round 4's: an option or another repository naming one owner while gh takes the command's
		// own repository from the working directory or GH_REPO (deep 1, quality B, final B2).
		{argv: []string{"pr", "comment", "5", "--body", "https://github.com/sjawhar/legion/pull/2"}, remotes: remotes(acmeClone), owner: "acme"},
		{argv: []string{"issue", "edit", "5", "--add-blocked-by", "https://github.com/sjawhar/legion/issues/9"}, remotes: remotes(acmeClone), err: "more than one owner"},
		{argv: []string{"pr", "comment", "1", "--body", "https://github.com/acme/widgets/pull/9"}, remotes: remotes(sjawharClone), owner: "sjawhar"},
		{argv: []string{"pr", "comment", "1", "--body", "https://github.com/sjawhar/legion/pull/2"}, remotes: remotes(ghRemote{name: "origin", url: "https://github.com/octocat/hello"}), owner: "octocat"},
		{argv: []string{"issue", "close", "3", "--duplicate-of", "https://github.com/acme/widgets/issues/1"}, remotes: remotes(sjawharClone), err: "more than one owner"},
		{argv: []string{"issue", "edit", "3", "--add-sub-issue", "https://github.com/acme/widgets/issues/1"}, remotes: remotes(sjawharClone), err: "more than one owner"},
		{argv: []string{"issue", "create", "--title", "t", "--body", "b", "--parent", "https://github.com/acme/widgets/issues/1"}, remotes: remotes(sjawharClone), err: "more than one owner"},
		{argv: []string{"repo", "sync", "--source", "acme/widgets"}, remotes: remotes(sjawharClone), err: "more than one owner"},
		{argv: []string{"issue", "develop", "3", "--branch-repo", "acme/widgets", "--name", "x"}, remotes: remotes(sjawharClone), err: "more than one owner"},
		{argv: []string{"issue", "close", "3", "--duplicate-of", "https://github.com/sjawhar/legion/issues/5"}, env: acme, err: "more than one owner, acme (GH_REPO acme/widgets) and sjawhar"},
		{argv: []string{"repo", "fork", "--org", "sjawhar", "--clone=false", "--remote=false"}, remotes: remotes(acmeClone), err: "more than one owner"},
		{argv: []string{"repo", "fork", "acme/widgets", "--org", "sjawhar", "--clone=false"}, remotes: remotes(acmeClone), err: "more than one owner"},
		{argv: []string{"issue", "transfer", "3", "sjawhar/legion"}, remotes: remotes(sjawharClone), owner: "sjawhar"},
		{argv: []string{"issue", "transfer", "3", "sjawhar/legion"}, remotes: remotes(acmeClone), err: "more than one owner"},
		// Round 4's minor findings: options mentioning URLs or a team, and project link and unlink,
		// which read GH_REPO with -R a plain string (deep 3, deep 4).
		{argv: []string{"issue", "create", "--title", "t", "--body", "b", "--blocked-by", "1,https://github.com/sjawhar/legion/issues/5"}, remotes: remotes(acmeClone), err: "more than one owner"},
		{argv: []string{"issue", "create", "--title", "t", "--body", "b", "--blocking", "https://github.com/acme/widgets/issues/5"}, remotes: remotes(acmeClone), owner: "acme"},
		{argv: []string{"project", "link", "1"}, env: map[string]string{"GH_REPO": "sjawhar/legion"}, remotes: remotes(acmeClone), owner: "sjawhar"},
		{argv: []string{"project", "link", "1", "--owner", "acme"}, env: map[string]string{"GH_REPO": "sjawhar/legion"}, err: "more than one owner"},
		{argv: []string{"project", "unlink", "1", "--owner", "acme", "--team", "sjawhar/core"}, err: "more than one owner"},
		{argv: []string{"project", "link", "1", "--team", "core", "-R", "acme/widgets"}, err: `"core" names no owner`},
		// Round 5's: gh takes a passthrough leaf's repository from the first argument even after
		// `--` (deep 1); `repo set-default` reads every remote's network whatever its argument
		// names (deep 2, quality 1); project link and unlink read no repository of their own
		// with --team (quality 2); a pr command reads OWNER/REPO#N and a URL other than a pull
		// request's as a branch of its own repository, and only agent-task view reads OWNER/REPO#N
		// (final F1).
		{argv: []string{"repo", "fork", "--", "sjawhar/legion"}, remotes: remotes(acmeClone), owner: "sjawhar"},
		{argv: []string{"repo", "fork", "--org", "acme", "--", "sjawhar/legion"}, err: "more than one owner"},
		{argv: []string{"repo", "fork", "--", "sjawhar/legion", "--depth=1"}, owner: "sjawhar"},
		{argv: []string{"repo", "clone", "--", "sjawhar/legion"}, remotes: remotes(acmeClone), owner: "sjawhar"},
		{argv: []string{"repo", "clone", "--", "sjawhar/legion", "--depth=1", "-R", "acme/w"}, owner: "sjawhar"},
		{argv: []string{"repo", "set-default", "sjawhar/legion"}, remotes: remotes(acmeClone), err: "more than one owner"},
		{argv: []string{"repo", "set-default", "sjawhar/legion"}, remotes: remotes(sjawharClone), owner: "sjawhar"},
		{argv: []string{"project", "link", "1", "--team", "sjawhar/core"}, remotes: remotes(acmeClone), owner: "sjawhar"},
		{argv: []string{"project", "unlink", "1", "--team", "sjawhar/core"}, env: acme, owner: "sjawhar"},
		{argv: []string{"pr", "comment", "sjawhar/legion#2", "--body", "hi"}, remotes: remotes(ghRemote{name: "origin", url: "https://github.com/octocat/hello"}), owner: "octocat"},
		{argv: []string{"pr", "view", "sjawhar/legion#2"}, remotes: remotes(acmeClone), owner: "acme"},
		{argv: []string{"pr", "view", "https://github.com/sjawhar/legion/issues/5"}, remotes: remotes(acmeClone), err: "more than one owner"},
		{argv: []string{"pr", "view", "https://github.com/sjawhar/legion"}, remotes: remotes(acmeClone), err: "more than one owner"},
		{argv: []string{"pr", "view", "https://github.com/sjawhar/legion/tree/x"}, remotes: remotes(sjawharClone), owner: "sjawhar"},
		{argv: []string{"pr", "view", "https://github.com/sjawhar/legion/pull/2/files"}, remotes: remotes(acmeClone), owner: "sjawhar"},
		{argv: []string{"issue", "edit", "https://github.com/sjawhar/legion/issues/5", "--title", "x"}, remotes: remotes(acmeClone), owner: "sjawhar"},
		// --version is gh's alone; before anything, gh reads the next word as a command, an
		// alias's or an extension's among them.
		{argv: []string{"--version"}},
		{argv: []string{"--version", "hello"}, help: ghWithAnExtensionAndANestedAlias, err: `"--version" comes before the command's name`},
		{argv: []string{"--version", "co"}, err: `"--version" comes before the command's name`},
		{argv: []string{"-R", "sjawhar/legion", "--version", "hello"}, help: ghWithAnExtensionAndANestedAlias, err: `"--version" comes before the command's name`},
		{argv: []string{"--help", "--version"}, err: `"--version" comes before the command's name`},
		// A first word gh's own help does not list, whose help gh may answer by installing an
		// extension, and the commands that run another program on gh's token.
		{argv: []string{"stack", "view"}, err: `"stack" is no command gh's own help lists`},
		{argv: []string{"ext", "exec", "hello"}, err: `"ext" is no command gh's own help lists`},
		{argv: []string{"extension", "exec", "hello"}, err: "`gh extension exec` runs another program"},
		{argv: []string{"help", "extension", "exec"}, err: "`gh extension exec` runs another program"},
		// Options name a repository as arguments do: by ghOptionKinds, an issue by URL among them,
		// and one a later gh adds refused; a URL in an option naming none is text.
		{argv: []string{"issue", "edit", "1", "--add-blocked-by", "https://github.com/sjawhar/legion/issues/2", "-R", "acme/widgets"}, err: "more than one owner"},
		{argv: []string{"issue", "edit", "1", "--add-sub-issue", "2", "-R", "acme/widgets"}, owner: "acme"},
		{argv: []string{"issue", "create", "--title", "t", "--body", "b", "--parent", "https://github.com/sjawhar/legion/issues/1"}, owner: "sjawhar"},
		{argv: []string{"repo", "sync", "acme/widgets", "--source", "sjawhar/legion"}, err: "more than one owner"},
		{argv: []string{"pr", "comment", "1", "--body", "https://github.com/sjawhar/legion/pull/2", "-R", "acme/widgets"}, owner: "acme"},
		{argv: []string{"pr", "comment", "1", "--body", "https://example.com/sjawhar/legion", "-R", "acme/widgets"}, owner: "acme"},
		{argv: []string{"repo", "edit", "acme/widgets", "--homepage", "https://github.com/sjawhar/legion"}, owner: "acme"},
		{argv: []string{"pr", "view", "1", "--mirror", "sjawhar/legion", "-R", "acme/widgets"}, help: ghWithARepositoryOption, err: "names a repository, an owner, a team or a URL in a way this does not know"},
		// gh api's placeholders as gh reads them, the colon form too, in a typed field's value.
		{argv: []string{"api", "graphql", "-F", "o=:owner", "-F", "r=:repo", "-f", "query=q"}, env: map[string]string{"GH_REPO": "sjawhar/legion"}, owner: "sjawhar"},
		{argv: []string{"api", "repos/:owner/:repo/pulls"}, env: map[string]string{"GH_REPO": "sjawhar/legion"}, owner: "sjawhar"},
		{argv: []string{"api", "graphql", "-f", "o=:owner", "-f", "query=q"}, env: map[string]string{"GH_REPO": "sjawhar/legion"}},
		{argv: []string{"api", "repos/{owner}/{repo}/commits/{branch}"}, env: map[string]string{"GH_REPO": "sjawhar/legion"}, owner: "sjawhar"},
		// agent-task view reads OWNER/REPO#N as that repository, and a URL through gh's pr
		// finder: only a pull request's names its repository. A pr command reads OWNER/REPO#N as a
		// branch of its own repository.
		{argv: []string{"agent-task", "view", "sjawhar/legion#1"}, owner: "sjawhar"},
		{argv: []string{"agent-task", "view", "https://github.com/sjawhar/legion/pull/1"}, remotes: remotes(acmeClone), owner: "sjawhar"},
		{argv: []string{"agent-task", "view", "https://github.com/sjawhar/legion/issues/1"}, remotes: remotes(acmeClone), err: "more than one owner"},
		{argv: []string{"pr", "view", "sjawhar/legion#1", "-R", "acme/widgets"}, owner: "acme"},
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
		// The third re-review's: gh's help of an extension runs the extension, so the target of
		// `gh help` is read as a command's path, a gh alias or an extension refused before anything
		// runs; gh's own help and its topics are not. And a plural slot, `{<numbers> | <urls>}`,
		// takes one or more arguments, every one a selector.
		{argv: []string{"help"}},
		{argv: []string{"help", "pr", "view"}},
		{argv: []string{"help", "environment"}},
		{argv: []string{"help", "co"}, err: `"co" is a gh alias`},
		{argv: []string{"help", "hello"}, help: ghWithAnExtensionAndANestedAlias, err: `"hello" is a gh extension`},
		{argv: []string{"help", "pr", "pv2"}, help: ghWithAnExtensionAndANestedAlias, err: `"pv2" is a gh alias`},
		{argv: []string{"-R", "acme/widgets", "help", "hello"}, help: ghWithAnExtensionAndANestedAlias, err: `"hello" is a gh extension`},
		{argv: []string{"help", "--bogus", "hello"}, err: "\"--bogus\" is an option of `gh help`"},
		{argv: []string{"issue", "edit", "1", "2", "--title", "x", "-R", "sjawhar/legion"}, owner: "sjawhar"},
		{argv: []string{"issue", "edit", "https://github.com/sjawhar/legion/issues/1", "https://github.com/sjawhar/legion/issues/2", "--title", "x"}, owner: "sjawhar"},
		{argv: []string{"issue", "edit", "https://github.com/sjawhar/legion/issues/1", "https://github.com/acme/widgets/issues/2", "--title", "x"}, err: "more than one owner"},
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
		{argv: []string{"nonexist"}, err: `"nonexist" is no command gh's own help lists`},
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
		{argv: []string{"issue", "transfer", "3", "sjawhar/legion"}, owner: "sjawhar"},
		{argv: []string{"label", "clone", "acme/widgets", "-R", "sjawhar/legion"}, err: "more than one owner"},
		{argv: []string{"secret", "set", "TOKEN", "--org", "acme", "-R", "sjawhar/x"}, err: "more than one owner"},
		{argv: []string{"auth", "status", "-h", "ghe.example.com"}, err: "another host"},
	} {
		help, listed := tc.help, tc.remotes
		if help == nil {
			help = savedGhHelp
		}
		if listed == nil {
			listed = noGitRepository
		}
		owner, err := ghTargetOf(tc.argv, tc.env, help, listed)
		switch {
		case tc.err != "":
			if err == nil || !strings.Contains(err.Error(), tc.err) {
				t.Errorf("ghTargetOf(%q, %v) = %q, %v; want a refusal containing %q", tc.argv, tc.env, owner, err, tc.err)
			}
		case err != nil || owner != tc.owner:
			t.Errorf("ghTargetOf(%q, %v) = %q, %v; want %q", tc.argv, tc.env, owner, err, tc.owner)
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
		_, err := ghTargetOf(strings.Fields(argv), nil, savedGhHelp, noGitRepository)
		if err == nil || !strings.Contains(err.Error(), want) || !strings.Contains(err.Error(), "sends no token") {
			t.Errorf("ghTargetOf(%q) = %v, want a refusal saying it sends no token and %q", argv, err, want)
		}
	}
}

// noGitRepository is the git remotes of a working directory that is no git repository: none.
func noGitRepository() ([]ghRemote, error) { return nil, nil }

// gh reads the repository of a command naming none from the working directory's git remotes, and
// readGitRemotes reads them with git as gh does: every fetch and push URL `git remote -v` prints,
// insteadOf applied, and every remote's gh-resolved setting; a directory that is no git repository
// has none.
func TestReadGitRemotesReadsWhatGhReads(t *testing.T) {
	if _, err := exec.LookPath("git"); err != nil {
		t.Skip("no git on this machine")
	}
	t.Setenv("GIT_CONFIG_NOSYSTEM", "1")
	t.Setenv("GIT_CONFIG_GLOBAL", os.DevNull)
	t.Chdir(t.TempDir())
	if remotes, err := readGitRemotes(); err != nil || remotes != nil {
		t.Errorf("readGitRemotes outside a git repository = %v, %v; want none", remotes, err)
	}
	for _, args := range [][]string{
		{"init", "-q"},
		{"remote", "add", "origin", "https://github.com/acme/widgets.git"},
		{"config", "remote.origin.pushurl", "https://github.com/acme/pushed.git"},
		{"remote", "add", "upstream", "gh:sjawhar/legion"},
		{"config", "url.git@github.com:.insteadOf", "gh:"},
		{"config", "remote.origin.gh-resolved", "base"},
	} {
		if out, err := exec.Command("git", args...).CombinedOutput(); err != nil {
			t.Fatalf("git %v: %v: %s", args, err, out)
		}
	}
	remotes, err := readGitRemotes()
	want := []ghRemote{
		{name: "origin", url: "https://github.com/acme/widgets.git"},
		{name: "origin", url: "https://github.com/acme/pushed.git"},
		{name: "upstream", url: "git@github.com:sjawhar/legion"},
		{name: "upstream", url: "git@github.com:sjawhar/legion"},
		{name: "origin", resolved: "base"},
	}
	if err != nil || !slices.Equal(remotes, want) {
		t.Errorf("readGitRemotes = %+v, %v; want %+v", remotes, err, want)
	}
}

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
