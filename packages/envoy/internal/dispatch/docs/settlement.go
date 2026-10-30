package docs

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"maps"
	"slices"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/reearth/ygo/crdt"
	"github.com/reearth/ygo/provider/websocket"

	"github.com/sjawhar/envoy/internal/dispatch/model"
	"github.com/sjawhar/envoy/internal/dispatch/pmdoc"
	"github.com/sjawhar/envoy/internal/dispatch/refs"
	"github.com/sjawhar/envoy/internal/dispatch/store"
)

// settleStep is how a settlement pass ended (settlePass).
type settleStep int

const (
	// settleDone ends the settlement, with the outcome the pass reports: the pass committed the
	// document's version, stopped short, or left the document to whatever owns its settlement now.
	settleDone settleStep = iota
	// settleAgain is a pass that did its part - committed a stamp or repairs, or planned the
	// repairs the next pass commits (repairPlan) - and leaves the rest to the next pass.
	settleAgain
	// settleMoved is a pass that found the document moved under what it read, and rolled back.
	settleMoved
)

// maxSettleRereads is how many times one settlement reads its document again after a pass found it
// moved under its read (settleMoved). A document a browser keeps moving faster than it renders is
// left owed after that: each of its updates arms a settlement of its own, and the resumption arms
// one for a row left standing.
const maxSettleRereads = 3

// maxSettlePasses bounds the passes of one settlement: one each for the stamp, the plan of the
// repairs, the repairs and the version, and one more for each read again (maxSettleRereads).
const maxSettlePasses = 4 + maxSettleRereads

// repairPlan is the ask repairs a pass found its document needs, worked out where no lock reaches:
// the tree that pass read, reconciled, with the answers its document has room for decided
// (weighAnswers), and that tree's rendering - for the read that stood at heldThrough and rendered
// as markdown. A later pass that reads the same document and reconciles it to the same tree commits
// the repairs with this rendering, so the pass that holds the document's locks renders nothing: the
// rendering says whether the repairs change the markdown a version stores (closureChangedMarkdown),
// and which version the events committed with them name.
type repairPlan struct {
	heldThrough int64
	markdown    string
	answers     answerDecision
	tree        *pmdoc.Node
	repaired    string
	repairedErr error
}

// settlementTake is what a settlement pass takes of its room before it copies the tree it renders:
// the durable version the room's instance held every update through (heldThrough, its roomCursor),
// which the pass reads again once it holds the document's locks, and what its version credits -
// the in-flight credits observed so far (creditThrough) and the latest edit source. An update
// observer credits a change only once the room holds it, so every in-flight credit the take
// observed is for a change the copy holds; one observed after it stays pending for the next
// version. The take holds state.mu for itself alone: held across the copy and its render it would
// block the update observer that credits and broadcasts every peer's keystroke.
type settlementTake struct {
	heldThrough   int64
	creditThrough uint64
	lastActor     *model.Actor
}

// copyTree is a copy of tree whose nodes and attributes its caller may change without changing
// tree's.
func copyTree(tree *pmdoc.Node) *pmdoc.Node {
	if tree == nil {
		return nil
	}
	copied := &pmdoc.Node{Type: tree.Type, Text: tree.Text, Attrs: maps.Clone(tree.Attrs), Marks: slices.Clone(tree.Marks)}
	if tree.Children != nil {
		copied.Children = make([]*pmdoc.Node, len(tree.Children))
		for index, child := range tree.Children {
			copied.Children[index] = copyTree(child)
		}
	}
	return copied
}

// settlement is one document's settlement, as its passes run it (settleRoomWithin): the room it
// settles, that room's state and the live instance it holds for the whole settlement, the room
// generation its passes stand at, which moves as a pass reads the room again, the repairs a pass
// planned for the next one to commit, and how the settlement reports a stop short.
type settlement struct {
	service      *Service
	room         string
	state        *roomState
	doc          *crdt.Doc
	settlements  *store.Pool
	generation   uint64
	plan         *repairPlan
	stoppedShort func(error) settleOutcome
}

// settlementPass is one pass of a settlement, and what that pass holds of the document: what it
// took of the room and rendered before it opened a transaction, what it read under the document's
// locks, and what it has written into the room and appended since.
type settlementPass struct {
	*settlement
	// taken is what the pass took of the room before it copied the tree it renders, and cursor the
	// cursor of the instance it settles, which gates the repairs it appends and takes them in once
	// they are stored (advanceRoomCursor).
	taken  settlementTake
	cursor *roomCursor
	// read is the tree the pass copied, with markdown and offsets its rendering and renderErr why
	// it has none. stamping says that tree lacks block ids, and stampChanged whether stamping them
	// changes the markdown a version stores. stampSuperseded says a settlement whose stamp commits
	// alone ends with it (applyStamp).
	read            *pmdoc.Node
	markdown        string
	offsets         []pmdoc.BlockOffset
	renderErr       error
	stamping        bool
	stampChanged    bool
	stampSuperseded bool
	// The pass's transaction and what it read under the document's locks: the ledger its version
	// records its events on, the document's owner row, the latest version, the update cursor a
	// version of this pass would record, and what that version credits.
	ledger *Ledger
	tx     pgx.Tx
	owner  artifactOwner
	latest struct {
		model.Version
		markdown         string
		docUpdateVersion int64
	}
	snapshotCursor int64
	credit         settlementVersionCredit
	// slots holds a suppression slot for each update this pass writes into the room, in the order
	// the room's persistence worker is handed them, and updates holds those updates. Every path
	// after a write finishes or discards all of them.
	slots   []*suppressSlot
	updates [][]byte
	// reconciliation is what the pass reconciled the document's ask blocks to, planned the repairs
	// of (planRepairs), and published the events it has appended, published once it commits.
	reconciliation settlementReconciliation
	planned        *repairPlan
	published      []model.Event
}

// settleRoomWithin settles room under parent and reports how the settlement ended (settleOutcome),
// leaving what follows to its caller: a timer's settlement under settleTimeout, the one Shutdown
// runs under its drain budget, one settleOwedNow runs under its caller's context. Before anything
// else - before it loads a cold room - it waits for one of the settlementWorkers slots, holding no
// connection, so every way a settlement starts is bounded and the excess waits there. When parent
// ends first, in that wait or after it, the settlement's database work is cancelled and its
// transaction rolls back, and nothing retries it or counts it as a failure: the document's
// pending-settlement row leaves it to resume (onLoadDocument, RunSettlementResumption). That is how
// a shutdown that cannot settle every room within its budget leaves the rest to the next process. A
// settlement refused a connection for overload (store.IsPoolBusy: the settlements pool's or the
// rooms pool's acquire bound) is left to resume the same way, so overload fails
// no room (maxSettleFailures). So is a settlement that has to write into a room Shutdown has begun
// closing, which refuses the write (allowInject).
//
// The settlement itself does only what no caller can do after it: a settlement that wrote into
// the room and cannot finish fails the room, whose reload arms the settlement again, and one the
// gate refuses on a room behind the durable head reloads the room (reloadRoomBehindHead). The
// retries, the re-arming and the log lines of a settlement cut short are its caller's: the timer's
// settlement and Shutdown's do them (followUpSettlement), and a caller that runs it synchronously
// and answers for its outcome itself reports the reason instead (settleOwedNow).
//
// The settlement runs in passes (settlePass), none of which renders the document while it holds the
// document's owner row. A pass that commits a part of the settlement - a stamp, repairs - or plans
// the repairs, or finds the document moved under its read, is followed by another, at most
// maxSettlePasses in all and maxSettleRereads reads again.
func (s *Service) settleRoomWithin(parent context.Context, room string, generation uint64) settleOutcome {
	settle := &settlement{service: s, room: room, generation: generation}
	// stoppedShort is the outcome of a settlement that stopped short with err having written
	// nothing into the room: a retry, unless its deadline or parent has ended, the pools refused it
	// a connection, or Shutdown refused the write. The generation it counts that retry's failure
	// against is the one the settlement stands at now, which its passes move as they read the room
	// again.
	settle.stoppedShort = func(err error) settleOutcome {
		next := settleRetry
		switch {
		case errors.Is(context.Cause(parent), errSettlementTimeout):
			next = settlePastDeadline
		case parent.Err() != nil:
			next = settleOutOfBudget
		case store.IsPoolBusy(err):
			next = settleOverloaded
		case s.shuttingDown(room) && errors.Is(err, ErrServiceUnavailable):
			next = settleRefusedInShutdown
		}
		return settleOutcome{next: next, err: err, generation: settle.generation}
	}
	if s.beforeSettleStart != nil {
		s.beforeSettleStart(room)
	}
	state := s.lockState(room)
	if s.roomGone(state) || state.roomGeneration != generation {
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
	if s.shuttingDown(room) && s.srv.GetDoc(room) == nil {
		return settleOutcome{}
	}
	release, err := s.settlements.acquire(parent)
	if err != nil {
		return settle.stoppedShort(fmt.Errorf("wait for a settlement slot: %w", err))
	}
	defer release()
	// The settlement counted itself running (settling) while it waited, so a write that opened
	// meanwhile took it over (openLiveWrite). Whatever moved the generation while it waited owns
	// the settlement now, as it would had it moved before the settlement started.
	state.mu.Lock()
	moved := s.roomGone(state) || state.roomGeneration != generation
	s.unlockState(room, state)
	if moved {
		return settleOutcome{}
	}
	// The pool every read and write of this settlement goes through, opened once here, before the
	// warm-up: the warm-up's injection reads the issue's state (allowInject) through it, and so
	// does each pass's transaction, so no settlement takes a connection admission counts for
	// requests. A settlement that cannot open it writes nothing and stops short, rather than
	// reading on through the request pool.
	settlements, err := s.store.Pool.Settlements()
	if err != nil {
		return settle.stoppedShort(fmt.Errorf("open the settlements pool: %w", err))
	}
	settle.settlements = settlements
	if s.srv.GetDoc(room) == nil {
		state.mu.Lock()
		state.settleWarming = true
		s.unlockState(room, state)
		err := s.srv.Apply(withQueryPool(parent, settlements), room, func(_ *crdt.Doc, _ func(func(*crdt.Transaction))) {})
		state.mu.Lock()
		state.settleWarming = false
		s.unlockState(room, state)
		if err != nil && !errors.Is(err, websocket.ErrNoChanges) {
			return settle.stoppedShort(fmt.Errorf("warm document for settlement: %w", err))
		}
	}
	// Hold the live document for the whole settlement. A close that lands after the generation
	// check above (a failure's eviction, a close of a room behind the durable head) retires the
	// room and moves the generation; reading the room again later would return nil and every
	// later step would dereference it. A nil here means exactly that close: the next access
	// reloads the durable document and schedules its own settlement, so this one just ends.
	if s.afterSettleWarm != nil {
		s.afterSettleWarm(room)
	}
	doc := s.srv.GetDoc(room)
	if doc == nil {
		return settleOutcome{}
	}
	settle.doc = doc
	moves := 0
	for passes := 1; ; passes++ {
		step, outcome := settle.settlePass(parent)
		switch step {
		case settleDone:
			return outcome
		case settleMoved:
			moves++
		}
		if moves > maxSettleRereads || passes == maxSettlePasses {
			slog.Info("dispatch: document settlement left owed after reading a moving document again; its next update or the resumption settles it",
				"room", room, "passes", passes)
			return settleOutcome{}
		}
	}
}

// settlePass is one pass of the settlement over doc, the room instance it holds, and the outcome
// the settlement reports when the pass ends it (settleDone). The pass takes its transaction from
// the pool the settlement opened for every read and write of its own (settleRoomWithin).
//
// Nothing renders the document under its owner row's lock, which serialises every writer of the
// issue. Before it opens its transaction the pass takes what its version credits
// (settlementTake), copies the live tree and renders it, both the markdown and the block offsets
// its version stores; when the tree lacks block ids it also renders a stamped copy, which says
// whether the stamp changes the markdown (take). Under the locks it checks that the room's
// instance still holds the durable version it held at the take: a durable update that landed since
// may be missing from the copy, and a version of it would record that update's cursor over text
// without it, so the pass rolls back and reads the document again instead (readLocked). Then:
//
//   - A tree that lacks block ids is stamped in the room, and the stamp committed alone
//     (commitStamp).
//   - A tree whose ask blocks need repairs and no plan for them is reconciled, rolled back, and
//     its repairs planned - their answers weighed and the repaired tree rendered - outside the
//     lock (planRepairs, repairPlan).
//   - A tree whose repairs match the plan has them written into the room, appended and committed
//     with the rows and events their reconciliation wrote, and no version (commitRepairs).
//   - Otherwise the pass writes the version from the pre-rendered output (commitVersion).
//
// Each pass but the last leaves the rest to the next, which reads the document again.
func (s *settlement) settlePass(parent context.Context) (settleStep, settleOutcome) {
	pass := &settlementPass{settlement: s}
	if ended, step, outcome := pass.take(); ended {
		return step, outcome
	}
	ledger := &Ledger{service: s.service, settling: true}
	ctx := withRoomCursor(store.WithTransactionTracking(withLedger(parent, ledger)), pass.cursor)
	tx, err := s.settlements.Begin(ctx)
	if err != nil {
		return settleDone, s.stoppedShort(fmt.Errorf("begin document transaction: %w", err))
	}
	// Rolled back apart from ctx: one the settlement's deadline ended still rolls back.
	defer tx.Rollback(context.WithoutCancel(ctx))
	pass.ledger, pass.tx = ledger, tx
	owner, open, err := lockArtifactOwner(ctx, tx, s.room)
	if err != nil {
		return settleDone, s.stoppedShort(err)
	}
	if !open {
		return settleDone, settleOutcome{}
	}
	pass.owner = owner
	// The owner row is read and locked in this transaction, so the repairs settlement injects
	// below do not read it again: that read would be a second pooled connection taken while
	// this transaction holds one, which is what deadlocks the pool.
	ctx = withOwnerVerified(ctx)
	if ended, step, outcome := pass.readLocked(ctx, parent); ended {
		return step, outcome
	}
	if ended, step, outcome := pass.applyStamp(ctx); ended {
		return step, outcome
	}
	if len(pass.slots) > 0 {
		// The stamp is in the room: it commits alone, and the pass ends with it. This return is
		// what keeps the copy lock reconcileAskBlocks takes below out of the transaction that
		// holds the stamp's pending row.
		return pass.commitStamp(ctx)
	}
	reconciliation, err := s.service.reconcileAskBlocks(ctx, tx, s.room, owner, pass.read, pass.credit)
	if err != nil {
		return settleDone, pass.abandon(err)
	}
	pass.reconciliation = reconciliation
	if s.service.afterSettleReconcile != nil {
		s.service.afterSettleReconcile(s.room)
	}
	if len(reconciliation.repairs) > 0 {
		if ended, step, outcome := pass.planRepairs(ctx); ended {
			return step, outcome
		}
		return pass.commitRepairs(ctx)
	}
	return pass.commitVersion(ctx)
}

// take is what the pass takes of its room before it opens a transaction, under no lock of the
// document's: the room's updates waited for, what its version credits (settlementTake), the live
// tree copied, and that copy rendered. ended says the pass ends here, on the step and outcome
// beside it; a pass that goes on holds what it took.
func (p *settlementPass) take() (bool, settleStep, settleOutcome) {
	s, room, state := p.service, p.room, p.state
	if s.hasPendingUpdates(room) {
		pendingCtx, cancel := context.WithTimeout(context.Background(), 100*time.Millisecond)
		err := s.waitForPendingUpdates(pendingCtx, room)
		cancel()
		if err != nil {
			if s.shuttingDown(room) {
				slog.Warn("dispatch: skip shutdown document settlement before persistence queue drains", "room", room, "error", err)
				return true, settleDone, settleOutcome{}
			}
			return true, settleDone, settleOutcome{next: settleRetrySoon, err: fmt.Errorf("wait for the room's updates to reach persistence: %w", err)}
		}
	}
	if s.hasDurableAppend(room) {
		appendCtx, cancel := context.WithTimeout(context.Background(), 100*time.Millisecond)
		err := s.waitForDurableAppends(appendCtx, room)
		cancel()
		if err != nil {
			if s.shuttingDown(room) {
				slog.Warn("dispatch: skip shutdown document settlement before durable append", "room", room, "error", err)
				return true, settleDone, settleOutcome{}
			}
			return true, settleDone, settleOutcome{next: settleRetrySoon, err: fmt.Errorf("wait for the room's updates to become durable: %w", err)}
		}
	}
	if s.beforeSettleTake != nil {
		s.beforeSettleTake(room)
	}
	// A room replaced since the settlement began is the replacement's to settle: its load arms that
	// settlement.
	if s.srv.GetDoc(room) != p.doc {
		return true, settleDone, settleOutcome{}
	}
	state.mu.Lock()
	if s.roomGone(state) {
		s.unlockState(room, state)
		return true, settleDone, settleOutcome{}
	}
	// An open live write is not in the room yet; finishLiveWrite arms the settlement once it is
	// published.
	if state.liveWriter != nil {
		state.settleDeferred = true
		s.unlockState(room, state)
		return true, settleDone, settleOutcome{}
	}
	p.generation = state.roomGeneration
	p.taken = settlementTake{creditThrough: state.creditSeq.Load(), lastActor: state.lastActor}
	s.unlockState(room, state)
	// The cursor of the instance this settlement holds (doc), which gates the repairs it appends
	// and takes them in once they are stored (advanceRoomCursor).
	p.cursor = s.cursorOf(p.doc)
	if p.cursor != nil {
		p.taken.heldThrough = p.cursor.heldThrough()
	}
	if s.afterSettleAuthorsTake != nil {
		s.afterSettleAuthorsTake(room)
	}
	// The tree is taken as of one moment under the document's lock (liveTree): the room's peers
	// and the service can write it while a walk of the live tree, which takes no lock, reads it,
	// and a torn read would be versioned as the document.
	read, err := s.liveTree(room, p.doc)
	if err != nil {
		if errors.Is(err, ErrDocSchema) {
			slog.Error("dispatch: settle document outside Proof schema", "room", room, "error", err)
			return true, settleDone, settleOutcome{}
		}
		return true, settleDone, p.stoppedShort(err)
	}
	p.read = read
	if s.afterSettleTreeRead != nil {
		s.afterSettleTreeRead(room, read)
	}
	if s.beforeSettleRender != nil {
		s.beforeSettleRender(room)
	}
	// What the document renders, with each identified block's range in it: the version a pass
	// that repairs nothing writes, and what a repair's own update is classified against
	// (closureChangedMarkdown), as a transactional live write classifies its own (applyLive): a
	// `doc_updates` row says whether the update changed the rendered markdown, and a repair that
	// renders the document exactly as it was changed none. A row that claimed otherwise would sit
	// past every later version's cursor, since no version follows it to move the cursor, and the
	// first settlement after a renderer change would version a document nobody had touched.
	p.markdown, p.offsets, p.renderErr = renderTreeWithOffsets(read)
	// A plan made for another read of the document is no plan for this one.
	if p.plan != nil && (p.plan.heldThrough != p.taken.heldThrough || p.plan.markdown != p.markdown) {
		p.plan = nil
	}
	// The stamp is classified on a copy stamped here: the stamp the pass writes into the room mints
	// other ids, which a rendering carries for the same blocks, or for none.
	p.stamping = pmdoc.BlockIDRepairCount(read) > 0
	if p.stamping {
		stamped := copyTree(read)
		pmdoc.EnsureBlockIDsCount(stamped)
		if s.beforeSettleRender != nil {
			s.beforeSettleRender(room)
		}
		p.stampChanged = closureChangedMarkdown(p.markdown, p.renderErr, stamped)
	}
	return false, settleDone, settleOutcome{}
}

// readLocked is what the pass reads once its transaction holds the document's owner row: the
// latest version, the document's cursor lock, the update cursor its version would record and the
// gate that cursor passes, then the authors its version would credit. ended says the pass ends
// here, on the step and outcome beside it.
func (p *settlementPass) readLocked(ctx context.Context, parent context.Context) (bool, settleStep, settleOutcome) {
	s, room, state := p.service, p.room, p.state
	latest, err := latestVersion(ctx, p.tx, room)
	if err != nil {
		return true, settleDone, p.stoppedShort(err)
	}
	p.latest = latest
	if err := s.lockSettlementCursor(ctx, p.tx, room); err != nil {
		if s.shuttingDown(room) {
			slog.Warn("dispatch: skip shutdown document settlement while cursor lock is held", "room", room, "error", err)
			return true, settleDone, settleOutcome{}
		}
		return true, settleDone, p.stoppedShort(fmt.Errorf("lock document cursor: %w", err))
	}
	if s.afterSettleLock != nil {
		s.afterSettleLock(room)
	}
	if s.hasPendingUpdates(room) {
		if s.shuttingDown(room) {
			slog.Warn("dispatch: skip shutdown document settlement with undurable updates", "room", room)
			return true, settleDone, settleOutcome{}
		}
		return true, settleDone, settleOutcome{next: settleReschedule, err: errors.New("the room holds updates its persistence has not taken")}
	}
	snapshotCursor, err := currentUpdateCursor(ctx, p.tx, room)
	if err != nil {
		return true, settleDone, p.stoppedShort(err)
	}
	p.snapshotCursor = snapshotCursor
	// The gate, before this settlement writes anything. The room it holds must stand at that
	// head: with two envoy-dispatch processes on one Postgres, an update the other process
	// appended is not in this room, and settling from it corrupts three things at once. Its
	// version would record doc_update_version = snapshotCursor over text that is missing the other
	// process's update, so contentChangedSinceVersion then reports nothing past that cursor and
	// those words are never versioned; clearSettlementPending would delete the one durable record
	// that the work is still owed; and reconcileAskBlocks would retract every open ask whose block
	// this stale tree cannot see.
	//
	// Nothing is lost by refusing. The transaction rolls back, so doc_settlements_pending still
	// stands. The refusal is a verdict, not a fault to retry: a room behind the head never catches
	// up without a reload (advanceRoomCursor), so every retry of this settlement on it is refused
	// the same way. Counted toward maxSettleFailures, the third refusal failed the room, and every
	// transaction that met the document until the failure's eviction had run answered
	// ErrServiceUnavailable (INFRA-218). The room is reloaded instead (reloadRoomBehindHead), and
	// the settlement that re-arms settles every update, this process's and the other's.
	//
	// The cursor is the one this settlement marked its context with, read through the accessor
	// its repairs' appends read (appendingCursor), so the two can never disagree.
	if err := refuseRoomBehindHead(room, appendingCursor(ctx, room), snapshotCursor); err != nil {
		// The rollback releases the document's advisory lock, which the room's close needs: a close
		// flushes the room's persistence, whose appends take that lock.
		_ = p.tx.Rollback(ctx)
		if parent.Err() != nil {
			return true, settleDone, p.stoppedShort(err)
		}
		s.reloadRoomBehindHead(parent, room, state, p.generation, p.doc, err)
		return true, settleDone, settleOutcome{next: settleReloaded}
	}
	// The room stands at the head; it must also still stand where it stood at the take. An update
	// that became durable since may not be in the tree this pass rendered, and versioned, that tree
	// would record the update's cursor over text without it. The rollback leaves the document as
	// it was, and the next pass reads it again.
	if p.cursor != nil && p.cursor.heldThrough() != p.taken.heldThrough {
		return true, settleMoved, settleOutcome{}
	}
	// The pending authors as this settlement's lock holds them: no append or write changes them
	// until its transaction ends. Its version lists them and deletes them; a settlement that
	// writes no version leaves them for the next version.
	owed, err := readPendingAuthors(ctx, p.tx, room)
	if err != nil {
		return true, settleDone, p.stoppedShort(err)
	}
	// The document's latest person sender as its stored updates and commits recorded it
	// (readLastPerson), which names who edited last once the room holds no in-flight credit that
	// decides (latestEditSource): every landed browser edit, in this process or another.
	durableLatest, err := readLastPerson(ctx, p.tx, room)
	if err != nil {
		return true, settleDone, p.stoppedShort(err)
	}
	state.mu.Lock()
	p.credit = settlementAuthors(state, owed, durableLatest, p.taken)
	state.mu.Unlock()
	if s.afterSettleRead != nil {
		s.afterSettleRead(room)
	}
	return false, settleDone, settleOutcome{}
}

// keep records a suppression slot and the update it suppresses, in the order the room's
// persistence worker is handed them.
func (p *settlementPass) keep(slot *suppressSlot, update []byte) {
	if slot != nil {
		p.slots, p.updates = append(p.slots, slot), append(p.updates, update)
	}
}

// finishSlots hands the room's persistence every update this pass wrote into the room, now that
// its transaction holds them.
func (p *settlementPass) finishSlots() {
	for index, slot := range p.slots {
		p.service.finishSuppressedPersistence(slot, p.updates[index])
	}
}

// abandon ends a settlement that cannot finish. What it wrote is in the room and its browsers
// and will never reach the store through this settlement, so its slots are discarded and the
// room fails, which reloads the document from the store and leaves its settlement to that load.
// A settlement that wrote nothing stops short as one that met any other fault does. Its
// transaction rolls back, so the authors it read stay where they were, and it marked no
// in-flight credit.
func (p *settlementPass) abandon(err error) settleOutcome {
	if len(p.slots) > 0 {
		p.service.discardSuppressedPersistence(p.room, p.slots...)
		p.service.failRoom(p.room, err)
		return settleOutcome{next: settleRoomFailed, err: err}
	}
	return p.stoppedShort(err)
}

// appendEvents appends events in this pass's transaction. Each event that carries a reconciled
// source's text stamps that source's new mention edges with the event that introduced them: the
// version event for the document itself, and each ask's own opened/edited event. writeVersionTx
// reconciled the edges earlier in this transaction, so stamping runs after the append and never
// touches sequence allocation.
func (p *settlementPass) appendEvents(ctx context.Context, events []model.Event) error {
	for _, planned := range events {
		appended, appendErr := p.service.events.Append(ctx, p.tx, planned)
		if appendErr != nil {
			return appendErr
		}
		if sourceKind, sourceID, ok := referenceSource(planned, p.room); ok {
			if err := refs.Stamp(ctx, p.tx, sourceKind, sourceID, appended.ID); err != nil {
				return err
			}
		}
		p.published = append(p.published, appended)
	}
	return nil
}

// committed ends a pass whose transaction committed: the ask blocks it indexed forget their
// recorded authors, its writes reach the room's persistence, and its events are published.
func (p *settlementPass) committed() {
	p.credit.consumeAskAuthors(p.reconciliation.indexedAskBlocks)
	p.finishSlots()
	for _, event := range p.published {
		p.service.events.Publish(event)
	}
	p.reconciliation.logCopiedBlocks(p.room)
	// A copy of an ask this settlement retracted opens its own ask at its next settlement,
	// which its pending row (reconcileAskBlocks, owedCopiesOf) also leaves to the resumption
	// should this process stop first.
	for _, copy := range p.reconciliation.owedCopies {
		p.service.scheduleSettle(copy)
	}
}

// applyStamp stamps the block ids the pass's tree lacks into the room; a pass whose tree lacks none
// writes nothing here. It is also where the pass meets a room gone or a settlement superseded, under
// the one hold of the room's state that records the blocks the stamp renamed. ended says the pass
// ends here, on the step and outcome beside it. A pass whose stamp is in the room (p.slots) goes on
// only to commitStamp; any other that goes on holds the ask blocks' sources read under that state.
func (p *settlementPass) applyStamp(ctx context.Context) (bool, settleStep, settleOutcome) {
	s, room, state := p.service, p.room, p.state
	// The stamp is written into the room as it stands, read inside the stamp's own transaction.
	var renamed []stampedAsk
	if p.stamping {
		slot, update, err := s.applySuppressed(ctx, room, p.doc, func(doc *crdt.Doc, origin any) (bool, error) {
			var minted int
			var stampErr error
			_, minted, renamed, stampErr = stampBlockIDs(doc, origin)
			return minted > 0, stampErr
		})
		p.keep(slot, update)
		if err != nil {
			if len(p.slots) == 0 && errors.Is(err, ErrDocSchema) {
				slog.Error("dispatch: settle document outside Proof schema", "room", room, "error", err)
				return true, settleDone, settleOutcome{}
			}
			return true, settleDone, p.abandon(err)
		}
	}

	state.mu.Lock()
	state.recordStampedAskBlocks(renamed)
	if s.roomGone(state) {
		s.unlockState(room, state)
		s.discardSuppressedPersistence(room, p.slots...)
		return true, settleDone, settleOutcome{}
	}
	// An open live write is not in the room yet, and since this settlement holds the owner row,
	// the write's transaction has already committed: the cursor this settlement versions against
	// includes its row. Write no version; finishLiveWrite arms a settlement once it is published.
	superseded := state.roomGeneration != p.generation
	if state.liveWriter != nil {
		state.settleDeferred = true
		superseded = true
	}
	if len(p.slots) > 0 {
		// The stamp commits alone, superseded or not: the room and its peers hold it, and dropped
		// here it would never reach the store (commitStamp). A superseded settlement ends with it;
		// any other reconciles and versions the stamped document in its next pass.
		p.stampSuperseded = superseded
		s.unlockState(room, state)
		return false, settleDone, settleOutcome{}
	}
	if superseded {
		s.unlockState(room, state)
		return true, settleDone, settleOutcome{}
	}
	if p.stamping {
		// The stamp found nothing to write: another writer stamped the ids first, and the next pass
		// reads the document as that writer left it.
		s.unlockState(room, state)
		return true, settleMoved, settleOutcome{}
	}
	// The ask blocks' sources are read now, after the tree they are reconciled on: an update in that
	// tree whose observer has already run has recorded who introduced its blocks, and one whose
	// observer has not left its blocks unseen, for the settlement that observer arms (askBlockSources).
	p.credit.askSources = state.askSources()
	state.mu.Unlock()
	return false, settleDone, settleOutcome{}
}

// commitStamp stores the stamp applyStamp put into the room and commits it alone, which is all the
// pass does: a superseded settlement ends with it, any other reconciles and versions the stamped
// document in its next pass.
func (p *settlementPass) commitStamp(ctx context.Context) (settleStep, settleOutcome) {
	s, room := p.service, p.room
	// The room holds the stamp, so what is left to store it runs apart from the settlement's
	// deadline, under a bound of its own (finishInRoom): a deadline falling between the write
	// and the commit would fail the room over an update the room already holds.
	ctx, cancel := finishInRoom(ctx)
	defer cancel()
	identityUpdate, err := mergeUpdates(p.updates)
	if err != nil {
		return settleDone, p.abandon(err)
	}
	if err := s.srv.BroadcastUpdate(ctx, room, identityUpdate); err != nil {
		return settleDone, p.abandon(fmt.Errorf("broadcast document identity update: %w", err))
	}
	repaired, err := s.persistence.AppendUpdateTx(ctx, p.tx, room, identityUpdate, p.stampChanged)
	if err != nil {
		return settleDone, p.abandon(err)
	}
	// The room already holds this repair (applySuppressed wrote it there), so the instance
	// takes the version it is stored at. It is recorded before the commit, under the advisory
	// lock this transaction holds until then, so nothing reads the cursor in between; a
	// rollback here fails the room, which reloads the document (abandon).
	advanceRoomCursor(p.cursor, repaired)
	if s.afterSettleRepairAppend != nil {
		s.afterSettleRepairAppend(room)
	}
	if err := p.tx.Commit(ctx); err != nil {
		return settleDone, p.abandon(fmt.Errorf("commit document identity update: %w", err))
	}
	p.finishSlots()
	if p.stampSuperseded {
		return settleDone, settleOutcome{}
	}
	return settleAgain, settleOutcome{}
}

// planRepairs answers for the repairs this pass's reconciliation found: it plans the ones no pass
// has planned - rolling this pass's transaction back, since the plan weighs their answers and
// renders the repaired tree, which no pass does under the document's locks - and takes the plan a
// pass before it made when this reconciliation is that plan's. ended says the pass ends here, on
// the step and outcome beside it; a pass that goes on holds the plan its repairs commit with.
func (p *settlementPass) planRepairs(ctx context.Context) (bool, settleStep, settleOutcome) {
	s, room := p.service, p.room
	if p.plan == nil {
		// Repairs no pass has planned: this pass's transaction goes, and the repairs are
		// planned without its locks, where their answers are weighed and the repaired tree
		// rendered. The next pass commits them if it reconciles the document to the same tree.
		_ = p.tx.Rollback(context.WithoutCancel(ctx))
		if s.beforeSettleRender != nil {
			s.beforeSettleRender(room)
		}
		answers, repaired, ok := p.reconciliation.weighAnswers(p.read, p.markdown)
		var repairedErr error
		if !ok {
			repaired, repairedErr = renderTree(p.read)
		}
		p.plan = &repairPlan{
			heldThrough: p.taken.heldThrough, markdown: p.markdown, answers: answers,
			tree: p.read, repaired: repaired, repairedErr: repairedErr,
		}
		return true, settleAgain, settleOutcome{}
	}
	p.planned, p.plan = p.plan, nil
	// Block ids are left out of the comparison: a decided ask's wording written back into its
	// block (writeDecidedWording) mints ids for the blocks it rewrites, other ones each time,
	// and no rendering carries a paragraph's id.
	if !p.reconciliation.applyAnswers(room, p.planned.answers) || !p.read.Equal(p.planned.tree) {
		// The ask rows moved since the plan was made, so its repairs are not this
		// reconciliation's: the next pass plans them again.
		return true, settleMoved, settleOutcome{}
	}
	return false, settleDone, settleOutcome{}
}

// commitRepairs writes the planned repairs into the room and commits them, with the ask rows and
// events their reconciliation wrote, and no version: the document they leave is versioned by the
// next pass, which renders it outside the lock. The pending-settlement row and the pending authors
// stay for that version. What the repaired document renders is the plan's.
func (p *settlementPass) commitRepairs(ctx context.Context) (settleStep, settleOutcome) {
	s, room, state := p.service, p.room, p.state
	// The repairs are made on the document as it stands (writeLive): the tree they were
	// reconciled on was read before settlement's database work.
	slot, update, err := s.applySuppressed(ctx, room, p.doc, p.reconciliation.writeLive)
	p.keep(slot, update)
	if err != nil {
		return settleDone, p.abandon(fmt.Errorf("write reconciled typed blocks: %w", err))
	}
	// Repairs that found nothing to write found the document moved under them: a browser
	// removed or changed the block a repair was for. The rollback leaves the document as the
	// browser left it, and the next pass reads it again.
	if len(p.slots) == 0 {
		return settleMoved, settleOutcome{}
	}
	p.published = make([]model.Event, 0, len(p.reconciliation.events)+1)
	// The room holds the repairs, so what is left to store them runs apart from the settlement's
	// deadline, under a bound of its own (finishInRoom): a deadline falling between the write and
	// the commit would fail the room over updates the room already holds.
	ctx, cancel := finishInRoom(ctx)
	defer cancel()
	merged, err := mergeUpdates(p.updates)
	if err != nil {
		return settleDone, p.abandon(err)
	}
	if err := s.srv.BroadcastUpdate(ctx, room, merged); err != nil {
		return settleDone, p.abandon(fmt.Errorf("broadcast document closure update: %w", err))
	}
	closureChanged := p.renderErr != nil || p.planned.repairedErr != nil || p.planned.repaired != p.markdown
	repaired, err := s.persistence.AppendUpdateTx(ctx, p.tx, room, merged, closureChanged)
	if err != nil {
		return settleDone, p.abandon(err)
	}
	advanceRoomCursor(p.cursor, repaired)
	if s.afterSettleRepairAppend != nil {
		s.afterSettleRepairAppend(room)
	}
	contentChanged, err := contentChangedSinceVersion(ctx, p.tx, room, p.latest.docUpdateVersion)
	if err != nil {
		return settleDone, p.abandon(err)
	}
	// The events name the version the next pass leaves the document at: the one after the
	// latest when that pass versions the repaired document, the latest otherwise.
	settledVersion := p.latest.Number
	if contentChanged && p.planned.repairedErr == nil && p.planned.repaired != p.latest.markdown {
		settledVersion++
	}
	state.mu.Lock()
	if s.roomGone(state) {
		s.unlockState(room, state)
		s.discardSuppressedPersistence(room, p.slots...)
		return settleDone, settleOutcome{}
	}
	s.unlockState(room, state)
	if err := p.reconciliation.nameVersion(ctx, p.tx, room, p.owner, settledVersion); err != nil {
		return settleDone, p.abandon(err)
	}
	if err := p.appendEvents(ctx, p.reconciliation.events); err != nil {
		return settleDone, p.abandon(err)
	}
	if err := s.commitConsuming(ctx, p.tx, []roomCapture{{room: room, capture: authorCapture{state: state}}}, func(err error) {
		if err == nil && state.roomGeneration == p.generation {
			state.settleFailures = 0
		}
	}); err != nil {
		return settleDone, p.abandon(fmt.Errorf("commit document settlement repairs: %w", err))
	}
	p.committed()
	return settleAgain, settleOutcome{}
}

// commitVersion ends a settlement whose document needs no stamp and no repairs: it writes the
// document's version, when the document now reads differently from the latest one, with the rows
// and events of its reconciliation, and clears the pending-settlement row the settlement was owed
// on.
func (p *settlementPass) commitVersion(ctx context.Context) (settleStep, settleOutcome) {
	s, room, state := p.service, p.room, p.state
	if p.renderErr != nil {
		if errors.Is(p.renderErr, ErrDocSchema) {
			slog.Error("dispatch: settle document outside Proof schema", "room", room, "error", p.renderErr)
			return settleDone, settleOutcome{}
		}
		return settleDone, p.stoppedShort(p.renderErr)
	}
	state.mu.Lock()
	if s.roomGone(state) {
		s.unlockState(room, state)
		return settleDone, settleOutcome{}
	}
	// A generation that moved while the pass reconciled moved for a change this pass's read may
	// lack - a browser's edit arming its settlement, a write opening - so the pass writes no
	// version, and the next reads the document again (or, for a write still open, leaves it to
	// the write).
	if state.roomGeneration != p.generation {
		s.unlockState(room, state)
		return settleMoved, settleOutcome{}
	}
	s.unlockState(room, state)

	// The version takes the rendering this pass made of read before its transaction. Only a
	// recorded repair changes read in place (settlementReconciliation.repair, repairAsk), and a pass
	// that reconciled any repairs has ended above, so read is the tree that rendering was made of.
	if s.beforeSettleVersion != nil {
		s.beforeSettleVersion(room, p.read)
	}

	// Settlement writes a version only when the document now reads differently from the latest
	// one. Indexing an ask block writes an `asks` row and an `ask.opened` event over words the
	// edit that wrote the block already versioned, so a version for that event alone would be
	// byte-identical, credited to nobody, and stale an approval pinned to what an agent had just
	// written (LEGION-273). Its tree writes - stamping block ids, restoring an ask block's
	// server-owned attributes - move the stored Proof state and often render the same markdown,
	// and a version for one of those would repeat the version before it too (LEGION-229
	// requirement 2).
	// contentChanged stays the first half of the test: a version an older renderer wrote is not
	// this settlement's to canonicalise when nothing has touched the document since.
	contentChanged, err := contentChangedSinceVersion(ctx, p.tx, room, p.latest.docUpdateVersion)
	if err != nil {
		return settleDone, p.abandon(err)
	}
	versioning := contentChanged && p.markdown != p.latest.markdown
	settledVersion := p.latest.Number
	if versioning {
		settledVersion++
	}
	p.published = make([]model.Event, 0, len(p.reconciliation.events)+1)
	// Every update this settlement read is settled once it commits, so the row that left the
	// settlement to a later load goes with that commit. It goes before the first event this
	// settlement appends: a transaction marking this document owed as a copy (owedCopiesOf) holds
	// its row while it waits for the events' commit-order lock, which this settlement would then be
	// holding. The pending authors stay unless this settlement's version lists them (deleted
	// below): a settlement that writes no version lists no one.
	if err := clearSettlementPending(ctx, p.tx, room); err != nil {
		return settleDone, p.abandon(err)
	}
	if err := p.reconciliation.nameVersion(ctx, p.tx, room, p.owner, settledVersion); err != nil {
		return settleDone, p.abandon(err)
	}
	if versioning {
		result, writeErr := s.writeVersionTx(ctx, p.tx, room, renderedTree{tree: p.read, markdown: p.markdown, offsets: p.offsets}, p.credit.actor, &versionWrite{
			authors:          p.credit.authors,
			capture:          &p.credit.capture,
			docUpdateVersion: &p.snapshotCursor,
		})
		if writeErr != nil {
			return settleDone, p.abandon(writeErr)
		}
		// The version lists the pending authors this settlement read, so its commit deletes them.
		if err := deleteCapturedPendingAuthors(ctx, p.tx, room, p.credit.capture); err != nil {
			return settleDone, p.abandon(err)
		}
		if s.afterSettleVersionWrite != nil {
			if err := s.afterSettleVersionWrite(room); err != nil {
				return settleDone, p.abandon(err)
			}
		}
		// A person's version opens or moves on the document's edit notice, under the owner lock
		// this settlement took first (lockArtifactOwner above): the sweep takes the two in that
		// order too (sendEditNotice).
		if err := s.recordEditNotice(ctx, p.tx, room, result.version.Number, p.credit.authors, p.credit.capture.latest); err != nil {
			return settleDone, p.abandon(err)
		}
		p.published = append(p.published, p.ledger.events...)
		// A document body cites nodes whose rows carry a backlink count, so the version event
		// names what this settle moved exactly as a message or comment write does.
		versionEvent := model.Event{
			IssueKey: p.owner.IssueKey,
			Type:     "artifact.version",
			Actor:    p.credit.actor,
			Payload:  ArtifactVersionEventPayload(room, p.owner.Name, result.version, nil, result.changes),
		}
		if p.owner.IssueKey == nil {
			versionEvent.ArtifactID = &room
		}
		if err := p.appendEvents(ctx, []model.Event{versionEvent}); err != nil {
			return settleDone, p.abandon(err)
		}
	}
	if err := p.appendEvents(ctx, p.reconciliation.events); err != nil {
		return settleDone, p.abandon(err)
	}
	// The commit marks the in-flight credits the version listed consumed before it releases the
	// room's state (commitConsuming); a settlement that writes no version takes out none.
	consumed := authorCapture{state: state}
	if versioning {
		consumed = p.credit.capture
	}
	if err := s.commitConsuming(ctx, p.tx, []roomCapture{{room: room, capture: consumed}}, func(err error) {
		// This runs before the publish below, the order Ledger.Commit keeps for every other
		// version write, so a subscriber acting on this version's artifact.version event acts
		// after it.
		if err == nil && state.roomGeneration == p.generation {
			state.settleFailures = 0
			state.unsettled = len(state.inflight) > 0
			if !state.unsettled {
				state.lastActor = nil
			}
		}
	}); err != nil {
		return settleDone, p.abandon(fmt.Errorf("commit document settlement: %w", err))
	}
	p.committed()
	s.sweepUnrecordedMarks(room, p.read)
	return settleDone, settleOutcome{}
}
