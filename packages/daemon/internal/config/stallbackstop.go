package config

import (
	"fmt"
	"math"
	"slices"
	"strconv"
	"time"

	"gopkg.in/yaml.v3"

	"github.com/sjawhar/legion/daemon/internal/claim"
	"github.com/sjawhar/legion/daemon/internal/runtime"
)

// stallBackstopKey is the per-role bound of the daemon's own stall backstop: how long after a
// claim's recorded last step (supervise.StepRecord) the daemon ends the turn that step belongs to
// and brings the session back with its task still owed (internal/daemon's backstopStalledTurns).
// It is a mapping of a Legion role to a decimal number of hours, the shape `worker_backends` and
// `role_config_files` give a per-role key, and like them it may name the controller.
const stallBackstopKey = "worker_stall_backstop_hours"

// StallLimits are the three numbers of the worker's own stall watch as this daemon's environment
// sets them, which it passes on to every session it starts (internal/daemon's
// stallLimitVariables): the grace over a call's own declared deadline, the bound for a call no
// deadline judges, and the gap between the recovery ladder's steps. Each is read exactly as the
// plugin reads it (`packages/pi-shared/src/stall-limits.ts`, integerSetting): a whole number of
// milliseconds of 0 or more, and the plugin's own default for a variable the deployment leaves
// out or writes badly, which is the default the plugin would then run on.
//
// The daemon holds them for one purpose: its backstop bound must stay above the whole ladder a
// working plugin runs, and the file is refused for a bound below it (checkStallBackstops). They
// are no legion.yaml key, so no settings block carries them (api's settingsExcluded).
type StallLimits struct {
	// CallBound is LEGION_STALL_CALL_BOUND_MS where the deployment set one, and nil where it set
	// none. It is a pointer because one value set there is every role's — the daemon copies one
	// name from its own environment to every session — while the default it replaces is the
	// role's own (StallCallBoundFor), and nothing else tells the two apart.
	CallBound *time.Duration
	// CallGrace is LEGION_STALL_CALL_GRACE_MS, StallCallGrace where the deployment set none.
	CallGrace time.Duration
	// Step is LEGION_STALL_STEP_MS, StallStep where the deployment set none.
	Step time.Duration
}

// CallBoundFor is the bound a working plugin holds a call no deadline judges to for role: the
// deployment's one value for every role where it set one, otherwise the role's own default.
func (l StallLimits) CallBoundFor(role claim.Role) time.Duration {
	if l.CallBound != nil {
		return *l.CallBound
	}
	return StallCallBoundFor(role)
}

// resolveStallLimits reads the three numbers from the daemon's own environment, as the plugin
// reads them from a worker's: a whole number of milliseconds of 0 or more, else the default. A
// value the plugin would refuse is the default here too, because the default is what the worker
// that reads the same variable will run on.
func resolveStallLimits(env func(string) string) StallLimits {
	limits := StallLimits{CallGrace: StallCallGrace, Step: StallStep}
	if bound, ok := stallMilliseconds(env("LEGION_STALL_CALL_BOUND_MS")); ok {
		limits.CallBound = &bound
	}
	if grace, ok := stallMilliseconds(env("LEGION_STALL_CALL_GRACE_MS")); ok {
		limits.CallGrace = grace
	}
	if step, ok := stallMilliseconds(env("LEGION_STALL_STEP_MS")); ok {
		limits.Step = step
	}
	return limits
}

// stallMilliseconds reads one LEGION_STALL_*_MS value: a whole number of milliseconds of 0 or
// more. An unset, empty or unreadable value answers false, which leaves the default standing.
func stallMilliseconds(value string) (time.Duration, bool) {
	if value == "" {
		return 0, false
	}
	ms, err := strconv.Atoi(value)
	if err != nil || ms < 0 {
		return 0, false
	}
	return time.Duration(ms) * time.Millisecond, true
}

// StallLadderFor is the whole ladder a working plugin runs for role before anything is left for
// the daemon to do: the bound it holds a call no deadline judges to, the grace over that bound,
// and the two gaps between the ladder's three recovery steps. It is the floor of the daemon's own
// backstop bound (checkStallBackstops) and the bound a role the file does not name runs on
// (StallBackstopFor), because the backstop exists to act where that ladder did not.
func (c Config) StallLadderFor(role claim.Role) time.Duration {
	return c.StallLimits.CallBoundFor(role) + c.StallLimits.CallGrace + 2*c.StallLimits.Step
}

// StallBackstopFor is how long after a claim's recorded last step the daemon ends role's turn
// itself: the bound `worker_stall_backstop_hours` names for that role, and the role's whole
// ladder (StallLadderFor) where the file names none. 0 is the backstop off for that role, which
// only the file can ask for: no sweep acts on a role whose bound is 0.
func (c Config) StallBackstopFor(role claim.Role) time.Duration {
	if bound, named := c.WorkerStallBackstops[role]; named {
		return bound
	}
	return c.StallLadderFor(role)
}

// readStallBackstops is `worker_stall_backstop_hours`: a mapping of a Legion role
// (RoleConfigRoles, the controller among them) to the number of hours the daemon's backstop waits
// after that role's last recorded step, a decimal among them as every other hours key is
// (readHours). 0 is the backstop off for that role and is the one reason this key reads its own
// numbers rather than calling readHours, which refuses 0 because every key it serves arms a timer
// that a zero would fire at once. A role named twice, a name that is not a Legion role, a value
// that is no number and a negative value are each refused naming the entry.
func readStallBackstops(value *yaml.Node, key string) (map[claim.Role]time.Duration, error) {
	if value.Tag == "!!null" {
		return nil, nil
	}
	if value.Kind != yaml.MappingNode {
		return nil, fmt.Errorf("%s must be a mapping of a role to the hours the daemon's backstop waits, e.g. {implementer: 6, tester: 0}", key)
	}
	bounds := map[claim.Role]time.Duration{}
	for i := 0; i+1 < len(value.Content); i += 2 {
		roleNode, hoursNode := value.Content[i], value.Content[i+1]
		role := claim.Role(roleNode.Value)
		if !slices.Contains(RoleConfigRoles, role) {
			return nil, fmt.Errorf("%s names %q, which is not a Legion role (%s)", key, roleNode.Value, runtime.JoinRoles(RoleConfigRoles))
		}
		if _, seen := bounds[role]; seen {
			return nil, fmt.Errorf("%s names %s twice", key, role)
		}
		field := key + "." + string(role)
		var hours float64
		if hoursNode.Kind != yaml.ScalarNode || (hoursNode.Tag != "!!int" && hoursNode.Tag != "!!float") || hoursNode.Decode(&hours) != nil {
			return nil, fmt.Errorf("%s must be a number of hours, or 0 to switch the backstop off for that role", field)
		}
		if hours < 0 {
			return nil, fmt.Errorf("%s must be 0 (the backstop off for that role) or a positive number of hours", field)
		}
		if hours > maxTimerSeconds/3600 {
			return nil, fmt.Errorf("%s must be at most %d", field, maxTimerSeconds/3600)
		}
		bound := time.Duration(math.Round(hours * float64(time.Hour)))
		if hours > 0 && bound <= 0 {
			return nil, fmt.Errorf("%s must be 0 (the backstop off for that role) or a positive number of hours", field)
		}
		bounds[role] = bound
	}
	return bounds, nil
}

// checkStallBackstops refuses a backstop bound below the whole ladder the role's own plugin runs
// (StallLadderFor), once the daemon's environment is settled: the bound that plugin holds a call
// no deadline judges to — the deployment's LEGION_STALL_CALL_BOUND_MS where it set one, otherwise
// the role's own default — plus the grace over it and the two gaps between the ladder's steps. A
// shorter bound would have the daemon end a turn while the plugin it exists to back up is still
// recovering it, and the daemon cannot see that ladder running, so the floor is the whole of what
// keeps the two apart. 0 is the backstop off for that role and is refused nothing.
func checkStallBackstops(cfg Config) error {
	for _, role := range RoleConfigRoles {
		bound, named := cfg.WorkerStallBackstops[role]
		if !named || bound == 0 {
			continue
		}
		ladder := cfg.StallLadderFor(role)
		if bound >= ladder {
			continue
		}
		return fmt.Errorf("%s.%s (%s) is below the whole stall ladder the %s's own plugin runs (%s + %s + 2 × %s = %s): the daemon's backstop would end a turn the plugin it backs up is still recovering; set 0 to switch the backstop off for that role",
			stallBackstopKey, role, bound, role,
			cfg.StallLimits.CallBoundFor(role), cfg.StallLimits.CallGrace, cfg.StallLimits.Step, ladder)
	}
	return nil
}
