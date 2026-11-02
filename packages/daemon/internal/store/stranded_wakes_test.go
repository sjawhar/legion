package store

import (
	"context"
	"testing"
	"time"
)

// The stranded report reads every wake whose woken role has taken no turn by its bound, a wake
// the daemon has already warned about included: a younger wake, one whose role took its turn and
// one of another project are each left out.
func TestUnansweredPodWakesAreEveryWakeWithNoFirstTurnByTheBound(t *testing.T) {
	store := migratedStore(t)
	ctx := context.Background()
	now := time.Date(2026, 11, 2, 12, 0, 0, 0, time.UTC)
	wake := func(project, issue, role string, at time.Time) int64 {
		t.Helper()
		id, err := store.RecordPodWake(ctx, PodResume{
			Project: project, Issue: issue, Role: role, Kind: "delivery", At: at,
		})
		if err != nil {
			t.Fatalf("record the wake of %s: %v", issue, err)
		}
		return id
	}
	old := wake("legion", "LEGION-208", "implementer", now.Add(-20*time.Minute))
	warned := wake("legion", "LEGION-209", "tester", now.Add(-30*time.Minute))
	if err := store.MarkPodResumeStuck(ctx, warned); err != nil {
		t.Fatalf("mark the resume stuck: %v", err)
	}
	answered := wake("legion", "LEGION-210", "reviewer", now.Add(-40*time.Minute))
	if err := store.MarkPodResumeFirstTurn(ctx, answered, now.Add(-35*time.Minute)); err != nil {
		t.Fatalf("stamp the first turn: %v", err)
	}
	wake("legion", "LEGION-211", "merger", now.Add(-time.Minute))
	wake("widgets", "WIDGETS-1", "implementer", now.Add(-20*time.Minute))

	unanswered, err := store.UnansweredPodWakes(ctx, "legion", now.Add(-10*time.Minute))
	if err != nil {
		t.Fatalf("UnansweredPodWakes: %v", err)
	}

	if len(unanswered) != 2 || unanswered[0].ID != old || unanswered[1].ID != warned {
		t.Fatalf("unanswered wakes = %+v, want the wake of %d and the warned one of %d", unanswered, old, warned)
	}
	if got := unanswered[0]; got.Issue != "LEGION-208" || got.Role != "implementer" || got.Kind != "delivery" ||
		!got.At.Equal(now.Add(-20*time.Minute)) || got.Stuck {
		t.Errorf("the oldest unanswered wake read back as %+v", got)
	}
	if !unanswered[1].Stuck {
		t.Error("the warned wake read back unstuck; the report reads it with its mark")
	}
}
