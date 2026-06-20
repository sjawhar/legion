// Package core implements the dispatch thread orchestration: parsing parent
// references, building meta markers, and the CreateThread workflow.
package core

import (
	"fmt"
	"regexp"
	"strconv"
	"strings"
)

// ParsedParent describes a parent reference. Repo is empty when the caller
// used the bare-number form (caller falls back to the dispatch default repo).
type ParsedParent struct {
	Repo        string
	IssueNumber int
	CommentID   int // 0 means no comment id present
}

// Accepted parent forms:
//
//	42
//	42#<commentID>
//	<owner>/<repo>#42
//
// We intentionally don't support `<owner>/<repo>#42#<commentID>` yet — no
// caller needs it and the triple-hash form is ugly. Add a `@<commentID>`
// suffix if/when the need arises.
var (
	bareForm = regexp.MustCompile(`^(\d+)(?:#(\d+))?$`)
	repoForm = regexp.MustCompile(`^([^/]+/[^/]+)#(\d+)$`)
)

// ParseParent parses a parent reference. Returns an error for invalid input.
func ParseParent(s string) (ParsedParent, error) {
	s = strings.TrimSpace(s)
	if s == "" {
		return ParsedParent{}, fmt.Errorf("Invalid parent: %s", s)
	}
	if m := repoForm.FindStringSubmatch(s); m != nil {
		n, err := parsePositiveInteger(m[2], "issue number")
		if err != nil {
			return ParsedParent{}, err
		}
		return ParsedParent{Repo: m[1], IssueNumber: n}, nil
	}
	m := bareForm.FindStringSubmatch(s)
	if m == nil {
		return ParsedParent{}, fmt.Errorf("Invalid parent: %s", s)
	}
	n, err := parsePositiveInteger(m[1], "issue number")
	if err != nil {
		return ParsedParent{}, err
	}
	out := ParsedParent{IssueNumber: n}
	if m[2] != "" {
		c, err := parsePositiveInteger(m[2], "comment id")
		if err != nil {
			return ParsedParent{}, err
		}
		out.CommentID = c
	}
	return out, nil
}

func parsePositiveInteger(value, label string) (int, error) {
	if value == "" {
		return 0, fmt.Errorf("Invalid parent %s: %s", label, value)
	}
	for i, r := range value {
		if r < '0' || r > '9' {
			return 0, fmt.Errorf("Invalid parent %s: %s", label, value)
		}
		if i == 0 && r == '0' {
			return 0, fmt.Errorf("Invalid parent %s: %s", label, value)
		}
	}
	n, err := strconv.Atoi(value)
	if err != nil || n <= 0 {
		return 0, fmt.Errorf("Invalid parent %s: %s", label, value)
	}
	return n, nil
}
