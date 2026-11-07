package admit

import (
	"context"
	"testing"

	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/sjawhar/legion/daemon/internal/intake"
	"github.com/sjawhar/legion/daemon/internal/phase"
	"github.com/sjawhar/legion/daemon/internal/record"
)

// statusWritesOf is every dispatch_status row queued for key, in outbox order, as the executor
// would take them.
func statusWritesOf(t *testing.T, pool *pgxpool.Pool, key string) []record.StatusWrite {
	t.Helper()
	rows, err := pool.Query(context.Background(),
		"select payload from outbox where issue = $1 and kind = $2 order by id", key, string(record.OutboxKindDispatchStatus))
	if err != nil {
		t.Fatalf("read the status writes of %s: %v", key, err)
	}
	defer rows.Close()
	var writes []record.StatusWrite
	for rows.Next() {
		var write record.StatusWrite
		if err := rows.Scan(&write); err != nil {
			t.Fatalf("decode a status write of %s: %v", key, err)
		}
		writes = append(writes, write)
	}
	if err := rows.Err(); err != nil {
		t.Fatalf("read the status writes of %s: %v", key, err)
	}
	return writes
}

// A child released into a live tree whose design gate is open takes a slot in the same pass and
// starts planning, and Dispatch is told so: the issue a person watches shows in_progress from the
// moment Legion works it, which is the whole of R2 (dispatch://INFRA-714/spec, dispatch://INFRA-326).
// The release itself writes todo, because the child enters the waiting line, so without the write
// this start queues the board would read todo for as long as the child runs.
func TestAReleasedChildsFirstPhaseTellsDispatchItIsInProgress(t *testing.T) {
	pool := migratedPool(t)
	admission := cfTree(t, pool, 3)
	openGate(t, pool, admission, "gate-approved")

	seedMirrorChild(t, pool, cfFirst, "todo", cfRoot)
	releaseChild(t, pool, admission, "release-first", cfFirst, cfRoot, 9)

	child := issue(t, pool, cfFirst)
	if child.Phase != phase.Planning {
		t.Fatalf("the released child = %#v, want it planning: its tree's gate is open and the cap has room", child)
	}
	if child.Status != "in_progress" {
		t.Fatalf("the released child's recorded status = %q, want in_progress: the phase it stands in is the status it carries", child.Status)
	}
	writes := statusWritesOf(t, pool, cfFirst)
	if len(writes) != 1 || writes[0].Status != "in_progress" || writes[0].Kind != record.OrdinaryStatusRow {
		t.Fatalf("the status writes queued for the released child = %#v, want one ordinary write of in_progress", writes)
	}
}

// The same child, released while the cap is full: it waits at admitted and is promoted by a later
// pass, which is the shape most children of a busy deployment take. The promotion writes the
// status of the phase it starts, so the board never reads todo for work Legion has begun.
func TestAChildPromotedByALaterPassTellsDispatchItIsInProgress(t *testing.T) {
	pool := migratedPool(t)
	admission := cfTree(t, pool, 1)
	openGate(t, pool, admission, "gate-approved")
	seedMirrorChild(t, pool, cfFirst, "todo", cfRoot)
	releaseChild(t, pool, admission, "release-first", cfFirst, cfRoot, 9)

	if waiting := issue(t, pool, cfFirst); waiting.Phase != phase.Admitted || len(statusWritesOf(t, pool, cfFirst)) != 0 {
		t.Fatalf("the released child at a full cap = %#v with %#v queued, want it admitted with no status write: it waits in the line",
			waiting, statusWritesOf(t, pool, cfFirst))
	}

	applyFact(t, pool, quiet(t, 3), "tick-with-room", intake.ControllerTick{})
	promoted := issue(t, pool, cfFirst)
	if promoted.Phase != phase.Planning || promoted.Status != "in_progress" {
		t.Fatalf("the child the later pass promoted = %#v, want it planning and in_progress", promoted)
	}
	writes := statusWritesOf(t, pool, cfFirst)
	if len(writes) != 1 || writes[0].Status != "in_progress" || writes[0].ObservedStatus != "todo" {
		t.Fatalf("the status writes queued for the promoted child = %#v, want one in_progress over the todo it waited at", writes)
	}
}
