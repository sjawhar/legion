package store

import (
	"context"
	"testing"
	"time"
)

// The release before 20261107052815 kept nothing of what a worker's agent was doing: its daemon
// dropped the tool frames of the stream. So the eight step columns arrive with no reading to
// carry forward, and the migration backfills none of them. There is nothing honest to backfill —
// a claim of that release has made calls nobody observed, and any time written for them would be
// invented — and an invented time is exactly what the backstop these columns exist for must never
// read: it would judge a worker that has been silent for hours as one that stepped at the
// upgrade. Such a claim is judged instead from the start of its next turn, which the daemon
// writes as that turn begins.
//
// The store's harness migrates to any version a test names (migrateThrough), so these rows are
// written as the released daemon left them, at the migration before this one, and read after this
// one ran.
func TestTheStepRecordArrivesEmptyForEveryClaimTheUpgradeFinds(t *testing.T) {
	store := emptyStore(t)
	ctx := context.Background()
	migrateThrough(t, store, 20261105022045)

	if _, err := store.Pool().Exec(ctx, `insert into claims (token, project, tree, issue, role, generation,
		session, session_file, state, launch_failures, prompt_failures, prompt_retires, uncertain_streak)
		values
		('legion-LEGION-208-architect', 'legion', 'LEGION-208', 'LEGION-208', 'architect', 1, 'ses_architect', '',
			'working', 0, 0, 0, 0),
		('legion-LEGION-209-implementer', 'legion', 'LEGION-208', 'LEGION-209', 'implementer', 1, 'ses_implementer', '',
			'idle', 0, 0, 0, 0)`); err != nil {
		t.Fatalf("seed the claims of the release this upgrades from: %v", err)
	}

	if _, err := store.Migrate(ctx); err != nil {
		t.Fatalf("migrate: %v", err)
	}

	for _, token := range []string{"legion-LEGION-208-architect", "legion-LEGION-209-implementer"} {
		var stepAt, turnStartedAt, startedAt, endedAt *time.Time
		var tool, callID, hash string
		var args int
		if err := store.Pool().QueryRow(ctx, `select step_at, turn_started_at, step_tool, step_call_id,
			step_args_bytes, step_args_hash, step_started_at, step_ended_at from claims where token = $1`, token).
			Scan(&stepAt, &turnStartedAt, &tool, &callID, &args, &hash, &startedAt, &endedAt); err != nil {
			t.Fatalf("read the step record of %s: %v", token, err)
		}
		for _, column := range []struct {
			name string
			at   *time.Time
		}{
			{"step_at", stepAt},
			{"turn_started_at", turnStartedAt},
			{"step_started_at", startedAt},
			{"step_ended_at", endedAt},
		} {
			if column.at != nil {
				t.Errorf("%s of %s = %s after the upgrade, want null: the release this upgrades from observed no step",
					column.name, token, *column.at)
			}
		}
		if tool != "" || callID != "" || args != 0 || hash != "" {
			t.Errorf("the step record of %s names the call %q/%q of %d bytes, hash %q after the upgrade; want no call at all",
				token, tool, callID, args, hash)
		}
	}
}
