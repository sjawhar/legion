package workflow

import (
	"testing"

	"github.com/sjawhar/legion/daemon/internal/claim"
	"github.com/sjawhar/legion/daemon/internal/config"
	"github.com/sjawhar/legion/daemon/internal/intake"
	"github.com/sjawhar/legion/daemon/internal/phase"
	"github.com/sjawhar/legion/daemon/internal/record"
)

// The boot's resume by rule, the one trigger main did not have, starts only a phase owner the daemon
// or its own exit suspended. One whose suspension predates migration 0038 (unknown) is left as it
// was, so a boot never overrides a drain made before the upgrade; the start its phase or its tree's
// re-admission writes later resumes it, as on main.
func TestTheBootStartsNoClaimSuspendedBeforeItsSourceWasRecorded(t *testing.T) {
	pool := migratedPool(t)
	ctx := t.Context()
	project, err := claim.ProjectToken("LEGION")
	if err != nil {
		t.Fatal(err)
	}
	for i, tc := range []struct{ key, by string }{{"LEGION-208", "daemon"}, {"LEGION-209", "unknown"}} {
		seedIssue(t, pool, record.Issue{Key: tc.key, Repo: "acme/widgets", DefaultBranch: "main", GitHubRepo: "acme/widgets", Tree: tc.key, Project: "LEGION", Title: tc.key, Phase: phase.Implementing,
			Generation: 1, Status: "in_progress", Rank: "U", HandedOver: true})
		seedSlot(t, pool, record.Slot{Issue: tc.key, Index: i})
		token, err := claim.NewToken(project, tc.key, claim.RoleImplementer)
		if err != nil {
			t.Fatal(err)
		}
		if _, err := pool.Exec(ctx, `insert into claims (token, project, tree, issue, role, generation, session, session_file,
			state, launch_failures, prompt_failures, prompt_retires, uncertain_streak, suspended_by)
			values ($1, $2, $3, $3, 'implementer', 1, 'ses', '/sessions/ses.jsonl', 'suspended', 0, 0, 0, 0, $4)`,
			string(token), project, tc.key, tc.by); err != nil {
			t.Fatalf("seed the %s implementer: %v", tc.key, err)
		}
	}

	if _, err := intake.ApplyFact(ctx, pool, "daemon", "boot:LEGION:1", intake.DaemonBooted{Boot: 1}, testEngine(config.DesignGateRootIssues, nil), admissionStub{}); err != nil {
		t.Fatalf("apply the boot: %v", err)
	}

	if got, want := superviseRequests(t, pool, "start"), map[string]int{"LEGION-208/implementer": 1}; !sameCounts(got, want) {
		t.Fatalf("the boot queued starts %v, want only the daemon-suspended implementer's %v", got, want)
	}
}
