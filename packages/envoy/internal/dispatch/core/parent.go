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
// used the bare-number form (caller resolves it against the dispatch repo).
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
//	<owner>/<repo>#42#<commentID>
//
// Owner and repo segments exclude `/`, `#`, and whitespace — the same shape
// the MCP shim uses to decide a parent already names its repo.
var (
	bareForm = regexp.MustCompile(`^(\d+)(?:#(\d+))?$`)
	repoForm = regexp.MustCompile(`^([^/\s#]+/[^/\s#]+)#(\d+)(?:#(\d+))?$`)
)

// ParseParent parses a parent reference. Returns an error for invalid input.
func ParseParent(s string) (ParsedParent, error) {
	s = strings.TrimSpace(s)
	if s == "" {
		return ParsedParent{}, fmt.Errorf("Invalid parent: %s", s)
	}
	var repo, issue, comment string
	if m := repoForm.FindStringSubmatch(s); m != nil {
		repo, issue, comment = m[1], m[2], m[3]
	} else if m := bareForm.FindStringSubmatch(s); m != nil {
		issue, comment = m[1], m[2]
	} else {
		return ParsedParent{}, fmt.Errorf("Invalid parent: %s", s)
	}
	n, err := parsePositiveInteger(issue, "issue number")
	if err != nil {
		return ParsedParent{}, err
	}
	out := ParsedParent{Repo: repo, IssueNumber: n}
	if comment != "" {
		c, err := parsePositiveInteger(comment, "comment id")
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
