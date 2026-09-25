// Package ghrepo is the one reading of a GitHub `<owner>/<name>` every Legion input takes: a
// project's configured repo (internal/config), `legion threads resolve --repo`, the repository a
// workspace is located and fetched from (internal/workspace, which `legion workspace-init`'s
// --repo reaches before anything touches the volume or the feed), and the intake consumer's
// repositories. Every path Legion derives from a repository joins its two names under a state
// directory, so each input is held to the same rule.
package ghrepo

import (
	"fmt"
	"strings"
	"unicode"
)

// Split is repository's owner and name, or a refusal that begins with what, the input's name: it
// must be exactly `<owner>/<name>`, both names non-empty and neither holding whitespace, and
// neither may be `.` or `..`, which joined under a state directory would name another directory
// than the repository's (and provisioning removes an incomplete clone at that path).
func Split(what, repository string) (owner, name string, err error) {
	owner, name, found := strings.Cut(repository, "/")
	if !found || owner == "" || name == "" || strings.Contains(name, "/") {
		return "", "", fmt.Errorf(`%s must be "owner/name" (got %q)`, what, repository)
	}
	if strings.ContainsFunc(repository, unicode.IsSpace) {
		return "", "", fmt.Errorf(`%s %q holds whitespace, which no GitHub owner or repository name does`, what, repository)
	}
	for _, segment := range []string{owner, name} {
		if segment == "." || segment == ".." {
			return "", "", fmt.Errorf(`%s %q has a %q segment, which names no GitHub owner or repository`, what, repository, segment)
		}
	}
	return owner, name, nil
}
