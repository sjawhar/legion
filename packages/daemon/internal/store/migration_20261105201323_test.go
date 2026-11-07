package store

import (
	"context"
	"strings"
	"testing"
)

// designGateStepVersion is the version of the migration this file tests, so the test can migrate
// through everything before it whatever lands beside it.
const designGateStepVersion = 20261105201323

// 20261105201323 keys the design gate by the step it holds. A deployment's design gate becomes a
// list of steps, each naming one document of the root issue, and the record keeps one row for
// each step rather than one for each issue.
//
// A row stored before this migration is the gate of a release that held the issue's primary
// document and nothing else, and this release resolves `gates.design: root-issues` to the one
// step spec, so the column's default is what reads every stored row as that step's row. Nothing
// is rewritten. The key then takes the step as well, so one issue holds a row for each step and
// no two rows of one step; and issues.ready_pending_step arrives beside ready_pending_version,
// backfilled to spec for the refusals the release before stored, with a shape check that refuses
// either half of the pair alone.
//
// The negative control is the rollback hazard the migration's header states: a daemon of an
// earlier release names `on conflict (issue)`, which no unique index answers once the key is
// (issue, step), so its writes are refused.
func TestTheStoredDesignGateBecomesTheSpecStepsRowWith20261105201323(t *testing.T) {
	ctx := context.Background()
	store := emptyStore(t)
	migrateThrough(t, store, designGateStepVersion-1)
	exec := func(statement string) {
		t.Helper()
		if _, err := store.Pool().Exec(ctx, statement); err != nil {
			t.Fatalf("%s: %v", statement, err)
		}
	}
	// Two roots as the release before this one left them: one with a design gate and a READY its
	// gate refused at version 4, one with neither.
	exec(`insert into issues (key, tree, project, title, phase, generation, status, rank, last_dispatch_seq, ready_pending_version)
		values ('LEGION-208', 'LEGION-208', 'LEGION', 'a gated root', 'merging', 1, 'retro', 'A', 1, 4)`)
	exec(`insert into issues (key, tree, project, title, phase, generation, status, rank, last_dispatch_seq)
		values ('LEGION-300', 'LEGION-300', 'LEGION', 'a root with no gate', 'admitted', 1, 'in_progress', 'B', 1)`)
	exec(`insert into design_gates (issue, artifact_id, latest_version, approved_version)
		values ('LEGION-208', '7d1e3a5c-9b2f-4c6e-8a40-3f5d7b9e1c26', 4, 3)`)

	if _, err := store.Migrate(ctx); err != nil {
		t.Fatalf("migrate through the design gate's step: %v", err)
	}

	var step, artifact string
	var latest int
	var approved *int
	if err := store.Pool().QueryRow(ctx, `select step, artifact_id, latest_version, approved_version from design_gates
		where issue = 'LEGION-208'`).Scan(&step, &artifact, &latest, &approved); err != nil {
		t.Fatalf("read the stored gate back: %v", err)
	}
	if step != "spec" || artifact != "7d1e3a5c-9b2f-4c6e-8a40-3f5d7b9e1c26" || latest != 4 || approved == nil || *approved != 3 {
		t.Fatalf("the stored gate = step %q on %s at version %d approved %v; want the spec step's row, otherwise as it was stored",
			step, artifact, latest, approved)
	}

	var pendingStep *string
	var pendingVersion *int
	if err := store.Pool().QueryRow(ctx, `select ready_pending_step, ready_pending_version from issues where key = 'LEGION-208'`).
		Scan(&pendingStep, &pendingVersion); err != nil {
		t.Fatalf("read the pending READY back: %v", err)
	}
	if pendingStep == nil || *pendingStep != "spec" || pendingVersion == nil || *pendingVersion != 4 {
		t.Fatalf("the refused READY waits on %v version %v, want the spec step at version 4: that is the step it was waiting on",
			pendingStep, pendingVersion)
	}
	if err := store.Pool().QueryRow(ctx, `select ready_pending_step from issues where key = 'LEGION-300'`).Scan(&pendingStep); err != nil {
		t.Fatalf("read the root with no refused READY back: %v", err)
	}
	if pendingStep != nil {
		t.Fatalf("a root with no refused READY waits on step %q, want none", *pendingStep)
	}

	// The key is the issue and the step: another step of the same issue is a row of its own.
	exec(`insert into design_gates (issue, step, artifact_id, latest_version)
		values ('LEGION-208', 'criteria', 'artifact-criteria', 2)`)
	var rows int
	if err := store.Pool().QueryRow(ctx, `select count(*) from design_gates where issue = 'LEGION-208'`).Scan(&rows); err != nil || rows != 2 {
		t.Fatalf("the gates of LEGION-208 = %d rows, %v; want one for each step", rows, err)
	}

	for _, refused := range []struct{ why, statement string }{
		{"a second row of one step", `insert into design_gates (issue, step, artifact_id, latest_version)
			values ('LEGION-208', 'criteria', 'artifact-criteria', 3)`},
		{"a step the gate never names", `insert into design_gates (issue, step, artifact_id, latest_version)
			values ('LEGION-300', 'verification', 'artifact-300', 1)`},
		{"a pending step with no version", `update issues set ready_pending_step = 'spec' where key = 'LEGION-300'`},
		{"a pending version with no step", `update issues set ready_pending_version = 2 where key = 'LEGION-300'`},
		{"a pending step the gate never names", `update issues set ready_pending_step = 'verification', ready_pending_version = 2
			where key = 'LEGION-300'`},
	} {
		_, err := store.Pool().Exec(ctx, refused.statement)
		if err == nil || !strings.Contains(err.Error(), "constraint") {
			t.Fatalf("%s = %v, want a constraint to refuse it", refused.why, err)
		}
	}

	// The rollback hazard: an earlier release's write names a key no index answers now.
	_, err := store.Pool().Exec(ctx, `insert into design_gates (issue, artifact_id, latest_version)
		values ('LEGION-300', 'artifact-300', 1) on conflict (issue) do update set latest_version = excluded.latest_version`)
	if err == nil || !strings.Contains(err.Error(), "ON CONFLICT") {
		t.Fatalf("an earlier release's gate write = %v, want it refused for naming the issue alone as its key", err)
	}
	t.Logf("after the migration an earlier release's gate write is refused: %v", err)
}
