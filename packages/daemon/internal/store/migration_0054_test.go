package store

import (
	"context"
	"testing"
)

// An issue pod runs every role in one container, `roles`, so a claim stored by a daemon of the
// layout before it, whose sandbox locator names a container per role, is rewritten to `roles` by
// 0054 in both tables that keep a locator, and then loads (Validate refuses the old value). The
// controller's locator, a tmux locator and a claim with no locator are left as they are.
func TestIssuePodLocatorsStoredBefore0054NameTheRolesContainer(t *testing.T) {
	store := emptyStore(t)
	ctx := context.Background()
	migrateThrough(t, store, 53)
	issue := `{"runtime":"sandbox","claim":"legion-LEGION-209-implementer","incarnation":"pod-uid-3/3","sandbox":{"namespace":"legion","name":"legion-legion-209","podUid":"pod-uid-3","container":"implementer","generation":3}}`
	controller := `{"runtime":"sandbox","claim":"legion-legion-controller","incarnation":"pod-uid-9/1","sandbox":{"namespace":"legion","name":"legion-legion-controller","podUid":"pod-uid-9","container":"controller","generation":1}}`
	tmux := `{"runtime":"tmux","claim":"legion-LEGION-210-tester","incarnation":"4242:1","tmux":{"window":"@1","pane":"%1"}}`
	if _, err := store.Pool().Exec(ctx, `insert into claims (token, project, tree, issue, role, generation, session, session_file, locator, state,
		launch_failures, prompt_failures, prompt_retires, uncertain_streak)
		values ('legion-LEGION-209-implementer', 'legion', 'LEGION-208', 'LEGION-209', 'implementer', 3, 'ses_i', '', $1, 'ready', 0, 0, 0, 0),
		('legion-legion-controller', 'legion', '', '', 'controller', 1, 'ses_c', '', $2, 'ready', 0, 0, 0, 0),
		('legion-LEGION-210-tester', 'legion', 'LEGION-208', 'LEGION-210', 'tester', 1, 'ses_t', '', $3, 'ready', 0, 0, 0, 0),
		('legion-LEGION-211-merger', 'legion', 'LEGION-208', 'LEGION-211', 'merger', 1, 'ses_m', '', null, 'queued', 0, 0, 0, 0)`,
		issue, controller, tmux); err != nil {
		t.Fatal(err)
	}
	if _, err := store.Pool().Exec(ctx, `insert into claim_capabilities (claim_token, generation, incarnation, locator, measured_at, reported_at, report)
		values ('legion-LEGION-209-implementer', 3, 'pod-uid-3/3', $1, now(), now(), '{"elapsedMs": 1, "rows": []}'),
		('legion-legion-controller', 1, 'pod-uid-9/1', $2, now(), now(), '{"elapsedMs": 1, "rows": []}')`, issue, controller); err != nil {
		t.Fatal(err)
	}
	if _, err := store.Migrate(ctx); err != nil {
		t.Fatalf("migrate: %v", err)
	}
	for _, table := range []string{"claims", "claim_capabilities"} {
		column := map[string]string{"claims": "token", "claim_capabilities": "claim_token"}[table]
		for token, want := range map[string]string{"legion-LEGION-209-implementer": "roles", "legion-legion-controller": "controller"} {
			var container string
			if err := store.Pool().QueryRow(ctx, `select locator->'sandbox'->>'container' from `+table+` where `+column+` = $1`, token).Scan(&container); err != nil {
				t.Fatalf("read %s's locator in %s: %v", token, table, err)
			}
			if container != want {
				t.Errorf("%s's locator in %s names container %q, want %q", token, table, container, want)
			}
		}
	}
	var unchanged bool
	if err := store.Pool().QueryRow(ctx, `select locator = $1::jsonb from claims where token = 'legion-LEGION-210-tester'`, tmux).Scan(&unchanged); err != nil || !unchanged {
		t.Errorf("the tmux locator changed (%v): unchanged %t", err, unchanged)
	}
	claims, err := store.Claims(ctx)
	if err != nil {
		t.Fatalf("Claims after 0054: %v", err)
	}
	if len(claims) != 4 {
		t.Fatalf("Claims after 0054 = %d claims, want 4", len(claims))
	}
	if _, err := store.CapabilityReports(ctx); err != nil {
		t.Fatalf("CapabilityReports after 0054: %v", err)
	}
}
