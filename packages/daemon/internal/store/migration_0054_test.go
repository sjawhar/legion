package store

import (
	"context"
	"strings"
	"testing"
)

// 0054 records where Dispatch put the approval a gate holds and the changes request its architect
// was told of. A gate the older daemon left migrates with neither recorded; afterwards each column
// refuses a version or a sequence of zero, and no check ties a mark to its version, so the older
// daemon, which writes approved_version alone, can still clear an approval on a gate that has a
// mark: the daemon keeps that shape itself (record.Postgres.PutGate).
func TestTheGatesReviewMarksArriveWith0054(t *testing.T) {
	store := emptyStore(t)
	ctx := context.Background()
	migrateThrough(t, store, 53)
	if _, err := store.Pool().Exec(ctx, `insert into issues
		(key, tree, project, title, phase, generation, status, rank, last_dispatch_seq)
		values ('LEGION-208', 'LEGION-208', 'LEGION', 'admitted root', 'admitted', 1, 'in_progress', 'A', 1)`); err != nil {
		t.Fatal(err)
	}
	if _, err := store.Pool().Exec(ctx, `insert into design_gates (issue, artifact_id, latest_version, approved_version)
		values ('LEGION-208', '7d1e3a5c-9b2f-4c6e-8a40-3f5d7b9e1c26', 4, 4)`); err != nil {
		t.Fatal(err)
	}
	if _, err := store.Migrate(ctx); err != nil {
		t.Fatalf("migrate: %v", err)
	}
	var approvedSeq, toldSeq *int64
	var approvedAt, toldAt *string
	var toldVersion *int
	var reason string
	if err := store.Pool().QueryRow(ctx, `select approved_seq, approved_at::text, told_changes_version, told_changes_reason, told_changes_seq, told_changes_at::text
		from design_gates where issue = 'LEGION-208'`).Scan(&approvedSeq, &approvedAt, &toldVersion, &reason, &toldSeq, &toldAt); err != nil {
		t.Fatalf("read the migrated gate: %v", err)
	}
	if approvedSeq != nil || approvedAt != nil || toldVersion != nil || reason != "" || toldSeq != nil || toldAt != nil {
		t.Fatalf("gate after 0054 = approved %v/%v told %v %q %v/%v, want the approval it held placed nowhere and nothing told",
			approvedSeq, approvedAt, toldVersion, reason, toldSeq, toldAt)
	}
	for _, accepted := range []string{
		`update design_gates set approved_seq = 12, approved_at = now() where issue = 'LEGION-208'`,
		// The older daemon's write of a changes request: approved_version alone.
		`update design_gates set approved_version = null where issue = 'LEGION-208'`,
		`update design_gates set told_changes_version = 4, told_changes_reason = 'name the rollback path', told_changes_seq = 13, told_changes_at = now() where issue = 'LEGION-208'`,
		// The older daemon's write of an approval, which knows nothing of the told record.
		`update design_gates set approved_version = 4 where issue = 'LEGION-208'`,
	} {
		if _, err := store.Pool().Exec(ctx, accepted); err != nil {
			t.Fatalf("%s: %v, want it accepted", accepted, err)
		}
	}
	for _, refused := range []struct{ why, statement string }{
		{"an approval sequence of zero", `update design_gates set approved_seq = 0 where issue = 'LEGION-208'`},
		{"a told version of zero", `update design_gates set told_changes_version = 0 where issue = 'LEGION-208'`},
		{"a told sequence of zero", `update design_gates set told_changes_seq = 0 where issue = 'LEGION-208'`},
	} {
		_, err := store.Pool().Exec(ctx, refused.statement)
		if err == nil || !strings.Contains(err.Error(), "check constraint") {
			t.Fatalf("%s = %v, want a check constraint to refuse it", refused.why, err)
		}
	}
}
