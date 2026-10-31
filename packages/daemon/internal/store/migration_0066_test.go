package store

import (
	"context"
	"strings"
	"testing"
)

// A phase-ended row is the message the daemon publishes to the role whose phase a CI-red
// transition ended (record.PhaseEnded). The outbox refuses a kind its constraint does not name, so
// the row the workflow now writes could not be queued before 0066, and the transaction that wrote
// it — the one that sends the work back to implementing — would fail with it: the negative control
// is that refusal, read before the migration. After it the row is queued, and so is every kind the
// constraint already named, since the migration rewrites the list whole and a kind left out would
// stop its own effect. A kind that is none is still refused.
func TestTheOutboxTakesThePhaseEndedKindWith0066(t *testing.T) {
	ctx := context.Background()
	store := emptyStore(t)
	migrateThrough(t, store, 65)
	queue := func(kind string) error {
		_, err := store.Pool().Exec(ctx, `insert into outbox (kind, issue, payload, attempts, next_at, last_error)
			values ($1, 'LEGION-208', '{}', 0, now(), '')`, kind)
		return err
	}
	err := queue("phase_ended")
	if err == nil || !strings.Contains(err.Error(), "outbox_kind_check") {
		t.Fatalf("queueing a phase_ended row before 0063 = %v, want the kind constraint to refuse it: it is the row the migration exists for", err)
	}
	t.Logf("before the migration the phase-ended row is refused: %v", err)
	if _, err := store.Migrate(ctx); err != nil {
		t.Fatalf("migrate through the phase-ended kind: %v", err)
	}
	for _, kind := range []string{
		"dispatch_status", "dispatch_message", "notice", "controller_notice", "supervise", "gate_seed",
		"linger_close", "workspace_remove", "merge_queue_publish", "issue_branch", "issue_suspend",
		"auto_merge_disable", "ready_merge", "check_compare", "check_look", "check_bound", "phase_ended",
	} {
		if err := queue(kind); err != nil {
			t.Fatalf("queue a %s row after 0063: %v", kind, err)
		}
	}
	t.Logf("after the migration the outbox takes all seventeen kinds, phase_ended among them")
	if err := queue("quiesce"); err == nil || !strings.Contains(err.Error(), "outbox_kind_check") {
		t.Fatalf("queueing a row of a kind that is none = %v, want the constraint to refuse it", err)
	}
}
