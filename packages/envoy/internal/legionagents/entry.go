// Package legionagents is the Legion agent registry the listener keeps in the legion_agents KV
// bucket (bus.LegionAgentBucket): one entry per Legion session the daemon ran, which outlives the
// session, so a finished issue's agents can still be named and asked. The daemon writes it through
// the listener's PUT routes; Dispatch reads it through the GET routes (cmd/listener/api.go).
//
// Key layout. session.<session_id> holds the entry, and two index keys hold the same entry JSON:
// issue.<ISSUE>.<session_id> and role.<role_token>.<session_id>. project.<legion_project> is
// reserved for the daemon's own record (INFRA-204), and no read here reaches it: every read is a
// session key or an issue or role prefix. Nothing is ever deleted, so a closed issue's agents keep
// their entries and no read replays delete markers.
package legionagents

import (
	"cmp"
	"errors"
	"fmt"
	"regexp"
	"slices"
	"strings"
	"time"
)

// Entry is one Legion session as the registry stores it. Readers ignore fields they do not know,
// so a later writer may add some.
type Entry struct {
	// SessionID is the Oh My Pi or Claude Code session id.
	SessionID string `json:"session_id"`
	// LegionProject is the Legion project token the daemon's role tokens carry (claim.ProjectToken,
	// legionProjectToken in packages/contracts): legion.yaml's project lowercased with every
	// non-alphanumeric dropped. It is not the Dispatch project.
	LegionProject string `json:"legion_project"`
	// Issue is the Dispatch issue key the session works, upper case.
	Issue string `json:"issue"`
	// Role is one of Roles.
	Role string `json:"role"`
	// RoleToken is legion-<legion_project>-<issue lower-cased>-<role>, byte for byte what
	// claim.NewToken and roleToken build.
	RoleToken string `json:"role_token"`
	// Title is the session's title as stamped (`Legion tester · INFRA-71`).
	Title string `json:"title"`
	// Harness is omp or claude.
	Harness string `json:"harness"`
	// StartedAt is when the session started, RFC 3339 UTC.
	StartedAt string `json:"started_at"`
	// RecordedAt is when the listener first recorded the session, RFC 3339 UTC. The listener sets
	// it; a writer's value is ignored.
	RecordedAt string `json:"recorded_at"`
}

// Agent is an entry with what a read derives from the other sessions of its role token, ordered by
// started_at: the session that replaced it and when, both nil for the newest. Neither is stored.
type Agent struct {
	Entry
	ReplacedBy *string `json:"replaced_by"`
	ReplacedAt *string `json:"replaced_at"`
}

// Roles are the six Legion roles, in the order the workflow reaches them (claim.Roles,
// LEGION_ROLES in packages/contracts). An issue's agents are listed in this order.
var Roles = []string{"architect", "planner", "implementer", "tester", "reviewer", "merger"}

// Harnesses are the agent harnesses a Legion session runs under.
var Harnesses = []string{"omp", "claude"}

// MaxBatch is the most entries one batch write takes.
const MaxBatch = 256

// The longest each keyed field may be. They keep every key this package writes far inside the
// subject bound bus.KeyValue holds a key to, so a validated entry is never refused by NATS halfway
// through its writes: the longest key, role.<role_token>.<session_id>, stays under 260 bytes.
const (
	maxSessionIDBytes = 128
	maxProjectBytes   = 64
	maxIssueBytes     = 32
	maxTitleBytes     = 1024
)

var (
	// sessionIDPattern is a session id that is one KV key token: nats.go's key alphabet without
	// the dot, which separates tokens, and the slash and equals sign, which no session id holds and
	// a slash would split the route's path.
	sessionIDPattern = regexp.MustCompile(`^[A-Za-z0-9_-]+$`)
	// projectTokenPattern is a Legion project token (claim.ProjectToken).
	projectTokenPattern = regexp.MustCompile(`^[a-z0-9]+$`)
	// issueKeyPattern is a Dispatch issue key (claim.IsIssueKey, DISPATCH_KEY_PATTERN).
	issueKeyPattern = regexp.MustCompile(`^[A-Z][A-Z0-9]*-[0-9]+$`)
	// roleTokenPattern is a workflow role's token: legion-<project>-<issue project>-<number>-<role>.
	roleTokenPattern = regexp.MustCompile(`^legion-[a-z0-9]+-[a-z0-9]+-[0-9]+-(architect|planner|implementer|tester|reviewer|merger)$`)
)

// ErrInvalid is a session id, issue key, role token or entry the registry refuses. Each refusal
// wraps it with the field and the rule it breaks.
var ErrInvalid = errors.New("invalid legion agent")

// ErrNotFound is a session the registry holds no entry for.
var ErrNotFound = errors.New("unknown legion agent")

// ErrTooMany is a batch of more than MaxBatch entries.
var ErrTooMany = fmt.Errorf("a batch takes at most %d legion agents", MaxBatch)

// ConflictError is a write that would move a recorded session to another issue or role: a session
// belongs to one issue and role for good, so no index key is ever orphaned.
type ConflictError struct {
	SessionID, Issue, Role string
}

func (e *ConflictError) Error() string {
	return fmt.Sprintf("legion agent %s is recorded for %s %s", e.SessionID, e.Issue, e.Role)
}

// BatchError is the refusal of one entry of a batch, which refuses the whole batch.
type BatchError struct {
	Index int
	Err   error
}

func (e *BatchError) Error() string { return fmt.Sprintf("entries[%d]: %v", e.Index, e.Err) }

func (e *BatchError) Unwrap() error { return e.Err }

func invalid(format string, args ...any) error {
	return fmt.Errorf("%w: %s", ErrInvalid, fmt.Sprintf(format, args...))
}

// ValidateSessionID refuses a session id that is not one key token of at most 128 bytes.
func ValidateSessionID(id string) error {
	if len(id) > maxSessionIDBytes {
		return invalid("session_id is %d bytes, past %d", len(id), maxSessionIDBytes)
	}
	if !sessionIDPattern.MatchString(id) {
		return invalid("session_id %q must be letters, digits, '-' and '_'", id)
	}
	return nil
}

// ValidateIssue refuses a key that is not a Dispatch issue key.
func ValidateIssue(issue string) error {
	if len(issue) > maxIssueBytes {
		return invalid("issue is %d bytes, past %d", len(issue), maxIssueBytes)
	}
	if !issueKeyPattern.MatchString(issue) {
		return invalid("issue %q is not a Dispatch issue key (PROJECT-123)", issue)
	}
	return nil
}

// ValidateRoleToken refuses a token that is not a workflow role's token.
func ValidateRoleToken(token string) error {
	if limit := len("legion-") + maxProjectBytes + 1 + maxIssueBytes + 1 + len("implementer"); len(token) > limit {
		return invalid("role_token is %d bytes, past %d", len(token), limit)
	}
	if !roleTokenPattern.MatchString(token) {
		return invalid("role_token %q is not legion-<legion_project>-<issue>-<role>", token)
	}
	return nil
}

// RoleToken is the token of issue's role under the Legion project token project, the one a valid
// entry carries.
func RoleToken(project, issue, role string) string {
	return "legion-" + project + "-" + strings.ToLower(issue) + "-" + role
}

// Validate refuses an entry the registry does not store. Both writes, one entry and a batch, refuse
// through it, so they refuse the same entries.
func Validate(entry Entry) error {
	if err := ValidateSessionID(entry.SessionID); err != nil {
		return err
	}
	if len(entry.LegionProject) > maxProjectBytes {
		return invalid("legion_project is %d bytes, past %d", len(entry.LegionProject), maxProjectBytes)
	}
	if !projectTokenPattern.MatchString(entry.LegionProject) {
		return invalid("legion_project %q is not a Legion project token (lower-case letters and digits)", entry.LegionProject)
	}
	if err := ValidateIssue(entry.Issue); err != nil {
		return err
	}
	if !slices.Contains(Roles, entry.Role) {
		return invalid("role %q is not one of %s", entry.Role, strings.Join(Roles, ", "))
	}
	if want := RoleToken(entry.LegionProject, entry.Issue, entry.Role); entry.RoleToken != want {
		return invalid("role_token %q is not legion-<legion_project>-<issue>-<role>, %q", entry.RoleToken, want)
	}
	if len(entry.Title) > maxTitleBytes {
		return invalid("title is %d bytes, past %d", len(entry.Title), maxTitleBytes)
	}
	if !slices.Contains(Harnesses, entry.Harness) {
		return invalid("harness %q is not one of %s", entry.Harness, strings.Join(Harnesses, ", "))
	}
	if !isUTCTimestamp(entry.StartedAt) {
		return invalid("started_at %q is not an RFC 3339 UTC time (2026-10-26T03:15:00Z)", entry.StartedAt)
	}
	return nil
}

func isUTCTimestamp(value string) bool {
	_, err := time.Parse(time.RFC3339, value)
	return err == nil && strings.HasSuffix(value, "Z")
}

// sameSession reports whether stored and next name the same issue and role, the fields a session
// keeps for good.
func sameSession(stored, next Entry) bool {
	return stored.LegionProject == next.LegionProject && stored.Issue == next.Issue &&
		stored.Role == next.Role && stored.RoleToken == next.RoleToken
}

// startedAt is the entry's start, the order a role token's sessions replace each other in. A stored
// entry was validated, so it parses; one that does not sorts first.
func (e Entry) startedAt() time.Time {
	at, _ := time.Parse(time.RFC3339, e.StartedAt)
	return at
}

// oldestFirst orders one role token's sessions by when they started, then by when they were
// recorded, then by id, so two reads order the same sessions the same way.
func oldestFirst(a, b Entry) int {
	return cmp.Or(a.startedAt().Compare(b.startedAt()), cmp.Compare(a.RecordedAt, b.RecordedAt), cmp.Compare(a.SessionID, b.SessionID))
}

// succession derives each session's replacement among entries, every one of one role token: the
// next session to start replaced it, at that session's start. It returns them newest first, the
// first being the role's current holder.
func succession(entries []Entry) []Agent {
	slices.SortFunc(entries, oldestFirst)
	agents := make([]Agent, len(entries))
	for i, entry := range entries {
		agents[i] = Agent{Entry: entry}
		if i+1 < len(entries) {
			next := entries[i+1]
			agents[i].ReplacedBy, agents[i].ReplacedAt = &next.SessionID, &next.StartedAt
		}
	}
	slices.Reverse(agents)
	return agents
}

// issueOrder lists an issue's agents by role in workflow order, then by role token, then newest
// first, each role token's replacements derived among its own sessions.
func issueOrder(entries []Entry) []Agent {
	byToken := map[string][]Entry{}
	for _, entry := range entries {
		byToken[entry.RoleToken] = append(byToken[entry.RoleToken], entry)
	}
	agents := make([]Agent, 0, len(entries))
	for _, sessions := range byToken {
		agents = append(agents, succession(sessions)...)
	}
	slices.SortStableFunc(agents, func(a, b Agent) int {
		return cmp.Or(
			cmp.Compare(slices.Index(Roles, a.Role), slices.Index(Roles, b.Role)),
			cmp.Compare(a.RoleToken, b.RoleToken),
			-oldestFirst(a.Entry, b.Entry),
		)
	})
	return agents
}
