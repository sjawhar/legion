package config

import (
	"testing"
	"time"

	"github.com/sjawhar/legion/daemon/internal/claim"
)

// A file that names no limit runs on the measured defaults: a controller's turn is bounded at
// half an hour, an implementer's and a reviewer's at six hours, and every other role's at two.
func TestTurnWatchdogDefaults(t *testing.T) {
	cfg, err := Load(writeConfigFile(t, minimalFile), noEnv)
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	for role, want := range map[claim.Role]time.Duration{
		claim.RoleController:  30 * time.Minute,
		claim.RoleArchitect:   2 * time.Hour,
		claim.RolePlanner:     2 * time.Hour,
		claim.RoleTester:      2 * time.Hour,
		claim.RoleMerger:      2 * time.Hour,
		claim.RoleImplementer: 6 * time.Hour,
		claim.RoleReviewer:    6 * time.Hour,
	} {
		if got := cfg.TurnLimitFor(role); got != want {
			t.Errorf("TurnLimitFor(%s) = %s, want %s", role, got, want)
		}
	}
}

// Every role is overridable one at a time, and 0 switches the watchdog off for that role alone.
func TestTurnWatchdogOverridesOneRoleAtATime(t *testing.T) {
	cfg, err := Load(writeConfigFile(t, minimalFile+"turn_watchdog_seconds: {implementer: 0, controller: 600}\n"), noEnv)
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	if got := cfg.TurnLimitFor(claim.RoleImplementer); got != 0 {
		t.Errorf("TurnLimitFor(implementer) = %s, want 0: the file switched the watchdog off for it", got)
	}
	if got := cfg.TurnLimitFor(claim.RoleController); got != 10*time.Minute {
		t.Errorf("TurnLimitFor(controller) = %s, want 10m", got)
	}
	if got := cfg.TurnLimitFor(claim.RoleReviewer); got != 6*time.Hour {
		t.Errorf("TurnLimitFor(reviewer) = %s, want its default of 6h", got)
	}
}

func TestTurnWatchdogRefusals(t *testing.T) {
	for _, tc := range []struct {
		name, line, want string
	}{
		{"a list", "turn_watchdog_seconds: [implementer]",
			"turn_watchdog_seconds must be a mapping of a role to the seconds its turn may produce no event (0 switches the watchdog off for that role), e.g. {implementer: 21600}"},
		{"an unknown role", "turn_watchdog_seconds: {coder: 60}",
			`turn_watchdog_seconds names "coder", which is not a role the daemon supervises (architect, planner, implementer, tester, reviewer, merger, controller)`},
		{"a role named twice", "turn_watchdog_seconds: {tester: 60, tester: 120}",
			"turn_watchdog_seconds names tester twice"},
		{"a value that is no whole number of seconds", "turn_watchdog_seconds: {tester: 2h}",
			"turn_watchdog_seconds.tester must be a whole number of seconds (0 switches the watchdog off for that role)"},
		{"a negative value", "turn_watchdog_seconds: {tester: -1}",
			"turn_watchdog_seconds.tester must not be negative"},
		{"a value past the timer bound", "turn_watchdog_seconds: {tester: 2147484}",
			"turn_watchdog_seconds.tester must be at most 2147483"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			_, err := Load(writeConfigFile(t, minimalFile+tc.line+"\n"), noEnv)
			if err == nil || err.Error() != tc.want {
				t.Fatalf("Load error = %v, want %q", err, tc.want)
			}
		})
	}
}
