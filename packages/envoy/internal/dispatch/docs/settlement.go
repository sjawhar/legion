package docs

import (
	"context"
	"errors"
	"fmt"
	"log/slog"

	"github.com/jackc/pgx/v5"
	"github.com/reearth/ygo/crdt"
	"github.com/reearth/ygo/provider/websocket"

	"github.com/sjawhar/envoy/internal/dispatch/model"
	"github.com/sjawhar/envoy/internal/dispatch/pmdoc"
	"github.com/sjawhar/envoy/internal/dispatch/refs"
	"github.com/sjawhar/envoy/internal/dispatch/store"
)

// settleStep is what a settlement's stage leaves for the stage after it (settleRoomWithin).
type settleStep int

const (
	// settleDone ends the settlement, with the outcome the stage reports beside it: the stage
	// committed the document's version, stopped short, or left the document to whatever owns its
	// settlement now.
	settleDone settleStep = iota
	// settleOn is no ending: the stage left the settlement going on, and the outcome beside it is
	// nothing. Only settleRoomWithin reads it; a settlement never ends on it.
	settleOn
	// settleStampOnly is the one stage ending that names the stage after it: the document moved
	// under the settlement's read, so the block-id stamp applyStamp wrote into the room is all
	// this settlement commits, and settleRoomWithin goes on to commitStamp and to no stage after
	// it. Only applyStamp answers it, and the outcome beside it is nothing.
	settleStampOnly
)

// settlement is one document's settlement, as its stages run it (settleRoomWithin): the room it
// settles, that room's state and the live instance it holds for the whole settlement, the room
// generation it stands at, the transaction and ledger its stages write through, what they read of
// the document and whom its version credits, what they wrote into the room, and how the settlement
// reports a stop short. The stages hand each other everything here; what one stage alone uses
// stays a local of that stage.
type settlement struct {
	service    *Service
	room       string
	state      *roomState
	doc        *crdt.Doc
	generation uint64
	// stoppedShort is the outcome of a settlement that stopped short with err having written
	// nothing into the room (settleRoomWithin).
	stoppedShort func(error) settleOutcome
	// cursor is the cursor of the instance this settlement holds (doc), which gates the room
	// updates it stores and its repairs, and takes them in once they are stored
	// (commitSettlement).
	cursor *roomCursor
	ledger *Ledger
	tx     pgx.Tx
	owner  artifactOwner
	latest struct {
		model.Version
		markdown         string
		docUpdateVersion int64
	}
	// first is the settlement's read of the room before its repairs, read at an exact commit
	// position; read is that read's tree, and markdown, offsets and renderErr are what it renders
	// as, which the version reuses when it is rendered from this same tree unchanged
	// (commitVersion).
	first     settlementRead
	read      *pmdoc.Node
	markdown  string
	offsets   []pmdoc.BlockOffset
	renderErr error
	credit    settlementVersionCredit
	// slots holds a suppression slot for each update this settlement writes into the room, in the
	// order the room's persistence worker is handed them, and updates holds those updates. Every
	// path after a write finishes or discards all of them. stamp is the block-id stamp this
	// settlement wrote into the room, nil for none; it is stored with the settlement's other
	// appends, after the reconciliation (storeRepair).
	slots   []*suppressSlot
	updates [][]byte
	stamp   []byte
	// renamed are the ask blocks the stamp renamed, and carried is whom each of them carried as
	// the room's state held it before the stamp (recordStampedAskBlocks). Both are recorded
	// durably in the transaction that stores the stamp, whichever way it commits
	// (recordStampedAskAuthors).
	renamed []stampedAsk
	carried map[string]model.Actor
	// reconciled is the tree the ask blocks are reconciled on, the stamp's when it stamped ids;
	// versioned is the one the version is rendered from, read again after the repairs when the
	// settlement wrote any.
	reconciled     *pmdoc.Node
	versioned      *pmdoc.Node
	reconciliation settlementReconciliation
	// snapshotCursor is the head the version records (doc_update_version), through which every one
	// of the room's updates it holds is stored.
	snapshotCursor int64
	// published are the events this settlement appended, published once its transaction commits.
	published []model.Event
}

// settleRoomWithin settles room under parent and reports how the settlement ended (settleOutcome),
// leaving what follows to its caller. A timer's settlement runs under no deadline; the one
// Shutdown runs gets its drain budget. When parent ends first the settlement's database work is
// cancelled and its transaction rolls back, and nothing retries it or counts it as a failure: the
// document's pending-settlement row leaves it to resume (onLoadDocument, RunSettlementResumption).
// So does a settlement that has to write into a room Shutdown has begun closing, which refuses the
// write (allowInject).
//
// Once Shutdown has begun closing room, a settlement loads no room of it - a room Shutdown closed
// stays closed, its settlement left to resume - unless mayLoad says this is Shutdown's own
// settlement of a document whose settlement this process had armed: the timer Shutdown stopped
// would have loaded the room, and a cold write (publishLiveWrite) arms one with no room resident.
//
// The settlement itself does only what no caller can do after it: a settlement that wrote into
// the room and cannot finish fails the room, whose reload arms the settlement again, and one whose
// room holds a replaced stored history closes the room for a reload (reloadReplacedRoom). The
// retries, the re-arming and the shutdown's log lines are its caller's: the timer's settlement and
// Shutdown's do them (followUpSettlement), and a caller that runs it synchronously and answers for
// its outcome itself reports the reason instead (settleOwedNow).
//
// The settlement runs in stages, each of which answers settleOn where the settlement goes on and
// the step and outcome it ends on otherwise: it takes the room's live instance (take), reads the
// document under the document's locks (readLocked), stamps the block ids the document lacks
// (applyStamp) and commits that stamp alone when the document moved under its read (commitStamp),
// reconciles the ask blocks (planRepairs), writes the repairs into the room and stores what it
// wrote (commitRepairs), and commits, with the document's version when the document reads
// differently from the latest one (commitVersion).
func (s *Service) settleRoomWithin(parent context.Context, room string, generation uint64, mayLoad bool) settleOutcome {
	settle := &settlement{service: s, room: room, generation: generation}
	// stoppedShort is the outcome of a settlement that stopped short with err having written
	// nothing into the room: a retry, unless parent has ended or Shutdown refused the write. The
	// generation it counts that retry's failure against is the one the settlement stands at now,
	// which its take reads again once the room is warm.
	settle.stoppedShort = func(err error) settleOutcome {
		next := settleRetry
		switch {
		case parent.Err() != nil:
			next = settleOutOfBudget
		case s.shuttingDown(room) && errors.Is(err, ErrServiceUnavailable):
			next = settleRefusedInShutdown
		}
		return settleOutcome{next: next, err: err, generation: settle.generation}
	}
	if s.beforeSettleStart != nil {
		s.beforeSettleStart(room)
	}
	state := s.lockState(room)
	if s.stopping.Load() || state.closed || state.failed != nil || state.roomGeneration != generation {
		s.unlockState(room, state)
		return settleOutcome{}
	}
	state.settling++
	s.unlockState(room, state)
	defer func() {
		state.mu.Lock()
		state.settling--
		s.unlockState(room, state)
	}()
	settle.state = state
	if step, outcome := settle.take(parent, mayLoad); step != settleOn {
		return outcome
	}
	ledger := &Ledger{service: s, ownTx: true}
	settle.cursor = s.cursorOf(settle.doc)
	ctx := withRoomCursor(store.WithTransactionTracking(withLedger(parent, ledger)), settle.cursor)
	tx, err := s.store.Pool.Begin(ctx)
	if err != nil {
		return settle.stoppedShort(fmt.Errorf("begin document transaction: %w", err))
	}
	defer tx.Rollback(ctx)
	settle.ledger, settle.tx = ledger, tx
	owner, open, err := lockArtifactOwner(ctx, tx, room)
	if err != nil {
		return settle.stoppedShort(err)
	}
	if !open {
		return settleOutcome{}
	}
	settle.owner = owner
	// The owner row is read and locked in this transaction, so the repairs settlement injects
	// below do not read it again: that read would be a second pooled connection taken while
	// this transaction holds one, which is what deadlocks the pool.
	ctx = withOwnerVerified(ctx)
	if step, outcome := settle.readLocked(ctx, parent); step != settleOn {
		return outcome
	}
	switch step, outcome := settle.applyStamp(ctx); step {
	case settleStampOnly:
		// The document moved under the settlement's read, so the stamp in the room is all it
		// commits. Committing it here rather than below is what keeps the copy lock
		// reconcileAskBlocks takes out of the transaction that holds the stamp's pending row.
		_, stamped := settle.commitStamp(ctx)
		return stamped
	case settleDone:
		return outcome
	}
	if step, outcome := settle.planRepairs(ctx); step != settleOn {
		return outcome
	}
	if step, outcome := settle.commitRepairs(ctx); step != settleOn {
		return outcome
	}
	_, outcome := settle.commitVersion(ctx)
	return outcome
}

// take is what the settlement takes of its room before it opens a transaction: the room warmed
// where no browser holds it, the live instance it settles, and the generation that instance stands
// at. mayLoad is its caller's answer for a document Shutdown has begun closing: a room of one
// stays closed unless this is Shutdown's own settlement of a document whose settlement this
// process had armed (settleRoomWithin).
func (st *settlement) take(parent context.Context, mayLoad bool) (settleStep, settleOutcome) {
	s, room, state := st.service, st.room, st.state
	shuttingDown := s.shuttingDown(room)
	if shuttingDown && !mayLoad && s.srv.GetDoc(room) == nil {
		return settleDone, settleOutcome{}
	}
	if s.srv.GetDoc(room) == nil {
		state.mu.Lock()
		state.settleWarming = true
		s.unlockState(room, state)
		warmCtx := parent
		if shuttingDown {
			warmCtx = withShutdownLoad(parent)
		}
		err := s.srv.Apply(warmCtx, room, func(_ *crdt.Doc, _ func(func(*crdt.Transaction))) {})
		state.mu.Lock()
		state.settleWarming = false
		s.unlockState(room, state)
		if err != nil && !errors.Is(err, websocket.ErrNoChanges) {
			return settleDone, st.stoppedShort(fmt.Errorf("warm document for settlement: %w", err))
		}
	}
	// Hold the live document for the whole settlement. A close that lands after the generation
	// check in settleRoomWithin (a failure's eviction, a close of a room whose stored history was
	// replaced) retires the room and moves the generation; reading the room again later would
	// return nil and every later stage would dereference it. A nil here means exactly that close:
	// the next access reloads the durable document and schedules its own settlement, so this one
	// just ends.
	if s.afterSettleWarm != nil {
		s.afterSettleWarm(room)
	}
	doc := s.srv.GetDoc(room)
	if doc == nil {
		return settleDone, settleOutcome{}
	}
	st.doc = doc
	// The settlement waits for none of the room's updates to reach its persistence: it stores
	// every update of the room through the point it reads the room at itself, under the document's
	// advisory lock (readSettlementTree), and an update the room commits after that point is no
	// part of its version. A wait for the room's worker could only wait on that same lock.
	state.mu.Lock()
	st.generation = state.roomGeneration
	s.unlockState(room, state)
	return settleOn, settleOutcome{}
}

// readLocked is what the settlement reads once its transaction holds the document's owner row: the
// latest version, the document's cursor lock, the catch-up that brings the room's instance to the
// document's durable head, the tree it reconciles and the rendering of that tree, and whom its
// version credits.
//
// It is given both of the settlement's contexts: ctx carries this settlement's ledger, cursor and
// transaction, and is what every read here runs under; parent is the lifetime the reload of a room
// whose stored history was replaced is given, once this transaction has been rolled back.
func (st *settlement) readLocked(ctx context.Context, parent context.Context) (settleStep, settleOutcome) {
	s, room, state, tx := st.service, st.room, st.state, st.tx
	latest, err := latestVersion(ctx, tx, room)
	if err != nil {
		return settleDone, st.stoppedShort(err)
	}
	st.latest = latest
	if err := s.lockSettlementCursor(ctx, tx, room); err != nil {
		if s.shuttingDown(room) {
			slog.Warn("dispatch: skip shutdown document settlement while cursor lock is held", "room", room, "error", err)
			return settleDone, settleOutcome{}
		}
		return settleDone, st.stoppedShort(fmt.Errorf("lock document cursor: %w", err))
	}
	if s.afterSettleLock != nil {
		s.afterSettleLock(room)
	}
	head, err := currentUpdateCursor(ctx, tx, room)
	if err != nil {
		return settleDone, st.stoppedShort(err)
	}
	// The gate, before this settlement reads the document or writes anything. The room it holds
	// must stand at that head: with two envoy-dispatch processes on one Postgres, an update the
	// other process appended can be stored before this room's catch-up has taken it in, and
	// settling from it corrupts three things at once. Its version would record a
	// doc_update_version at or past that head over text that is missing the other process's
	// update, so contentChangedSinceVersion then reports nothing past that cursor and those words
	// are never versioned; clearSettlementPending would delete the one durable record that the
	// work is still owed; and reconcileAskBlocks would retract every open ask whose block this
	// stale tree cannot see.
	//
	// So a room behind the head is caught up here, under the document's advisory lock this
	// transaction holds, reading through that transaction (catchUpRoom): no append can land
	// meanwhile, so the catch-up always brings the room to that head, and the settlement
	// proceeds. The rows it applies are another process's content change, which re-arms the
	// room's settlement when one is armed or running (rearmSettle) - this one is - and that moves
	// the room's generation, so this one writes no version (the supersession check in applyStamp)
	// and the one it re-armed versions every update, this process's and the other's. Shutdown's
	// settlement re-arms nothing and versions the caught-up room itself, since nothing is left for
	// the next process. A history replaced under the room is the one case catch-up cannot mend:
	// the transaction rolls back, which keeps doc_settlements_pending and releases the lock the
	// room's close needs, and the room is closed and reloaded (reloadReplacedRoom).
	//
	// The cursor is the one this settlement marked its context with, which its repairs' appends
	// are gated on (appendingCursor), so the two can never disagree.
	if st.cursor != nil && st.cursor.heldThrough() != head {
		outcome, err := s.catchUpRoom(ctx, tx, room, st.doc)
		switch {
		case err != nil:
			return settleDone, st.stoppedShort(err)
		case outcome == catchUpGone:
			// The room no longer holds the instance this settlement took: whatever replaced it
			// loads at the head, and its load arms the settlement the document owes.
			return settleDone, settleOutcome{}
		case outcome == catchUpReplaced:
			// The rollback releases the document's advisory lock, which the room's close needs: a
			// close flushes the room's persistence, whose appends take that lock.
			_ = tx.Rollback(ctx)
			refusal := fmt.Errorf("%w: room %s holds the document through update %d, and the stored history was replaced under it (head %d)",
				ErrRoomBehindHead, room, st.cursor.heldThrough(), head)
			if parent.Err() != nil {
				return settleDone, st.stoppedShort(refusal)
			}
			s.reloadReplacedRoom(parent, room, state, st.generation, st.doc, refusal)
			return settleDone, settleOutcome{next: settleReloaded}
		}
		if through := st.cursor.heldThrough(); through != head {
			return settleDone, st.stoppedShort(fmt.Errorf("%w: room %s holds the document through update %d after its catch-up, the stored document stands at %d",
				ErrRoomBehindHead, room, through, head))
		}
	}
	// The settlement reads the document three times, and each read has its own name: read is this
	// one, before the database work; reconciled is the tree the ask blocks are reconciled on, the
	// stamp's when it stamped ids; versioned is the one the version is rendered from, read again
	// after the repairs when the settlement wrote any. A browser's edit made in between is in a
	// later one and not an earlier one. read and versioned are each the room at an exact commit
	// position (readSettlementTree), every update through which this transaction stores before it
	// versions, and the stamp reads inside its own transaction: the room's peers and the service
	// can write it while a walk of the live tree, which takes no lock, reads it, and a torn read
	// would be versioned as the document. Each bounds the in-flight credits its authors take before
	// it reads, so a normal credit, which follows its room update, is for a change the read holds.
	// The one change a read can lack whose author it takes is a committed transaction's write,
	// credited at its commit and published after the read; the supersession check in applyStamp
	// then writes no version, since the write still holds its slot or its publish moved the
	// generation.
	first, err := s.readSettlementTree(state, st.doc, room, s.afterSettleAuthorsTake, false)
	if err != nil {
		if errors.Is(err, ErrDocSchema) {
			slog.Error("dispatch: settle document outside Proof schema", "room", room, "error", err)
			return settleDone, settleOutcome{}
		}
		return settleDone, st.stoppedShort(err)
	}
	st.first, st.read = first, first.tree
	if s.afterSettleTreeRead != nil {
		s.afterSettleTreeRead(room, st.read)
	}
	// Whom the ask blocks' reconciliation names, as the document's pending authors stand before
	// this settlement stores anything. It stores nothing until it has reconciled them: the copy
	// lock that reconciliation can take (copiedAskSources, owedCopiesOf) comes before any
	// pending-settlement row, which each append writes (markUpdateOwed), in every transaction
	// (lockProjectCopies). Its version's authors are taken again once it has stored the room's
	// updates it holds (settlementCredit).
	credit, err := settlementCredit(ctx, tx, state, room, first)
	if err != nil {
		return settleDone, st.stoppedShort(err)
	}
	st.credit = credit

	// What the document renders as read. The version reuses this rendering, offsets included,
	// when it is rendered from this same tree unchanged (commitVersion).
	st.markdown, st.offsets, st.renderErr = renderTreeWithOffsets(st.read)
	if s.afterSettleRead != nil {
		s.afterSettleRead(room)
	}
	return settleOn, settleOutcome{}
}

// keep records a suppression slot and the update it suppresses, in the order the room's
// persistence worker is handed them.
func (st *settlement) keep(slot *suppressSlot, update []byte) {
	if slot != nil {
		st.slots, st.updates = append(st.slots, slot), append(st.updates, update)
	}
}

// finishSlots hands the room's persistence every update this settlement wrote into the room, now
// that its transaction holds them.
func (st *settlement) finishSlots() {
	for index, slot := range st.slots {
		st.service.finishSuppressedPersistence(slot, st.updates[index])
	}
}

// abandon ends a settlement that cannot finish. What it wrote is in the room and its browsers
// and will never reach the store through this settlement, so its slots are discarded and the
// room fails, which reloads the document from the store and leaves its settlement to that load.
// A settlement that wrote nothing stops short as one that met any other fault does. Its
// transaction rolls back, so the authors it read stay where they were, it marked no in-flight
// credit, and the room updates it stored are left to the room's persistence.
func (st *settlement) abandon(err error) settleOutcome {
	if len(st.slots) > 0 {
		st.service.discardSuppressedPersistence(st.room, st.slots...)
		st.service.failRoom(st.room, err)
		return settleOutcome{next: settleRoomFailed, err: err}
	}
	return st.stoppedShort(err)
}

// commitFailed ends a settlement whose commit returned an error. Its outcome is unknown, and the
// room's cursor took in the versions it appended (commitSettlement), so a room it stored any
// update of fails too, and reloads the durable document whichever way the commit went.
func (st *settlement) commitFailed(advanced bool, err error) settleOutcome {
	if advanced && len(st.slots) == 0 {
		st.service.failRoom(st.room, err)
		return settleOutcome{next: settleRoomFailed, err: err}
	}
	return st.abandon(err)
}

// applyStamp stamps the block ids the settlement's read lacks into the room; a settlement whose
// read lacks none writes nothing here. It is also where the settlement meets a room gone or a
// document moved under its read, under the one hold of the room's state that records the ask
// blocks the stamp renamed. A settlement that holds a stamp in the room and meets a document
// that moved under its read ends here with settleStampOnly, which goes on to commitStamp and to
// nothing after it; any other goes on holding the ask blocks' sources read under that state.
func (st *settlement) applyStamp(ctx context.Context) (settleStep, settleOutcome) {
	s, room, state := st.service, st.room, st.state
	st.reconciled = st.read
	if pmdoc.BlockIDRepairCount(st.read) > 0 {
		slot, update, err := s.applySuppressed(ctx, room, st.doc, func(doc *crdt.Doc, origin any) error {
			var stampErr error
			st.reconciled, _, st.renamed, stampErr = stampBlockIDs(doc, origin)
			return stampErr
		})
		st.keep(slot, update)
		if err != nil {
			if len(st.slots) == 0 && errors.Is(err, ErrDocSchema) {
				slog.Error("dispatch: settle document outside Proof schema", "room", room, "error", err)
				return settleDone, settleOutcome{}
			}
			return settleDone, st.abandon(err)
		}
		if slot != nil {
			st.stamp = update
		}
	}

	state.mu.Lock()
	// The authors the stamp carried are recorded durably in this transaction, the one that stores
	// the stamp, whichever way it commits below (recordStampedAskAuthors).
	st.carried = state.recordStampedAskBlocks(st.renamed)
	if s.stopping.Load() || state.closed || state.failed != nil {
		s.unlockState(room, state)
		s.discardSuppressedPersistence(room, st.slots...)
		return settleDone, settleOutcome{}
	}
	// An open live write is not in the room yet, and since this settlement holds the owner row,
	// the write's transaction has already committed: the cursor this settlement versions against
	// includes its row. Write no version; finishLiveWrite arms a settlement once it is published.
	superseded := state.roomGeneration != st.generation
	if state.liveWriter != nil {
		state.settleDeferred = true
		superseded = true
	}
	if superseded {
		s.unlockState(room, state)
		if len(st.slots) == 0 {
			return settleDone, settleOutcome{}
		}
		return settleStampOnly, settleOutcome{}
	}
	// The ask blocks' sources are read now, after the tree they are reconciled on: an update in that
	// tree whose observer has already run has recorded who introduced its blocks, and one whose
	// observer has not left its blocks unseen, for the settlement that observer arms (askBlockSources).
	st.credit.askSources = state.askSources()
	state.mu.Unlock()
	// Recorded before reconcileAskBlocks reads the durable authors, so a block the stamp renamed
	// is found under its minted id there too.
	if err := recordStampedAskAuthors(ctx, st.tx, room, st.renamed, st.carried); err != nil {
		return settleDone, st.abandon(err)
	}
	return settleOn, settleOutcome{}
}

// commitStamp commits the block-id stamp of a settlement the document moved under
// (settleStampOnly): that stamp is in the room and its browsers, so it is stored and broadcast and
// its transaction committed with no version, and the settlement ends with it.
func (st *settlement) commitStamp(ctx context.Context) (settleStep, settleOutcome) {
	s, room, tx := st.service, st.room, st.tx
	if err := st.ledger.storeRepair(ctx, tx, room, st.cursor, st.stamp); err != nil {
		return settleDone, st.abandon(fmt.Errorf("store superseded document identity update: %w", err))
	}
	identityUpdate, err := mergeUpdates(st.updates)
	if err != nil {
		return settleDone, st.abandon(err)
	}
	if err := s.srv.BroadcastUpdate(ctx, room, identityUpdate); err != nil {
		return settleDone, st.abandon(fmt.Errorf("broadcast superseded document identity update: %w", err))
	}
	if err := recordStampedAskAuthors(ctx, tx, room, st.renamed, st.carried); err != nil {
		return settleDone, st.abandon(err)
	}
	if advanced, err := s.commitSettlement(ctx, tx, st.ledger, room, authorCapture{}, nil); err != nil {
		return settleDone, st.commitFailed(advanced, fmt.Errorf("commit superseded document identity update: %w", err))
	}
	st.finishSlots()
	return settleDone, settleOutcome{}
}

// planRepairs reconciles the document's ask blocks on the tree the settlement read, which says
// what each block's row and the document owe each other: the repairs to write into the room, the
// events to append, and the copies to arm.
func (st *settlement) planRepairs(ctx context.Context) (settleStep, settleOutcome) {
	s := st.service
	reconciliation, err := s.reconcileAskBlocks(ctx, st.tx, st.room, st.owner, st.reconciled, st.credit, st.markdown)
	if err != nil {
		return settleDone, st.abandon(err)
	}
	st.reconciliation = reconciliation
	if s.afterSettleReconcile != nil {
		s.afterSettleReconcile(st.room)
	}
	return settleOn, settleOutcome{}
}

// commitRepairs writes the reconciled repairs into the room and stores what the settlement holds,
// in the room's commit order, and reads the document again when it wrote any: that read is what
// its version is rendered from, and whom that version credits is taken over it.
func (st *settlement) commitRepairs(ctx context.Context) (settleStep, settleOutcome) {
	s, room, tx, ledger := st.service, st.room, st.tx, st.ledger
	// What the settlement stores, it stores from here on, in the room's commit order: the room's
	// updates through its read, then each repair it wrote into the room after the room's updates
	// before it (storeRepair), then, for a settlement that wrote any, the room's updates through the
	// read its version is rendered from.
	if err := ledger.flushThrough(ctx, tx, room, st.first.point); err != nil {
		return settleDone, st.abandon(fmt.Errorf("store the room's updates through the settlement's read: %w", err))
	}
	if st.stamp != nil {
		if err := ledger.storeRepair(ctx, tx, room, st.cursor, st.stamp); err != nil {
			return settleDone, st.abandon(fmt.Errorf("store document identity update: %w", err))
		}
	}
	if len(st.reconciliation.repairs) > 0 {
		// The repairs are made on the document as it stands (writeLive): the tree they were
		// reconciled on was read before settlement's database work.
		slot, update, err := s.applySuppressed(ctx, room, st.doc, st.reconciliation.writeLive)
		st.keep(slot, update)
		if err != nil {
			return settleDone, st.abandon(fmt.Errorf("write reconciled typed blocks: %w", err))
		}
		if slot != nil {
			if err := ledger.storeRepair(ctx, tx, room, st.cursor, update); err != nil {
				return settleDone, st.abandon(fmt.Errorf("store reconciled typed blocks: %w", err))
			}
		}
	}

	st.versioned = st.reconciled
	versionRead := st.first
	if len(st.slots) > 0 {
		update, err := mergeUpdates(st.updates)
		if err != nil {
			return settleDone, st.abandon(err)
		}
		if err := s.srv.BroadcastUpdate(ctx, room, update); err != nil {
			return settleDone, st.abandon(fmt.Errorf("broadcast document closure update: %w", err))
		}
		// The version is the document as it stands after the repairs, read at an exact commit
		// position with every update through it stored first, not the tree settlement reconciled
		// before them: a peer's edit made since is in the room, its browsers and, stored here, its
		// stored updates, and a version rendered from the earlier tree would leave it out. An edit
		// made after this read is in neither the version nor its authors.
		if versionRead, err = s.readSettlementTree(st.state, st.doc, room, s.afterSettleAuthorsTake, true); err != nil {
			return settleDone, st.abandon(err)
		}
		if err := ledger.flushThrough(ctx, tx, room, versionRead.point); err != nil {
			return settleDone, st.abandon(fmt.Errorf("store the room's updates through the settlement's version read: %w", err))
		}
		st.versioned = versionRead.tree
	}
	// The version's authors, taken once the room's updates it holds are stored, and the head it
	// records (doc_update_version), through which every one of them is stored.
	credit, err := settlementCredit(ctx, tx, st.state, room, versionRead)
	if err != nil {
		return settleDone, st.abandon(err)
	}
	st.credit = credit
	snapshotCursor, err := currentUpdateCursor(ctx, tx, room)
	if err != nil {
		return settleDone, st.abandon(err)
	}
	st.snapshotCursor = snapshotCursor
	if len(st.slots) > 0 && s.afterSettleVersionRead != nil {
		s.afterSettleVersionRead(room)
	}
	return settleOn, settleOutcome{}
}

// appendEvents appends events in this settlement's transaction. Each event that carries a
// reconciled source's text stamps that source's new mention edges with the event that introduced
// them: the version event for the document itself, and each ask's own opened/edited event.
// writeVersionTx reconciled the edges earlier in this transaction, so stamping runs after the
// append and never touches sequence allocation.
func (st *settlement) appendEvents(ctx context.Context, events []model.Event) error {
	for _, planned := range events {
		appended, appendErr := st.service.events.Append(ctx, st.tx, planned)
		if appendErr != nil {
			return appendErr
		}
		if sourceKind, sourceID, ok := referenceSource(planned, st.room); ok {
			if err := refs.Stamp(ctx, st.tx, sourceKind, sourceID, appended.ID); err != nil {
				return err
			}
		}
		st.published = append(st.published, appended)
	}
	return nil
}

// commitVersion renders the document the settlement settled, writes its version when the document
// now reads differently from the latest one, and commits the transaction all of that and the
// settlement's stored updates are in.
func (st *settlement) commitVersion(ctx context.Context) (settleStep, settleOutcome) {
	s, room, state, tx, ledger := st.service, st.room, st.state, st.tx, st.ledger
	// read is the settlement's private copy of the document (readSettlementTree), and nothing but
	// reconcileAskBlocks changes it in place, which records each change it makes (changedTree).
	// versioned is another tree once a stamp replaced the read or a write read the document again.
	// So the version takes the rendering of the read when it is that tree, unchanged.
	markdown, offsets, err := st.markdown, st.offsets, st.renderErr
	reused := st.versioned == st.read && !st.reconciliation.changedTree()
	if s.beforeSettleVersionRender != nil {
		s.beforeSettleVersionRender(room, st.read, reused)
	}
	if !reused {
		markdown, offsets, err = renderTreeWithOffsets(st.versioned)
	}
	if err != nil {
		if len(st.slots) == 0 && errors.Is(err, ErrDocSchema) {
			slog.Error("dispatch: settle document outside Proof schema", "room", room, "error", err)
			return settleDone, settleOutcome{}
		}
		return settleDone, st.abandon(err)
	}

	state.mu.Lock()
	// A settlement that wrote into the room commits what it wrote even when the document has moved
	// since its read: the room and its peers hold those updates, and dropped here they would never
	// reach the store. Its version is the document as it stood at its last read, after the repairs,
	// every update of which this transaction stores; a move made after that read is in a later
	// version.
	if s.stopping.Load() || state.closed || state.failed != nil || (state.roomGeneration != st.generation && len(st.slots) == 0) {
		s.unlockState(room, state)
		s.discardSuppressedPersistence(room, st.slots...)
		return settleDone, settleOutcome{}
	}
	s.unlockState(room, state)

	st.published = make([]model.Event, 0, len(st.reconciliation.events)+1)
	// Settlement writes a version only when the document now reads differently from the latest
	// one. Indexing an ask block writes an `asks` row and an `ask.opened` event over words the
	// edit that wrote the block already versioned, so a version for that event alone would be
	// byte-identical and credited to nobody (LEGION-273, where it staled an approval pinned to what
	// an agent had just written; carryApproval now carries that approval to such a version
	// instead, an event nothing in the log would explain). Its tree writes - stamping block ids,
	// restoring an ask block's server-owned attributes - move the stored Proof state and often
	// render the same markdown,
	// and a version for one of those would repeat the version before it too (LEGION-229
	// requirement 2).
	// contentChanged stays the first half of the test: a version an older renderer wrote is not
	// this settlement's to canonicalise when nothing has touched the document since.
	contentChanged, err := contentChangedSinceVersion(ctx, tx, room, st.latest.docUpdateVersion)
	if err != nil {
		return settleDone, st.abandon(err)
	}
	versioning := contentChanged && markdown != st.latest.markdown
	settledVersion := st.latest.Number
	if versioning {
		settledVersion++
	}
	// Every update this settlement read is settled once it commits, so the row that left the
	// settlement to a later load goes with that commit. It goes before the first event this
	// settlement appends. A transaction marking this document owed as a copy (owedCopiesOf) holds
	// its row until it commits, and this settlement then waits for it holding its owner row and
	// room lock, which that transaction never takes; nor does it take what this settlement goes on
	// to lock, its events locking only this document's owner row. The pending authors stay unless
	// this settlement's version lists them (deleted below): a settlement that writes no version
	// lists no one.
	if err := clearSettlementPending(ctx, tx, room); err != nil {
		return settleDone, st.abandon(err)
	}
	if err := st.reconciliation.nameVersion(ctx, tx, room, st.owner, settledVersion); err != nil {
		return settleDone, st.abandon(err)
	}
	if versioning {
		result, writeErr := s.writeVersionTx(ctx, tx, room, renderedTree{tree: st.versioned, markdown: markdown, offsets: offsets}, st.credit.actor, &versionWrite{
			authors:          st.credit.authors,
			capture:          &st.credit.capture,
			docUpdateVersion: &st.snapshotCursor,
		})
		if writeErr != nil {
			return settleDone, st.abandon(writeErr)
		}
		// The version lists the pending authors this settlement read, so its commit deletes them.
		if err := deleteCapturedPendingAuthors(ctx, tx, room, st.credit.capture); err != nil {
			return settleDone, st.abandon(err)
		}
		if s.afterSettleVersionWrite != nil {
			if err := s.afterSettleVersionWrite(room); err != nil {
				return settleDone, st.abandon(err)
			}
		}
		// A person's version opens or moves on the document's edit notice, under the owner lock
		// this settlement took first (lockArtifactOwner above): the sweep takes the two in that
		// order too (sendEditNotice).
		if err := s.recordEditNotice(ctx, tx, room, result.version.Number, st.credit.authors, st.credit.capture.latest); err != nil {
			return settleDone, st.abandon(err)
		}
		st.published = append(st.published, ledger.events...)
		// A document body cites nodes whose rows carry a backlink count, so the version event
		// names what this settle moved exactly as a message or comment write does.
		versionEvent := model.Event{
			IssueKey: st.owner.IssueKey,
			Type:     "artifact.version",
			Actor:    st.credit.actor,
			Payload:  ArtifactVersionEventPayload(room, st.owner.Name, result.version, nil, result.changes),
		}
		if st.owner.IssueKey == nil {
			versionEvent.ArtifactID = &room
		}
		if err := st.appendEvents(ctx, []model.Event{versionEvent}); err != nil {
			return settleDone, st.abandon(err)
		}
	}
	if err := st.appendEvents(ctx, st.reconciliation.events); err != nil {
		return settleDone, st.abandon(err)
	}
	// The commit marks the in-flight credits the version listed consumed before it releases the
	// room's state, and the room updates the settlement stored marked stored (commitSettlement); a
	// settlement that writes no version takes out none.
	committed := authorCapture{state: state}
	if versioning {
		committed = st.credit.capture
	}
	if advanced, err := s.commitSettlement(ctx, tx, ledger, room, committed, func(err error) {
		// This runs before the publish below, the order Ledger.Commit keeps for every other
		// version write, so a subscriber acting on this version's artifact.version event acts
		// after it.
		if err == nil && state.roomGeneration == st.generation {
			state.settleFailures = 0
			state.unsettled = len(state.inflight) > 0
			if !state.unsettled {
				state.lastActor = nil
			}
		}
	}); err != nil {
		return settleDone, st.commitFailed(advanced, fmt.Errorf("commit document settlement: %w", err))
	}
	st.credit.consumeAskAuthors(st.reconciliation.indexedAskBlocks)
	st.finishSlots()
	for _, event := range st.published {
		s.events.Publish(event)
	}
	st.reconciliation.logCopiedBlocks(room)
	// A copy of an ask this settlement retracted opens its own ask at its next settlement, which
	// its pending row (reconcileAskBlocks, owedCopiesOf) also leaves to the resumption should this
	// process stop first.
	for _, copy := range st.reconciliation.owedCopies {
		s.scheduleSettle(copy)
	}
	s.sweepUnrecordedMarks(room, st.versioned)
	return settleDone, settleOutcome{}
}
