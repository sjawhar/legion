package main

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"io"
	"maps"
	"net/url"
	"os"
	"os/exec"
	"regexp"
	"slices"
	"strconv"
	"strings"
	"syscall"
	"time"

	"github.com/sjawhar/legion/daemon/internal/ghconfig"
)

const ghUsage = "usage: legion gh [gh args…]  (the gh first on a Legion pane's or pod's PATH: runs LEGION_GH_PATH with GH_CONFIG_DIR set to the gh directory of the owner the command names, under LEGION_GH_OWNERS_DIR)"

// runGh is `legion gh`, the gh a pane or pod runs. It reads the owner of the repository the
// command acts on as gh resolves it (ghTargetOf) and execs the real gh (LEGION_GH_PATH) as `gh`
// with its arguments untouched and GH_CONFIG_DIR set to that owner's gh directory under
// LEGION_GH_OWNERS_DIR; a command naming no repository keeps GH_CONFIG_DIR, the issue's owner's.
// A command it cannot read for certain, one naming two owners, and one naming an owner the bundle
// holds no directory for are refused, exit 2, naming why, and the real gh never runs: no token but
// the target owner's is ever sent. Each of the three variables is runtime-set and required; an
// unset one is refused, exit 2, naming it. With no LEGION_GH_PATH, --help prints this command's
// usage, so `legion gh --help` outside a pane answers as every legion command does.
func runGh(_ context.Context, args []string, _, stderr io.Writer) int {
	env := processEnvironment()
	if env["LEGION_GH_PATH"] == "" && len(args) == 1 && helpRequested(args[0]) {
		fmt.Fprintln(stderr, ghUsage)
		return 0
	}
	for _, name := range []string{"LEGION_GH_PATH", "GH_CONFIG_DIR", "LEGION_GH_OWNERS_DIR"} {
		if env[name] == "" {
			fmt.Fprintf(stderr, "legion gh: %s is unset: a Legion pane or pod sets it, and nothing else runs this gh\n%s\n", name, ghUsage)
			return 2
		}
	}
	gh := env["LEGION_GH_PATH"]
	owner, err := ghTargetOf(args, env, func(path []string) (string, error) { return readGhHelp(gh, path) })
	if err != nil {
		fmt.Fprintf(stderr, "legion gh: %v\n", err)
		return 2
	}
	dir := env["GH_CONFIG_DIR"]
	if owner != "" {
		if dir, err = ownerGHConfigDir(env["LEGION_GH_OWNERS_DIR"], owner); err != nil {
			fmt.Fprintf(stderr, "legion gh: %v\n", err)
			return 2
		}
	}
	childEnv := make([]string, 0, len(env))
	for _, pair := range os.Environ() {
		if !strings.HasPrefix(pair, "GH_CONFIG_DIR=") {
			childEnv = append(childEnv, pair)
		}
	}
	childEnv = append(childEnv, "GH_CONFIG_DIR="+dir)
	err = syscall.Exec(gh, append([]string{"gh"}, args...), childEnv)
	fmt.Fprintf(stderr, "legion gh: run %s: %v\n", gh, err)
	return 1
}

// ghTargetOf is the owner, lowercased, of the repository the gh command argv (gh's arguments,
// without `gh`) acts on, empty when the command names none. It fails closed: whenever it cannot be
// certain which owner the command targets it is an error, which names the argument it could not
// place, and nothing is sent. Only a command that names no repository at all names no owner, and
// gh then acts on its working directory's repository, the issue's; a command whose last --help is
// true names none either, since gh only prints its help.
//
// It reads the command as gh does, from gh's own help of each command on the way (help): the
// path of subcommands down to the leaf the arguments run, every option's arity, and the arguments
// the leaf's USAGE line names. A word its parent's help lists as a gh alias, whose expansion may
// name a repository of its own, or as an extension, which reads gh's token itself, is an error
// before its help is read, since reading it would run them; so is such a word as the target of
// `gh help`, which for an extension gh runs (ghHelpTarget), and `gh copilot`, which runs another
// program on gh's token, and a word whose help is another command's that lists no such alias of
// its own. Before the leaf only the inherited -R/--repo and --help/-h may come, read with
// the leaf's own options; anything else there, a subcommand gh does not know, help that lists an
// option incompletely or not at all, an option the leaf's help does not list, and an argument its
// USAGE has no place for or names in a way this does not know (ghSlotKinds) are errors. The leaf's
// options are read with pflag's rules, and after `--` the rest are its arguments, unless its
// USAGE passes them on (`gh repo clone <repository> [<directory>] [-- <gitflags>...]`).
//
// Every selector the leaf's help names counts: a `<repository>` (or another repository) argument,
// an `<owner>`, a URL argument that is a github.com URL, a search `<query>`'s repo:/owner:/user:/
// org: qualifiers, `gh api`'s endpoint (repos/<owner>/, orgs/<owner>/, users/<owner>/, a search
// endpoint's q) and a GraphQL query's owner variable (`-F owner=<login>`), and the options naming
// a repository or an owner (-R/--repo, --repos, a name in --repos under --org, --owner, --org …).
// They must all name one owner; two are an error. Only when none names one does GH_REPO name the
// owner, for a leaf that reads it: one with the inherited -R, and gh api when its arguments hold a
// {owner} or {repo} placeholder gh fills from it. A GraphQL query naming its repository otherwise,
// by a node id or in the query text, names none gh could resolve, and runs as the issue owner's.
// GH_HOST naming another host than github.com, where gh would send no token of the bundle's, and
// a GH_TOKEN or GITHUB_TOKEN, which gh sends in place of any directory's token, are errors.
func ghTargetOf(argv []string, env map[string]string, help func(path []string) (string, error)) (string, error) {
	if host := env["GH_HOST"]; host != "" && !strings.EqualFold(host, "github.com") {
		return "", fmt.Errorf("GH_HOST is %q, another host than github.com, so it sends no token; unset GH_HOST", host)
	}
	for _, name := range []string{"GH_TOKEN", "GITHUB_TOKEN"} {
		if env[name] != "" {
			return "", fmt.Errorf("%s is set, and gh sends it in place of the token of the owner the command targets, so it sends no token; unset %s", name, name)
		}
	}
	walk := ghWalk{help: help}
	var rest []string
	var cmd *ghCommand
	for i := 0; i < len(argv) && cmd == nil; i++ {
		arg := argv[i]
		switch {
		case len(walk.path) == 0 && arg == "--version":
			return "", nil
		case len(walk.path) == 0 && arg == "help":
			return "", ghHelpTarget(argv[i+1:], help)
		case arg == "-R" || arg == "--repo":
			if i+1 == len(argv) {
				return "", cannotPlace(arg, "has no value", false)
			}
			rest = append(rest, arg, argv[i+1])
			i++
		case strings.HasPrefix(arg, "--repo=") || (strings.HasPrefix(arg, "-R") && len(arg) > 2) ||
			arg == "--help" || strings.HasPrefix(arg, "--help=") || arg == "-h":
			// The leaf reads these as its own, in order with the rest of its arguments.
			rest = append(rest, arg)
		case strings.HasPrefix(arg, "-"):
			return "", cannotPlace(arg, "comes before the command's name, and only -R/--repo and --help may", false)
		default:
			if err := walk.check(arg); err != nil {
				return "", err
			}
			read, err := walk.read(arg)
			if err != nil {
				return "", err
			}
			if !read.group {
				cmd = &read
				rest = append(rest, argv[i+1:]...)
			}
		}
	}
	if cmd == nil {
		// No leaf: `gh`, or a group alone, which prints its help.
		return "", nil
	}
	if slices.Equal(cmd.path, []string{"copilot"}) {
		return "", errors.New("cannot tell which repository the command targets: `gh copilot` runs another program, which reads gh's token itself, so it sends no token; run gh's own commands, with -R <owner>/<repo>")
	}
	parsed, err := cmd.parse(rest)
	if err != nil || parsed.help {
		return "", err
	}
	sel := selectors{api: cmd.isAPI(), owners: map[string]string{}}
	if err := sel.fromCommand(cmd, parsed); err != nil {
		return "", err
	}
	switch len(sel.owners) {
	case 1:
		for owner := range sel.owners {
			return owner, nil
		}
	case 0:
		// GH_REPO names the repository of a leaf with the inherited -R, and of gh api only through
		// the {owner} and {repo} placeholders gh fills from it.
		readsGHRepo := cmd.readsGHRepo()
		if sel.api {
			args := strings.Join(rest, " ")
			readsGHRepo = strings.Contains(args, "{owner}") || strings.Contains(args, "{repo}")
		}
		if !readsGHRepo || env["GH_REPO"] == "" {
			return "", nil
		}
		return repositoryOwner(env["GH_REPO"], "GH_REPO", sel.api)
	}
	var listed []string
	for owner, what := range sel.owners {
		listed = append(listed, fmt.Sprintf("%s (%s)", owner, what))
	}
	slices.Sort(listed)
	return "", fmt.Errorf("the command names more than one owner, %s, so it sends no token; name one repository", strings.Join(listed, " and "))
}

// cannotPlace is ghTargetOf's refusal of an argument it cannot place: what it is and why, and how
// to name the repository instead, which for gh api is its endpoint (it has no -R).
func cannotPlace(arg, why string, api bool) error {
	advice := "pass -R <owner>/<repo>"
	if api {
		advice = "name it in the endpoint, repos/<owner>/<repo>/…"
	}
	return fmt.Errorf("cannot tell which repository the command targets: %q %s, so it sends no token; %s", arg, why, advice)
}

// ghFlag is one option of a gh command as its help lists it: whether it takes a value, whether
// only after `=` (pflag's NoOptDefVal, `--commit string[="last"]`), whether its help cannot say
// (a placeholder that is itself an option, or a count), and the placeholder naming its value.
type ghFlag struct {
	value, optional, uncertain bool
	placeholder                string
}

// ghSlot is one argument a leaf's USAGE line names: the names it may take
// (`[<number> | <url> | <branch>]` is number, url and branch) and whether it takes more than one.
type ghSlot struct {
	names    []string
	variadic bool
}

// ghCommand is what gh's help of one command says: its canonical path (an alias resolved) and the
// cobra aliases it lists for itself; for a group of subcommands, the gh aliases and extensions it
// lists among them; and, for a leaf, its options, its arguments and whether its USAGE passes on
// the arguments after `--`.
type ghCommand struct {
	path                  []string
	aliases               map[string]bool
	group                 bool
	ghAliases, extensions map[string]bool
	flags                 map[string]ghFlag
	shorts                map[byte]string
	slots                 []ghSlot
	passthrough           bool
}

var (
	// ghFlagLine is one option line of gh's help: an optional shorthand, the long name, the
	// placeholder naming its value when it takes one, and pflag's optional-value marker.
	ghFlagLine = regexp.MustCompile(`^\s+(?:-([A-Za-z0-9]), )?--([A-Za-z0-9][A-Za-z0-9-]*)(?: (\S+))?(?:\s{2,}|\s*$)`)
	// ghCommandWord is one word of a command path.
	ghCommandWord = regexp.MustCompile(`^[a-z0-9][a-z0-9-]*$`)
	// ghSlotName is a named argument of a USAGE line, `<name>`.
	ghSlotName = regexp.MustCompile(`<([^>]+)>`)
	// ghListedCommand is one entry of a help's COMMANDS section, `  <name>:  <description>`.
	ghListedCommand = regexp.MustCompile(`^\s+(\S+):(?:\s|$)`)
)

// readGhCommand reads gh's help of path and parses it (parseGhHelp).
func readGhCommand(path []string, help func(path []string) (string, error)) (ghCommand, error) {
	text, err := help(path)
	if err != nil {
		return ghCommand{}, fmt.Errorf("cannot read `%s`: %w; run gh's own command, and pass -R <owner>/<repo> to name its repository", strings.Join(append([]string{"gh"}, path...), " "), err)
	}
	return parseGhHelp(path, text)
}

// parseGhHelp reads gh's help of the command requested, gh's own for none. Its USAGE line names
// the command's canonical path, which is requested itself or a cobra alias of it the help's
// ALIASES section lists; a path that is a strict prefix of requested is gh's help of a parent,
// which gh prints for a subcommand it does not know, and any other path is the help of the command
// a gh alias stands for, whose expansion may carry arguments of its own: both are errors. A help
// with a `… COMMANDS` section, or a USAGE naming `<command>`, is a group's, and its ALIAS COMMANDS
// and EXTENSION COMMANDS sections list the gh aliases and extensions among its subcommands. A
// leaf's help must list its options, --help among them, every one in a form this reads; anything
// else is an error, since an option's arity decides which argument is a repository.
func parseGhHelp(requested []string, text string) (ghCommand, error) {
	spelled := strings.Join(append([]string{"gh"}, requested...), " ")
	what := "`" + spelled + "`"
	lines := strings.Split(text, "\n")
	var usage, section string
	cmd := ghCommand{
		aliases: map[string]bool{}, ghAliases: map[string]bool{}, extensions: map[string]bool{},
		flags: map[string]ghFlag{}, shorts: map[byte]string{},
	}
	for i, line := range lines {
		if line == "" || line[0] == ' ' || line[0] == '\t' {
			entry := strings.TrimSpace(line)
			switch {
			case entry == "":
			case section == "FLAGS" || section == "INHERITED FLAGS":
				if strings.HasPrefix(entry, "-") {
					if err := cmd.addFlag(line); err != nil {
						return ghCommand{}, fmt.Errorf("cannot read the options of %s: %w; pass -R <owner>/<repo>", what, err)
					}
				}
			case section == "ALIASES":
				for _, alias := range strings.Split(entry, ",") {
					cmd.aliases[strings.TrimSpace(alias)] = true
				}
			case section == "ALIAS COMMANDS" || section == "EXTENSION COMMANDS":
				listed := ghListedCommand.FindStringSubmatch(line)
				if listed == nil {
					return ghCommand{}, fmt.Errorf("cannot read %s's %s: %q; run gh's own commands, with -R <owner>/<repo>", what, section, entry)
				}
				if section == "ALIAS COMMANDS" {
					cmd.ghAliases[listed[1]] = true
				} else {
					cmd.extensions[listed[1]] = true
				}
			}
			continue
		}
		section = strings.TrimSpace(line)
		if strings.HasSuffix(section, "COMMANDS") {
			cmd.group = true
		}
		if section == "USAGE" && usage == "" {
			for _, next := range lines[i+1:] {
				if strings.TrimSpace(next) != "" {
					usage = strings.TrimSpace(next)
					break
				}
			}
		}
	}
	words := strings.Fields(usage)
	if len(words) == 0 || words[0] != "gh" {
		return ghCommand{}, fmt.Errorf("cannot read %s: its help names no usage; run gh's own command, and pass -R <owner>/<repo> to name its repository", what)
	}
	n := 1
	for n < len(words) && ghCommandWord.MatchString(words[n]) {
		n++
	}
	cmd.path = words[1:n]
	switch {
	case len(cmd.path) < len(requested) && slices.Equal(cmd.path, requested[:len(cmd.path)]):
		return ghCommand{}, fmt.Errorf("gh has no command %s, so it sends no token; pass -R <owner>/<repo>", what)
	case !slices.Equal(cmd.path, requested) && !cmd.aliases[spelled]:
		return ghCommand{}, fmt.Errorf("cannot tell which repository the command targets: %s prints the help of `gh %s`, which lists no such alias, so it is a gh alias, whose expansion may name a repository of its own, and it sends no token; run the gh command it stands for, with -R <owner>/<repo>", what, strings.Join(cmd.path, " "))
	}
	args := strings.Join(words[n:], " ")
	if ghUsageNamesSubcommands(args) {
		cmd.group = true
	}
	if cmd.group {
		return cmd, nil
	}
	if _, ok := cmd.flags["help"]; !ok {
		return ghCommand{}, fmt.Errorf("cannot read the options of %s: its help lists no --help, so it is no command's whole help; pass -R <owner>/<repo>", what)
	}
	cmd.slots, cmd.passthrough = ghSlots(args)
	return cmd, nil
}

// ghUsageNamesSubcommands says whether the arguments of a USAGE line, after the command's path,
// are a group's: its subcommand first.
func ghUsageNamesSubcommands(args string) bool {
	return strings.HasPrefix(args, "<command>") || strings.HasPrefix(args, "[subcommand]") || strings.HasPrefix(args, "<subcommand>")
}

// ghHelpTarget reads the words of `gh help <words>` as the path of the command whose help it asks
// for, refusing a gh alias or an extension among them as ghTargetOf refuses one run directly: gh
// runs `gh help <extension>` as `gh <extension> --help`, which runs the extension on gh's token.
// gh's own help, a command's and a help topic's, sends nothing, and is not named. A word past a
// leaf is that leaf's argument, which gh's help ignores; an option, which `gh help` takes none of,
// is an error.
func ghHelpTarget(words []string, help func(path []string) (string, error)) error {
	walk := ghWalk{help: help}
	for i, word := range words {
		if err := walk.check(word); err != nil {
			return err
		}
		if strings.HasPrefix(word, "-") {
			return cannotPlace(word, "is an option of `gh help`, which takes only the names of commands", false)
		}
		if i == len(words)-1 {
			return nil
		}
		read, err := walk.read(word)
		if err != nil || !read.group {
			return err
		}
	}
	return nil
}

// ghWalk walks a command path down gh's own help (help), gh's for the first word: each word is
// checked against its parent's help before its own is read (check), since reading the help of a
// gh alias or an extension the parent lists runs it, then read as the command after the path so
// far (read), a group the parent of the next word.
type ghWalk struct {
	help   func(path []string) (string, error)
	parent *ghCommand
	path   []string
}

// check refuses word when its parent's help lists it as a gh alias or an extension (mayRead),
// reading gh's own help for the first word.
func (w *ghWalk) check(word string) error {
	if w.parent == nil {
		gh, err := readGhCommand(nil, w.help)
		if err != nil {
			return err
		}
		w.parent = &gh
	}
	return w.parent.mayRead(word)
}

// read reads word's help as the command after the walk's path, which becomes the command's
// canonical one, and a group the parent of the next word.
func (w *ghWalk) read(word string) (ghCommand, error) {
	cmd, err := readGhCommand(append(slices.Clone(w.path), word), w.help)
	if err != nil {
		return ghCommand{}, err
	}
	w.path = cmd.path
	if cmd.group {
		w.parent = &cmd
	}
	return cmd, nil
}

// mayRead refuses word as a subcommand of the group c when c's help lists it as a gh alias, whose
// expansion may name a repository of its own, or an extension, which reads gh's token itself:
// reading the help of either runs it, a shell alias or an extension with whatever it holds.
func (c *ghCommand) mayRead(word string) error {
	switch {
	case c.ghAliases[word]:
		return fmt.Errorf("cannot tell which repository the command targets: %q is a gh alias, whose expansion may name a repository of its own, so it sends no token; run the gh command it stands for, with -R <owner>/<repo>", word)
	case c.extensions[word]:
		return fmt.Errorf("cannot tell which repository the command targets: %q is a gh extension, which reads gh's token itself, so it sends no token; run gh's own commands, with -R <owner>/<repo>", word)
	}
	return nil
}

// addFlag records one option line of a FLAGS section, refusing one this cannot read.
func (c *ghCommand) addFlag(line string) error {
	match := ghFlagLine.FindStringSubmatch(line)
	if match == nil {
		return fmt.Errorf("an option line %q", strings.TrimSpace(line))
	}
	flag := ghFlag{placeholder: match[3]}
	if flag.placeholder != "" {
		flag.value = true
		if before, _, found := strings.Cut(flag.placeholder, "[="); found && before != "" {
			flag.optional, flag.placeholder = true, before
		}
		// pflag names a count's type, which takes no value, as it names a value's.
		if strings.HasPrefix(flag.placeholder, "-") || flag.placeholder == "count" {
			flag.uncertain = true
		}
	}
	c.flags[match[2]] = flag
	if match[1] != "" {
		c.shorts[match[1][0]] = match[2]
	}
	return nil
}

// ghSlots reads the arguments of a USAGE line after the command's path: each `<name>`, `[…]` or
// `{…}` group naming arguments is a slot; `[flags]`, an option a usage names (`[--owner | --repo]`,
// `-s <shell>`) and a `[-- <gitflags>...]` passthrough are not, and the last marks passthrough.
func ghSlots(args string) (slots []ghSlot, passthrough bool) {
	var groups []string
	depth, start := 0, -1
	for i, r := range args {
		switch {
		case r == '[' || r == '{':
			if depth == 0 {
				start = i
			}
			depth++
		case r == ']' || r == '}':
			depth--
			if depth == 0 && start >= 0 {
				groups = append(groups, args[start:i+1])
				start = -1
			}
		case r == ' ' && depth == 0:
			if start >= 0 {
				groups = append(groups, args[start:i])
				start = -1
			}
		default:
			if depth == 0 && start < 0 {
				start = i
			}
		}
	}
	if start >= 0 {
		groups = append(groups, args[start:])
	}
	for _, group := range groups {
		inner := strings.Trim(group, "[]{}")
		switch {
		case group == "[flags]":
		case strings.HasPrefix(inner, "-- ") || inner == "--":
			passthrough = true
		case strings.HasPrefix(inner, "-"):
		default:
			var names []string
			for _, m := range ghSlotName.FindAllStringSubmatch(group, -1) {
				names = append(names, m[1])
			}
			if len(names) == 0 {
				names = strings.Fields(strings.ReplaceAll(inner, "|", " "))
			}
			slots = append(slots, ghSlot{names: names, variadic: strings.Contains(group, "...")})
		}
	}
	return slots, passthrough
}

// ghArgKind is what one argument name of gh's usages selects — a repository, another repository a
// command acts on beside its own, an owner, a URL that may name a repository, a search query, a
// repository's name (`gh repo create [<name>]`, OWNER/NAME or a name alone), or nothing ("") — and
// whether it stands for one or more arguments, as a plural name does with no `...` (`gh issue edit
// {<numbers> | <urls>}` edits one or more issues).
type ghArgKind struct {
	kind string
	many bool
}

// ghSlotKinds is the ghArgKind of each argument name gh's usages spell, gh 2.98.0's and 2.65.0's,
// every command of their references. A name it does not hold is one this cannot read: an argument
// filling it is refused, never read as naming nothing.
var ghSlotKinds = map[string]ghArgKind{
	"repository":             {kind: "repository"},
	"destination-repo":       {kind: "second-repository"},
	"destination-repository": {kind: "second-repository"},
	"source-repository":      {kind: "second-repository"},
	"owner":                  {kind: "owner"},
	"url":                    {kind: "url"},
	"urls":                   {kind: "url", many: true},
	"pr-url":                 {kind: "url"},
	"discussion-url":         {kind: "url"},
	"comment-url":            {kind: "url"},
	"query":                  {kind: "query", many: true},
	"name":                   {kind: "name"},
	// Arguments naming no repository, several of them in one slot.
	"files": {many: true}, "numbers": {many: true}, "sources": {many: true},
	// Arguments naming no repository, one to a slot.
	"alias": {}, "asset-name": {}, "branch": {}, "cache-id": {}, "cache-key": {}, "comment-id": {},
	"commit-sha": {}, "commit-SHA": {}, "dest": {}, "directory": {}, "expansion": {}, "file-path": {},
	"filename": {}, "flag": {}, "gist": {}, "id": {}, "image-uri": {}, "key": {}, "key-file": {},
	"key-id": {}, "keyPrefix": {}, "license-key": {}, "local-port": {}, "new-filename": {},
	"newFilename": {}, "new-name": {}, "number": {}, "old-filename": {}, "oldFilename": {},
	"path": {}, "pattern": {}, "pr-branch": {}, "pr-number": {}, "prompt": {}, "remote-port": {},
	"ruleset-id": {}, "run-id": {}, "secret-name": {}, "session-id": {}, "shell": {}, "skill": {},
	"skill[@version]": {}, "spdx-id": {}, "SPDX-ID": {}, "tag": {}, "task description": {},
	"template": {}, "type": {}, "urlTemplate": {}, "value": {}, "variable-name": {},
	"workflow-id": {}, "workflow-name": {},
}

// ghKindRank orders the kinds of a slot's names: the slot is of its most specific name's kind
// (`[<number> | <url> | <branch>]` is a URL's when the argument is one).
var ghKindRank = map[string]int{"": 0, "name": 1, "url": 2, "query": 3, "owner": 4, "second-repository": 5, "repository": 6}

// kind is what a slot's argument selects, its most specific name's ghSlotKinds kind, or the first
// of its names ghSlotKinds does not hold, which makes it a slot this cannot read.
func (s ghSlot) kind() (kind, unknown string) {
	if len(s.names) == 0 {
		return "", "an argument with no name"
	}
	for _, name := range s.names {
		arg, known := ghSlotKinds[name]
		if !known {
			return "", name
		}
		if ghKindRank[arg.kind] > ghKindRank[kind] {
			kind = arg.kind
		}
	}
	return kind, ""
}

// takesMany says whether the slot takes one or more arguments: spelled with `...`, or naming one
// ghSlotKinds takes as several (a plural name, a search query's words).
func (s ghSlot) takesMany() bool {
	return s.variadic || slices.ContainsFunc(s.names, func(name string) bool { return ghSlotKinds[name].many })
}

// slotsFor is the slot each of n positional arguments fills, in order, as gh's usage places them:
// the first slot taking more than one takes every argument the slots after it leave (`gh codespace
// cp <sources>... <dest>`), and a usage with none takes no more arguments than it has slots.
func (c *ghCommand) slotsFor(n int) ([]ghSlot, bool) {
	many := slices.IndexFunc(c.slots, ghSlot.takesMany)
	if many < 0 {
		if n > len(c.slots) {
			return nil, false
		}
		return c.slots[:n], true
	}
	after := len(c.slots) - many - 1
	placed := make([]ghSlot, n)
	for i := range placed {
		switch {
		case i < many:
			placed[i] = c.slots[i]
		case i >= max(n-after, many+1):
			placed[i] = c.slots[len(c.slots)-(n-i)]
		default:
			placed[i] = c.slots[many]
		}
	}
	return placed, true
}

// readsGHRepo says whether the leaf takes the inherited -R, whose default GH_REPO is.
func (c *ghCommand) readsGHRepo() bool {
	return c.flags["repo"].placeholder == "[HOST/]OWNER/REPO"
}

// isAPI says whether the leaf is gh api, whose argument is an endpoint and which has no -R.
func (c *ghCommand) isAPI() bool { return slices.Equal(c.path, []string{"api"}) }

// quoted is the leaf as a refusal names it, `gh <path>`.
func (c *ghCommand) quoted() string { return "`gh " + strings.Join(c.path, " ") + "`" }

// ghArgs is a leaf's arguments as pflag reads them: its positional arguments in order, every
// value given each option, by long name, and whether gh only prints the leaf's help.
type ghArgs struct {
	positionals []string
	values      map[string][]string
	help        bool
}

// parse reads args, the arguments of the leaf with its path taken out, as pflag does, refusing an
// option the leaf's help does not list or does not say the arity of. --help is a boolean whose
// last value decides; a -h no option of the leaf takes is pflag's request for help, which ends
// the reading there.
func (c *ghCommand) parse(args []string) (ghArgs, error) {
	api, leaf := c.isAPI(), c.quoted()
	parsed := ghArgs{values: map[string][]string{}}
	for i := 0; i < len(args); i++ {
		arg := args[i]
		switch {
		case arg == "--":
			if !c.passthrough {
				parsed.positionals = append(parsed.positionals, args[i+1:]...)
			}
			return parsed, nil
		case strings.HasPrefix(arg, "--"):
			name, value, hasValue := strings.Cut(arg[2:], "=")
			flag, ok := c.flags[name]
			if !ok {
				return ghArgs{}, cannotPlace(arg, "is no option "+leaf+"'s help lists", api)
			}
			if flag.uncertain {
				return ghArgs{}, cannotPlace(arg, "is an option whose help does not say whether it takes a value", api)
			}
			if !hasValue {
				switch {
				case !flag.value:
					value, hasValue = "true", true
				case flag.optional:
					// Its default, which names nothing.
				case i+1 == len(args):
					return ghArgs{}, cannotPlace(arg, "has no value", api)
				default:
					i++
					value, hasValue = args[i], true
				}
			}
			if hasValue {
				if err := parsed.set(name, flag, value, arg, api); err != nil {
					return ghArgs{}, err
				}
			}
		case strings.HasPrefix(arg, "-") && arg != "-":
			// pflag's shorthands, each in turn: `-f=value` gives any its value; a boolean or an
			// optional value takes its default and leaves the rest of the word to the next
			// shorthand; any other takes the rest of the word, or the next argument.
			for shorts := arg[1:]; shorts != ""; {
				short, rest := shorts[0], shorts[1:]
				name, ok := c.shorts[short]
				if !ok {
					if short == 'h' {
						parsed.help = true
						return parsed, nil
					}
					return ghArgs{}, cannotPlace(arg, fmt.Sprintf("names -%c, which %s's help does not list", short, leaf), api)
				}
				flag := c.flags[name]
				if flag.uncertain {
					return ghArgs{}, cannotPlace(arg, "is an option whose help does not say whether it takes a value", api)
				}
				value, hasValue := "", true
				switch {
				case len(rest) > 1 && rest[0] == '=':
					value, rest = rest[1:], ""
				case !flag.value:
					value = "true"
				case flag.optional:
					hasValue = false
				case rest != "":
					value, rest = rest, ""
				case i+1 < len(args):
					i++
					value = args[i]
				default:
					return ghArgs{}, cannotPlace(arg, "has no value", api)
				}
				if hasValue {
					if err := parsed.set(name, flag, value, arg, api); err != nil {
						return ghArgs{}, err
					}
				}
				shorts = rest
			}
		default:
			parsed.positionals = append(parsed.positionals, arg)
		}
	}
	return parsed, nil
}

// set records value given the option name: --help's as a boolean, any other's taking a value in
// the order given.
func (p *ghArgs) set(name string, flag ghFlag, value, arg string, api bool) error {
	switch {
	case name == "help":
		help, err := strconv.ParseBool(value)
		if err != nil {
			return cannotPlace(arg, "gives --help a value that is no boolean", api)
		}
		p.help = help
	case flag.value:
		p.values[name] = append(p.values[name], value)
	}
	return nil
}

// selectors is every owner a command names, each with the argument that names it.
type selectors struct {
	api    bool
	owners map[string]string
}

func (s *selectors) add(owner, what string) {
	if _, ok := s.owners[owner]; !ok {
		s.owners[owner] = what
	}
}

func (s *selectors) repository(value, what string) error {
	owner, err := repositoryOwner(value, what, s.api)
	if err == nil {
		s.add(owner, what)
	}
	return err
}

func (s *selectors) owner(value, what string) error {
	owner, err := checkedOwner(value, what, s.api)
	if err == nil {
		s.add(owner, what)
	}
	return err
}

// fromCommand collects the owners the leaf's arguments and options name.
func (s *selectors) fromCommand(cmd *ghCommand, parsed ghArgs) error {
	leaf := cmd.quoted()
	targeted, second := len(parsed.values["repo"]) > 0 && cmd.readsGHRepo(), ""
	if s.api {
		if len(parsed.positionals) > 1 {
			return cannotPlace(parsed.positionals[1], "is an argument "+leaf+"'s usage has no place for", true)
		}
		search := false
		if len(parsed.positionals) == 1 {
			var err error
			if search, err = s.endpoint(parsed.positionals[0]); err != nil {
				return err
			}
		}
		// A GraphQL query's variables named for an owner (`-F owner=<login>`) name the owner it
		// reads; `{owner}` is GH_REPO's, which gh fills. Any other query names no repository gh
		// could resolve, and runs as the issue owner's. A search endpoint's q field is its query.
		graphql := len(parsed.positionals) == 1 && parsed.positionals[0] == "graphql"
		for _, option := range []string{"field", "raw-field"} {
			for _, field := range parsed.values[option] {
				key, value, _ := strings.Cut(field, "=")
				switch {
				case graphql && (key == "owner" || key == "org" || key == "organization" || key == "login") && value != "{owner}":
					if err := s.owner(value, "the field "+field); err != nil {
						return err
					}
				case search && key == "q":
					if option == "field" && strings.HasPrefix(value, "@") {
						return cannotPlace(field, "reads the search query from a file", true)
					}
					if err := s.query(strings.Fields(value)); err != nil {
						return err
					}
				}
			}
		}
	} else {
		placed, ok := cmd.slotsFor(len(parsed.positionals))
		if !ok {
			return cannotPlace(parsed.positionals[len(cmd.slots)], "is an argument "+leaf+"'s usage has no place for", false)
		}
		var query []string
		for i, arg := range parsed.positionals {
			kind, unknown := placed[i].kind()
			if unknown != "" {
				return cannotPlace(arg, fmt.Sprintf("fills <%s> of %s's usage, an argument this does not know", unknown, leaf), false)
			}
			switch kind {
			case "repository", "name":
				if kind == "name" && (!slices.Equal(cmd.path, []string{"repo", "create"}) || !strings.Contains(arg, "/")) {
					continue
				}
				if err := s.repository(arg, "the repository "+arg); err != nil {
					return err
				}
				targeted = true
			case "second-repository":
				if err := s.repository(arg, "the repository "+arg); err != nil {
					return err
				}
				second = arg
			case "owner":
				if err := s.owner(arg, "the owner "+arg); err != nil {
					return err
				}
				targeted = true
			case "url":
				if !isWebURL(arg) {
					continue
				}
				if err := s.repository(arg, "the URL "+arg); err != nil {
					return err
				}
				targeted = true
			case "query":
				query = append(query, arg)
			}
		}
		if err := s.query(query); err != nil {
			return err
		}
	}
	orgs := parsed.values["org"]
	for _, name := range slices.Sorted(maps.Keys(parsed.values)) {
		flag := cmd.flags[name]
		for _, value := range parsed.values[name] {
			for _, item := range strings.Split(value, ",") {
				what := "--" + name + " " + item
				switch {
				case name == "hostname":
					if !strings.EqualFold(item, "github.com") {
						return cannotPlace(what, "names another host than github.com", s.api)
					}
				case name == "owner" || name == "org" || name == "repo-owner" || name == "target-owner" || name == "source-owner":
					if err := s.owner(item, what); err != nil {
						return err
					}
				case name == "repos" && !strings.Contains(item, "/"):
					// gh reads a name alone as a repository of the --org, whose owner that is.
					if len(orgs) == 0 {
						return cannotPlace(what, "names a repository by its name alone, and no --org says whose", s.api)
					}
				case name == "repo" || name == "repos" || name == "branch-repo" ||
					flag.placeholder == "repository" || flag.placeholder == "repositories":
					if err := s.repository(item, what); err != nil {
						return err
					}
				case name == "url" && isWebURL(item):
					if err := s.repository(item, what); err != nil {
						return err
					}
				}
			}
		}
	}
	if second != "" && cmd.readsGHRepo() && !targeted {
		return cannotPlace(second, "names a second repository while the command's own repository is not named", false)
	}
	return nil
}

// ghQualifier is a search query's argument that gh passes on, and GitHub reads, as one qualifier:
// a name, a colon and a value holding no space, quote, parenthesis or backslash, negated by a
// leading "-".
var ghQualifier = regexp.MustCompile(`^(-?)([A-Za-z][A-Za-z-]*):([^\s"'()\\]+)$`)

// query collects the owners a search query names in its repo:<owner>/<repo>, owner:, user: and
// org: qualifiers, each word of words one argument gh joins into the query. A word holding a colon
// that is no plain qualifier is refused, since gh quotes a word holding a space and GitHub reads a
// quoted or grouped qualifier otherwise. So is a query naming an owner whose scope is not that
// owner's: one excluding an owner (`-repo:`), or joining its words with OR, NOT or parentheses.
func (s *selectors) query(words []string) error {
	var named, joined string
	for _, word := range words {
		if !strings.Contains(word, ":") {
			if joined == "" && (strings.EqualFold(word, "OR") || strings.EqualFold(word, "NOT") || strings.ContainsAny(word, "()")) {
				joined = word
			}
			continue
		}
		qualifier := ghQualifier.FindStringSubmatch(word)
		if qualifier == nil {
			return cannotPlace(word, "is a search qualifier this cannot read for certain, since gh and GitHub read a quoted, spaced or grouped one otherwise", s.api)
		}
		switch strings.ToLower(qualifier[2]) {
		case "repo", "owner", "user", "org":
			if qualifier[1] == "-" {
				return cannotPlace(word, "excludes an owner, so the search's scope is no one owner's", s.api)
			}
			if strings.EqualFold(qualifier[2], "repo") {
				if err := s.repository(qualifier[3], "the qualifier "+word); err != nil {
					return err
				}
			} else if err := s.owner(qualifier[3], "the qualifier "+word); err != nil {
				return err
			}
			if named == "" {
				named = word
			}
		}
	}
	if named != "" && joined != "" {
		return cannotPlace(joined, "joins the query's repo:/owner: qualifiers with others, so the search's scope is no one owner's ("+named+" and more)", s.api)
	}
	return nil
}

// endpoint collects the owner a gh api endpoint names: repos/<owner>/, orgs/<owner>/ or
// users/<owner>/, with or without the API's host before it, and a search endpoint's q, reporting
// whether it is a search endpoint, whose q field is its query too. Its {owner} placeholder names
// none, since GH_REPO fills it; a repository named by id (repositories/<id>) and another host are
// errors. gh reads an endpoint holding "://" as a whole URL.
func (s *selectors) endpoint(endpoint string) (search bool, err error) {
	path := endpoint
	if strings.Contains(endpoint, "://") {
		parsed, err := url.Parse(endpoint)
		if err != nil || !strings.EqualFold(parsed.Host, "api.github.com") {
			return false, cannotPlace(endpoint, "names another host than api.github.com", true)
		}
		path = parsed.Path
		if parsed.RawQuery != "" {
			path += "?" + parsed.RawQuery
		}
	}
	path, rawQuery, _ := strings.Cut(strings.TrimPrefix(path, "/"), "?")
	segments := strings.Split(path, "/")
	switch first := strings.ToLower(segments[0]); {
	case first == "repositories":
		return false, cannotPlace(endpoint, "names a repository by its id", true)
	case (first == "repos" || first == "orgs" || first == "users") && len(segments) > 1:
		if segments[1] == "{owner}" {
			return false, nil
		}
		return false, s.owner(segments[1], "the endpoint "+endpoint)
	case first == "search":
		query, err := url.ParseQuery(rawQuery)
		if err != nil {
			return true, cannotPlace(endpoint, "has a query string this cannot read", true)
		}
		for _, q := range query["q"] {
			if err := s.query(strings.Fields(q)); err != nil {
				return true, err
			}
		}
		return true, nil
	}
	return false, nil
}

// isWebURL says whether s is an http or https URL, as gh reads a URL argument: its scheme in any
// case.
func isWebURL(s string) bool {
	u, err := url.Parse(s)
	return err == nil && (u.Scheme == "http" || u.Scheme == "https")
}

// repositoryOwner is the owner, lowercased, of a repository named as gh names one — OWNER/REPO,
// HOST/OWNER/REPO, or a github.com URL — refused when it names no owner, another host, or an
// owner that is no GitHub login.
func repositoryOwner(value, what string, api bool) (string, error) {
	host, rest := "", value
	if parsed, err := url.Parse(value); err == nil && parsed.Scheme != "" && parsed.Host != "" {
		host, rest = parsed.Host, strings.Trim(parsed.Path, "/")
	} else if parts := strings.Split(strings.Trim(value, "/"), "/"); len(parts) >= 3 {
		host, rest = parts[0], strings.Join(parts[1:], "/")
	}
	if host != "" && !strings.EqualFold(host, "github.com") && !strings.EqualFold(host, "www.github.com") {
		return "", cannotPlace(value, "names another host than github.com", api)
	}
	parts := strings.Split(rest, "/")
	if len(parts) < 2 || parts[0] == "" || parts[1] == "" {
		return "", cannotPlace(value, "names no owner", api)
	}
	return checkedOwner(parts[0], what, api)
}

// checkedOwner is owner lowercased, refused when it is no GitHub login.
func checkedOwner(owner, what string, api bool) (string, error) {
	lower := strings.ToLower(owner)
	if !ghconfig.IsOwner(lower) {
		return "", cannotPlace(what, fmt.Sprintf("names the owner %q, which is no GitHub login", owner), api)
	}
	return lower, nil
}

// readGhHelp is the real gh's help of the command path, `gh <path> --help` (`gh --help` for
// gh's own), refused when gh answers it with an error.
func readGhHelp(gh string, path []string) (string, error) {
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	cmd := exec.CommandContext(ctx, gh, append(slices.Clone(path), "--help")...)
	var stdout, stderr bytes.Buffer
	cmd.Stdout, cmd.Stderr = &stdout, &stderr
	if err := cmd.Run(); err != nil {
		return "", fmt.Errorf("gh answered its help with %v: %s", err, strings.TrimSpace(stderr.String()))
	}
	if stdout.Len() == 0 {
		return "", errors.New("gh printed no help")
	}
	return stdout.String(), nil
}
