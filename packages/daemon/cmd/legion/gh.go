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
	"strings"
	"syscall"
	"time"

	"github.com/sjawhar/legion/daemon/internal/ghconfig"
)

const ghUsage = "usage: legion gh [gh args…]  (the gh first on a Legion pane's or pod's PATH: runs LEGION_GH_PATH with GH_CONFIG_DIR set to the gh directory of the owner the command names, under LEGION_GH_OWNERS_DIR)"

// ghAPIRepoOwner is the owner of a `gh api` endpoint under repos/, with or without its leading
// slash or the API host before it.
var ghAPIRepoOwner = regexp.MustCompile(`^(?:https://api\.github\.com)?/?repos/([^/?#]+)/`)

// ghFlagLine is one option line of gh's help: an optional shorthand, the long name, and the
// placeholder word naming its value when it takes one ("  -q, --jq expression   Filter…").
var ghFlagLine = regexp.MustCompile(`^\s+(?:-([A-Za-z0-9]), )?--([A-Za-z0-9][A-Za-z0-9-]*)(?: (\S+))?(?:\s{2,}|\s*$)`)

// ghFlags is which of a gh command's options take a value, by long name and by shorthand.
type ghFlags struct {
	long  map[string]bool
	short map[rune]bool
}

// runGh is `legion gh`, the gh a pane or pod runs. It reads the owner of the repository the
// command acts on as gh resolves it (ghTargetOf) and execs the real gh (LEGION_GH_PATH) as `gh`
// with its arguments untouched and GH_CONFIG_DIR set to that owner's gh directory under
// LEGION_GH_OWNERS_DIR; a command naming no repository keeps GH_CONFIG_DIR, the issue's owner's.
// A command naming an owner the bundle holds no directory for, or one naming no GitHub login, is
// refused, exit 2, and the real gh never runs: no token but the target owner's is ever sent. Each
// of the three variables is runtime-set and required; an unset one is refused, exit 2, naming it.
// With no LEGION_GH_PATH, --help prints this command's usage, so `legion gh --help` outside a pane
// answers as every legion command does.
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
	owner, named, err := ghTargetOf(args, env, func(path []string) (ghFlags, error) { return readGhFlags(gh, path) })
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
// without `gh`) acts on, as gh itself resolves it, and whether the command names one at all:
//
//   - `gh api <endpoint>`: the owner of an endpoint under repos/<owner>/; its {owner} placeholder
//     is GH_REPO's owner, as gh fills it. GH_REPO names nothing else for gh api.
//   - `gh repo <sub> [<repository>]`: the repository argument, `[HOST/]OWNER/REPO` or a URL (for
//     `gh repo list [<owner>]`, the owner), then -R/--repo, then GH_REPO.
//   - any other command: a github.com URL given as its first positional argument (a pull request's
//     or issue's, which gh reads its repository from over -R), then -R/--repo, then GH_REPO.
//
// Positional arguments are told from option values by each option's arity, read from gh's own
// help of the command (flags, asked only for a command with an option to read). A command that
// names none is not named: gh acts on its working directory's repository, the issue's. A selector
// naming a host other than github.com, or an owner that is no GitHub login, is an error.
func ghTargetOf(argv []string, env map[string]string, flags func(path []string) (ghFlags, error)) (owner string, named bool, err error) {
	if len(argv) == 0 || strings.HasPrefix(argv[0], "-") {
		return "", false, nil
	}
	group, rest := argv[0], argv[1:]
	path := []string{group}
	if group != "api" {
		// The subcommand is the group's first argument that is neither an option nor -R's value,
		// the one option a group (`gh pr -R x view`) takes before it.
		for i := 0; i < len(rest); i++ {
			switch arg := rest[i]; {
			case arg == "-R" || arg == "--repo":
				i++
			case strings.HasPrefix(arg, "-"):
			default:
				path = append(path, arg)
				rest = append(append([]string{}, rest[:i]...), rest[i+1:]...)
				i = len(rest)
			}
		}
	}
	var arity ghFlags
	if group == "api" || group == "pr" || group == "issue" || group == "repo" {
		if hasOption(rest) {
			if arity, err = flags(path); err != nil {
				return "", false, err
			}
		}
	} else {
		arity = ghFlags{long: map[string]bool{"repo": true}, short: map[rune]bool{'R': true}}
	}
	positionals, repoFlag := splitGhArgs(rest, arity)

	switch group {
	case "api":
		if len(positionals) == 0 {
			return "", false, nil
		}
		match := ghAPIRepoOwner.FindStringSubmatch(positionals[0])
		if match == nil {
			return "", false, nil
		}
		if match[1] == "{owner}" {
			return repositoryOwnerOf(env["GH_REPO"], "GH_REPO")
		}
		return checkedOwner(match[1], "gh api "+positionals[0])
	case "repo":
		if len(positionals) > 0 {
			if len(path) == 2 && path[1] == "list" {
				return checkedOwner(positionals[0], "gh repo list "+positionals[0])
			}
			if owner, named, err := repositoryOwnerOf(positionals[0], "the repository "+positionals[0]); err != nil || named {
				return owner, named, err
			}
		}
	default:
		if len(positionals) > 0 && (strings.HasPrefix(positionals[0], "https://") || strings.HasPrefix(positionals[0], "http://")) {
			if owner, named, err := repositoryOwnerOf(positionals[0], "the URL "+positionals[0]); err != nil || named {
				return owner, named, err
			}
		}
	}
	if repoFlag != "" {
		return repositoryOwnerOf(repoFlag, "-R "+repoFlag)
	}
	return repositoryOwnerOf(env["GH_REPO"], "GH_REPO")
}

// hasOption says whether args hold an option whose arity decides what follows it.
func hasOption(args []string) bool {
	for _, arg := range args {
		if arg == "--" {
			return false
		}
		if strings.HasPrefix(arg, "-") && arg != "-" {
			return true
		}
	}
	return false
}

// splitGhArgs walks args as gh's option parser does (pflag): a long option takes its value after
// `=` or, when it takes one, the next argument; a shorthand group's options before the first one
// taking a value are switches, and that one takes the rest of the group or the next argument; `--`
// ends the options, and what follows it is no selector (`gh repo clone <repo> -- <git flags>`).
// It answers the positional arguments before `--` and the last -R/--repo value.
func splitGhArgs(args []string, arity ghFlags) (positionals []string, repo string) {
	for i := 0; i < len(args); i++ {
		arg := args[i]
		switch {
		case arg == "--":
			return positionals, repo
		case strings.HasPrefix(arg, "--"):
			name, value, hasValue := strings.Cut(arg[2:], "=")
			if !hasValue && arity.long[name] && i+1 < len(args) {
				i++
				value, hasValue = args[i], true
			}
			if name == "repo" && hasValue {
				repo = value
			}
		case strings.HasPrefix(arg, "-") && arg != "-":
			shorts := []rune(arg[1:])
			for j, short := range shorts {
				if !arity.short[short] {
					continue
				}
				value := strings.TrimPrefix(string(shorts[j+1:]), "=")
				if value == "" && j+1 == len(shorts) && i+1 < len(args) {
					i++
					value = args[i]
				}
				if short == 'R' {
					repo = value
				}
				break
			}
		default:
			positionals = append(positionals, arg)
		}
	}
	return positionals, repo
}

// repositoryOwnerOf is the owner, lowercased, of a repository named as gh names one —
// `OWNER/REPO`, `HOST/OWNER/REPO`, or a URL — and whether value names one at all; "" names none.
// A host other than github.com, and an owner that is no GitHub login, are errors naming what.
func repositoryOwnerOf(value, what string) (string, bool, error) {
	if value == "" {
		return "", false, nil
	}
	host, rest := "", value
	if parsed, err := url.Parse(value); err == nil && parsed.Scheme != "" && parsed.Host != "" {
		host, rest = parsed.Host, strings.Trim(parsed.Path, "/")
	} else if parts := strings.Split(strings.Trim(value, "/"), "/"); len(parts) >= 3 {
		host, rest = parts[0], strings.Join(parts[1:], "/")
	}
	if host != "" && !strings.EqualFold(host, "github.com") && !strings.EqualFold(host, "www.github.com") {
		return "", false, fmt.Errorf("%s names the host %q: a pane's gh holds tokens for github.com alone", what, host)
	}
	parts := strings.Split(rest, "/")
	if len(parts) < 2 || parts[0] == "" || parts[1] == "" {
		return "", false, nil
	}
	return checkedOwner(parts[0], what)
}

// checkedOwner is owner lowercased, refused when it is no GitHub login.
func checkedOwner(owner, what string) (string, bool, error) {
	lower := strings.ToLower(owner)
	if !ghconfig.IsOwner(lower) {
		return "", false, fmt.Errorf("%s names the owner %q, which is no GitHub login", what, owner)
	}
	return lower, true, nil
}

// readGhFlags is the arity of the gh command path's options, read from the real gh's own help.
func readGhFlags(gh string, path []string) (ghFlags, error) {
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	cmd := exec.CommandContext(ctx, gh, append(append([]string{}, path...), "--help")...)
	var stdout, stderr bytes.Buffer
	cmd.Stdout, cmd.Stderr = &stdout, &stderr
	if err := cmd.Run(); err != nil {
		return ghFlags{}, fmt.Errorf("read the options of gh %s from its help: %v: %s", strings.Join(path, " "), err, strings.TrimSpace(stderr.String()))
	}
	flags, err := parseGhFlags(stdout.String())
	if err != nil {
		return ghFlags{}, fmt.Errorf("read the options of gh %s from its help: %w", strings.Join(path, " "), err)
	}
	return flags, nil
}

// parseGhFlags reads, from gh's help of one command, which options take a value: an option line
// whose name a placeholder word follows. A help listing no --help is no command's help.
func parseGhFlags(help string) (ghFlags, error) {
	flags := ghFlags{long: map[string]bool{}, short: map[rune]bool{}}
	for _, line := range strings.Split(help, "\n") {
		match := ghFlagLine.FindStringSubmatch(line)
		if match == nil {
			continue
		}
		takesValue := match[3] != ""
		flags.long[match[2]] = takesValue
		if match[1] != "" {
			flags.short[[]rune(match[1])[0]] = takesValue
		}
	}
	if _, ok := flags.long["help"]; !ok {
		return ghFlags{}, errors.New("the help lists no --help option")
	}
	return flags, nil
}
