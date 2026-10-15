package store

import (
	"context"
	"slices"
	"testing"
)

// deliveryProductionJobNamesVersion is the migration that turns delivery_settings' production job
// into a list and adds the apply step (LEGION-761).
const deliveryProductionJobNamesVersion = 109

// TestMigrationTurnsTheProductionJobIntoAList runs the shipped files: the stored name becomes a
// one-name list with no apply step, an older binary's write of production_job_name alone (a task of
// the earlier release during the roll) becomes the list, this release's write of the list alone keeps
// production_job_name at its first name for that older binary's reads, and an empty list is refused.
func TestMigrationTurnsTheProductionJobIntoAList(t *testing.T) {
	ctx := context.Background()
	store := openEmptyTestStore(t)
	if err := store.migrate(ctx, migrationsThrough(t, deliveryProductionJobNamesVersion-1)); err != nil {
		t.Fatalf("migrate through %d: %v", deliveryProductionJobNamesVersion-1, err)
	}
	const olderInsert = `insert into delivery_settings (deploy_repo, deploy_workflow_path, production_job_name, pr_checks_workflow_path, updated_by)
		values ('acme/widgets', '.github/workflows/deploy.yml', $1, '.github/workflows/pr-checks.yml', '{"kind":"system","id":"test"}')`
	if _, err := store.Pool.Exec(ctx, olderInsert, "widgets-release / widgets-release"); err != nil {
		t.Fatalf("seed settings before the migration: %v", err)
	}
	if err := store.migrate(ctx, migrationsThrough(t, deliveryProductionJobNamesVersion)); err != nil {
		t.Fatalf("migrate to %d: %v", deliveryProductionJobNamesVersion, err)
	}
	read := func() ([]string, string, *string) {
		t.Helper()
		var names []string
		var name string
		var step *string
		if err := store.Pool.QueryRow(ctx, `select production_job_names, production_job_name, production_apply_step_name from delivery_settings where singleton`).Scan(&names, &name, &step); err != nil {
			t.Fatalf("read settings: %v", err)
		}
		return names, name, step
	}
	if names, _, step := read(); !slices.Equal(names, []string{"widgets-release / widgets-release"}) || step != nil {
		t.Fatalf("after the migration: names %v, step %v; want the stored name as a one-name list and no step", names, step)
	}
	if _, err := store.Pool.Exec(ctx, `update delivery_settings set production_job_name = 'release / release' where singleton`); err != nil {
		t.Fatalf("older binary's update: %v", err)
	}
	if names, _, _ := read(); !slices.Equal(names, []string{"release / release"}) {
		t.Fatalf("after an older binary's write: names %v, want [release / release]", names)
	}
	if _, err := store.Pool.Exec(ctx, `update delivery_settings set production_job_names = array['a / a', 'b / b'] where singleton`); err != nil {
		t.Fatalf("this release's update: %v", err)
	}
	if names, name, _ := read(); !slices.Equal(names, []string{"a / a", "b / b"}) || name != "a / a" {
		t.Fatalf("after this release's write: names %v, name %q; want both names and the first as production_job_name", names, name)
	}
	if _, err := store.Pool.Exec(ctx, `delete from delivery_settings`); err != nil {
		t.Fatalf("clear settings: %v", err)
	}
	if _, err := store.Pool.Exec(ctx, olderInsert, "x / x"); err != nil {
		t.Fatalf("older binary's insert: %v", err)
	}
	if names, _, _ := read(); !slices.Equal(names, []string{"x / x"}) {
		t.Fatalf("after an older binary's insert: names %v, want [x / x]", names)
	}
	if _, err := store.Pool.Exec(ctx, `update delivery_settings set production_job_names = '{}' where singleton`); err == nil {
		t.Fatal("an empty production_job_names was stored, want the check to refuse it")
	}
}
