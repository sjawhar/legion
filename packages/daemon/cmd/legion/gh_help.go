package main

import (
	"fmt"
	"regexp"
	"slices"
	"strings"
)

// ghFlag is one option of a gh command as its help lists it: whether it takes a value, whether
// only after `=` (pflag's NoOptDefVal, `--commit string[="last"]`), whether its help cannot say
// (a placeholder that is itself an option, or a count), the placeholder naming its value, and what
// its value selects (ghOptionKinds).
type ghFlag struct {
	value, optional, uncertain bool
	placeholder, description   string
	selects                    string
}

// ghArg is one name of an argument a leaf's USAGE line names, with its ghArgKind, or none when
// ghArgKinds holds no such name.
type ghArg struct {
	name  string
	kind  ghArgKind
	known bool
}

// ghSlot is one argument a leaf's USAGE line names: the names it may take
// (`[<number> | <url> | <branch>]` is number, url and branch) and whether it takes more than one.
type ghSlot struct {
	args     []ghArg
	variadic bool
}

// ghCommand is what gh's help of one command says: its canonical path (an alias resolved) and the
// cobra aliases it lists for itself; for a group of subcommands, every subcommand and help topic it
// lists, and the gh aliases and extensions among them; and, for a leaf, its options, its arguments
// and whether its USAGE passes on the arguments after `--`.
type ghCommand struct {
	path                          []string
	aliases                       map[string]bool
	group                         bool
	listed, ghAliases, extensions map[string]bool
	flags                         map[string]ghFlag
	shorts                        map[byte]string
	slots                         []ghSlot
	passthrough                   bool
}

var (
	// ghFlagLine is one option line of gh's help: an optional shorthand, the long name, the
	// placeholder naming its value when it takes one, with pflag's optional-value marker, and the
	// option's description.
	ghFlagLine = regexp.MustCompile(`^\s+(?:-([A-Za-z0-9]), )?--([A-Za-z0-9][A-Za-z0-9-]*)(?: (\S+))?(?:\s{2,}(.*)|\s*)$`)
	// ghCommandWord is one word of a command path.
	ghCommandWord = regexp.MustCompile(`^[a-z0-9][a-z0-9-]*$`)
	// ghSlotName is a named argument of a USAGE line, `<name>`.
	ghSlotName = regexp.MustCompile(`<([^>]+)>`)
	// ghListedCommand is one entry of a help's COMMANDS or HELP TOPICS section,
	// `  <name>:  <description>`.
	ghListedCommand = regexp.MustCompile(`^\s+(\S+):(?:\s|$)`)
)

// ghRunsAnotherProgram are the commands that hand gh's token to a program gh does not hold, by
// their canonical paths: refused before their help is read.
var ghRunsAnotherProgram = map[string]bool{"copilot": true, "extension exec": true}

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
// with a `… COMMANDS` section, or a USAGE naming `<command>`, is a group's: its COMMANDS and HELP
// TOPICS sections list what may follow it, and its ALIAS COMMANDS and EXTENSION COMMANDS sections
// the gh aliases and extensions among them. A leaf's help must list its options, --help among
// them, every one in a form this reads; anything else is an error, since an option's arity decides
// which argument is a repository.
func parseGhHelp(requested []string, text string) (ghCommand, error) {
	spelled := strings.Join(append([]string{"gh"}, requested...), " ")
	what := "`" + spelled + "`"
	lines := strings.Split(text, "\n")
	var usage, section string
	cmd := ghCommand{
		aliases: map[string]bool{}, listed: map[string]bool{}, ghAliases: map[string]bool{}, extensions: map[string]bool{},
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
			case strings.HasSuffix(section, "COMMANDS") || section == "HELP TOPICS":
				listed := ghListedCommand.FindStringSubmatch(line)
				if listed == nil {
					return ghCommand{}, fmt.Errorf("cannot read %s's %s: %q; run gh's own commands, with -R <owner>/<repo>", what, section, entry)
				}
				cmd.listed[listed[1]] = true
				switch section {
				case "ALIAS COMMANDS":
					cmd.ghAliases[listed[1]] = true
				case "EXTENSION COMMANDS":
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
	for name, flag := range cmd.flags {
		flag.selects = ghOptionKind(cmd.path, name, flag)
		cmd.flags[name] = flag
	}
	cmd.slots, cmd.passthrough = ghSlots(cmd.path, args)
	return cmd, nil
}

// ghUsageNamesSubcommands says whether the arguments of a USAGE line, after the command's path,
// are a group's: its subcommand first.
func ghUsageNamesSubcommands(args string) bool {
	return strings.HasPrefix(args, "<command>") || strings.HasPrefix(args, "[subcommand]") || strings.HasPrefix(args, "<subcommand>")
}

// ghHelpTarget reads the words of `gh help <words>` as the path of the command whose help it asks
// for, refusing what ghTargetOf refuses running it directly (ghWalk.check): gh runs `gh help
// <extension>` as `gh <extension> --help`, which runs the extension on gh's token. gh's own help, a
// command's and a help topic's, sends nothing, and is not named. A word past a leaf is that leaf's
// argument, which gh's help ignores; an option, which `gh help` takes none of, is an error.
func ghHelpTarget(words []string, help func(path []string) (string, error)) error {
	walk := ghWalk{help: help}
	for i, word := range words {
		if strings.HasPrefix(word, "-") {
			return cannotPlace(word, "is an option of `gh help`, which takes only the names of commands", false)
		}
		if err := walk.check(word); err != nil {
			return err
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

// check refuses word, reading gh's own help for the first: a command that runs another program
// (ghRunsAnotherProgram), a gh alias or an extension its parent lists (mayRead), and a first word
// gh's own help does not list, whose help gh may answer by installing an extension for it (gh's
// official-extension stubs, `gh stack` and the like).
func (w *ghWalk) check(word string) error {
	if w.parent == nil {
		gh, err := readGhCommand(nil, w.help)
		if err != nil {
			return err
		}
		w.parent = &gh
	}
	if path := strings.Join(append(slices.Clone(w.path), word), " "); ghRunsAnotherProgram[path] {
		return fmt.Errorf("cannot tell which repository the command targets: `gh %s` runs another program, which reads gh's token itself, so it sends no token; run gh's own commands, with -R <owner>/<repo>", path)
	}
	if err := w.parent.mayRead(word); err != nil {
		return err
	}
	if len(w.path) == 0 && !w.parent.listed[word] {
		return fmt.Errorf("cannot tell which repository the command targets: %q is no command gh's own help lists, and gh may answer it by installing an extension, so it sends no token; run gh's own commands, with -R <owner>/<repo>", word)
	}
	return nil
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
	flag := ghFlag{placeholder: match[3], description: match[4]}
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

// ghSlots reads the arguments of the leaf path's USAGE line after the command's path: each
// `<name>`, `[…]` or `{…}` group naming arguments is a slot; `[flags]`, an option a usage names
// (`[--owner | --repo]`, `-s <shell>`) and a `[-- <gitflags>...]` passthrough are not, and the
// last marks passthrough.
func ghSlots(path []string, args string) (slots []ghSlot, passthrough bool) {
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
			slot := ghSlot{variadic: strings.Contains(group, "...")}
			for _, name := range names {
				kind, known := ghArgKindOf(path, name)
				slot.args = append(slot.args, ghArg{name: name, kind: kind, known: known})
			}
			slots = append(slots, slot)
		}
	}
	return slots, passthrough
}

// ghArgKind is what one argument name of gh's usages selects — a repository, another repository a
// command acts on beside its own, an owner, a URL that may name a repository, a search query, the
// repository a command creates (OWNER/NAME, or a name alone, which names none), an issue or pull
// request number (which names a repository written OWNER/REPO#N), or nothing ("") — and whether it
// stands for one or more arguments, as a plural name does with no `...` (`gh issue edit
// {<numbers> | <urls>}` edits one or more issues).
type ghArgKind struct {
	kind string
	many bool
}

// ghSlotKinds is the ghArgKind of each argument name gh's usages spell, gh 2.98.0's and 2.65.0's,
// every command of their references; a leaf's quirk (ghLeafQuirks) may give one of its own names
// another. A name neither holds is one this cannot read: an argument filling it is refused, never
// read as naming nothing.
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
	"number":                 {kind: "number"},
	"numbers":                {kind: "number", many: true},
	"pr-number":              {kind: "number"},
	// Arguments naming no repository, several of them in one slot.
	"files": {many: true}, "sources": {many: true},
	// Arguments naming no repository, one to a slot. A discussion's comment-id may be a node id of
	// a discussion in any repository, which, as a GraphQL query's node id, names none gh could
	// resolve.
	"alias": {}, "asset-name": {}, "branch": {}, "cache-id": {}, "cache-key": {}, "comment-id": {},
	"commit-sha": {}, "commit-SHA": {}, "dest": {}, "directory": {}, "expansion": {}, "file-path": {},
	"filename": {}, "flag": {}, "gist": {}, "id": {}, "image-uri": {}, "key": {}, "key-file": {},
	"key-id": {}, "keyPrefix": {}, "license-key": {}, "local-port": {}, "name": {}, "new-filename": {},
	"newFilename": {}, "new-name": {}, "old-filename": {}, "oldFilename": {}, "path": {}, "pattern": {},
	"pr-branch": {}, "prompt": {}, "remote-port": {}, "ruleset-id": {}, "run-id": {}, "secret-name": {},
	"session-id": {}, "shell": {}, "skill": {}, "skill[@version]": {}, "spdx-id": {}, "SPDX-ID": {},
	"tag": {}, "task description": {}, "template": {}, "type": {}, "urlTemplate": {}, "value": {},
	"variable-name": {}, "workflow-id": {}, "workflow-name": {},
}

// ghLeafQuirk is where gh, for one leaf, departs from what its help says, in what the leaf reads
// as its repository: GH_REPO for its own repository though its -R is a plain string
// (readsGHRepo); every remote's network whatever names its repository (readsAllRemotes);
// OWNER/REPO#N in a number's place as that repository's (readsRepositoryNumber); a URL as naming
// its pull request, and so its repository, only at /OWNER/REPO/pull/N, any other as the name of a
// branch of its own repository (pullRequestFinder); an option naming its target in place of its
// own repository (targetOption); and argument names of its own kind (slotKinds).
type ghLeafQuirk struct {
	readsGHRepo, readsAllRemotes, readsRepositoryNumber, pullRequestFinder bool
	targetOption                                                           string
	slotKinds                                                              map[string]ghArgKind
}

// ghLeafQuirks are gh 2.98.0's and 2.65.0's quirks, by leaf path, read from gh's source: project
// link and unlink call gh's repository override with -R a plain string, and act on --team when it
// is given; repo set-default resolves every remote's network before it reads its argument; agent-
// task view is gh's one reader of OWNER/REPO#N (ParseFullReference) and reads a URL through the pr
// finder, as every pr leaf does (quirkOf); and repo create's <name> is the repository it creates.
var ghLeafQuirks = map[string]ghLeafQuirk{
	"project link":     {readsGHRepo: true, targetOption: "team"},
	"project unlink":   {readsGHRepo: true, targetOption: "team"},
	"repo set-default": {readsAllRemotes: true},
	"agent-task view":  {readsRepositoryNumber: true, pullRequestFinder: true},
	"repo create":      {slotKinds: map[string]ghArgKind{"name": {kind: "new-repository"}}},
}

// quirkOf is the leaf path's ghLeafQuirk, every pr leaf reading a URL through gh's pr finder.
func quirkOf(path []string) ghLeafQuirk {
	quirk := ghLeafQuirks[strings.Join(path, " ")]
	if len(path) > 0 && path[0] == "pr" {
		quirk.pullRequestFinder = true
	}
	return quirk
}

// ghArgKindOf is the ghArgKind of the argument name in the usage of the leaf path, its quirk's or
// ghSlotKinds', and whether either holds it.
func ghArgKindOf(path []string, name string) (ghArgKind, bool) {
	if kind, ok := quirkOf(path).slotKinds[name]; ok {
		return kind, true
	}
	kind, ok := ghSlotKinds[name]
	return kind, ok
}

// ghKindRank orders the kinds of a slot's names: the slot is of its most specific name's kind
// (`[<number> | <url> | <branch>]` is a URL's when the argument is one).
var ghKindRank = map[string]int{"": 0, "number": 1, "new-repository": 2, "url": 3, "query": 4, "owner": 5, "second-repository": 6, "repository": 7}

// kind is what a slot's argument selects, its most specific name's kind, or the first of its names
// neither table holds, which makes it a slot this cannot read.
func (s ghSlot) kind() (kind, unknown string) {
	if len(s.args) == 0 {
		return "", "an argument with no name"
	}
	for _, arg := range s.args {
		if !arg.known {
			return "", arg.name
		}
		if ghKindRank[arg.kind.kind] > ghKindRank[kind] {
			kind = arg.kind.kind
		}
	}
	return kind, ""
}

// takes says whether one of the slot's names is of kind: a slot whose most specific name is a URL
// may still take a number, which may name its repository (OWNER/REPO#N).
func (s ghSlot) takes(kind string) bool {
	return slices.ContainsFunc(s.args, func(arg ghArg) bool { return arg.kind.kind == kind })
}

// takesMany says whether the slot takes one or more arguments: spelled with `...`, or naming one
// the tables take as several (a plural name, a search query's words).
func (s ghSlot) takesMany() bool {
	return s.variadic || slices.ContainsFunc(s.args, func(arg ghArg) bool { return arg.kind.many })
}

// slotsFor is the slot each of n positional arguments fills, in order, as gh's usage places them
// (slotIndexes).
func (c *ghCommand) slotsFor(n int) ([]ghSlot, bool) {
	indexes, ok := c.slotIndexes(n)
	placed := make([]ghSlot, len(indexes))
	for i, index := range indexes {
		placed[i] = c.slots[index]
	}
	return placed, ok
}

// slotIndexes is the index in c.slots of the slot each of n positional arguments fills, in order,
// as gh's usage places them: the first slot taking more than one takes every argument the slots
// after it leave (`gh codespace cp <sources>... <dest>`), and a usage with none takes no more
// arguments than it has slots.
func (c *ghCommand) slotIndexes(n int) ([]int, bool) {
	many := slices.IndexFunc(c.slots, ghSlot.takesMany)
	if many < 0 && n > len(c.slots) {
		return nil, false
	}
	after := len(c.slots) - many - 1
	indexes := make([]int, n)
	for i := range indexes {
		switch {
		case many < 0 || i < many:
			indexes[i] = i
		case i >= max(n-after, many+1):
			indexes[i] = len(c.slots) - (n - i)
		default:
			indexes[i] = many
		}
	}
	return indexes, true
}

// readsOwnRepository says whether gh resolves the leaf's own repository, from GH_REPO or the
// working directory, when n positional arguments are given and nothing else names it: a leaf that
// reads GH_REPO (readsGHRepo), and one whose usage has a slot for a repository the n arguments
// leave empty (`gh repo view [<repository>]`, `gh repo fork [<repository>]`, `gh repo sync
// [<destination-repository>]`), where gh takes the working directory's.
func (c *ghCommand) readsOwnRepository(n int) bool {
	if c.readsGHRepo() {
		return true
	}
	indexes, _ := c.slotIndexes(n)
	for index, slot := range c.slots {
		if (slot.takes("repository") || slot.takes("second-repository")) && !slices.Contains(indexes, index) {
			return true
		}
	}
	return false
}

// ghOptionKinds is what the value of each option of gh's helps that names a repository, an owner,
// an organization, a team or a URL selects, keyed `--<name> <placeholder>`, or `<path> --<name>
// <placeholder>` for one whose meaning is one command's own: a repository, the repositories of the
// command's --org ("org-repositories", a name alone that organization's), an owner, a host, a URL
// that may name a repository ("url": an issue or pull request by number or URL), or nothing
// ("nothing"). It holds every such option of gh 2.98.0's and 2.65.0's references: an option whose
// help mentions one that it does not hold is one this cannot read, refused when it is given. An
// option that mentions none is no selector: gh reads its value as text (a body, a title), never as
// a repository to act on.
var ghOptionKinds = map[string]string{
	"--repo [HOST/]OWNER/REPO":     "repository",
	"--repo OWNER/REPO":            "repository",
	"--repo string":                "repository",
	"--repo strings":               "repository",
	"--branch-repo string":         "repository",
	"--template repository":        "repository",
	"--project owner/number":       "repository",
	"repo sync --source string":    "repository",
	"--repos repositories":         "org-repositories",
	"--owner string":               "owner",
	"--owner strings":              "owner",
	"--org string":                 "owner",
	"--org organization":           "owner",
	"--org login":                  "owner",
	"--repo-owner string":          "owner",
	"--source-owner string":        "owner",
	"--target-owner string":        "owner",
	"--hostname string":            "host",
	"--url string":                 "url",
	"--add-blocked-by number":      "url",
	"--add-blocking number":        "url",
	"--remove-blocked-by number":   "url",
	"--remove-blocking number":     "url",
	"--add-sub-issue number":       "url",
	"--remove-sub-issue number":    "url",
	"--parent number":              "url",
	"--duplicate-of string":        "url",
	"--blocked-by numbers":         "url",
	"--blocking numbers":           "url",
	"project link --team string":   "repository", // OWNER/TEAM: the team's organization owns it
	"project unlink --team string": "repository", // likewise
	"--team-mentions string":       "nothing",    // a search filter, not a repository gh acts on
	"--reviewer handle":            "nothing",    // a user or team of the pull request's own repository
	"--review-requested user":      "nothing",    // a search filter, likewise
	"--signer-repo string":         "nothing",    // matched against an attestation, never requested
	"--signer-workflow string":     "nothing",    // likewise
	"--source-digest string":       "nothing",    // likewise
	"--source-ref string":          "nothing",    // likewise
	"--tuf-url string":             "nothing",    // a TUF mirror, not GitHub
	"--exclude strings":            "nothing",    // repositories left out of `gh status`
	"--head string":                "nothing",    // a branch
	"--branch string":              "nothing",    // a branch
	"--homepage URL":               "nothing",    // the new repository's home page
	"--team name":                  "nothing",    // a team of the new repository's organization
	"repo create --source string":  "nothing",    // a local directory
	"--fork-name string":           "nothing",
	"--remote string":              "nothing",
	"--description string":         "nothing",
	"--gitignore string":           "nothing",
	"--license string":             "nothing",
	"--default-branch name":        "nothing",
	"--add-topic strings":          "nothing",
	"--remove-topic strings":       "nothing",
	"--visibility string":          "nothing",
	"--visibility strings":         "nothing",
	"--limit int":                  "nothing",
	"--order string":               "nothing",
	"--sort string":                "nothing",
	"--match strings":              "nothing",
	"--include-forks string":       "nothing",
	"--user username":              "nothing", // a user of the --org, which names the owner
}

// ghMentionsSelector is an option's description or placeholder that mentions a repository, an
// owner, an organization, a team or URLs.
var ghMentionsSelector = regexp.MustCompile(`(?i)repositor|owner|organi[sz]ation|\borg\b|\burls?\b|\brepo\b|\bteams?\b`)

// ghOptionKind is what the value of the leaf path's option name selects: its ghOptionKinds kind,
// "unknown" for one whose help mentions a repository, an owner, an organization, a team or URLs
// that the table does not hold, and "" for any other, which selects nothing. An option taking no
// value selects nothing.
func ghOptionKind(path []string, name string, flag ghFlag) string {
	if !flag.value {
		return "nothing"
	}
	key := "--" + name + " " + flag.placeholder
	if kind, ok := ghOptionKinds[strings.Join(path, " ")+" "+key]; ok {
		return kind
	}
	if kind, ok := ghOptionKinds[key]; ok {
		return kind
	}
	if ghMentionsSelector.MatchString(flag.description) || ghMentionsSelector.MatchString(flag.placeholder) {
		return "unknown"
	}
	return ""
}

// readsGHRepo says whether the leaf reads GH_REPO for its own repository: it takes the inherited
// -R, or its quirk says it does.
func (c *ghCommand) readsGHRepo() bool {
	return c.flags["repo"].placeholder == "[HOST/]OWNER/REPO" || quirkOf(c.path).readsGHRepo
}

// isAPI says whether the leaf is gh api, whose argument is an endpoint and which has no -R.
func (c *ghCommand) isAPI() bool { return slices.Equal(c.path, []string{"api"}) }

// quoted is the leaf as a refusal names it, `gh <path>`.
func (c *ghCommand) quoted() string { return "`gh " + strings.Join(c.path, " ") + "`" }
