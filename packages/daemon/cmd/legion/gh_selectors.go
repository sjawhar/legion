package main

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"maps"
	"net/url"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"slices"
	"strings"
	"time"

	"github.com/sjawhar/legion/daemon/internal/ghconfig"
)

// selectors is every owner a command names, each with the argument that names it, and whether gh
// api's arguments hold a placeholder gh fills from the command's own repository (readsBase).
type selectors struct {
	api       bool
	owners    map[string]string
	readsBase bool
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

// ghRepositoryNumber is an issue or pull request named OWNER/REPO#N, which gh reads in a number's
// place as that repository's.
var ghRepositoryNumber = regexp.MustCompile(`^([^/\s#]+/[^/\s#]+)#[0-9]+$`)

// fromCommand collects the owners the leaf's arguments and options name.
func (s *selectors) fromCommand(cmd *ghCommand, parsed ghArgs) error {
	leaf := cmd.quoted()
	targeted, second := len(parsed.values["repo"]) > 0 && cmd.readsGHRepo(), ""
	if s.api {
		if err := s.fromAPI(parsed); err != nil {
			return err
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
			if placed[i].takes("number") && ghRepositoryNumber.MatchString(arg) {
				if err := s.repository(ghRepositoryNumber.FindStringSubmatch(arg)[1], "the issue or pull request "+arg); err != nil {
					return err
				}
				targeted = true
				continue
			}
			switch kind {
			case "repository":
				if err := s.repository(arg, "the repository "+arg); err != nil {
					return err
				}
				targeted = true
			case "new-repository":
				// OWNER/NAME names the new repository's owner; a name alone, none.
				if strings.Contains(arg, "/") {
					if err := s.repository(arg, "the repository "+arg); err != nil {
						return err
					}
					targeted = true
				}
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
				if isWebURL(arg) {
					if err := s.repository(arg, "the URL "+arg); err != nil {
						return err
					}
					targeted = true
				}
			case "query":
				query = append(query, arg)
			}
		}
		if err := s.query(query); err != nil {
			return err
		}
	}
	if err := s.fromOptions(cmd, parsed); err != nil {
		return err
	}
	if second != "" && cmd.readsGHRepo() && !targeted {
		return cannotPlace(second, "names a second repository while the command's own repository is not named", false)
	}
	return nil
}

// fromAPI collects the owners gh api's endpoint and fields name: the endpoint's (endpoint); a
// GraphQL query's variable named for an owner (`-F owner=<login>`), unless it holds a placeholder;
// and a search endpoint's q field, its query. A placeholder gh fills from the command's own
// repository, in the endpoint or a typed (-F) field's value, which gh fills and a raw (-f) one's it
// does not, sets readsBase. Any other GraphQL query names no repository gh could resolve.
func (s *selectors) fromAPI(parsed ghArgs) error {
	if len(parsed.positionals) > 1 {
		return cannotPlace(parsed.positionals[1], "is an argument `gh api`'s usage has no place for", true)
	}
	search, graphql := false, false
	if len(parsed.positionals) == 1 {
		endpoint := parsed.positionals[0]
		s.readsBase = readsBaseRepository(endpoint)
		graphql = endpoint == "graphql"
		var err error
		if search, err = s.endpoint(endpoint); err != nil {
			return err
		}
	}
	for _, option := range []string{"field", "raw-field"} {
		for _, field := range parsed.values[option] {
			key, value, _ := strings.Cut(field, "=")
			if option == "field" && readsBaseRepository(value) {
				s.readsBase = true
			}
			switch {
			case graphql && (key == "owner" || key == "org" || key == "organization" || key == "login") && !ghPlaceholder.MatchString(value):
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
	return nil
}

// fromOptions collects the owners the leaf's options name, by what each selects (ghOptionKind): a
// repository or an owner, each of a comma-separated list; a repository of --org's, a name alone
// that organization's; github.com alone as a host; a web URL; nothing. An option this cannot read
// is refused, and a github.com web URL given any other option is a selector too.
func (s *selectors) fromOptions(cmd *ghCommand, parsed ghArgs) error {
	orgs := parsed.values["org"]
	for _, name := range slices.Sorted(maps.Keys(parsed.values)) {
		flag := cmd.flags[name]
		if s.api && (name == "field" || name == "raw-field") {
			continue
		}
		for _, value := range parsed.values[name] {
			if flag.selects == "unknown" {
				return cannotPlace("--"+name+" "+value, "is an option whose help names a repository, an owner or a URL in a way this does not know", s.api)
			}
			if flag.selects == "" {
				if isWebURL(value) && isGitHubWebHost(value) {
					if err := s.repository(value, "--"+name+" "+value); err != nil {
						return err
					}
				}
				continue
			}
			for _, item := range strings.Split(value, ",") {
				what := "--" + name + " " + item
				var err error
				switch flag.selects {
				case "host":
					if !strings.EqualFold(item, "github.com") {
						err = cannotPlace(what, "names another host than github.com", s.api)
					}
				case "owner":
					err = s.owner(item, what)
				case "org-repositories":
					// gh reads a name alone as a repository of the --org, whose owner that is.
					switch {
					case strings.Contains(item, "/"):
						err = s.repository(item, what)
					case len(orgs) == 0:
						err = cannotPlace(what, "names a repository by its name alone, and no --org says whose", s.api)
					}
				case "repository":
					err = s.repository(item, what)
				case "url":
					if isWebURL(item) {
						err = s.repository(item, what)
					}
				}
				if err != nil {
					return err
				}
			}
		}
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
// whether it is a search endpoint, whose q field is its query too. A placeholder in the owner's
// place names none (GH_REPO's, or the working directory's, fills it); a repository named by id
// (repositories/<id>) and another host are errors. gh reads an endpoint holding "://" as a whole
// URL.
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
		if ghPlaceholder.MatchString(segments[1]) {
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

// ghPlaceholder is gh api's placeholder as gh reads it, in its endpoint and its typed fields'
// values: :owner, :repo or :branch, or a name in braces, of which gh fills {owner}, {repo} and
// {branch} and leaves any other as it is.
var ghPlaceholder = regexp.MustCompile(`(\:(owner|repo|branch)\b|\{[a-z]+\})`)

// readsBaseRepository says whether s holds a placeholder gh fills from the command's own
// repository, GH_REPO's or the working directory's: its owner or its name, not its branch.
func readsBaseRepository(s string) bool {
	return slices.ContainsFunc(ghPlaceholder.FindAllString(s, -1), func(p string) bool {
		return p == ":owner" || p == ":repo" || p == "{owner}" || p == "{repo}"
	})
}

// isWebURL says whether s is an http or https URL, as gh reads a URL argument: its scheme in any
// case.
func isWebURL(s string) bool {
	u, err := url.Parse(s)
	return err == nil && (u.Scheme == "http" || u.Scheme == "https")
}

// isGitHubWebHost says whether the URL s is github.com's.
func isGitHubWebHost(s string) bool {
	u, err := url.Parse(s)
	return err == nil && isGitHubHost(u.Hostname())
}

// isGitHubHost says whether host is github.com as gh reads a host: github.com, or one of its
// subdomains (www.github.com, ssh.github.com), in any case.
func isGitHubHost(host string) bool {
	host = strings.ToLower(host)
	return host == "github.com" || strings.HasSuffix(host, ".github.com")
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

// ghRemote is one setting of the working directory's git remotes that gh reads to find the
// repository of a command naming none: a remote's fetch or push URL as `git remote -v` prints it,
// or its gh-resolved setting (`gh repo set-default`'s), "base" for the remote's own repository or
// [HOST/]OWNER/REPO for another.
type ghRemote struct {
	name, url, resolved string
}

// what is the remote setting as a refusal names it.
func (r ghRemote) what() string {
	if r.resolved != "" {
		return fmt.Sprintf("remote.%s.gh-resolved %s", r.name, r.resolved)
	}
	return fmt.Sprintf("remote %q (%s)", r.name, r.url)
}

// ghSSHRemote is a remote in scp-like form, [USER@]HOST:PATH.
var ghSSHRemote = regexp.MustCompile(`^(?:[^@/:]+@)?([^/:]+):(.*)$`)

// owner is the owner, lowercased, of the github.com repository the remote setting names, "" for
// one naming none gh reads: a remote on another host by https or git, or a local path, and a
// gh-resolved "base", whose remote's own URL is read beside it. why says why a setting is one this
// cannot read: an ssh remote on another host, which gh may read as an alias of github.com from
// ssh's configuration, and one naming no owner that is a GitHub login.
func (r ghRemote) owner() (owner, why string) {
	if r.resolved != "" {
		if r.resolved == "base" {
			return "", ""
		}
		owner, err := repositoryOwner(r.resolved, "", false)
		if err != nil {
			return "", "names no github.com repository this can read"
		}
		return owner, ""
	}
	var host, path string
	ssh := false
	if parsed, err := url.Parse(r.url); err == nil && parsed.Scheme != "" && strings.Contains(r.url, "://") {
		switch strings.ToLower(parsed.Scheme) {
		case "https", "http", "git":
		case "ssh", "git+ssh", "ssh+git":
			ssh = true
		default:
			return "", ""
		}
		host, path = parsed.Hostname(), parsed.Path
	} else if m := ghSSHRemote.FindStringSubmatch(r.url); m != nil {
		host, path, ssh = m[1], m[2], true
	} else {
		return "", ""
	}
	switch {
	case isGitHubHost(host):
	case ssh:
		return "", "is an ssh remote whose host gh may read as github.com through ssh's configuration"
	default:
		return "", ""
	}
	parts := strings.Split(strings.Trim(path, "/"), "/")
	if owner = strings.ToLower(parts[0]); len(parts) < 2 || !ghconfig.IsOwner(owner) {
		return "", "names no owner that is a GitHub login"
	}
	return owner, ""
}

// ghTokenOwner is the owner whose gh directory under LEGION_GH_OWNERS_DIR GH_CONFIG_DIR names, the
// owner whose token gh sends for a command naming no repository; "" for none (an ownerless
// controller's GH_CONFIG_DIR, the bundle root).
func ghTokenOwner(env map[string]string) string {
	dir := filepath.Clean(env["GH_CONFIG_DIR"])
	if filepath.Dir(dir) != filepath.Clean(env["LEGION_GH_OWNERS_DIR"]) || !ghconfig.IsOwner(filepath.Base(dir)) {
		return ""
	}
	return filepath.Base(dir)
}

// workingDirectory refuses a command naming no repository unless every github.com repository the
// working directory's git remotes name (remotes) is the owner's whose token gh sends for it
// (ghTokenOwner): gh takes such a command's repository from those remotes, an `upstream` before an
// `origin` and a gh-resolved one before either, so a clone of another owner's repository, or a
// workspace with another owner's remote beside its own, would get the wrong owner's token. A
// working directory that is no git repository names none.
func workingDirectory(env map[string]string, remotes func() ([]ghRemote, error), api bool) error {
	advice := "name the repository with -R <owner>/<repo>"
	if api {
		advice = "name it in the endpoint, repos/<owner>/<repo>/…"
	}
	listed, err := remotes()
	if err != nil {
		return fmt.Errorf("cannot tell which repository the command targets: the working directory's git remotes, from which gh takes the repository of a command naming none, cannot be read (%v), so it sends no token; %s", err, advice)
	}
	token := ghTokenOwner(env)
	for _, remote := range listed {
		owner, why := remote.owner()
		if why != "" {
			return fmt.Errorf("cannot tell which repository the command targets: the working directory's %s %s, so it sends no token; %s", remote.what(), why, advice)
		}
		if owner != "" && owner != token {
			whose := "no owner's"
			if token != "" {
				whose = token + "'s"
			}
			return fmt.Errorf("cannot tell which repository the command targets: the working directory's %s names %s, and gh takes the repository of a command naming none from there while it sends %s token, so it sends no token; %s", remote.what(), owner, whose, advice)
		}
	}
	return nil
}

// readGitRemotes is the working directory's git remotes as gh reads them, with git itself: every
// fetch and push URL `git remote -v` prints, and every remote's gh-resolved setting; none for a
// directory that is no git repository, or where no git runs, since gh then reads none either.
func readGitRemotes() ([]ghRemote, error) {
	lines, err := gitLines("remote", "-v")
	if err != nil {
		return nil, err
	}
	var remotes []ghRemote
	for _, line := range lines {
		name, rest, _ := strings.Cut(line, "\t")
		address, kind, found := strings.Cut(rest, " (")
		if name == "" || !found || (kind != "fetch)" && kind != "push)") {
			return nil, fmt.Errorf("git remote -v printed %q", line)
		}
		remotes = append(remotes, ghRemote{name: name, url: address})
	}
	if lines == nil {
		return nil, nil
	}
	resolved, err := gitLines("config", "--get-regexp", `^remote\..*\.gh-resolved$`)
	if err != nil {
		return nil, err
	}
	for _, line := range resolved {
		key, value, _ := strings.Cut(line, " ")
		name := strings.TrimSuffix(strings.TrimPrefix(key, "remote."), ".gh-resolved")
		remotes = append(remotes, ghRemote{name: name, resolved: value})
	}
	return remotes, nil
}

// gitLines runs git with args in the working directory and answers the lines it prints: none for
// a directory that is no git repository, where no git runs, or, for `git config --get-regexp`,
// where nothing matches.
func gitLines(args ...string) ([]string, error) {
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	cmd := exec.CommandContext(ctx, "git", args...)
	cmd.Env = append(os.Environ(), "LC_ALL=C")
	var stdout, stderr bytes.Buffer
	cmd.Stdout, cmd.Stderr = &stdout, &stderr
	err := cmd.Run()
	var exit *exec.ExitError
	switch {
	case errors.Is(err, exec.ErrNotFound):
		return nil, nil
	case errors.As(err, &exit) && strings.Contains(stderr.String(), "not a git repository"):
		return nil, nil
	case errors.As(err, &exit) && args[0] == "config" && exit.ExitCode() == 1:
		return nil, nil
	case err != nil:
		return nil, fmt.Errorf("git %s: %v: %s", strings.Join(args, " "), err, strings.TrimSpace(stderr.String()))
	}
	var lines []string
	for _, line := range strings.Split(stdout.String(), "\n") {
		if line != "" {
			lines = append(lines, line)
		}
	}
	if lines == nil {
		lines = []string{}
	}
	return lines, nil
}
