package legion

import "strings"

// session_title.go holds the one reading of a Legion session's title. Both halves of the rule
// that places an issue in a tree ask it — the project-wide read of the issues that carry phase
// work (PhaseWorkKeys, which the Legion view and the metrics both stand on) and the metrics'
// own window reads — so it lives here, beside the walk, rather than in either reader.

// roles are Legion's six worker types, in the order the workflow runs them. The daemon's
// claim.Roles is the same list in the daemon's own module (packages/daemon/internal/claim/
// token.go), which this module does not import; a deployment changes each role's prompt, model
// and harness, not the roles.
var roles = []string{"architect", "planner", "implementer", "tester", "reviewer", "merger"}

// The two shapes a Legion session titles itself (packages/pi-legion/src/session-title.ts): a
// phase worker or a root architect names its role and its issue, a controller names its project.
const (
	legionTitlePrefix     = "Legion "
	legionTitleSeparator  = " · "
	legionControllerTitle = "controller"
)

// SessionTitle is what a session's title says the session is: one of the six roles working one
// issue, or a project's controller.
type SessionTitle struct {
	Role string
	// Issue is the issue the role works; a root architect's is its tree's root.
	Issue string
	// Project is a controller's project; empty for a phase worker.
	Project string
	// Controller is whether this is the project's controller session.
	Controller bool
}

// ParseSessionTitle reads a session title as Legion writes it: `Legion <role> · <KEY>` for a root
// architect or a phase worker, and `Legion controller · <PROJECT>` for a controller. A title of
// any other shape, or one naming a role Legion does not run, is not a Legion session.
func ParseSessionTitle(title string) (SessionTitle, bool) {
	rest, isLegion := strings.CutPrefix(strings.TrimSpace(title), legionTitlePrefix)
	if !isLegion {
		return SessionTitle{}, false
	}
	role, subject, found := strings.Cut(rest, legionTitleSeparator)
	if !found {
		return SessionTitle{}, false
	}
	role, subject = strings.TrimSpace(role), strings.TrimSpace(subject)
	if subject == "" {
		return SessionTitle{}, false
	}
	if role == legionControllerTitle {
		return SessionTitle{Role: legionControllerTitle, Project: subject, Controller: true}, true
	}
	for _, known := range roles {
		if role == known {
			return SessionTitle{Role: role, Issue: subject}, true
		}
	}
	return SessionTitle{}, false
}

// Works is whether the session works issue: a phase worker or a root architect of that very
// issue, never a controller, whose subject is a project.
func (t SessionTitle) Works(issue string) bool {
	return !t.Controller && t.Issue == issue
}

// RoleSessionWorks is whether a session titled title is a Legion role working issue. It is the
// title half of what makes an issue carry Legion phase work, which the project-wide read asks
// for every (issue, session) pair it finds (PhaseWorkKeys) and the metrics' own window read asks
// for the pairs of its scope (metrics.readRoleEvents).
func RoleSessionWorks(title, issue string) bool {
	parsed, ok := ParseSessionTitle(title)
	return ok && parsed.Works(issue)
}

// OfProject is whether the session belongs to the project: a controller by the project it names
// (the daemon lowercases the project token into subjects and paths, and the title uppercases it
// again, so the comparison ignores case), a phase worker by the project its issue's key names.
func (t SessionTitle) OfProject(project string) bool {
	if t.Controller {
		return strings.EqualFold(t.Project, project)
	}
	key, _, found := strings.Cut(t.Issue, "-")
	return found && strings.EqualFold(key, project)
}
