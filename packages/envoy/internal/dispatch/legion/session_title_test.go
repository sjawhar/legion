package legion

import "testing"

// TestASessionTitleNamesARoleAndItsIssue is the title parsing every rule reads a Legion session
// through (packages/pi-legion/src/session-title.ts).
func TestASessionTitleNamesARoleAndItsIssue(t *testing.T) {
	for _, one := range []struct {
		title string
		want  SessionTitle
		ok    bool
	}{
		{"Legion implementer · INFRA-1077", SessionTitle{Role: "implementer", Issue: "INFRA-1077"}, true},
		{"Legion architect · INFRA-979", SessionTitle{Role: "architect", Issue: "INFRA-979"}, true},
		{"Legion controller · INFRA", SessionTitle{Role: "controller", Project: "INFRA", Controller: true}, true},
		{"Legion gardener · INFRA-1", SessionTitle{}, false},
		{"Legion architect", SessionTitle{}, false},
		{"A person's session", SessionTitle{}, false},
		{"", SessionTitle{}, false},
	} {
		t.Run(one.title, func(t *testing.T) {
			got, ok := ParseSessionTitle(one.title)
			if ok != one.ok || got != one.want {
				t.Fatalf("ParseSessionTitle(%q) = %+v, %t; want %+v, %t", one.title, got, ok, one.want, one.ok)
			}
			if ok && !got.OfProject("INFRA") {
				t.Errorf("%q is not read as INFRA's", one.title)
			}
		})
	}
}

// TestRoleSessionWorksIsTheTitleHalfOfPhaseWork: the one rule both readers of the phase-work
// fact ask (PhaseWorkKeys) — a phase worker or a root architect of that very issue, never a
// controller, and never a role working another issue.
func TestRoleSessionWorksIsTheTitleHalfOfPhaseWork(t *testing.T) {
	for _, one := range []struct {
		title, issue string
		want         bool
	}{
		{"Legion implementer · INFRA-1077", "INFRA-1077", true},
		{"Legion architect · INFRA-1077", "INFRA-1077", true},
		{"Legion implementer · INFRA-1077", "INFRA-980", false},
		{"Legion controller · INFRA", "INFRA", false},
		{"A person's session", "INFRA-1077", false},
		{"", "INFRA-1077", false},
	} {
		if got := RoleSessionWorks(one.title, one.issue); got != one.want {
			t.Errorf("RoleSessionWorks(%q, %q) = %t, want %t", one.title, one.issue, got, one.want)
		}
	}
}

// TestWorkerTitleIsTheOneStatementOfTheWorkerRule: which titles are a Legion worker's — a phase
// worker's or a root architect's, never the project controller's — is said once (WorkerTitle),
// and the three readers of it ask it rather than stating it again: the project-wide read's
// RoleSessions (PhaseWorkKeys), the board's index of the sessions that can move that answer
// (api's legionPhaseWork.observe) and the issue half below (RoleSessionWorks).
func TestWorkerTitleIsTheOneStatementOfTheWorkerRule(t *testing.T) {
	for _, one := range []struct {
		title string
		want  SessionTitle
		ok    bool
	}{
		{"Legion implementer · INFRA-1077", SessionTitle{Role: "implementer", Issue: "INFRA-1077"}, true},
		{"Legion architect · INFRA-979", SessionTitle{Role: "architect", Issue: "INFRA-979"}, true},
		{"Legion controller · INFRA", SessionTitle{}, false},
		{"A person's session", SessionTitle{}, false},
		{"", SessionTitle{}, false},
	} {
		got, ok := WorkerTitle(one.title)
		if ok != one.ok || got != one.want {
			t.Errorf("WorkerTitle(%q) = %+v, %t; want %+v, %t", one.title, got, ok, one.want, one.ok)
		}
		// A title WorkerTitle refuses works no issue either: the issue half asks it first.
		if works := RoleSessionWorks(one.title, one.want.Issue); !one.ok && works {
			t.Errorf("RoleSessionWorks(%q, %q) = true, want false: the title is no worker's", one.title, one.want.Issue)
		}
	}
}
