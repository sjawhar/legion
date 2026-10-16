package main

import (
	"context"
	"fmt"
	"io"
	"net/url"
	"os"
	"regexp"
	"slices"
	"strings"
	"syscall"
)

const ghUsage = "usage: legion gh [gh args…]  (the gh first on a Legion pane's or pod's PATH: runs LEGION_GH_PATH with GH_CONFIG_DIR set to the gh directory of the owner the command names, under LEGION_GH_OWNERS_DIR)"

// ghURLOwner is the owner of a github.com URL.
var ghURLOwner = regexp.MustCompile(`^https?://github\.com/([^/]+)/`)

// ghAPIRepoOwner is the owner of a `gh api` path under repos/, with or without its leading slash
// or the API host before it.
var ghAPIRepoOwner = regexp.MustCompile(`^(?:https?://api\.github\.com)?/?repos/([^/]+)/[^/]+`)

// runGh is `legion gh`, the gh a pane or pod runs: it reads the owner of the repository the
// command acts on (OwnerOf) and execs the real gh (LEGION_GH_PATH) as `gh` with its arguments
// untouched and GH_CONFIG_DIR replaced by that owner's gh directory under LEGION_GH_OWNERS_DIR,
// when the bundle holds the owner, or left at the issue's owner's otherwise. Each of the three
// variables is runtime-set and required; an unset one is refused, exit 2, naming it. With no
// LEGION_GH_PATH, --help prints this command's usage, so `legion gh --help` outside a pane answers
// as every legion command does.
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
	dir := ownerGHConfigDir(env["LEGION_GH_OWNERS_DIR"], env["GH_CONFIG_DIR"], OwnerOf(args, env))
	childEnv := make([]string, 0, len(env))
	for _, pair := range os.Environ() {
		if !strings.HasPrefix(pair, "GH_CONFIG_DIR=") {
			childEnv = append(childEnv, pair)
		}
	}
	childEnv = append(childEnv, "GH_CONFIG_DIR="+dir)
	err := syscall.Exec(env["LEGION_GH_PATH"], append([]string{"gh"}, args...), childEnv)
	fmt.Fprintf(stderr, "legion gh: run %s: %v\n", env["LEGION_GH_PATH"], err)
	return 1
}

// OwnerOf is the owner, lowercased, of the repository the gh command argv (gh's arguments, without
// `gh`) acts on, read in order from: GH_REPO in env; -R/--repo's value, `[HOST/]OWNER/REPO` or a
// URL; a `gh api` path under repos/; any argument that is a github.com URL; and `gh repo <sub>
// <OWNER/REPO|URL>`. A command that names none is "", and gh then acts on the repository of its
// working directory, the issue's.
func OwnerOf(argv []string, env map[string]string) string {
	if owner := repositoryOwner(env["GH_REPO"]); owner != "" {
		return owner
	}
	for i, arg := range argv {
		var value string
		switch {
		case arg == "-R" || arg == "--repo":
			if i+1 < len(argv) {
				value = argv[i+1]
			}
		case strings.HasPrefix(arg, "--repo="):
			value = strings.TrimPrefix(arg, "--repo=")
		case strings.HasPrefix(arg, "-R"):
			value = strings.TrimPrefix(strings.TrimPrefix(arg, "-R"), "=")
		default:
			continue
		}
		if owner := repositoryOwner(value); owner != "" {
			return owner
		}
	}
	if len(argv) > 0 && argv[0] == "api" {
		for _, arg := range argv[1:] {
			if match := ghAPIRepoOwner.FindStringSubmatch(arg); match != nil {
				return strings.ToLower(match[1])
			}
		}
	}
	for _, arg := range argv {
		if match := ghURLOwner.FindStringSubmatch(arg); match != nil {
			return strings.ToLower(match[1])
		}
	}
	if len(argv) > 2 && argv[0] == "repo" {
		end := len(argv)
		if separator := slices.Index(argv, "--"); separator >= 0 {
			end = separator
		}
		for _, arg := range argv[2:end] {
			if !strings.HasPrefix(arg, "-") {
				return repositoryOwner(arg)
			}
		}
	}
	return ""
}

// repositoryOwner is the owner, lowercased, of a repository named as gh names one: `OWNER/REPO`,
// `HOST/OWNER/REPO`, or a URL; "" for anything else.
func repositoryOwner(repository string) string {
	if repository == "" {
		return ""
	}
	if parsed, err := url.Parse(repository); err == nil && parsed.Scheme != "" && parsed.Host != "" {
		repository = parsed.Host + parsed.Path
	}
	parts := strings.Split(strings.Trim(repository, "/"), "/")
	switch {
	case len(parts) == 2 && parts[0] != "" && parts[1] != "":
		return strings.ToLower(parts[0])
	case len(parts) >= 3 && parts[1] != "" && parts[2] != "":
		return strings.ToLower(parts[1])
	default:
		return ""
	}
}
