package main

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"io"
	"net/url"
	"os"
	"os/exec"
	"regexp"
	"slices"
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
	owner, named, err := ghTargetOf(args, env, func(path []string) (string, error) { return readGhHelp(gh, path) })
	if err != nil {
		fmt.Fprintf(stderr, "legion gh: %v\n", err)
		return 2
	}
	dir := env["GH_CONFIG_DIR"]
	if named {
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
// without `gh`) acts on, and whether the command names one at all. It fails closed: whenever it
// cannot be certain which owner the command targets it is an error, which names the argument it
// could not place, and nothing is sent. Only a command that names no repository at all is not
// named, and gh then acts on its working directory's repository, the issue's.
//
// It reads the command as gh does, from gh's own help of each command on the way (help): the
// path of subcommands down to the leaf the arguments run, every option's arity, and the arguments
// the leaf's USAGE line names. Before the leaf only the inherited -R/--repo and --help may come;
// anything else there, a subcommand gh does not know, help that lists an option incompletely or
// not at all, and an option the leaf's help does not list are errors. The leaf's options are read
// with pflag's rules, and after `--` the rest are its arguments, unless its USAGE passes them on
// (`gh repo clone <repository> [<directory>] [-- <gitflags>...]`).
//
// Every selector the leaf's help names counts: a `<repository>` (or another repository) argument,
// an `<owner>`, a URL argument that is a github.com URL, a search `<query>`'s repo:/owner:/user:/
// org: qualifiers, `gh api`'s endpoint (repos/<owner>/, orgs/<owner>/, users/<owner>/) and a
// GraphQL query's owner variable (`-F owner=<login>`), and the options naming a repository or an
// owner (-R/--repo, --repos, --owner, --org …). They must all name one owner; two are an error.
// Only when none names one does GH_REPO name the owner, for a leaf that reads it: one with the
// inherited -R, and gh api when its arguments hold a {owner} or {repo} placeholder gh fills from
// it. A GraphQL query naming its repository otherwise, by a node id or in the query text, names
// none gh could resolve, and runs as the issue owner's.
func ghTargetOf(argv []string, env map[string]string, help func(path []string) (string, error)) (owner string, named bool, err error) {
	var path, rest []string
	var cmd *ghCommand
	for i := 0; i < len(argv) && cmd == nil; i++ {
		arg := argv[i]
		switch {
		case arg == "--help" || arg == "-h" || (len(path) == 0 && (arg == "--version" || arg == "help")):
			return "", false, nil
		case arg == "-R" || arg == "--repo":
			if i+1 == len(argv) {
				return "", false, cannotPlace(arg, "has no value", false)
			}
			rest = append(rest, arg, argv[i+1])
			i++
		case strings.HasPrefix(arg, "--repo=") || (strings.HasPrefix(arg, "-R") && len(arg) > 2):
			rest = append(rest, arg)
		case strings.HasPrefix(arg, "-"):
			return "", false, cannotPlace(arg, "comes before the command's name, and only -R/--repo may", false)
		default:
			read, err := readGhCommand(append(slices.Clone(path), arg), help)
			if err != nil {
				return "", false, err
			}
			path = read.path
			if !read.group {
				cmd = &read
				rest = append(rest, argv[i+1:]...)
			}
		}
	}
	if cmd == nil {
		// No leaf: `gh`, or a group alone, which prints its help.
		return "", false, nil
	}
	api := slices.Equal(cmd.path, []string{"api"})
	parsed, err := cmd.parse(rest)
	if err != nil || parsed.help {
		return "", false, err
	}
	sel := selectors{api: api}
	if err := sel.fromCommand(cmd, parsed); err != nil {
		return "", false, err
	}
	switch len(sel.owners) {
	case 1:
		for owner := range sel.owners {
			return owner, true, nil
		}
	case 0:
		// GH_REPO names the repository of a leaf with the inherited -R, and of gh api only through
		// the {owner} and {repo} placeholders gh fills from it.
		readsGHRepo := cmd.readsGHRepo()
		if api {
			readsGHRepo = strings.Contains(strings.Join(rest, " "), "{owner}") || strings.Contains(strings.Join(rest, " "), "{repo}")
		}
		if !readsGHRepo || env["GH_REPO"] == "" {
			return "", false, nil
		}
		owner, err := repositoryOwner(env["GH_REPO"], "GH_REPO", api)
		return owner, err == nil, err
	}
	var listed []string
	for owner, what := range sel.owners {
		listed = append(listed, fmt.Sprintf("%s (%s)", owner, what))
	}
	slices.Sort(listed)
	return "", false, fmt.Errorf("the command names more than one owner, %s, so it sends no token; name one repository", strings.Join(listed, " and "))
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
// (a placeholder that is itself an option), and the placeholder naming its value.
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

// ghCommand is what gh's help of one command says: its canonical path (an alias resolved), whether
// it is a group of subcommands, and, for a leaf, its options, its arguments and whether its USAGE
// passes on the arguments after `--`.
type ghCommand struct {
	path        []string
	group       bool
	flags       map[string]ghFlag
	shorts      map[rune]string
	slots       []ghSlot
	passthrough bool
}

var (
	// ghFlagLine is one option line of gh's help: an optional shorthand, the long name, the
	// placeholder naming its value when it takes one, and pflag's optional-value marker.
	ghFlagLine = regexp.MustCompile(`^\s+(?:-([A-Za-z0-9]), )?--([A-Za-z0-9][A-Za-z0-9-]*)(?: (\S+))?(?:\s{2,}|\s*$)`)
	// ghCommandWord is one word of a command path.
	ghCommandWord = regexp.MustCompile(`^[a-z0-9][a-z0-9-]*$`)
	// ghSlotName is a named argument of a USAGE line, `<name>`.
	ghSlotName = regexp.MustCompile(`<([^>]+)>`)
)

// readGhCommand reads gh's help of path and parses it (parseGhHelp).
func readGhCommand(path []string, help func(path []string) (string, error)) (ghCommand, error) {
	text, err := help(path)
	if err != nil {
		return ghCommand{}, fmt.Errorf("cannot read `gh %s`: %w; run gh's own command, and pass -R <owner>/<repo> to name its repository", strings.Join(path, " "), err)
	}
	return parseGhHelp(path, text)
}

// parseGhHelp reads gh's help of the command requested. Its USAGE line names the command's
// canonical path, which is requested itself or, for an alias, the command it stands for; a path
// that is a strict prefix of requested is gh's help of a parent, which gh prints for a subcommand
// it does not know, and an error. A help with a `… COMMANDS` section, or a USAGE naming
// `<command>`, is a group's. A leaf's help must list its options, --help among them, every one in
// a form this reads; anything else is an error, since an option's arity decides which argument
// is a repository.
func parseGhHelp(requested []string, text string) (ghCommand, error) {
	what := "`gh " + strings.Join(requested, " ") + "`"
	lines := strings.Split(text, "\n")
	var usage, section string
	cmd := ghCommand{flags: map[string]ghFlag{}, shorts: map[rune]string{}}
	for i, line := range lines {
		if line == "" || line[0] == ' ' || line[0] == '\t' {
			if section == "FLAGS" || section == "INHERITED FLAGS" {
				if strings.HasPrefix(strings.TrimSpace(line), "-") {
					if err := cmd.addFlag(line); err != nil {
						return ghCommand{}, fmt.Errorf("cannot read the options of %s: %w; pass -R <owner>/<repo>", what, err)
					}
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
	if len(cmd.path) < len(requested) && slices.Equal(cmd.path, requested[:len(cmd.path)]) {
		return ghCommand{}, fmt.Errorf("gh has no command %s, so it sends no token; pass -R <owner>/<repo>", what)
	}
	if len(cmd.path) == 0 {
		return ghCommand{}, fmt.Errorf("cannot read %s: its usage %q names no command", what, usage)
	}
	args := strings.Join(words[n:], " ")
	if strings.HasPrefix(args, "<command>") || strings.HasPrefix(args, "[subcommand]") || strings.HasPrefix(args, "<subcommand>") {
		cmd.group = true
	}
	if cmd.group {
		return cmd, nil
	}
	if _, ok := cmd.flags["help"]; !ok {
		return ghCommand{}, fmt.Errorf("cannot read the options of %s: its help lists no --help, so it is no command's whole help; pass -R <owner>/<repo>", what)
	}
	if _, taken := cmd.shorts['h']; !taken {
		cmd.shorts['h'] = "help"
	}
	cmd.slots, cmd.passthrough = ghSlots(args)
	return cmd, nil
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
		if strings.HasPrefix(flag.placeholder, "-") {
			flag.uncertain = true
		}
	}
	c.flags[match[2]] = flag
	if match[1] != "" {
		c.shorts[[]rune(match[1])[0]] = match[2]
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

// readsGHRepo says whether the leaf takes the inherited -R, whose default GH_REPO is.
func (c *ghCommand) readsGHRepo() bool {
	return c.flags["repo"].placeholder == "[HOST/]OWNER/REPO"
}

// ghArgs is a leaf's arguments as pflag reads them: its positional arguments in order, every
// value of each option by long name, and whether --help is among them.
type ghArgs struct {
	positionals []string
	values      map[string][]string
	help        bool
}

// parse reads args, the arguments of the leaf with its path taken out, as pflag does, refusing an
// option the leaf's help does not list or does not say the arity of.
func (c *ghCommand) parse(args []string) (ghArgs, error) {
	api := slices.Equal(c.path, []string{"api"})
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
				return ghArgs{}, cannotPlace(arg, "is no option `gh "+strings.Join(c.path, " ")+"`'s help lists", api)
			}
			if name == "help" {
				parsed.help = true
				return parsed, nil
			}
			if flag.uncertain {
				return ghArgs{}, cannotPlace(arg, "is an option whose help does not say whether it takes a value", api)
			}
			if flag.value && !flag.optional && !hasValue {
				if i+1 == len(args) {
					return ghArgs{}, cannotPlace(arg, "has no value", api)
				}
				i++
				value, hasValue = args[i], true
			}
			if flag.value && hasValue {
				parsed.values[name] = append(parsed.values[name], value)
			}
		case strings.HasPrefix(arg, "-") && arg != "-":
			shorts := []rune(arg[1:])
			for j, short := range shorts {
				name, ok := c.shorts[short]
				if !ok {
					return ghArgs{}, cannotPlace(arg, fmt.Sprintf("names -%c, which `gh %s`'s help does not list", short, strings.Join(c.path, " ")), api)
				}
				if name == "help" {
					parsed.help = true
					return parsed, nil
				}
				flag := c.flags[name]
				if flag.uncertain || flag.optional {
					return ghArgs{}, cannotPlace(arg, "is an option whose help does not say how its shorthand takes a value", api)
				}
				if !flag.value {
					continue
				}
				value := strings.TrimPrefix(string(shorts[j+1:]), "=")
				if value == "" {
					if i+1 == len(args) {
						return ghArgs{}, cannotPlace(arg, "has no value", api)
					}
					i++
					value = args[i]
				}
				parsed.values[name] = append(parsed.values[name], value)
				break
			}
		default:
			parsed.positionals = append(parsed.positionals, arg)
		}
	}
	return parsed, nil
}

// selectors is every owner a command names, each with the argument that names it.
type selectors struct {
	api    bool
	owners map[string]string
}

func (s *selectors) add(owner, what string) {
	if s.owners == nil {
		s.owners = map[string]string{}
	}
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
	targeted, second := len(parsed.values["repo"]) > 0 && cmd.readsGHRepo(), ""
	if s.api {
		if len(parsed.positionals) > 0 {
			if err := s.endpoint(parsed.positionals[0]); err != nil {
				return err
			}
		}
		// A GraphQL query's variables named for an owner (`-F owner=<login>`) name the owner it
		// reads; `{owner}` is GH_REPO's, which gh fills. Any other query names no repository gh
		// could resolve, and runs as the issue owner's.
		if len(parsed.positionals) > 0 && parsed.positionals[0] == "graphql" {
			for _, field := range append(slices.Clone(parsed.values["field"]), parsed.values["raw-field"]...) {
				key, value, _ := strings.Cut(field, "=")
				if (key == "owner" || key == "org" || key == "organization" || key == "login") && value != "{owner}" {
					if err := s.owner(value, "the field "+field); err != nil {
						return err
					}
				}
			}
		}
	} else {
		for i, arg := range parsed.positionals {
			if len(cmd.slots) == 0 {
				break
			}
			slot := cmd.slots[min(i, len(cmd.slots)-1)]
			if i >= len(cmd.slots) && !slot.variadic && slot.kind() != "query" {
				continue
			}
			switch kind := slot.kind(); {
			case kind == "repository":
				if err := s.repository(arg, "the repository "+arg); err != nil {
					return err
				}
				targeted = true
			case kind == "second-repository":
				if err := s.repository(arg, "the repository "+arg); err != nil {
					return err
				}
				second = arg
			case kind == "owner":
				if err := s.owner(arg, "the owner "+arg); err != nil {
					return err
				}
				targeted = true
			case kind == "url" && (strings.HasPrefix(arg, "https://") || strings.HasPrefix(arg, "http://")):
				if err := s.repository(arg, "the URL "+arg); err != nil {
					return err
				}
				targeted = true
			case kind == "query":
				if err := s.query(arg); err != nil {
					return err
				}
			case kind == "name" && slices.Equal(cmd.path, []string{"repo", "create"}) && strings.Contains(arg, "/"):
				if err := s.repository(arg, "the repository "+arg); err != nil {
					return err
				}
				targeted = true
			}
		}
	}
	for name, values := range parsed.values {
		flag := cmd.flags[name]
		for _, value := range values {
			for _, item := range strings.Split(value, ",") {
				what := "--" + name + " " + item
				switch {
				case name == "hostname":
					if !strings.EqualFold(item, "github.com") {
						return cannotPlace(what, "names another host than github.com", s.api)
					}
				case name == "owner" || name == "org" || name == "repo-owner" || name == "target-owner":
					if err := s.owner(item, what); err != nil {
						return err
					}
				case name == "repo" || name == "repos" || name == "branch-repo" ||
					flag.placeholder == "repository" || flag.placeholder == "repositories":
					if err := s.repository(item, what); err != nil {
						return err
					}
				case name == "url" && (strings.HasPrefix(item, "https://") || strings.HasPrefix(item, "http://")):
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

// kind is what a slot's argument selects: a repository (`<repository>`), another repository a
// command acts on beside its own (`<destination-repo>`, `<source-repository>`), an owner, a URL, a
// search query, a repository's name (`gh repo create [<name>]`), or nothing.
func (s ghSlot) kind() string {
	kind := ""
	for _, name := range s.names {
		switch {
		case name == "repository":
			return "repository"
		case strings.Contains(name, "repo"):
			kind = "second-repository"
		case name == "owner":
			return "owner"
		case name == "query":
			return "query"
		case name == "url" || name == "urls" || strings.HasSuffix(name, "-url") || strings.HasSuffix(name, "-urls"):
			if kind == "" {
				kind = "url"
			}
		case name == "name" && kind == "":
			kind = "name"
		}
	}
	return kind
}

// query collects the owners a search query's qualifiers name: repo:<owner>/<repo>, and owner:,
// user: and org: <owner>, each also negated.
func (s *selectors) query(query string) error {
	for _, word := range strings.Fields(query) {
		qualifier, value, ok := strings.Cut(strings.TrimPrefix(word, "-"), ":")
		if !ok {
			continue
		}
		value = strings.Trim(value, `"'`)
		switch qualifier {
		case "repo":
			if err := s.repository(value, "the qualifier "+word); err != nil {
				return err
			}
		case "owner", "user", "org":
			if err := s.owner(value, "the qualifier "+word); err != nil {
				return err
			}
		}
	}
	return nil
}

// endpoint collects the owner a gh api endpoint names: repos/<owner>/, orgs/<owner>/ or
// users/<owner>/, with or without the API's host before it. Its {owner} placeholder names none,
// since GH_REPO fills it; a repository named by id (repositories/<id>) and another host are errors.
func (s *selectors) endpoint(endpoint string) error {
	path := endpoint
	if strings.HasPrefix(endpoint, "https://") || strings.HasPrefix(endpoint, "http://") {
		parsed, err := url.Parse(endpoint)
		if err != nil || !strings.EqualFold(parsed.Host, "api.github.com") {
			return cannotPlace(endpoint, "names another host than api.github.com", true)
		}
		path = parsed.Path
	}
	path, _, _ = strings.Cut(strings.TrimPrefix(path, "/"), "?")
	segments := strings.Split(path, "/")
	switch {
	case segments[0] == "repositories":
		return cannotPlace(endpoint, "names a repository by its id", true)
	case (segments[0] == "repos" || segments[0] == "orgs" || segments[0] == "users") && len(segments) > 1:
		if segments[1] == "{owner}" {
			return nil
		}
		return s.owner(segments[1], "the endpoint "+endpoint)
	}
	return nil
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

// readGhHelp is the real gh's help of the command path, `gh <path> --help`, refused when gh
// answers it with an error.
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
