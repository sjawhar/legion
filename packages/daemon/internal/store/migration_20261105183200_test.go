package store

import (
	"context"
	"testing"
	"time"

	"github.com/sjawhar/legion/daemon/internal/claim"
	"github.com/sjawhar/legion/daemon/internal/pods"
)

// oneOpenResumeVersion is the version of the migration this file tests, so the test can migrate
// through everything before it whatever lands beside it.
const oneOpenResumeVersion = 20261105183200

// 20261105183200 makes one open resume per role the whole of the wake's dedupe: it adds the
// closed_at column, closes every open resume the new index would refuse, creates that partial
// unique index, and drops the marks table the two overlapping layers needed. A database upgrading
// holds the shapes the release before it allowed, so the upgrade must run on those rather than on
// a clean database, and each closed row must leave its role able to take a wake afterwards —
// which is the whole point of closing it, since one open resume refuses every later wake of its
// role and only a release closes one.
//
// Two shapes are closed. Where the two layers let more than one open resume of a role through,
// the newest is kept, because it is the wake the daemon is acting on, and the older ones are
// closed at the newer's own time, which is when that pod came back for the role. And a single
// open resume of a pod that was released again afterwards is closed at that release's time: no
// release closed a resume before this migration, so a wake whose role's first turn the daemon
// never saw left one behind, and that row would refuse the role's every wake for as long as the
// issue lives.
//
// A role whose one open resume is of the newest release keeps it, and a resume that already had
// its first turn is not moved: closing is its own column, so nothing here writes a first turn
// that never happened.
func TestTheOpenResumesTheIndexRefusesAreClosedBy20261105183200(t *testing.T) {
	ctx := context.Background()
	store := emptyStore(t)
	migrateThrough(t, store, oneOpenResumeVersion-1)

	at := time.Date(2026, 11, 5, 12, 0, 0, 0, time.UTC)
	stamped := at.Add(-time.Hour)
	releasedAt := at.Add(30 * time.Minute)
	// LEGION-209's pod was released after its one open resume, and LEGION-208's newest release is
	// the one its open resumes are of, so only the first is stale.
	var staleRelease, currentRelease int64
	if err := store.Pool().QueryRow(ctx, `insert into pod_releases (project, issue, generation, at)
		values ('legion', 'LEGION-209', 1, $1) returning id`, releasedAt).Scan(&staleRelease); err != nil {
		t.Fatalf("seed LEGION-209's release: %v", err)
	}
	if err := store.Pool().QueryRow(ctx, `insert into pod_releases (project, issue, generation, at)
		values ('legion', 'LEGION-208', 1, $1) returning id`, at.Add(-2*time.Hour)).Scan(&currentRelease); err != nil {
		t.Fatalf("seed LEGION-208's release: %v", err)
	}

	for _, row := range []struct {
		issue, role string
		release     int64
		at          time.Time
		firstTurn   *time.Time
	}{
		// Two open resumes of one role, which the layers before this migration allowed: the
		// newer stays open and the older is closed at the newer's time.
		{issue: "LEGION-208", role: "implementer", release: currentRelease, at: at.Add(-2 * time.Minute)},
		{issue: "LEGION-208", role: "implementer", release: currentRelease, at: at},
		// A resume with its first turn stamped is left as it is.
		{issue: "LEGION-208", role: "implementer", release: currentRelease, at: at.Add(-time.Hour), firstTurn: &stamped},
		// Another role of the same pod keeps its one open resume: its release is the newest.
		{issue: "LEGION-208", role: "architect", release: currentRelease, at: at},
		// The stale shape, and the one this round found: one open resume, no duplicate, and the
		// pod released again since.
		{issue: "LEGION-209", role: "implementer", release: staleRelease - 1, at: at},
	} {
		if _, err := store.Pool().Exec(ctx, `insert into pod_resumes (release_id, project, issue, role, kind, at, first_turn_at)
			values ($1, 'legion', $2, $3, 'delivery', $4, $5)`, row.release, row.issue, row.role, row.at, row.firstTurn); err != nil {
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
		{issue: "LEGION-209", role: "implementer", open: 0},
	} {
		var open int
		if err := store.Pool().QueryRow(ctx, `select count(*) from pod_resumes
			where issue = $1 and role = $2 and first_turn_at is null and closed_at is null`,
			want.issue, want.role).Scan(&open); err != nil {
			t.Fatalf("count the open resumes of %s's %s: %v", want.issue, want.role, err)
		}
		if open != want.open {
			t.Errorf("%s's %s has %d open resumes, want %d", want.issue, want.role, open, want.open)
		}
	}

	// Nothing closed here reports a first turn: that column is the turn the daemon observed, and
	// none of these rows had one.
	var invented int
	if err := store.Pool().QueryRow(ctx, `select count(*) from pod_resumes
		where closed_at is not null and first_turn_at is not null`).Scan(&invented); err != nil {
		t.Fatalf("count the closed resumes carrying a first turn: %v", err)
	}
	if invented != 0 {
		t.Errorf("%d closed resumes carry a first turn, want none: closing is closed_at's", invented)
	}
	var stamps int
	if err := store.Pool().QueryRow(ctx, `select count(*) from pod_resumes where first_turn_at = $1`, stamped).Scan(&stamps); err != nil {
		t.Fatalf("read the stamped resume: %v", err)
	}
	if stamps != 1 {
		t.Errorf("%d resumes carry the seeded first turn, want the one that was stamped", stamps)
	}

	// Each closed row says when its pod went: the duplicate at the newer resume's own time, the
	// stale one at the release that stopped its pod.
	var duplicateClosed, staleClosed time.Time
	if err := store.Pool().QueryRow(ctx, `select closed_at from pod_resumes
		where issue = 'LEGION-208' and role = 'implementer' and at = $1`, at.Add(-2*time.Minute)).Scan(&duplicateClosed); err != nil {
		t.Fatalf("read the duplicate resume: %v", err)
	}
	if !duplicateClosed.Equal(at) {
		t.Errorf("the older resume was closed at %s, want the newer's own time, %s", duplicateClosed, at)
	}
	if err := store.Pool().QueryRow(ctx, `select closed_at from pod_resumes
		where issue = 'LEGION-209' and role = 'implementer'`).Scan(&staleClosed); err != nil {
		t.Fatalf("read the stale resume: %v", err)
	}
	if !staleClosed.Equal(releasedAt) {
		t.Errorf("the stale resume was closed at %s, want the release that stopped its pod, %s", staleClosed, releasedAt)
	}

	// What closing is for: the role of the stale resume can be woken again, and the one whose
	// resume is still open cannot.
	taken, err := store.TakePodWake(ctx, "legion", "LEGION-209", claim.RoleImplementer, pods.WakeBackstop, at.Add(time.Hour))
	if err != nil {
		t.Fatalf("take LEGION-209's backstop wake: %v", err)
	}
	if !taken {
		t.Error("LEGION-209's implementer could not be woken after the migration: its stale resume still refuses every wake")
	}
	if taken, err := store.TakePodWake(ctx, "legion", "LEGION-208", claim.RoleImplementer, pods.WakeBackstop, at.Add(time.Hour)); err != nil {
		t.Fatalf("take LEGION-208's backstop wake: %v", err)
	} else if taken {
		t.Error("LEGION-208's implementer took a second wake while its own resume is open, want the index to refuse it")
	}

	// The marks table has no reader left.
	var marks bool
	if err := store.Pool().QueryRow(ctx, `select exists (select 1 from information_schema.tables
		where table_name = 'pod_wake_marks')`).Scan(&marks); err != nil {
		t.Fatalf("read whether the marks table is there: %v", err)
	}
	if marks {
		t.Error("pod_wake_marks is still there, want it dropped with the layer that read it")
	}
}
