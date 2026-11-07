package store

import (
	"context"
	"strings"
	"testing"
)

// readmittedApprovalStepVersion is the version of the migration this file tests, so the test can
// migrate through everything before it whatever lands beside it.
const readmittedApprovalStepVersion = 20261106150254

// 20261106150254 names the step a kept approval is of. A pair stored before this migration was
// kept from the gate of a release that held the issue's primary document and nothing else, and
// this release resolves `gates.design: root-issues` to the one step spec, so the column's default
// reads every stored pair as that step's. The check admits the three step names and nothing else,
// and an insert that names no step still lands, which is what an earlier release's write does.
func TestTheStoredKeptApprovalBecomesTheSpecStepsWith20261106150254(t *testing.T) {
	ctx := context.Background()
	store := emptyStore(t)
	migrateThrough(t, store, readmittedApprovalStepVersion-1)
	exec := func(statement string) {
		t.Helper()
		if _, err := store.Pool().Exec(ctx, statement); err != nil {
			t.Fatalf("seed %q: %v", statement, err)
		}
	}
	exec(`insert into issues (key, tree, project, title, phase, generation, status, rank, last_dispatch_seq)
		values ('LEGION-208', 'LEGION-208', 'LEGION', 'a re-admitted root', 'admitted', 2, 'in_progress', 'A', 1)`)
	exec(`insert into readmitted_approvals (issue, approved_version, latest_version) values ('LEGION-208', 3, 4)`)

	if _, err := store.Migrate(ctx); err != nil {
		t.Fatalf("migrate through the kept approval's step: %v", err)
	}

	var step string
	var approved, latest int
	if err := store.Pool().QueryRow(ctx, `select step, approved_version, latest_version from readmitted_approvals
		where issue = 'LEGION-208'`).Scan(&step, &approved, &latest); err != nil {
		t.Fatalf("read the stored pair back: %v", err)
	}
	if step != "spec" || approved != 3 || latest != 4 {
		t.Fatalf("the stored pair = %s, %d, %d; want the spec step's pair at 3 below 4", step, approved, latest)
	}

	// An earlier release's write names no step and still lands, on the column's default.
	exec(`insert into readmitted_approvals (issue, approved_version, latest_version) values ('LEGION-208', 4, 5)
		on conflict (issue) do update set approved_version = excluded.approved_version, latest_version = excluded.latest_version`)
	if err := store.Pool().QueryRow(ctx, `select step from readmitted_approvals where issue = 'LEGION-208'`).Scan(&step); err != nil ||
		step != "spec" {
		t.Fatalf("the pair after an earlier release's write = %q, %v; want the spec step", step, err)
	}

	_, err := store.Pool().Exec(ctx, `update readmitted_approvals set step = 'design' where issue = 'LEGION-208'`)
	if err == nil || !strings.Contains(err.Error(), "check constraint") {
		t.Fatalf("a pair of a step no deployment gates = %v, want it refused by the check constraint", err)
	}
	t.Logf("a step the record does not know is refused: %v", err)
}
