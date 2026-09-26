// Package ghrepo is the one reading of a GitHub `<owner>/<name>` every Legion input takes, parsed
// once where it enters: a project's configured repo (internal/config), `legion threads resolve
// --repo`, and `legion workspace-init`'s --repo, before anything touches the volume or the feed.
// Everything past those boundaries carries the Repository, so nothing splits the string again.
// Every path Legion derives from a repository joins its two names under a state directory, so
// each input is held to the same rule.
package ghrepo

import (
	"fmt"
	"regexp"
	"strings"
	"unicode"
)

var (
	// ownerName is GitHub's grammar for an account: letters, digits and single hyphens, beginning
	// and ending with a letter or digit.
	ownerName = regexp.MustCompile(`^[A-Za-z0-9]+(?:-[A-Za-z0-9]+)*$`)
	// repositoryName is GitHub's grammar for a repository's own name: letters, digits, `-`, `_`
	// and `.` (`.` and `..` alone are refused as segments before it is asked).
	repositoryName = regexp.MustCompile(`^[A-Za-z0-9._-]+$`)
)

// Repository is a GitHub repository as Parse read it. Its names are unexported, so outside this
// package a Repository is one Parse returned or the zero value, no repository: no caller can build
// one Parse would refuse, whose names, joined under a state directory, would name another
// directory than the repository's.
type Repository struct {
	owner, name string
}

// Owner is the repository's owner, the name before the slash.
func (r Repository) Owner() string { return r.owner }

// Name is the repository's own name, the one after the slash.
func (r Repository) Name() string { return r.name }

// IsZero is whether r is the zero Repository, no repository. Every other Repository is one Parse
// returned, so a carrier that refuses the zero value holds only parsed repositories.
func (r Repository) IsZero() bool { return r == Repository{} }

// String is the repository as GitHub names it, `<owner>/<name>`.
func (r Repository) String() string {
	return r.owner + "/" + r.name
}

// MustParse is Parse of a repository the caller wrote as a literal, a test's fixture: it panics on
// one Parse refuses.
func MustParse(repository string) Repository {
	parsed, err := Parse("repository", repository)
	if err != nil {
		panic(err)
	}
	return parsed
}

// Parse is repository's owner and name, or a refusal that begins with what, the input's name: it
// must be exactly `<owner>/<name>`, both names non-empty and neither holding whitespace; neither
// may be `.` or `..`, which joined under a state directory would name another directory than the
// repository's (and provisioning removes an incomplete clone at that path); and each must be one
// GitHub allows. That last rule keeps out what a GitHub name never holds but a path, a URL, a NATS
// subject or a terminal would act on: control bytes, `#` and `?`, a hyphen that begins an owner, and the NATS
// wildcards `*` and `>`, with which intake's `notifications.github.<owner>.<name>.>` filter would
// match other repositories' events.
func Parse(what, repository string) (Repository, error) {
	owner, name, found := strings.Cut(repository, "/")
	if !found || owner == "" || name == "" || strings.Contains(name, "/") {
		return Repository{}, fmt.Errorf(`%s must be "owner/name" (got %q)`, what, repository)
	}
	if strings.ContainsFunc(repository, unicode.IsSpace) {
		return Repository{}, fmt.Errorf(`%s %q holds whitespace, which no GitHub owner or repository name does`, what, repository)
	}
	for _, segment := range []string{owner, name} {
		if segment == "." || segment == ".." {
			return Repository{}, fmt.Errorf(`%s %q has a %q segment, which names no GitHub owner or repository`, what, repository, segment)
		}
	}
	if !ownerName.MatchString(owner) {
		return Repository{}, fmt.Errorf(`%s %q has an owner GitHub does not allow: letters, digits and single hyphens, not beginning or ending with a hyphen`, what, repository)
	}
	if !repositoryName.MatchString(name) {
		return Repository{}, fmt.Errorf(`%s %q has a name GitHub does not allow: letters, digits, "-", "_" and "."`, what, repository)
	}
	return Repository{owner: owner, name: name}, nil
}
