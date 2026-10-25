package config

import (
	"fmt"
	"slices"
	"time"

	"gopkg.in/yaml.v3"

	"github.com/sjawhar/legion/daemon/internal/claim"
	"github.com/sjawhar/legion/daemon/internal/runtime"
)

// turnWatchdogKey is the legion.yaml key: a mapping of a role to the seconds a turn of that role
// may produce no event before the daemon's turn watchdog acts on it
// (internal/supervise/watchdog.go).
const turnWatchdogKey = "turn_watchdog_seconds"

// turnWatchdogDefaults are the limits a deployment that names none runs on, per role. They come
// from what real turns do, measured on a deployment over the 24 hours to 2026-10-25 across 2,059
// completed turns of every role: the longest silence inside an open turn was 2h24m (an
// implementer, a subagent running) and 1h00m (a reviewer), where an architect's was 12m18s, a
// planner's 16m24s, a tester's 21m25s, a merger's 23s, and a controller's whole turn never
// reached 6m23s. Each default is several times its role's measured worst case, so the watchdog
// acts on a turn no work explains and on nothing else.
var turnWatchdogDefaults = map[claim.Role]time.Duration{
	claim.RoleController:  30 * time.Minute,
	claim.RoleArchitect:   2 * time.Hour,
	claim.RolePlanner:     2 * time.Hour,
	claim.RoleTester:      2 * time.Hour,
	claim.RoleMerger:      2 * time.Hour,
	claim.RoleImplementer: 6 * time.Hour,
	claim.RoleReviewer:    6 * time.Hour,
}

// turnWatchdogRoles are the roles the key may name, in the order a refusal lists them: the six
// issue roles and the controller, whose own claim the daemon supervises like any other.
func turnWatchdogRoles() []claim.Role {
	return append(slices.Clone(claim.Roles), claim.RoleController)
}

// TurnLimitFor is how long a turn of role may produce no event before the turn watchdog acts, and
// zero where the deployment switched the watchdog off for that role. Every role is settled at
// load, so this reads the map rather than falling back.
func (c Config) TurnLimitFor(role claim.Role) time.Duration {
	return c.TurnWatchdog[role]
}

// readTurnWatchdog is `turn_watchdog_seconds`: a mapping of a role to a whole number of seconds,
// 0 switching the watchdog off for that role. A name that is no role, a role named twice, a value
// that is not a whole number of seconds, a negative one and one past the timer bound are each
// refused naming the entry, as every other duration key's value is.
func readTurnWatchdog(value *yaml.Node, key string) (map[claim.Role]time.Duration, error) {
	if value.Tag == "!!null" {
		return nil, nil
	}
	if value.Kind != yaml.MappingNode {
		return nil, fmt.Errorf("%s must be a mapping of a role to the seconds its turn may produce no event (0 switches the watchdog off for that role), e.g. {implementer: 21600}", key)
	}
	limits := map[claim.Role]time.Duration{}
	for i := 0; i+1 < len(value.Content); i += 2 {
		roleNode, secondsNode := value.Content[i], value.Content[i+1]
		role := claim.Role(roleNode.Value)
		if !slices.Contains(turnWatchdogRoles(), role) {
			return nil, fmt.Errorf("%s names %q, which is not a role the daemon supervises (%s)", key, roleNode.Value, runtime.JoinRoles(turnWatchdogRoles()))
		}
		if _, seen := limits[role]; seen {
			return nil, fmt.Errorf("%s names %s twice", key, role)
		}
		var seconds int
		if secondsNode.Kind != yaml.ScalarNode || secondsNode.Decode(&seconds) != nil {
			return nil, fmt.Errorf("%s.%s must be a whole number of seconds (0 switches the watchdog off for that role)", key, role)
		}
		if seconds < 0 {
			return nil, fmt.Errorf("%s.%s must not be negative", key, role)
		}
		if seconds > maxTimerSeconds {
			return nil, fmt.Errorf("%s.%s must be at most %d", key, role, maxTimerSeconds)
		}
		limits[role] = time.Duration(seconds) * time.Second
	}
	return limits, nil
}

// settleTurnWatchdog is every role's limit: the default unless the file named one for that role.
// Every role is present, so the supervisor reads one map and no caller repeats the defaults.
func settleTurnWatchdog(file map[claim.Role]time.Duration) map[claim.Role]time.Duration {
	limits := make(map[claim.Role]time.Duration, len(turnWatchdogDefaults))
	for _, role := range turnWatchdogRoles() {
		limits[role] = turnWatchdogDefaults[role]
	}
	for role, limit := range file {
		limits[role] = limit
	}
	return limits
}
