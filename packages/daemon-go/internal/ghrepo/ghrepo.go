// Package ghrepo is the one reading of a GitHub `<owner>/<name>` every Legion input takes, parsed
// once where it enters: a project's configured repo (internal/config), `legion threads resolve
// --repo`, and `legion workspace-init`'s --repo, before anything touches the volume or the feed.
// Everything past those boundaries carries the Repository, so nothing splits the string again.
// Every path Legion derives from a repository joins its two names under a state directory, so
// each input is held to the same rule.
package ghrepo

import (
	"fmt"
	"strings"
	"unicode"
)

// Repository is a GitHub repository as Parse read it.
type Repository struct {
	Owner, Name string
}

// String is the repository as GitHub names it, `<owner>/<name>`.
func (r Repository) String() string {
	return r.Owner + "/" + r.Name
}

// Parse is repository's owner and name, or a refusal that begins with what, the input's name: it
// must be exactly `<owner>/<name>`, both names non-empty and neither holding whitespace, and
// neither may be `.` or `..`, which joined under a state directory would name another directory
// than the repository's (and provisioning removes an incomplete clone at that path).
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
	return Repository{Owner: owner, Name: name}, nil
}
