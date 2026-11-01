package store

import (
	"context"
	"errors"
	"slices"
	"strings"
	"testing"

	"github.com/jackc/pgx/v5/pgconn"

	"github.com/sjawhar/envoy/internal/pgmigrate"
)

// deliveryDropProductionJobNameVersion is the migration that drops delivery_settings'
// production_job_name with the trigger that kept it at the list's first name (LEGION-761).
const deliveryDropProductionJobNameVersion = 111

// TestMigrationDropsTheProductionJobName runs the shipped files: a row written before the migration
// keeps its list and apply step, the column, the trigger and its function are gone, a write of the
// list and the apply step reads back as written, the list's check still refuses an empty list, and a
// write naming the dropped column (a task older than the release that added 0109) is refused.
func TestMigrationDropsTheProductionJobName(t *testing.T) {
	ctx := context.Background()
	store := openEmptyTestStore(t)
	if err := store.migrate(ctx, migrationsThrough(t, deliveryDropProductionJobNameVersion-1)); err != nil {
		t.Fatalf("migrate through %d: %v", deliveryDropProductionJobNameVersion-1, err)
	}
	if _, err := store.Pool.Exec(ctx, `
		insert into delivery_settings (deploy_repo, deploy_workflow_path, production_job_names, production_apply_step_name, pr_checks_workflow_path, updated_by)
		values ('acme/widgets', '.github/workflows/deploy.yml', array['widgets-release / widgets-release', 'release / release'], 'Run ./.github/actions/apply', '.github/workflows/pr-checks.yml', '{"kind":"system","id":"test"}')
	`); err != nil {
		t.Fatalf("seed settings before the migration: %v", err)
	}
	if err := store.migrate(ctx, migrationsThrough(t, deliveryDropProductionJobNameVersion)); err != nil {
		t.Fatalf("migrate to %d: %v", deliveryDropProductionJobNameVersion, err)
	}
	read := func() ([]string, *string) {
		t.Helper()
		var names []string
		var step *string
		if err := store.Pool.QueryRow(ctx, `select production_job_names, production_apply_step_name from delivery_settings where singleton`).Scan(&names, &step); err != nil {
			t.Fatalf("read settings: %v", err)
		}
		return names, step
	}
	if names, step := read(); !slices.Equal(names, []string{"widgets-release / widgets-release", "release / release"}) || step == nil || *step != "Run ./.github/actions/apply" {
		t.Fatalf("after the migration: names %v, step %v; want the row as written before it", names, step)
	}
	var column, trigger, function bool
	if err := store.Pool.QueryRow(ctx, `
		select
			exists (select 1 from information_schema.columns where table_schema = current_schema() and table_name = 'delivery_settings' and column_name = 'production_job_name'),
			exists (select 1 from pg_trigger where tgrelid = 'delivery_settings'::regclass and tgname = 'delivery_settings_production_job_names'),
			exists (select 1 from pg_proc where pronamespace = current_schema()::regnamespace and proname = 'delivery_settings_production_job_names')
	`).Scan(&column, &trigger, &function); err != nil {
		t.Fatalf("read the catalog: %v", err)
	}
	if column || trigger || function {
		t.Fatalf("after the migration: column %t, trigger %t, function %t; want all three gone", column, trigger, function)
	}
	if _, err := store.Pool.Exec(ctx, `update delivery_settings set production_job_names = array['production / production-apply'], production_apply_step_name = 'Run ./.github/actions/iac-apply-stack' where singleton`); err != nil {
		t.Fatalf("update the list and the apply step: %v", err)
	}
	if names, step := read(); !slices.Equal(names, []string{"production / production-apply"}) || step == nil || *step != "Run ./.github/actions/iac-apply-stack" {
		t.Fatalf("after an update: names %v, step %v; want what it wrote", names, step)
	}
	if _, err := store.Pool.Exec(ctx, `delete from delivery_settings`); err != nil {
		t.Fatalf("clear settings: %v", err)
	}
	if _, err := store.Pool.Exec(ctx, `
		insert into delivery_settings (deploy_repo, deploy_workflow_path, production_job_names, production_apply_step_name, pr_checks_workflow_path, updated_by)
		values ('acme/widgets', '.github/workflows/deploy.yml', array['a / a', 'b / b'], null, '.github/workflows/pr-checks.yml', '{"kind":"system","id":"test"}')
	`); err != nil {
		t.Fatalf("insert the list without an apply step: %v", err)
	}
	if names, step := read(); !slices.Equal(names, []string{"a / a", "b / b"}) || step != nil {
		t.Fatalf("after an insert: names %v, step %v; want both names and no step", names, step)
	}
	if _, err := store.Pool.Exec(ctx, `update delivery_settings set production_job_names = '{}' where singleton`); err == nil {
		t.Fatal("an empty production_job_names was stored, want the check to refuse it")
	}
	_, err := store.Pool.Exec(ctx, `update delivery_settings set production_job_name = 'release / release' where singleton`)
	var pgErr *pgconn.PgError
	if !errors.As(err, &pgErr) || pgErr.Code != "42703" {
		t.Fatalf("a write naming production_job_name = %v, want undefined_column (42703)", err)
	}
}

// TestCensusRefusesTheDropOverARowWhoseColumnDriftedFromTheList takes 0111's census with the
// shipped file over a database at 110, as a deploy carrying 0111 does: a row whose
// production_job_name 0109's trigger wrote as the list's first name answers 0 and passes, and one
// whose column differs from it, the value the drop would lose, answers 1 and refuses the deploy.
// The row lists two names, so a census reading another element than the first counts the kept row.
func TestCensusRefusesTheDropOverARowWhoseColumnDriftedFromTheList(t *testing.T) {
	ctx := context.Background()
	store := openEmptyTestStore(t)
	if err := store.migrate(ctx, migrationsThrough(t, deliveryDropProductionJobNameVersion-1)); err != nil {
		t.Fatalf("migrate through %d: %v", deliveryDropProductionJobNameVersion-1, err)
	}
	take := func() (*pgmigrate.Report, int64) {
		t.Helper()
		report, err := census(ctx, store.Pool.Config().ConnString(), migrationsThrough(t, deliveryDropProductionJobNameVersion), pgmigrate.CensusOptions{})
		if err != nil {
			t.Fatalf("census: %v", err)
		}
		if len(report.Pending) != 1 || report.Pending[0].Migration.Version != deliveryDropProductionJobNameVersion || report.Pending[0].Count == nil {
			t.Fatalf("want one pending migration, %d, whose census answered:\n%s", deliveryDropProductionJobNameVersion, reportText(report))
		}
		return report, *report.Pending[0].Count
	}
	if _, err := store.Pool.Exec(ctx, `
		insert into delivery_settings (deploy_repo, deploy_workflow_path, production_job_names, pr_checks_workflow_path, updated_by)
		values ('acme/widgets', '.github/workflows/deploy.yml', array['widgets-release / widgets-release', 'release / release'], '.github/workflows/pr-checks.yml', '{"kind":"system","id":"test"}')
	`); err != nil {
		t.Fatalf("seed settings: %v", err)
	}
	if report, count := take(); count != 0 || report.Refused() {
		t.Fatalf("a row whose column is the list's first name: census counts %d, refusals %v; want 0 and none:\n%s", count, refusals(report), reportText(report))
	}
	// 0109's trigger sets production_job_name to production_job_names[1] on every insert and update,
	// so no write it sees can leave the two apart: only a write made while the trigger is disabled
	// can, which is the one way production could hold the row 0111's census exists to catch. The
	// trigger is enabled again before the census, so it reads schema 110 as production has it.
	for _, statement := range []string{
		`alter table delivery_settings disable trigger delivery_settings_production_job_names`,
		`update delivery_settings set production_job_name = 'release / release' where singleton`,
		`alter table delivery_settings enable trigger delivery_settings_production_job_names`,
	} {
		if _, err := store.Pool.Exec(ctx, statement); err != nil {
			t.Fatalf("%s: %v", statement, err)
		}
	}
	report, count := take()
	const want = "0111_delivery_drop_production_job_name.up.sql: its census counts 1 (0111_delivery_drop_production_job_name.census.sql)"
	if got := refusals(report); count != 1 || len(got) != 1 || !strings.HasPrefix(got[0], want) {
		t.Fatalf("a row whose column drifted from the list: census counts %d, refusals %v; want 1 and one refusal starting %q:\n%s", count, got, want, reportText(report))
	}
	if out := reportText(report); !strings.Contains(out, "census: REFUSED "+want) || !strings.HasSuffix(out, "census: REFUSED (1 reason)\n") {
		t.Errorf("report:\n%s", out)
	}
}
