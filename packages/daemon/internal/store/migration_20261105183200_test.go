package store

import (
	"context"
	"testing"
	"time"
)

// oneOpenResumeVersion is the version of the migration this file tests, so the test can migrate
// through everything before it whatever lands beside it.
const oneOpenResumeVersion = 20261105183200

// 20261105183200 makes one open resume per role the whole of the wake's dedupe: it closes every
// open resume of a role but the newest, creates the partial unique index, and drops the marks
// table the two overlapping layers needed. A database upgrading holds both — marks of the wakes
// it took, and, where the two layers let one through, more than one resume of a role with no
// first turn — so the upgrade must run on that shape rather than on a clean one.
//
// The newest open resume is kept because it is the wake the daemon is acting on; the older ones
// are closed at the newer's own time, which is when that pod came back for the role. A role with
// one open resume keeps it untouched, and a resume that already had its first turn is not moved.
func TestTheOpenResumesAreClosedToOnePerRoleBy20261105183200(t *testing.T) {
	ctx := context.Background()
	store := emptyStore(t)
	migrateThrough(t, store, oneOpenResumeVersion-1)

	at := time.Date(2026, 11, 5, 12, 0, 0, 0, time.UTC)
	stamped := at.Add(-time.Hour)
	for _, row := range []struct {
		issue, role string
		at          time.Time
		firstTurn   *time.Time
	}{
		// Two open resumes of one role, which the layers before this migration allowed: the
		// newer stays open and the older is closed at the newer's time.
		{issue: "LEGION-208", role: "implementer", at: at.Add(-2 * time.Minute)},
		{issue: "LEGION-208", role: "implementer", at: at},
		// A closed resume of the same role is left as it is.
		{issue: "LEGION-208", role: "implementer", at: at.Add(-time.Hour), firstTurn: &stamped},
		// Another role of the same pod, and another issue, each keep their one open resume.
		{issue: "LEGION-208", role: "architect", at: at},
		{issue: "LEGION-209", role: "implementer", at: at},
	} {
		if _, err := store.Pool().Exec(ctx, `insert into pod_resumes (release_id, project, issue, role, kind, at, first_turn_at)
			values (0, 'legion', $1, $2, 'delivery', $3, $4)`, row.issue, row.role, row.at, row.firstTurn); err != nil {
			t.Fatalf("seed the resume of %s's %s: %v", row.issue, row.role, err)
		}
	}
	if _, err := store.Pool().Exec(ctx, `insert into pod_wake_marks (project, issue, kind, key, at)
		values ('legion', 'LEGION-208', 'checks', 'head1', $1)`, at); err != nil {
		t.Fatalf("seed a wake mark: %v", err)
	}

	if _, err := store.Migrate(ctx); err != nil {
		t.Fatalf("migrate through %d: %v", oneOpenResumeVersion, err)
	}

	for _, want := range []struct {
		issue, role string
		open        int
	}{
		{issue: "LEGION-208", role: "implementer", open: 1},
		{issue: "LEGION-208", role: "architect", open: 1},
		{issue: "LEGION-209", role: "implementer", open: 1},
	} {
		var open int
		if err := store.Pool().QueryRow(ctx, `select count(*) from pod_resumes
			where issue = $1 and role = $2 and first_turn_at is null`, want.issue, want.role).Scan(&open); err != nil {
			t.Fatalf("count the open resumes of %s's %s: %v", want.issue, want.role, err)
		}
		if open != want.open {
			t.Errorf("%s's %s has %d open resumes, want %d", want.issue, want.role, open, want.open)
		}
	}
	// The one left open is the newest, and the older one is closed at its time rather than at
	// the migration's, so the records say when that pod came back.
	var openAt, closedAt time.Time
	if err := store.Pool().QueryRow(ctx, `select max(at) filter (where first_turn_at is null),
		max(first_turn_at) filter (where at = $1) from pod_resumes where issue = 'LEGION-208' and role = 'implementer'`,
		at.Add(-2*time.Minute)).Scan(&openAt, &closedAt); err != nil {
		t.Fatalf("read the implementer's resumes: %v", err)
	}
	if !openAt.Equal(at) {
		t.Errorf("the open resume is the one at %s, want the newest, %s", openAt, at)
	}
	if !closedAt.Equal(at) {
		t.Errorf("the older resume was closed at %s, want the newer's own time, %s", closedAt, at)
	}

	// The index is what the take relies on, and the marks table has no reader left.
	if _, err := store.Pool().Exec(ctx, `insert into pod_resumes (release_id, project, issue, role, kind, at)
		values (0, 'legion', 'LEGION-208', 'implementer', 'checks', $1)`, at.Add(time.Minute)); err == nil {
		t.Error("a second open resume of one role was accepted, want the unique index to refuse it")
	}
	var marks bool
	if err := store.Pool().QueryRow(ctx, `select exists (select 1 from information_schema.tables
		where table_name = 'pod_wake_marks')`).Scan(&marks); err != nil {
		t.Fatalf("read whether the marks table is there: %v", err)
	}
	if marks {
		t.Error("pod_wake_marks is still there, want it dropped with the layer that read it")
	}
}
