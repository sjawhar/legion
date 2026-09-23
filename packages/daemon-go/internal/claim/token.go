package claim

import (
	"fmt"
	"regexp"
	"strings"
)

var (
	// issueKey is a Dispatch issue key, the one shape an issue has (legion-roles.ts:30).
	issueKey = regexp.MustCompile(`^([A-Z][A-Z0-9]*)-([0-9]+)$`)
	// projectToken is a project as a token spells it (legion-roles.ts:28).
	projectToken = regexp.MustCompile(`^[a-z0-9]+$`)
	// notAlphanumeric is everything ProjectToken drops.
	notAlphanumeric = regexp.MustCompile(`[^a-z0-9]`)
)

// Roles are the six roles a claim can be on, in the order the workflow reaches them.
var Roles = []Role{RoleArchitect, RolePlanner, RoleImplementer, RoleTester, RoleReviewer, RoleMerger}

// ProjectToken is the project as every token, and the private tmux server, spell it: the
// operator's `project` lowercased with every character outside [a-z0-9] dropped — the shipped
// daemon's rule (packages/daemon/src/daemon/config.ts:1429-1433), so `sjawhar/legion` names the
// same role topics under either daemon.
func ProjectToken(project string) (string, error) {
	token := notAlphanumeric.ReplaceAllString(strings.ToLower(project), "")
	if token == "" {
		return "", fmt.Errorf("project %q must include at least one alphanumeric character", project)
	}
	return token, nil
}

// IsIssueKey reports whether key is a Dispatch issue key (`LEGION-208`).
func IsIssueKey(key string) bool { return issueKey.MatchString(key) }

// IsRole reports whether role is one of the six a claim can be on.
func IsRole(role Role) bool {
	for _, known := range Roles {
		if role == known {
			return true
		}
	}
	return false
}

// NewToken is the token of the claim on issue's role: `legion-<project>-<issue project,
// lowercased>-<number>-<role>`, byte for byte the shipped roleToken
// (packages/contracts/src/legion-roles.ts:48-55). The token is the Envoy role the agent claims, so
// it has Envoy's role-token shape, and a peer that derives it from the issue and role in either
// language reaches the same agent. project is already a ProjectToken.
func NewToken(project, issue string, role Role) (Token, error) {
	if !projectToken.MatchString(project) {
		return "", fmt.Errorf("claim token: project %q is not a project token", project)
	}
	match := issueKey.FindStringSubmatch(issue)
	if match == nil {
		return "", fmt.Errorf("claim token: %q is not an issue key", issue)
	}
	if !IsRole(role) {
		return "", fmt.Errorf("claim token: %q is not a role", role)
	}
	return Token(fmt.Sprintf("legion-%s-%s-%s-%s", project, strings.ToLower(match[1]), match[2], role)), nil
}

// ControllerToken is the project controller's role token, `legion-<project>-controller`
// (legion-roles.ts:66-69). project is already a ProjectToken.
func ControllerToken(project string) Token { return Token("legion-" + project + "-controller") }
