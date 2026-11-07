package config

import (
	"testing"
	"time"

	"github.com/sjawhar/legion/daemon/internal/claim"
)

// backstopFile is minimalFile with the key under test, written so each case names only its own
// roles.
func backstopFile(t *testing.T, body string) string {
	t.Helper()
	return writeConfigFile(t, minimalFile+"controller: daemon\n"+body)
}

// A file that names no bound runs every role on its own stall ladder's length: the bound that
// role's plugin holds a call no deadline judges to, the grace over it, and the two gaps between
// the ladder's steps. The implementer and the reviewer wait four hours inside one call, so their
// ladder is the long one; every other role's and the controller's is the hour's.
func TestTheStallBackstopDefaultsToEachRolesOwnLadder(t *testing.T) {
	cfg, err := Load(backstopFile(t, ""), noEnv)
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	if cfg.WorkerStallBackstops != nil {
		t.Fatalf("worker_stall_backstop_hours = %v, want none named", cfg.WorkerStallBackstops)
	}
	short := StallCallBound + StallCallGrace + 2*StallStep
	long := StallCallBoundLong + StallCallGrace + 2*StallStep
	for role, want := range map[claim.Role]time.Duration{
		claim.RoleArchitect:   short,
		claim.RolePlanner:     short,
		claim.RoleImplementer: long,
		claim.RoleTester:      short,
		claim.RoleReviewer:    long,
		claim.RoleMerger:      short,
		claim.RoleController:  short,
	} {
		if got := cfg.StallBackstopFor(role); got != want {
			t.Errorf("StallBackstopFor(%s) = %s, want %s", role, got, want)
		}
		if got := cfg.StallLadderFor(role); got != want {
			t.Errorf("StallLadderFor(%s) = %s, want %s", role, got, want)
		}
	}
}

// A role the key names runs on the bound it names, in hours and decimal hours alike, and every
// role it leaves out keeps its own ladder's length. The controller is a role the key may name,
// as it is one `role_config_files` may name.
func TestTheStallBackstopTakesAPerRoleBound(t *testing.T) {
	cfg, err := Load(backstopFile(t, "worker_stall_backstop_hours:\n  implementer: 6\n  controller: 1.5\n"), noEnv)
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	if got := cfg.StallBackstopFor(claim.RoleImplementer); got != 6*time.Hour {
		t.Errorf("the implementer's bound = %s, want 6h", got)
	}
	if got := cfg.StallBackstopFor(claim.RoleController); got != 90*time.Minute {
		t.Errorf("the controller's bound = %s, want 1h30m", got)
	}
	if want := StallCallBoundLong + StallCallGrace + 2*StallStep; cfg.StallBackstopFor(claim.RoleReviewer) != want {
		t.Errorf("the reviewer's bound = %s, want its own ladder %s", cfg.StallBackstopFor(claim.RoleReviewer), want)
	}
}

// 0 is the backstop off for that role, and it is the one value below the role's ladder the file
// may set: no sweep acts on a role whose bound is 0, so nothing of the ladder is in its way.
func TestAZeroStallBackstopSwitchesTheRoleOff(t *testing.T) {
	cfg, err := Load(backstopFile(t, "worker_stall_backstop_hours: {tester: 0, implementer: 0}\n"), noEnv)
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	for _, role := range []claim.Role{claim.RoleTester, claim.RoleImplementer} {
		if got := cfg.StallBackstopFor(role); got != 0 {
			t.Errorf("StallBackstopFor(%s) = %s, want the backstop off", role, got)
		}
	}
	if want := StallCallBound + StallCallGrace + 2*StallStep; cfg.StallBackstopFor(claim.RoleMerger) != want {
		t.Errorf("the merger's bound = %s, want its own ladder %s: 0 is one role's answer, not the file's", cfg.StallBackstopFor(claim.RoleMerger), want)
	}
}

// A bound below the role's whole ladder is refused with the arithmetic in the message, because
// the daemon would end a turn the plugin it backs up is still recovering. The ladder's own length
// passes: that is the first bound at which nothing of the plugin's is left to run.
func TestAStallBackstopBelowTheLadderIsRefusedWithItsArithmetic(t *testing.T) {
	_, err := Load(backstopFile(t, "worker_stall_backstop_hours: {implementer: 4}\n"), noEnv)
	want := "worker_stall_backstop_hours.implementer (4h0m0s) is below the whole stall ladder the implementer's own plugin runs (4h0m0s + 5m0s + 2 × 2m0s = 4h9m0s): the daemon's backstop would end a turn the plugin it backs up is still recovering; set 0 to switch the backstop off for that role"
	if err == nil || err.Error() != want {
		t.Fatalf("Load = %v, want %q", err, want)
	}

	ladder := StallCallBoundLong + StallCallGrace + 2*StallStep
	cfg, err := Load(backstopFile(t, "worker_stall_backstop_hours: {implementer: 4.15}\n"), noEnv)
	if err != nil {
		t.Fatalf("Load of the ladder's own length: %v", err)
	}
	if got := cfg.StallBackstopFor(claim.RoleImplementer); got != ladder {
		t.Fatalf("the implementer's bound = %s, want the ladder %s accepted", got, ladder)
	}
}

// The floor is the ladder the worker will actually run, so it reads the deployment's own
// LEGION_STALL_* numbers where it sets them: one LEGION_STALL_CALL_BOUND_MS is every role's, as
// the daemon copies one name from its own environment to every session, so a bound that clears
// the default ladder can still be below the deployment's own.
func TestTheStallBackstopFloorReadsTheEnvironmentsBound(t *testing.T) {
	env := envMap(map[string]string{
		"LEGION_STALL_CALL_BOUND_MS":  "21600000",
		"LEGION_STALL_CALL_GRACE_MS":  "600000",
		"LEGION_STALL_STEP_MS":        "180000",
		"LEGION_STALL_NOT_A_LIMIT_MS": "1",
	})
	_, err := Load(backstopFile(t, "worker_stall_backstop_hours: {tester: 5}\n"), env)
	want := "worker_stall_backstop_hours.tester (5h0m0s) is below the whole stall ladder the tester's own plugin runs (6h0m0s + 10m0s + 2 × 3m0s = 6h16m0s): the daemon's backstop would end a turn the plugin it backs up is still recovering; set 0 to switch the backstop off for that role"
	if err == nil || err.Error() != want {
		t.Fatalf("Load = %v, want %q", err, want)
	}

	// The same numbers move the default every role the file leaves out runs on, the
	// implementer's included: the variable replaces the role's own default for every role alike.
	cfg, err := Load(backstopFile(t, ""), env)
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	for _, role := range []claim.Role{claim.RoleTester, claim.RoleImplementer, claim.RoleController} {
		if got, want := cfg.StallBackstopFor(role), 6*time.Hour+16*time.Minute; got != want {
			t.Errorf("StallBackstopFor(%s) = %s, want the environment's own ladder %s", role, got, want)
		}
	}

	// A value the worker's own plugin would refuse — not a whole number of milliseconds, or
	// below zero — is the default there and here, since the default is what that worker runs on.
	bad := envMap(map[string]string{"LEGION_STALL_CALL_BOUND_MS": "soon", "LEGION_STALL_STEP_MS": "-1"})
	cfg, err = Load(backstopFile(t, ""), bad)
	if err != nil {
		t.Fatalf("Load with unreadable numbers: %v", err)
	}
	if got, want := cfg.StallBackstopFor(claim.RoleTester), StallCallBound+StallCallGrace+2*StallStep; got != want {
		t.Errorf("StallBackstopFor(tester) = %s, want the plugin's own defaults %s", got, want)
	}
}

// What the key refuses before anything else is read.
func TestStallBackstopRefusals(t *testing.T) {
	for _, tc := range []struct{ name, body, want string }{
		{"not a mapping", "worker_stall_backstop_hours: 6\n",
			"worker_stall_backstop_hours must be a mapping of a role to the hours the daemon's backstop waits, e.g. {implementer: 6, tester: 0}"},
		{"an unknown role", "worker_stall_backstop_hours: {coder: 6}\n",
			`worker_stall_backstop_hours names "coder", which is not a Legion role (architect, planner, implementer, tester, reviewer, merger, controller)`},
		{"a role named twice", "worker_stall_backstop_hours: {tester: 2, tester: 3}\n",
			"worker_stall_backstop_hours names tester twice"},
		{"a value that is no number", "worker_stall_backstop_hours: {tester: soon}\n",
			"worker_stall_backstop_hours.tester must be a number of hours, or 0 to switch the backstop off for that role"},
		{"a negative bound", "worker_stall_backstop_hours: {tester: -1}\n",
			"worker_stall_backstop_hours.tester must be 0 (the backstop off for that role) or a positive number of hours"},
		{"a bound past the timer bound", "worker_stall_backstop_hours: {tester: 1000}\n",
			"worker_stall_backstop_hours.tester must be at most 596"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			_, err := Load(backstopFile(t, tc.body), noEnv)
			if err == nil || err.Error() != tc.want {
				t.Fatalf("Load = %v, want %q", err, tc.want)
			}
		})
	}
}
