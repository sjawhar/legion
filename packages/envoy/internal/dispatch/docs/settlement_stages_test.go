package docs

import (
	"context"
	"errors"
	"testing"
	"time"

	"github.com/jackc/pgx/v5"

	"github.com/sjawhar/envoy/internal/dispatch/pmdoc"
	"github.com/sjawhar/envoy/internal/dispatch/store"
)

// A settlement's stages are tested one at a time, each over a live service and its PostgreSQL:
// a stage reads the room's live instance and writes through a transaction holding the document's
// owner row, so none of them can be called without the stages before it having run
// (settleRoomWithin). Each test here runs the stages before the one it tests exactly as
// settleRoomWithin runs them, so the sequence itself is under test too:
// TestSettlementStagesSettleTheDocumentInOrder fails when two of them are swapped.

// stageSettlement is the settlement settleRoomWithin builds for the room's current generation,
// before any stage has run: the room's state, the generation the room stands at, and how the
// settlement reports a stop short.
func stageSettlement(t *testing.T, service *Service, artifactID string) *settlement {
	t.Helper()
	state := service.room(artifactID)
	state.mu.Lock()
	generation := state.roomGeneration
	state.mu.Unlock()
	settle := &settlement{service: service, room: artifactID, state: state, generation: generation}
	settle.stoppedShort = func(err error) settleOutcome {
		return settleOutcome{next: settleRetry, err: err, generation: settle.generation}
	}
	return settle
}

// openStageTransaction opens what settleRoomWithin opens between take and readLocked: the
// settlement's ledger and the cursor of the instance it holds, a transaction holding the
// document's owner row, and the context the stages after take read and write through. The
// transaction is rolled back when the test ends, and what the settlement wrote into the room is
// discarded with it, as abandon discards it, so a settlement left half-run holds neither the
// document's advisory lock nor the room's persistence worker. A settlement whose own stage
// committed the transaction finished its writes itself.
func openStageTransaction(t *testing.T, settle *settlement, parent context.Context) context.Context {
	t.Helper()
	service, artifactID := settle.service, settle.room
	ledger := &Ledger{service: service, ownTx: true}
	settle.cursor = service.cursorOf(settle.doc)
	ctx := withRoomCursor(store.WithTransactionTracking(withLedger(parent, ledger)), settle.cursor)
	tx, err := service.store.Pool.Begin(ctx)
	if err != nil {
		t.Fatalf("begin the settlement's transaction: %v", err)
	}
	t.Cleanup(func() {
		if err := tx.Rollback(context.Background()); errors.Is(err, pgx.ErrTxClosed) {
			return
		}
		service.discardSuppressedPersistence(artifactID, settle.slots...)
	})
	settle.ledger, settle.tx = ledger, tx
	owner, open, err := lockArtifactOwner(ctx, tx, artifactID)
	if err != nil {
		t.Fatalf("lock the document's owner row: %v", err)
	}
	if !open {
		t.Fatal("the document's owner is closed")
	}
	settle.owner = owner
	return withOwnerVerified(ctx)
}

// requireStageGoesOn requires the stage named to have left the settlement going on, rather than
// ending it with an outcome of its own.
func requireStageGoesOn(t *testing.T, stage string, step settleStep, outcome settleOutcome) {
	t.Helper()
	if step != settleOn {
		t.Fatalf("%s ended the settlement with outcome %+v", stage, outcome)
	}
}

// seedAnsweredAskBlock seeds a document holding one ask block, settles it so the block is
// indexed, and answers that ask in its row alone, so the block disagrees with its row until a
// settlement repairs it. It returns the markdown the repaired document reads as.
func seedAnsweredAskBlock(t *testing.T, service *Service, artifactID string) string {
	t.Helper()
	seedServiceText(t, service, artifactID, ":::ask{#ask-block urgency=\"med\" multiple=\"false\" state=\"open\"}\nShip it?\n:::\n\nContext.\n")
	service.settleRoom(artifactID, 0)
	answer := answerBlockAsk(t, service, artifactID)
	return ":::ask{#ask-block urgency=\"med\" multiple=\"false\" state=\"answered\" answered_by=\"alice\" answered_at=\"" +
		answer.At.Format(time.RFC3339Nano) + "\" selected=\"[]\"}\nShip it?\n:::\n\nContext.\n"
}

// take holds the room's live instance for the whole settlement and reads the generation that
// instance stands at, which is the generation every later stage compares the room against.
func TestSettlementTakeHoldsTheRoomsLiveInstance(t *testing.T) {
	service, artifactID := newTestService(t)
	service.settle = time.Hour
	seedServiceText(t, service, artifactID, "Context.\n")
	settleCurrentGeneration(t, service, artifactID)

	settle := stageSettlement(t, service, artifactID)
	step, outcome := settle.take(context.Background(), false)
	requireStageGoesOn(t, "take", step, outcome)
	if settle.doc == nil || settle.doc != service.srv.GetDoc(artifactID) {
		t.Fatalf("take held %p, want the room's live instance %p", settle.doc, service.srv.GetDoc(artifactID))
	}
	state := service.room(artifactID)
	state.mu.Lock()
	generation := state.roomGeneration
	state.mu.Unlock()
	if settle.generation != generation {
		t.Fatalf("take stands at generation %d, want the room's %d", settle.generation, generation)
	}
}

// readLocked reads the tree the settlement reconciles, at an exact commit position, with the
// rendering its version reuses, the latest version it compares that rendering against, and whom
// that version would credit.
func TestSettlementReadLockedReadsTheTreeTheSettlementReconciles(t *testing.T) {
	service, artifactID := newTestService(t)
	service.settle = time.Hour
	seedServiceText(t, service, artifactID, "Context.\n")
	settleCurrentGeneration(t, service, artifactID)

	settle := stageSettlement(t, service, artifactID)
	parent := context.Background()
	step, outcome := settle.take(parent, false)
	requireStageGoesOn(t, "take", step, outcome)
	ctx := openStageTransaction(t, settle, parent)
	step, outcome = settle.readLocked(ctx, parent)
	requireStageGoesOn(t, "readLocked", step, outcome)

	if settle.read == nil || settle.read != settle.first.tree {
		t.Fatalf("readLocked read %p, want the tree of its read at an exact commit position %p", settle.read, settle.first.tree)
	}
	if settle.markdown != "Context.\n" || settle.renderErr != nil {
		t.Fatalf("readLocked rendered %q (%v), want the seeded text", settle.markdown, settle.renderErr)
	}
	if settle.latest.markdown != "Context.\n" {
		t.Fatalf("readLocked read the latest version as %q, want the settled text", settle.latest.markdown)
	}
	if settle.credit.state != settle.state {
		t.Fatalf("readLocked credited room state %p, want the settlement's %p", settle.credit.state, settle.state)
	}
}

// applyStamp stamps into the room the block ids the document lacks, holding the stamp's update
// from the room's persistence until the settlement stores it.
func TestSettlementApplyStampStampsTheBlockIdsTheDocumentLacks(t *testing.T) {
	service, artifactID := newTestService(t)
	service.settle = time.Hour
	owedStamp(t, service, artifactID)

	settle := stageSettlement(t, service, artifactID)
	parent := context.Background()
	step, outcome := settle.take(parent, false)
	requireStageGoesOn(t, "take", step, outcome)
	ctx := openStageTransaction(t, settle, parent)
	step, outcome = settle.readLocked(ctx, parent)
	requireStageGoesOn(t, "readLocked", step, outcome)
	if repairs := pmdoc.BlockIDRepairCount(settle.read); repairs != 1 {
		t.Fatalf("the document the settlement read needs %d block ids, want the one the test left it", repairs)
	}
	step, outcome = settle.applyStamp(ctx)
	// settleOn and not settleStampOnly: nothing moved this room, so the settlement goes on to
	// its repairs rather than ending on the stamp it just wrote.
	requireStageGoesOn(t, "applyStamp", step, outcome)

	if settle.stamp == nil || len(settle.slots) != 1 {
		t.Fatalf("applyStamp wrote %d updates into the room with stamp %v, want the one stamp", len(settle.slots), settle.stamp != nil)
	}
	if repairs := pmdoc.BlockIDRepairCount(settle.reconciled); repairs != 0 {
		t.Fatalf("the stamped tree still needs %d block ids", repairs)
	}
}

// commitStamp commits the stamp of a settlement the document moved under: the stamp is in the
// room and its browsers, so it is stored and committed on its own, and the settlement writes no
// version.
func TestSettlementCommitStampCommitsTheStampAlone(t *testing.T) {
	service, artifactID := newTestService(t)
	service.settle = time.Hour
	owedStamp(t, service, artifactID)
	versions := latestVersionNumber(t, service, artifactID)

	settle := stageSettlement(t, service, artifactID)
	parent := context.Background()
	step, outcome := settle.take(parent, false)
	requireStageGoesOn(t, "take", step, outcome)
	ctx := openStageTransaction(t, settle, parent)
	step, outcome = settle.readLocked(ctx, parent)
	requireStageGoesOn(t, "readLocked", step, outcome)
	// The document moves under the settlement's read, as a live write's slot moves it
	// (openLiveWrite): what moved it owns the settlement from here, and the stamp already in the
	// room is all this one commits.
	state := service.room(artifactID)
	state.mu.Lock()
	state.roomGeneration = service.nextRoomGeneration()
	state.mu.Unlock()
	step, outcome = settle.applyStamp(ctx)
	if step != settleStampOnly || outcome != (settleOutcome{}) {
		t.Fatalf("applyStamp ended at step %d with outcome %+v, want the stamp alone left to commit", step, outcome)
	}

	step, outcome = settle.commitStamp(ctx)
	if step != settleDone || outcome != (settleOutcome{}) {
		t.Fatalf("commitStamp ended at step %d with outcome %+v, want the settlement ended", step, outcome)
	}
	requireNoSuppressedSlots(t, service, artifactID, "after the stamp committed alone")
	if repairs := pmdoc.BlockIDRepairCount(persistedProofTree(t, service.store, artifactID)); repairs != 0 {
		t.Fatalf("the stored document still needs %d block ids after its stamp committed", repairs)
	}
	if number := latestVersionNumber(t, service, artifactID); number != versions {
		t.Fatalf("the stamp that committed alone wrote version %d, want the document left at %d", number, versions)
	}
}

// planRepairs reconciles the document's ask blocks against their rows, which says what the
// document owes each block.
func TestSettlementPlanRepairsReconcilesTheAnsweredAskBlock(t *testing.T) {
	service, artifactID := newTestService(t)
	service.settle = time.Hour
	seedAnsweredAskBlock(t, service, artifactID)

	settle := stageSettlement(t, service, artifactID)
	parent := context.Background()
	step, outcome := settle.take(parent, false)
	requireStageGoesOn(t, "take", step, outcome)
	ctx := openStageTransaction(t, settle, parent)
	step, outcome = settle.readLocked(ctx, parent)
	requireStageGoesOn(t, "readLocked", step, outcome)
	step, outcome = settle.applyStamp(ctx)
	requireStageGoesOn(t, "applyStamp", step, outcome)
	step, outcome = settle.planRepairs(ctx)
	requireStageGoesOn(t, "planRepairs", step, outcome)

	if len(settle.reconciliation.repairs) != 1 {
		t.Fatalf("planRepairs planned %d repairs, want the answered ask block's one", len(settle.reconciliation.repairs))
	}
	if blockID := settle.reconciliation.repairs[0].blockID; blockID != "ask-block" {
		t.Fatalf("planRepairs planned a repair of block %q, want the document's ask block", blockID)
	}
}

// commitRepairs writes the planned repairs into the room and reads the document again: the
// version is rendered from that later read, which holds the repairs, and the head it records is
// read once every update through it is stored.
func TestSettlementCommitRepairsWritesTheRepairIntoTheRoom(t *testing.T) {
	service, artifactID := newTestService(t)
	service.settle = time.Hour
	seedAnsweredAskBlock(t, service, artifactID)

	settle := stageSettlement(t, service, artifactID)
	parent := context.Background()
	step, outcome := settle.take(parent, false)
	requireStageGoesOn(t, "take", step, outcome)
	ctx := openStageTransaction(t, settle, parent)
	step, outcome = settle.readLocked(ctx, parent)
	requireStageGoesOn(t, "readLocked", step, outcome)
	step, outcome = settle.applyStamp(ctx)
	requireStageGoesOn(t, "applyStamp", step, outcome)
	step, outcome = settle.planRepairs(ctx)
	requireStageGoesOn(t, "planRepairs", step, outcome)
	step, outcome = settle.commitRepairs(ctx)
	requireStageGoesOn(t, "commitRepairs", step, outcome)

	if len(settle.slots) != 1 {
		t.Fatalf("commitRepairs wrote %d updates into the room, want the one repair", len(settle.slots))
	}
	if settle.versioned == nil || settle.versioned == settle.read {
		t.Fatal("commitRepairs left the version to be rendered from the tree read before the repairs")
	}
	if settle.snapshotCursor <= 0 {
		t.Fatalf("commitRepairs recorded head %d for the version, want the document's", settle.snapshotCursor)
	}
}

// The stages settle a document in the order settleRoomWithin runs them, and the last of them,
// commitVersion, is the stage this test calls directly: the repair the reconciliation plans
// reaches the room, the store and the document's version. Swap two of the stages below and the
// test fails - with the repair planned after the stage that writes it, the settlement writes
// nothing into the room and versions the document unrepaired.
func TestSettlementStagesSettleTheDocumentInOrder(t *testing.T) {
	service, artifactID := newTestService(t)
	service.settle = time.Hour
	want := seedAnsweredAskBlock(t, service, artifactID)

	settle := stageSettlement(t, service, artifactID)
	parent := context.Background()
	step, outcome := settle.take(parent, false)
	requireStageGoesOn(t, "take", step, outcome)
	ctx := openStageTransaction(t, settle, parent)
	step, outcome = settle.readLocked(ctx, parent)
	requireStageGoesOn(t, "readLocked", step, outcome)
	step, outcome = settle.applyStamp(ctx)
	requireStageGoesOn(t, "applyStamp", step, outcome)
	step, outcome = settle.planRepairs(ctx)
	requireStageGoesOn(t, "planRepairs", step, outcome)
	// What each of the two repair stages leaves, asserted between them rather than after both,
	// because that is what holds their order: planRepairs reconciles and writes nothing, and
	// commitRepairs is what puts the repair it planned into the room. Asserted only at the end,
	// the pair passes in either order - run the other way round, commitRepairs finds no
	// reconciliation and writes nothing, and planRepairs then fills the field it no longer reads.
	if len(settle.reconciliation.repairs) != 1 {
		t.Fatalf("planRepairs reconciled %d repairs, want the one the answered ask block needs", len(settle.reconciliation.repairs))
	}
	if len(settle.slots) != 0 {
		t.Fatalf("planRepairs wrote %d updates into the room, want none: commitRepairs writes them", len(settle.slots))
	}
	step, outcome = settle.commitRepairs(ctx)
	requireStageGoesOn(t, "commitRepairs", step, outcome)
	if len(settle.slots) != 1 {
		t.Fatalf("the settlement wrote %d updates into the room, want the one repair its reconciliation planned", len(settle.slots))
	}
	step, outcome = settle.commitVersion(ctx)
	if step != settleDone || outcome != (settleOutcome{}) {
		t.Fatalf("commitVersion ended at step %d with outcome %+v, want the settlement ended", step, outcome)
	}

	requireNoSuppressedSlots(t, service, artifactID, "after the settlement's stages ran")
	waitForDocumentText(t, service, artifactID, want)
	waitForPersistedProofText(t, service.store, artifactID, want)
	requireLatestVersionMarkdown(t, service, artifactID, want)
}
