package store

import (
	"context"
	"testing"

	"github.com/sjawhar/legion/daemon/internal/claim"
	"github.com/sjawhar/legion/daemon/internal/supervise"
)

// A database that admitted trees before the lifecycle barrier existed keeps them runnable: the
// migration opens epoch one for each workflow root (under the normalized project token its claims
// use, not the issue row's Dispatch key) and for each operator tree no workflow issue backs, and
// binds the existing claims to it. Without that, the first relaunch of a live claim would be
// refused as unbound and its next start as having no lifecycle.
func TestTheLifecycleMigrationOpensEpochOneForTreesAdmittedBeforeIt(t *testing.T) {
	store := emptyStore(t)
	ctx := context.Background()
	migrateThrough(t, store, 30)
	if _, err := store.Pool().Exec(ctx, `insert into issues
		(key, tree, project, title, phase, generation, status, rank, last_dispatch_seq)
		values ('LEGION-208', 'LEGION-208', 'LEGION', 'root', 'implementing', 1, 'in_progress', 'A', 1),
		       ('LEGION-209', 'LEGION-208', 'LEGION', 'child', 'implementing', 1, 'in_progress', 'B', 1)`); err != nil {
		t.Fatal(err)
	}
	for _, c := range []supervise.Claim{
		{Token: claim.Token("legion-legion-legion-209-implementer"), Project: "legion", Tree: "LEGION-208", Issue: "LEGION-209", Role: claim.RoleImplementer, State: supervise.StateIdle, Generation: 2},
		{Token: claim.Token("legion-legion-legion-300-architect"), Project: "legion", Tree: "LEGION-300", Issue: "LEGION-300", Role: claim.RoleArchitect, State: supervise.StateIdle, Generation: 1},
	} {
		if _, err := store.Pool().Exec(ctx, `insert into claims (token, project, tree, issue, role, generation, session, session_file,
			state, launch_failures, prompt_failures, prompt_retires, uncertain_streak)
			values ($1, $2, $3, $4, $5, $6, '', '', $7, 0, 0, 0, 0)`,
			string(c.Token), c.Project, c.Tree, c.Issue, string(c.Role), int64(c.Generation), string(c.State)); err != nil {
			t.Fatal(err)
		}
	}
	if _, err := store.Migrate(ctx); err != nil {
		t.Fatalf("migrate: %v", err)
	}
	for _, want := range []struct{ tree, authority string }{{"LEGION-208", "workflow"}, {"LEGION-300", "operator"}} {
		var epoch int64
		var authority string
		var started bool
		if err := store.Pool().QueryRow(ctx, `select epoch, authority, cleanup_started from tree_lifecycles
			where project = 'legion' and tree = $1`, want.tree).Scan(&epoch, &authority, &started); err != nil {
			t.Fatalf("lifecycle of %s: %v", want.tree, err)
		}
		if epoch != 1 || authority != want.authority || started {
			t.Fatalf("lifecycle of %s = epoch %d, %s, cleanup started %t; want open epoch 1, %s", want.tree, epoch, authority, started, want.authority)
		}
	}
	for _, token := range []string{"legion-legion-legion-209-implementer", "legion-legion-legion-300-architect"} {
		c := supervise.Claim{Token: claim.Token(token)}
		loaded, err := store.Claims(ctx)
		if err != nil {
			t.Fatal(err)
		}
		for _, l := range loaded {
			if l.Token == c.Token {
				c = l
			}
		}
		if c.TreeEpoch != 1 {
			t.Fatalf("claim %s after the migration is bound to epoch %d, want 1", token, c.TreeEpoch)
		}
		if err := store.CheckLaunch(ctx, c); err != nil {
			t.Fatalf("relaunch check of %s after the migration: %v", token, err)
		}
	}
}
