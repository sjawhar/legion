package store

import (
	"context"
	"testing"
	"time"

	"github.com/jackc/pgx/v5"

	"github.com/sjawhar/legion/daemon/internal/pods"
)

// holdRows is every hold of the pod, oldest first, with whether its streak is still running.
func holdRows(t *testing.T, st *Store, issue string) []struct {
	PodHold
	Ended bool
} {
	t.Helper()
	rows, err := st.Pool().Query(context.Background(), `select id, project, issue, reason, detail, first_at, at, passes, ended
		from pod_holds where issue = $1 order by id`, issue)
	if err != nil {
		t.Fatalf("read the holds of %s: %v", issue, err)
	}
	defer rows.Close()
	var out []struct {
		PodHold
		Ended bool
	}
	for rows.Next() {
		var row struct {
			PodHold
			Ended bool
		}
		if err := rows.Scan(&row.ID, &row.Project, &row.Issue, &row.Reason, &row.Detail,
			&row.FirstAt, &row.At, &row.Passes, &row.Ended); err != nil {
			t.Fatalf("read the holds of %s: %v", issue, err)
		}
		out = append(out, row)
	}
	if err := rows.Err(); err != nil {
		t.Fatalf("read the holds of %s: %v", issue, err)
	}
	return out
}

// A hold row is a streak and not a tally of a pod's whole life: every pass of the same reason
// counts into the running streak, and the streak ends when the pod is released, when the rule
// reads another reason for the pod, or when a pass reads no reason worth a row. A pod held for
// the same reason after any of those opens a streak of its own, with its own first pass, so a
// window's rows are the holds of that window.
func TestAPodsHoldStreakEndsWhenItIsReleasedOrItsReasonChanges(t *testing.T) {
	st := migratedStore(t)
	ctx := context.Background()
	at := time.Now().UTC().Add(-time.Hour).Truncate(time.Millisecond)
	hold := func(reason string, pass int) {
		t.Helper()
		if err := st.RecordPodHold(ctx, PodHold{Project: "legion", Issue: rootIssue, Reason: reason,
			At: at.Add(time.Duration(pass) * time.Minute)}); err != nil {
			t.Fatalf("record the %s hold: %v", reason, err)
		}
	}

	hold("image", 0)
	hold("image", 1)
	if rows := holdRows(t, st, rootIssue); len(rows) != 1 || rows[0].Passes != 2 || rows[0].Ended ||
		!rows[0].FirstAt.Equal(at) || !rows[0].At.Equal(at.Add(time.Minute)) {
		t.Fatalf("after two passes of one reason the holds are %+v, want one running streak of two passes", rows)
	}

	// Monday's streak ends when the pod is released; Friday's hold for the same reason is its
	// own streak, with its own first pass and its own count.
	if _, err := st.RecordPodRelease(ctx, PodRelease{Project: "legion", Issue: rootIssue, Generation: 1,
		Roles: []string{"implementer"}, At: at.Add(2 * time.Minute)}); err != nil {
		t.Fatalf("record the release: %v", err)
	}
	hold("image", 3)
	rows := holdRows(t, st, rootIssue)
	if len(rows) != 2 {
		t.Fatalf("after the release and a later hold the holds are %+v, want two streaks", rows)
	}
	if !rows[0].Ended || rows[0].Passes != 2 || !rows[0].FirstAt.Equal(at) {
		t.Fatalf("the first streak is %+v, want it ended at two passes from %s", rows[0], at)
	}
	if rows[1].Ended || rows[1].Passes != 1 || !rows[1].FirstAt.Equal(at.Add(3*time.Minute)) {
		t.Fatalf("the second streak is %+v, want a running streak of one pass from %s", rows[1], at.Add(3*time.Minute))
	}

	// A pass that reads another reason ends the running streak and opens that reason's own.
	hold("unreported", 4)
	rows = holdRows(t, st, rootIssue)
	if len(rows) != 3 || !rows[1].Ended || rows[2].Reason != "unreported" || rows[2].Ended {
		t.Fatalf("after a pass under another reason the holds are %+v, want the image streak ended and an unreported one running", rows)
	}

	// A pass that holds the pod for nothing worth a row ends what is running, so the next hold
	// of that reason is a streak of its own rather than a continuation days later.
	if err := st.EndPodHoldStreaks(ctx, "legion", rootIssue); err != nil {
		t.Fatalf("end the streaks: %v", err)
	}
	hold("unreported", 5)
	rows = holdRows(t, st, rootIssue)
	if len(rows) != 4 || !rows[2].Ended || rows[3].Passes != 1 || !rows[3].FirstAt.Equal(at.Add(5*time.Minute)) {
		t.Fatalf("after a pass that held nothing the holds are %+v, want a fourth streak of one pass", rows)
	}
	// Every streak is still readable in a window that covers it, which is what R8 reads.
	records, err := st.PodReleaseRecords(ctx, "legion", at.Add(-time.Minute))
	if err != nil {
		t.Fatal(err)
	}
	if len(records.Holds) != 4 || len(records.Releases) != 1 {
		t.Fatalf("the window holds %d holds and %d releases, want 4 and 1", len(records.Holds), len(records.Releases))
	}
}

// R6: the stranded report reads the wakes of a pod's newest release alone. A wake of an older
// release that never reached a first turn is a pod the daemon has released again since, not one
// a wake never brought back, and reading it would list that role as stranded the moment its next
// release was recorded. The stuck pass, which warns about a wake once whatever came after it,
// keeps reading every unfinished wake.
func TestTheStrandedReadSeesOnlyTheWakesOfTheNewestRelease(t *testing.T) {
	st := migratedStore(t)
	ctx := context.Background()
	at := time.Now().UTC().Add(-time.Hour).Truncate(time.Millisecond)
	first, err := st.RecordPodRelease(ctx, PodRelease{Project: "legion", Issue: rootIssue, Generation: 1,
		Roles: []string{"implementer"}, At: at})
	if err != nil {
		t.Fatalf("record the first release: %v", err)
	}
	wake := func(release int64, role string, when time.Time) {
		t.Helper()
		if err := st.Tx(ctx, func(tx pgx.Tx) error {
			_, err := RecordPodWakeTx(ctx, tx, PodResume{Release: release, Project: "legion", Issue: rootIssue,
				Role: role, Kind: pods.WakeDelivery, At: when})
			return err
		}); err != nil {
			t.Fatalf("record the wake of %s: %v", role, err)
		}
	}
	wake(first, "implementer", at.Add(time.Minute))

	bound := time.Now().UTC()
	if unfinished, err := st.UnfinishedPodResumes(ctx, "legion", bound); err != nil || len(unfinished) != 1 {
		t.Fatalf("unfinished wakes of the only release = %+v, %v; want the one wake", unfinished, err)
	}

	// The pod was released again: the wake of the release before it is over, whatever became of
	// it, and only the new release's wakes measure anything.
	second, err := st.RecordPodRelease(ctx, PodRelease{Project: "legion", Issue: rootIssue, Generation: 1,
		Roles: []string{"implementer"}, At: at.Add(30 * time.Minute)})
	if err != nil {
		t.Fatalf("record the second release: %v", err)
	}
	unfinished, err := st.UnfinishedPodResumes(ctx, "legion", bound)
	if err != nil || len(unfinished) != 0 {
		t.Fatalf("unfinished wakes after the second release = %+v, %v; want none of the release that is over", unfinished, err)
	}
	if stuck, err := st.StuckPodResumes(ctx, "legion", bound); err != nil || len(stuck) != 1 {
		t.Fatalf("the stuck pass reads %+v, %v; want the unfinished wake it has still to warn about", stuck, err)
	}

	wake(second, "tester", at.Add(31*time.Minute))
	unfinished, err = st.UnfinishedPodResumes(ctx, "legion", bound)
	if err != nil || len(unfinished) != 1 || unfinished[0].Role != "tester" {
		t.Fatalf("unfinished wakes of the newest release = %+v, %v; want the tester's alone", unfinished, err)
	}
}
