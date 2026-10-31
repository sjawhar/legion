package docs

import (
	"bytes"
	"context"
	"errors"
	"log/slog"
	"math"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/reearth/ygo/crdt"

	"github.com/sjawhar/envoy/internal/dispatch/model"
	"github.com/sjawhar/envoy/internal/dispatch/store"
	"github.com/sjawhar/envoy/internal/dispatch/store/storetest"
)

// A tail-rule compaction folds the rows older than the tail into the newest of them and leaves
// every younger row as it was written, byte for byte and under the transaction that wrote it. The
// folded row says it changed content when a row it folded past the settlement cursor did, and the
// document reads as it did.
func TestATailCompactionFoldsOnlyTheRowsOlderThanTheTail(t *testing.T) {
	database := storetest.Open(t)
	versioned := NewPgVersioned(database)
	for _, test := range []struct {
		name           string
		agedUpTo       int64
		cursor         int64
		outcome        compactOutcome
		rows           int
		contentChanged bool
	}{
		{name: "every row young", agedUpTo: 0, cursor: 2, outcome: compactNothing, rows: 5},
		{name: "old rows the cursor covers below young ones", agedUpTo: 3, cursor: 2, outcome: compactCompacted, rows: 3, contentChanged: false},
		{name: "old rows past the cursor below young ones", agedUpTo: 3, cursor: 1, outcome: compactCompacted, rows: 3, contentChanged: true},
		{name: "every row old", agedUpTo: 5, cursor: 2, outcome: compactCompacted, rows: 1, contentChanged: true},
	} {
		t.Run(test.name, func(t *testing.T) {
			artifactID := createDocument(t, database, "one")
			writer := newProofHistory(t, 101)
			for index, write := range []struct {
				markdown string
				content  bool
			}{
				{"one\n", true}, {"one\n\ntwo\n", true}, {"one\n\ntwo\n", false}, {"one\n\ntwo\n", false}, {"three\n", true},
			} {
				if _, err := versioned.AppendUpdateWithCredit(context.Background(), artifactID, writer.write(write.markdown), write.content, nil); err != nil {
					t.Fatalf("append update %d: %v", index+1, err)
				}
			}
			setSettlementCursor(t, database, artifactID, test.cursor)
			ageUpdatesThrough(t, database, artifactID, test.agedUpTo, time.Hour)
			before, reading := storedRows(t, database, artifactID), storedReading(t, versioned, artifactID)

			result := compactUnderTail(t, versioned, artifactID)
			if result.outcome != test.outcome || result.deleted != 5-test.rows {
				t.Fatalf("compaction = %+v, want outcome %d with %d rows deleted", result, test.outcome, 5-test.rows)
			}
			if test.outcome == compactNothing {
				requireRowsAsStored(t, database, artifactID, before)
			} else {
				after := requireCompactedFrom(t, database, artifactID, before, test.agedUpTo, test.cursor)
				if after[0].contentChanged != test.contentChanged {
					t.Fatalf("the folded row's content_changed = %v, want %v", after[0].contentChanged, test.contentChanged)
				}
			}
			if rows := storedUpdateCount(t, database, artifactID); rows != test.rows {
				t.Fatalf("compaction left %d rows, want %d", rows, test.rows)
			}
			if got := storedReading(t, versioned, artifactID); got != reading {
				t.Fatalf("the compacted document reads\n%s\nwant\n%s", got, reading)
			}
		})
	}
}

// A room's close compacts its document under the tail rule: the rows older than the tail fold into
// the newest of them, the young row the last edit wrote stays as written, and the document reads
// as it did.
func TestARoomCloseCompactionFoldsOnlyTheRowsOlderThanTheTail(t *testing.T) {
	service, artifactID := newTestService(t)
	service.settle = time.Hour
	foldPoint := writeRoomRows(t, service, artifactID)
	ageUpdatesThrough(t, service.store, artifactID, foldPoint, time.Hour)
	before, cursor := storedRows(t, service.store, artifactID), settlementCursor(t, service.store, artifactID)
	reading := storedReading(t, NewPgVersioned(service.store), artifactID)

	if err := service.Evict(context.Background(), artifactID); err != nil {
		t.Fatalf("evict the document's room: %v", err)
	}
	requireCompactedFrom(t, service.store, artifactID, before, foldPoint, cursor)
	if got := storedReading(t, NewPgVersioned(service.store), artifactID); got != reading {
		t.Fatalf("the compacted document reads\n%s\nwant\n%s", got, reading)
	}
	if got, err := service.Text(context.Background(), artifactID); err != nil || got != "First.\n\nSecond.\n\nThird.\n" {
		t.Fatalf("text after the room's close = %q (%v), want the three paragraphs", got, err)
	}
}

// A room's close does not wait for a document lock another holder has: it leaves the rows as stored
// for a later compaction, which folds them once the lock is free.
func TestARoomCloseCompactionLeavesABusyDocumentForALaterCompaction(t *testing.T) {
	service, artifactID := newTestService(t)
	service.settle = time.Hour
	foldPoint := writeRoomRows(t, service, artifactID)
	ageUpdatesThrough(t, service.store, artifactID, foldPoint, time.Hour)
	before, cursor := storedRows(t, service.store, artifactID), settlementCursor(t, service.store, artifactID)

	ctx := context.Background()
	unlock := holdDocumentLock(t, service.store, artifactID)
	if err := service.Evict(ctx, artifactID); err != nil {
		t.Fatalf("evict the document's room: %v", err)
	}
	unlock()
	requireRowsAsStored(t, service.store, artifactID, before)
	if result := compactUnderTail(t, service.persistence.(*PgVersioned), artifactID); result.outcome != compactCompacted {
		t.Fatalf("the later compaction = %+v, want it compacted", result)
	}
	requireCompactedFrom(t, service.store, artifactID, before, foldPoint, cursor)
}

// A failed room's eviction waits for the document's lock and compacts under the tail rule before
// the room recovers: the rows older than the tail fold, the young row stays as written, and the
// recovered room reads the document as it was.
func TestAFailedRoomsEvictionCompactionWaitsForTheLockAndFoldsOnlyOldRows(t *testing.T) {
	service, artifactID := newTestService(t)
	service.settle = time.Hour
	foldPoint := writeRoomRows(t, service, artifactID)
	ageUpdatesThrough(t, service.store, artifactID, foldPoint, time.Hour)
	before, cursor := storedRows(t, service.store, artifactID), settlementCursor(t, service.store, artifactID)
	reading := storedReading(t, NewPgVersioned(service.store), artifactID)

	ctx := context.Background()
	unlock := holdDocumentLock(t, service.store, artifactID)
	service.failRoom(artifactID, errors.New("test: fail the room"))
	waitFor(t, 10*time.Second, "the failed room's compaction to wait for the document's lock", func() bool {
		return lockWaits(t, ctx, service.store, "select pg_advisory_lock(%") > 0
	})
	requireRowsAsStored(t, service.store, artifactID, before)
	unlock()
	recoveryCtx, cancel := context.WithTimeout(ctx, recoveryBound)
	defer cancel()
	if err := service.awaitRoomRecovery(recoveryCtx, artifactID); err != nil {
		t.Fatalf("wait for the failed room's recovery: %v", err)
	}
	requireCompactedFrom(t, service.store, artifactID, before, foldPoint, cursor)
	if got := storedReading(t, NewPgVersioned(service.store), artifactID); got != reading {
		t.Fatalf("the compacted document reads\n%s\nwant\n%s", got, reading)
	}
	if got, err := service.Text(ctx, artifactID); err != nil || got != "First.\n\nSecond.\n\nThird.\n" {
		t.Fatalf("text after the room's recovery = %q (%v), want the three paragraphs", got, err)
	}
}

// A prune that deletes rows at or below the fold point between a compaction's read and its write
// drops the fold: the rows stay as the prune left them, and the next compaction folds those.
func TestACompactionDropsItsFoldWhenAPruneRunsInsideIt(t *testing.T) {
	database, versioned, artifactID := newHeldCompactionDocument(t)
	var left []updateRow
	var reading string
	duringCompactionFold(versioned, artifactID, func() {
		if err := versioned.PruneAfter(context.Background(), artifactID, 2, nil); err != nil {
			t.Fatalf("prune to version 2: %v", err)
		}
		left, reading = storedRows(t, database, artifactID), storedReading(t, versioned, artifactID)
	})
	if result := compactUnderTail(t, versioned, artifactID); result.outcome != compactDropped {
		t.Fatalf("compaction across a prune = %+v, want it dropped", result)
	}
	requireRowsAsStored(t, database, artifactID, left)
	cursor := settlementCursor(t, database, artifactID)
	if result := compactUnderTail(t, versioned, artifactID); result.outcome != compactCompacted || result.deleted != 1 {
		t.Fatalf("the next compaction = %+v, want the pruned rows folded", result)
	}
	requireCompactedFrom(t, database, artifactID, left, 2, cursor)
	if got := storedReading(t, versioned, artifactID); got != reading {
		t.Fatalf("the compacted document reads\n%s\nwant the pruned one\n%s", got, reading)
	}
}

// A prune whose checkpoint committed and whose deletion did not, found by a compaction's write, is
// finished and the fold dropped: the rows past the ceiling go, and the next compaction folds the
// rows the prune left.
func TestACompactionFinishesAStrandedPruneAndFoldsOnTheNextAttempt(t *testing.T) {
	database, versioned, artifactID := newHeldCompactionDocument(t)
	var stored []updateRow
	var reading string
	duringCompactionFold(versioned, artifactID, func() {
		if _, err := database.Pool.Exec(context.Background(), `
			insert into doc_checkpoints (artifact_id, ceiling, state) values ($1, 3, null)
		`, artifactID); err != nil {
			t.Fatalf("strand a prune checkpoint: %v", err)
		}
		stored, reading = storedRows(t, database, artifactID), storedReading(t, versioned, artifactID)
	})
	if result := compactUnderTail(t, versioned, artifactID); result.outcome != compactDropped {
		t.Fatalf("compaction across a stranded prune = %+v, want it dropped", result)
	}
	var checkpoints int
	if err := database.Pool.QueryRow(context.Background(), `select count(*) from doc_checkpoints where artifact_id = $1`, artifactID).Scan(&checkpoints); err != nil {
		t.Fatalf("count prune checkpoints: %v", err)
	}
	if checkpoints != 0 {
		t.Fatalf("%d prune checkpoints remain, want the compaction to have finished the prune", checkpoints)
	}
	left := stored[:3]
	requireRowsAsStored(t, database, artifactID, left)
	cursor := settlementCursor(t, database, artifactID)
	if result := compactUnderTail(t, versioned, artifactID); result.outcome != compactCompacted || result.deleted != 2 {
		t.Fatalf("the next compaction = %+v, want the rows the prune left folded", result)
	}
	requireCompactedFrom(t, database, artifactID, left, 3, cursor)
	if got := storedReading(t, versioned, artifactID); got != reading {
		t.Fatalf("the compacted document reads\n%s\nwant the pruned one\n%s", got, reading)
	}
}

// A rebuild between a compaction's read and its write drops the fold: the rebuilt history stands
// as the rebuild wrote it.
func TestACompactionDropsItsFoldWhenARebuildRunsInsideIt(t *testing.T) {
	database, versioned, artifactID := newHeldCompactionDocument(t)
	var left []updateRow
	duringCompactionFold(versioned, artifactID, func() {
		ctx := context.Background()
		tx, err := database.Pool.Begin(ctx)
		if err != nil {
			t.Fatalf("begin the rebuild: %v", err)
		}
		defer tx.Rollback(ctx)
		if _, err := versioned.RebuildTx(ctx, tx, artifactID, newProofHistory(t, 303).write("rebuilt\n")); err != nil {
			t.Fatalf("rebuild the document: %v", err)
		}
		if err := tx.Commit(ctx); err != nil {
			t.Fatalf("commit the rebuild: %v", err)
		}
		left = storedRows(t, database, artifactID)
	})
	if result := compactUnderTail(t, versioned, artifactID); result.outcome != compactDropped {
		t.Fatalf("compaction across a rebuild = %+v, want it dropped", result)
	}
	requireRowsAsStored(t, database, artifactID, left)
	if got := docMarkdown(loadDocument(t, versioned, artifactID).Doc); got != "rebuilt\n" {
		t.Fatalf("the rebuilt document reads %q, want the rebuild's", got)
	}
}

// A delete between a compaction's read and its write drops the fold: the document stays deleted.
func TestACompactionDropsItsFoldWhenADeleteRunsInsideIt(t *testing.T) {
	database, versioned, artifactID := newHeldCompactionDocument(t)
	duringCompactionFold(versioned, artifactID, func() {
		if err := versioned.Delete(context.Background(), artifactID); err != nil {
			t.Fatalf("delete the document: %v", err)
		}
	})
	if result := compactUnderTail(t, versioned, artifactID); result.outcome != compactDropped {
		t.Fatalf("compaction across a delete = %+v, want it dropped", result)
	}
	if rows := storedUpdateCount(t, database, artifactID); rows != 0 {
		t.Fatalf("the deleted document holds %d rows after the compaction, want none", rows)
	}
}

// A compaction by another process sharing the database, between this one's read and its write,
// drops this one's fold: the rows stay as the other compaction wrote them, and the document reads
// as it did.
func TestACompactionDropsItsFoldWhenAnotherProcessCompactsInsideIt(t *testing.T) {
	database, versioned, artifactID := newHeldCompactionDocument(t)
	reading := storedReading(t, versioned, artifactID)
	var left []updateRow
	duringCompactionFold(versioned, artifactID, func() {
		other := NewPgVersioned(database)
		if result := compactUnderTail(t, other, artifactID); result.outcome != compactCompacted {
			t.Fatalf("the other process's compaction = %+v, want it compacted", result)
		}
		left = storedRows(t, database, artifactID)
	})
	if result := compactUnderTail(t, versioned, artifactID); result.outcome != compactDropped {
		t.Fatalf("compaction across another process's = %+v, want it dropped", result)
	}
	requireRowsAsStored(t, database, artifactID, left)
	if got := storedReading(t, versioned, artifactID); got != reading {
		t.Fatalf("the document reads\n%s\nwant\n%s", got, reading)
	}
}

// An older build's keep-1 compaction, which folds every row into the head, between a compaction's
// read and its write drops the fold: the one row it left stands, and the document reads as it did.
func TestACompactionDropsItsFoldWhenAnOlderBuildCompactsInsideIt(t *testing.T) {
	database, versioned, artifactID := newHeldCompactionDocument(t)
	reading := storedReading(t, versioned, artifactID)
	var left []updateRow
	duringCompactionFold(versioned, artifactID, func() {
		if deleted, err := NewPgVersioned(database).Compact(context.Background(), artifactID, 1); err != nil || deleted != 4 {
			t.Fatalf("the older build's compaction deleted %d rows (%v), want 4", deleted, err)
		}
		left = storedRows(t, database, artifactID)
	})
	if result := compactUnderTail(t, versioned, artifactID); result.outcome != compactDropped {
		t.Fatalf("compaction across an older build's = %+v, want it dropped", result)
	}
	requireRowsAsStored(t, database, artifactID, left)
	if got := storedReading(t, versioned, artifactID); got != reading {
		t.Fatalf("the document reads\n%s\nwant\n%s", got, reading)
	}
}

// A settlement between a compaction's read and its write moves the cursor past every row the
// compaction folds. The compaction reads the cursor under the lock it writes under, so the folded
// row does not claim content past the cursor, and a later settlement versions nothing more.
func TestACompactionReadsTheCursorASettlementMovedInsideIt(t *testing.T) {
	service, artifactID := newTestService(t)
	service.settle = time.Hour
	seedServiceText(t, service, artifactID, "First.\n")
	if _, err := joinedReplaceText(service, artifactID, "Second.\n", model.Actor{Kind: "user", ID: "alice"}); err != nil {
		t.Fatalf("write a content update: %v", err)
	}
	rows := storedRows(t, service.store, artifactID)
	foldPoint := rows[len(rows)-1].version
	if before := settlementCursor(t, service.store, artifactID); before >= foldPoint {
		t.Fatalf("the cursor stands at %d before the settlement, want it below the head %d", before, foldPoint)
	}
	ageUpdatesThrough(t, service.store, artifactID, everyVersion, time.Hour)
	persist := service.persistence.(*PgVersioned)
	var settled []updateRow
	var cursor int64
	duringCompactionFold(persist, artifactID, func() {
		settleCurrentGeneration(t, service, artifactID)
		settled, cursor = storedRows(t, service.store, artifactID), settlementCursor(t, service.store, artifactID)
	})
	if result := compactUnderTail(t, persist, artifactID); result.outcome != compactCompacted {
		t.Fatalf("compaction across a settlement = %+v, want it compacted", result)
	}
	if cursor < foldPoint {
		t.Fatalf("the settlement left the cursor at %d, want it at or past the fold point %d", cursor, foldPoint)
	}
	after := requireCompactedFrom(t, service.store, artifactID, settled, foldPoint, cursor)
	if after[0].contentChanged {
		t.Fatal("the folded row claims content past the cursor the settlement moved")
	}
	versions := nextVersionNumber(t, service.store, artifactID)
	if err := service.Evict(context.Background(), artifactID); err != nil {
		t.Fatalf("evict the compacted document: %v", err)
	}
	if got, err := service.Text(context.Background(), artifactID); err != nil || got != "Second.\n" {
		t.Fatalf("text after compaction = %q (%v), want the replacement", got, err)
	}
	settleCurrentGeneration(t, service, artifactID)
	if again := nextVersionNumber(t, service.store, artifactID); again != versions {
		t.Fatalf("a settlement after the compaction wrote version %d, want none past %d", again-1, versions-1)
	}
}

// Rows that do not fold are left as stored and logged once. A second compaction of the same rows
// skips them without folding again or logging, and a compaction after they changed folds again.
func TestACompactionSkipsAnUnfoldableLogUntilItChanges(t *testing.T) {
	logs := &storetest.LockedLog{}
	previous := slog.Default()
	slog.SetDefault(slog.New(slog.NewTextHandler(logs, &slog.HandlerOptions{Level: slog.LevelWarn})))
	t.Cleanup(func() { slog.SetDefault(previous) })

	// Each later row leans on an insert no row holds ("x"), so the fold parks more than its
	// pending queue holds; the writer writes the update that changes the log below.
	rows, writer := unfoldableHistory()
	text := writer.GetText("t")
	database := storetest.Open(t)
	versioned := NewPgVersioned(database)
	shrinkPendingItems(t, nil, versioned)
	artifactID := createDocument(t, database, "unfoldable")
	for index, row := range rows {
		if _, err := versioned.AppendUpdate(context.Background(), artifactID, row); err != nil {
			t.Fatalf("append update %d: %v", index+1, err)
		}
	}
	ageUpdatesThrough(t, database, artifactID, everyVersion, time.Hour)
	stored := storedRows(t, database, artifactID)
	folds := 0
	versioned.duringCompactionFold = func(string) { folds++ }
	const logged = "compaction leaves them as stored"

	for attempt, want := range []int{1, 1} {
		if result := compactUnderTail(t, versioned, artifactID); result.outcome != compactUnfoldable {
			t.Fatalf("compaction %d = %+v, want the rows left as unfoldable", attempt+1, result)
		}
		if folds != want || strings.Count(logs.String(), logged) != 1 {
			t.Fatalf("after compaction %d: %d folds and %d log lines, want %d folds and one line:\n%s",
				attempt+1, folds, strings.Count(logs.String(), logged), want, logs.String())
		}
		requireRowsAsStored(t, database, artifactID, stored)
	}

	before := writer.StateVector()
	writer.Transact(func(txn *crdt.Transaction) { text.Insert(txn, 0, "z", nil) })
	if _, err := versioned.AppendUpdate(context.Background(), artifactID, crdt.EncodeStateAsUpdateV1(writer, before)); err != nil {
		t.Fatalf("append the changing update: %v", err)
	}
	ageUpdatesThrough(t, database, artifactID, everyVersion, time.Hour)
	if result := compactUnderTail(t, versioned, artifactID); result.outcome != compactUnfoldable {
		t.Fatalf("compaction after the log changed = %+v, want the rows left as unfoldable", result)
	}
	if folds != 2 || strings.Count(logs.String(), logged) != 2 {
		t.Fatalf("after the log changed: %d folds and %d log lines, want 2 of each", folds, strings.Count(logs.String(), logged))
	}
}

// A browser's edit lands in the store while the document's compaction is held inside its fold:
// the compaction holds no lock then. The compaction then folds the rows it read, and the edit's row,
// above them, stays as written.
func TestABrowserAppendLandsWhileACompactionIsInsideItsFold(t *testing.T) {
	service, artifactID := newTestService(t)
	service.settle = time.Hour
	seedServiceText(t, service, artifactID, "First.\n")
	editLiveTree(t, service, artifactID, appendBlocks(t, "Second.\n"))
	waitForLandedAppends(t, service, artifactID)
	ageUpdatesThrough(t, service.store, artifactID, everyVersion, time.Hour)
	rows := storedRows(t, service.store, artifactID)
	foldPoint := rows[len(rows)-1].version
	persist := service.persistence.(*PgVersioned)
	var written []updateRow
	var cursor int64
	duringCompactionFold(persist, artifactID, func() {
		editLiveTree(t, service, artifactID, appendBlocks(t, "Third.\n"))
		ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
		defer cancel()
		if err := service.waitForDurableAppends(ctx, artifactID); err != nil {
			t.Fatalf("the browser's append did not land while the compaction was inside its fold: %v", err)
		}
		written, cursor = storedRows(t, service.store, artifactID), settlementCursor(t, service.store, artifactID)
	})
	if result := compactUnderTail(t, persist, artifactID); result.outcome != compactCompacted {
		t.Fatalf("compaction around a browser append = %+v, want it compacted", result)
	}
	if len(written) <= len(rows) {
		t.Fatalf("the browser's append added no row: %d rows before, %d after", len(rows), len(written))
	}
	requireCompactedFrom(t, service.store, artifactID, written, foldPoint, cursor)
	if got := docMarkdown(loadDocument(t, persist, artifactID).Doc); got != "First.\n\nSecond.\n\nThird.\n" {
		t.Fatalf("the compacted document reads %q, want the three paragraphs", got)
	}
}

// An agent's write joined to its request's transaction commits while the document's compaction is
// held inside its fold, and the compaction then folds the rows it read beneath the write's row.
func TestAJoinedWriteCommitsWhileACompactionIsInsideItsFold(t *testing.T) {
	service, artifactID := newTestService(t)
	service.settle = time.Hour
	seedServiceText(t, service, artifactID, "First.\n")
	editLiveTree(t, service, artifactID, appendBlocks(t, "Second.\n"))
	waitForLandedAppends(t, service, artifactID)
	ageUpdatesThrough(t, service.store, artifactID, everyVersion, time.Hour)
	rows := storedRows(t, service.store, artifactID)
	foldPoint := rows[len(rows)-1].version
	persist := service.persistence.(*PgVersioned)
	var written []updateRow
	var cursor int64
	duringCompactionFold(persist, artifactID, func() {
		done := make(chan error, 1)
		go func() {
			_, err := joinedReplaceText(service, artifactID, "Replaced.\n", model.Actor{Kind: "session", ID: "agent"})
			done <- err
		}()
		select {
		case err := <-done:
			if err != nil {
				t.Fatalf("the joined write: %v", err)
			}
		case <-time.After(30 * time.Second):
			t.Fatal("the joined write did not commit while the compaction was inside its fold")
		}
		written, cursor = storedRows(t, service.store, artifactID), settlementCursor(t, service.store, artifactID)
	})
	if result := compactUnderTail(t, persist, artifactID); result.outcome != compactCompacted {
		t.Fatalf("compaction around a joined write = %+v, want it compacted", result)
	}
	if len(written) <= len(rows) {
		t.Fatalf("the joined write added no row: %d rows before, %d after", len(rows), len(written))
	}
	requireCompactedFrom(t, service.store, artifactID, written, foldPoint, cursor)
	if got := docMarkdown(loadDocument(t, persist, artifactID).Doc); got != "Replaced.\n" {
		t.Fatalf("the compacted document reads %q, want the joined write's", got)
	}
}

// A compaction takes every connection it uses from the compaction pool: none of the shared or the
// document-rooms pool is in use while it is inside its fold, and neither pool hands out a
// connection across the whole compaction, its write included.
func TestACompactionHoldsNoSharedOrRoomsConnection(t *testing.T) {
	database, versioned, artifactID := newHeldCompactionDocument(t)
	compactionPool, err := database.Pool.Compaction()
	if err != nil {
		t.Fatalf("open the compaction pool: %v", err)
	}
	before, compactionAcquires := database.Pool.Use(), compactionPool.Stat().AcquireCount()
	var during store.ConnectionUse
	duringCompactionFold(versioned, artifactID, func() { during = database.Pool.Use() })
	if result := compactUnderTail(t, versioned, artifactID); result.outcome != compactCompacted {
		t.Fatalf("compaction = %+v, want it compacted", result)
	}
	after := database.Pool.Use()
	if during.InUse != 0 {
		t.Fatalf("%d shared or rooms connections in use inside the compaction's fold, want none", during.InUse)
	}
	if after.InUse != 0 || after.Returned != before.Returned {
		t.Fatalf("shared and rooms pools went from %+v to %+v across the compaction, want no connection handed out", before, after)
	}
	if taken := compactionPool.Stat().AcquireCount() - compactionAcquires; taken != 2 {
		t.Fatalf("the compaction took %d compaction-pool connections, want its read's and its write's", taken)
	}
}

// newHeldCompactionDocument is a document of five rows, every one but the newest older than the
// tail, on a PgVersioned no service shares.
func newHeldCompactionDocument(t *testing.T) (*store.Store, *PgVersioned, string) {
	t.Helper()
	database := storetest.Open(t)
	versioned := NewPgVersioned(database)
	artifactID := createDocument(t, database, "one")
	writer := newProofHistory(t, 101)
	appendUpdates(t, versioned, artifactID,
		writer.write("one\n"), writer.write("one\n\ntwo\n"), writer.write("three\n"),
		writer.write("three\n\nfour\n"), writer.write("five\n"))
	ageUpdatesThrough(t, database, artifactID, 4, time.Hour)
	return database, versioned, artifactID
}

// writeRoomRows writes the document through its room - a seed and two browser edits - and returns
// the version of the row before the newest, the fold point once the rows through it are old.
func writeRoomRows(t *testing.T, service *Service, artifactID string) int64 {
	t.Helper()
	seedServiceText(t, service, artifactID, "First.\n")
	editLiveTree(t, service, artifactID, appendBlocks(t, "Second.\n"))
	editLiveTree(t, service, artifactID, appendBlocks(t, "Third.\n"))
	waitForLandedAppends(t, service, artifactID)
	rows := storedRows(t, service.store, artifactID)
	if len(rows) < 3 {
		t.Fatalf("the room stored %d rows, want at least the seed and two edits", len(rows))
	}
	return rows[len(rows)-2].version
}

// duringCompactionFold runs interfere the first time a compaction of artifactID on versioned has
// read and folded its rows and not yet taken the lock to write them back.
func duringCompactionFold(versioned *PgVersioned, artifactID string, interfere func()) {
	var once sync.Once
	versioned.duringCompactionFold = func(room string) {
		if room == artifactID {
			once.Do(interfere)
		}
	}
}

// everyVersion is the ceiling ageUpdatesThrough ages every row of a document under.
const everyVersion int64 = math.MaxInt64

// ageUpdatesThrough makes the document's rows through version created age before the database's
// clock: time.Hour is older than both compactTail and compactMaxTailAge.
func ageUpdatesThrough(t *testing.T, database *store.Store, artifactID string, through int64, age time.Duration) {
	t.Helper()
	if _, err := database.Pool.Exec(context.Background(), `
		update doc_updates set created_at = now() - make_interval(secs => $3) where artifact_id = $1 and version <= $2
	`, artifactID, through, age.Seconds()); err != nil {
		t.Fatalf("age document updates: %v", err)
	}
}

// compactUnderTail compacts the document under the tail rule, waiting for its lock.
func compactUnderTail(t *testing.T, versioned *PgVersioned, artifactID string) compactResult {
	t.Helper()
	result, err := versioned.compactRoom(context.Background(), artifactID, tailRule, true)
	if err != nil {
		t.Fatalf("compact the document: %v", err)
	}
	return result
}

// compactAged makes every row of the document old and compacts it under the tail rule, which folds
// the whole log into its head row, as a room's close does once the document has rested.
func compactAged(t *testing.T, versioned *PgVersioned, database *store.Store, artifactID string) compactResult {
	t.Helper()
	ageUpdatesThrough(t, database, artifactID, everyVersion, time.Hour)
	return compactUnderTail(t, versioned, artifactID)
}

// updateRow is one doc_updates row as stored: its bytes and the transaction that wrote it.
type updateRow struct {
	version        int64
	xmin           string
	update         []byte
	contentChanged bool
}

func storedRows(t *testing.T, database *store.Store, artifactID string) []updateRow {
	t.Helper()
	rows, err := database.Pool.Query(context.Background(), `
		select version, xmin::text, update, content_changed from doc_updates where artifact_id = $1 order by version
	`, artifactID)
	if err != nil {
		t.Fatalf("read document rows: %v", err)
	}
	defer rows.Close()
	var stored []updateRow
	for rows.Next() {
		var row updateRow
		if err := rows.Scan(&row.version, &row.xmin, &row.update, &row.contentChanged); err != nil {
			t.Fatalf("scan document row: %v", err)
		}
		stored = append(stored, row)
	}
	if err := rows.Err(); err != nil {
		t.Fatalf("read document rows: %v", err)
	}
	return stored
}

// requireRowsAsStored fails unless the document's rows are want, byte for byte and each under the
// transaction that wrote it.
func requireRowsAsStored(t *testing.T, database *store.Store, artifactID string, want []updateRow) {
	t.Helper()
	got := storedRows(t, database, artifactID)
	if len(got) != len(want) {
		t.Fatalf("the document holds %d rows, want the %d stored", len(got), len(want))
	}
	for index := range want {
		if !sameRow(got[index], want[index]) {
			t.Fatalf("row %d is version %d under %s, want version %d under %s as stored", index, got[index].version, got[index].xmin, want[index].version, want[index].xmin)
		}
	}
}

// requireCompactedFrom fails unless the document's rows are before compacted at foldPoint: no row
// below it, the fold point's row claiming content when a row it folded past cursor did, and every
// newer row as it was, byte for byte and under the transaction that wrote it. It returns the rows.
func requireCompactedFrom(t *testing.T, database *store.Store, artifactID string, before []updateRow, foldPoint, cursor int64) []updateRow {
	t.Helper()
	contentChanged := false
	var newer []updateRow
	for _, row := range before {
		if row.version <= foldPoint {
			contentChanged = contentChanged || (row.version > cursor && row.contentChanged)
		} else {
			newer = append(newer, row)
		}
	}
	got := storedRows(t, database, artifactID)
	if len(got) != 1+len(newer) || got[0].version != foldPoint {
		t.Fatalf("the document holds rows %v, want the fold point %d and the %d rows above it", rowVersions(got), foldPoint, len(newer))
	}
	if got[0].contentChanged != contentChanged {
		t.Fatalf("the folded row's content_changed = %v, want %v for cursor %d", got[0].contentChanged, contentChanged, cursor)
	}
	for index, row := range newer {
		if !sameRow(got[index+1], row) {
			t.Fatalf("row %d was rewritten: version %d under %s, want version %d under %s as written", row.version, got[index+1].version, got[index+1].xmin, row.version, row.xmin)
		}
	}
	return got
}

func sameRow(a, b updateRow) bool {
	return a.version == b.version && a.xmin == b.xmin && a.contentChanged == b.contentChanged && bytes.Equal(a.update, b.update)
}

func rowVersions(rows []updateRow) []int64 {
	versions := make([]int64, 0, len(rows))
	for _, row := range rows {
		versions = append(versions, row.version)
	}
	return versions
}

// storedReading is what the document's stored state reads as: its markdown and its whole tree with
// block ids, marks and margin records.
func storedReading(t *testing.T, versioned *PgVersioned, artifactID string) string {
	t.Helper()
	loaded := loadDocument(t, versioned, artifactID)
	if loaded.Doc == nil {
		return ""
	}
	return docMarkdown(loaded.Doc) + "\n" + rawReading(t, loaded.Doc)
}

// settlementCursor is the latest version's doc_update_version, 0 for none.
func settlementCursor(t *testing.T, database *store.Store, artifactID string) int64 {
	t.Helper()
	var cursor int64
	if err := database.Pool.QueryRow(context.Background(), `
		select coalesce((
			select doc_update_version from artifact_versions where artifact_id = $1 order by number desc limit 1
		), 0)
	`, artifactID).Scan(&cursor); err != nil {
		t.Fatalf("read the settlement cursor: %v", err)
	}
	return cursor
}

// setSettlementCursor stands the latest version's cursor at version, as a settlement through it
// leaves it.
func setSettlementCursor(t *testing.T, database *store.Store, artifactID string, version int64) {
	t.Helper()
	if _, err := database.Pool.Exec(context.Background(), `
		update artifact_versions set doc_update_version = $2
		where artifact_id = $1 and number = (select max(number) from artifact_versions where artifact_id = $1)
	`, artifactID, version); err != nil {
		t.Fatalf("set the settlement cursor: %v", err)
	}
}
