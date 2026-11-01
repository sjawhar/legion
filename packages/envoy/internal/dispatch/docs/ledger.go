package docs

import (
	"context"
	"errors"
	"fmt"
	"slices"
	"strings"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/reearth/ygo/persistence"

	"github.com/sjawhar/envoy/internal/dispatch/model"
	"github.com/sjawhar/envoy/internal/dispatch/refs"
	"github.com/sjawhar/envoy/internal/dispatch/store"
)

// Ledger is what the document operations joined to one database transaction produce until it
// ends: the events they generate, their writes to live documents (see liveWrite) and the versions
// they write. Join is the only way to join a transaction, so an operation that has one always has
// its ledger. Commit ends the transaction the way every caller must: it runs the transaction's
// write queue, records the writes' authors and takes out the authors its versions listed in the
// transaction itself, commits, marks the in-flight credits its versions listed consumed, records
// the writes' latest edit source in their rooms, and publishes the writes to their rooms, in that
// order. Discard, which callers defer right after Join, drops whatever a transaction that did not
// commit left behind.
//
// A joined transaction runs in two phases. Its read phase takes the locks it needs - advisory
// locks, none of which gives it a transaction id - reads, and does every operation's document
// work; each operation queues its writes (write) instead of running them, and the caller queues
// its own beside them (Queue). The write phase is Commit: every queued write puts its statements
// on one batch, in the order the writes were made, so the events they append keep their order,
// and the batch goes to the server pipelined, in one round trip (store.WriteBatch), with the
// statements recording the transaction's settlement credit after them; the commit follows. The
// transaction holds a transaction id from the batch's first write to its commit, and neither
// document work nor a wait for this process to read a result runs in between. What a queued write
// leaves that a later read of the same transaction would have seen - a document's head, a
// version's number and time, an anchor's quote, an approval request's state, a source's
// references - the ledger holds in memory (pendingRoom, NextVersion, RegisterPendingAnchor,
// pendingApproval, referenced), and the read takes it from there.
//
// A writer that runs its operations in a transaction of its own, which no caller joined, carries
// no `tx` and marks its ledger `ownTx` instead: a settlement (settleRoomWithin) and the block-id
// backfill (backfillBlockIDs) are the two. Such a ledger queues its writes all the same (writer),
// and its writer runs the queue before it commits. Those are the ledgers document operations run
// under: every operation that writes a document either joins its caller's transaction or runs in
// the transaction of one of those two writers. A background pass with no document operation
// queues its writes on a bare ledger (queueWrites).
type Ledger struct {
	service *Service
	tx      pgx.Tx
	// writer is the transaction whose writes this ledger queues: the joined transaction, a
	// writer's own transaction, or a background pass's (queueFor).
	writer pgx.Tx
	// ownTx marks a ledger whose operations run inside a transaction of the writer's own, which
	// no caller joined: a settlement's (settleRoomWithin) and the block-id backfill's
	// (backfillBlockIDs).
	ownTx  bool
	events []model.Event
	// live holds, per document, the writes this transaction made to it; order is the order it
	// first wrote them in.
	live     map[string]*liveWrite
	order    []string
	versions []ledgerVersion
	// seeds holds, per document this transaction seeded (SeedText), the actor that wrote its
	// first text and the tree it wrote, which credit records and registers as that actor's ask
	// blocks once the transaction has committed.
	seeds map[string]ledgerSeed
	// rebuilds are the documents this transaction rebuilds (RebuildDocument), whose rooms refuse
	// loads until it ends, committed or not.
	rebuilds []string
	// copies are the documents showing a copy of an ask this transaction answered or resolved,
	// whose settlement it marked owed (SettleCopiesOf); Commit arms each once it has committed.
	copies []string
	// flushes are the room updates this transaction stored ahead of its own appends, one flush
	// per document (flushThrough), which its commit marks stored.
	flushes []*RoomFlush
	// queue is the transaction's write phase (WriteStep): every write its operations and its
	// caller made, in the order they made them, which Commit sends as one batch before it commits.
	// sealed is set once the write phase has begun or the ledger was discarded, after which
	// nothing more is queued (Queue). batch is the write phase's batch while runQueue builds it, on
	// which a write made meanwhile in the ledger's transaction puts its statements (write).
	queue  []WriteStep
	sealed bool
	batch  *store.WriteBatch
	// artifacts is the reference key of each artifacts row this transaction creates and has not
	// written yet, by artifact id (RegisterPendingArtifact).
	artifacts map[string]string
	// rooms holds, per document whose stored history this transaction has queued a write to, the
	// state its queued writes leave it in (pendingRoom).
	rooms map[string]*pendingRoom
	// numbers is, per document this transaction has numbered a version of, the number it gave the
	// last one (NextVersion), and latest that version as it will stand once written, which
	// latestVersion answers for it; now is the transaction's start, read once (Now).
	numbers map[string]int
	latest  map[string]pendingVersion
	now     *time.Time
	// anchors holds, per document, the anchored ask and comment rows this transaction writes, as
	// they will stand (RegisterPendingAnchor), in the order they were first registered.
	anchors map[string][]*PendingAnchor
	// approvals holds, per document, its open approval request as this transaction's queued
	// writes leave it (pendingApproval).
	approvals map[string]*pendingApproval
	// reviews holds, per document, the newest review this transaction has queued (QueueReview),
	// which the table answers only once the write phase has run (PendingReview).
	reviews map[string]model.ArtifactReview
	// referenced is, per source, the targets its references cite once this transaction's queued
	// reconciliation of them is written, and blockIndexes, per document, what its block index holds
	// once its queued indexing is written: what a later reconciliation of the same source in this
	// transaction plans against (planReplaceCounted, planBlockIndex).
	referenced   map[refs.Target][]refs.Target
	blockIndexes map[string]refs.BlockIndex
	// askBlocks holds, per document, the block asks whose rows this transaction has queued writes
	// of, by block id, as they will stand (recordAskBlock, loadAskBlocks).
	askBlocks map[string]map[string]model.Ask
	// owedAuthors holds, per document, the pending authors this transaction has queued writes of
	// (upsertPendingAuthors), by actorKey: the appends it makes for a room credit the browser
	// edits they store, and doc_pending_authors holds none of those rows until the write phase
	// has run, so the versions this transaction writes read them from here as well as from the
	// table (transactionPendingAuthors).
	owedAuthors map[string]map[string]model.Actor
}

// WriteStep is one step of a transaction's write phase: it puts the statements of one write on the
// phase's batch, with the callbacks that read their results (store.WriteBatch), and runs nothing.
// Everything the write decides on - its locks, its reads, the document work - runs before it is
// queued, in the transaction's read phase, so a step renders nothing, walks no tree and encodes no
// document, and it reads no result: the batch is sent once every step has queued its statements.
// A value a later statement needs from an earlier one is derived in the later statement's SQL (an
// event's sequence from its owner row, its id as store.LatestEventID). A value the caller needs
// once Commit has returned - an appended event's id, a row's generated timestamp - is written by
// the statement's callback through a variable the step captured, and only that caller reads it. A
// step's own error - an argument it cannot encode - ends the write phase as a failed statement
// would, after the statements queued before it.
type WriteStep func(b *store.WriteBatch) error

// ErrQueueSealed refuses a write queued once the ledger's write phase has begun (Commit) or the
// ledger was discarded: it would run after statements it was meant to precede, or in no
// transaction at all. It is a programming error in the caller, which Queue reports rather than
// panics on, so a handler answers it as any other failure of its transaction.
var ErrQueueSealed = errors.New("docs: the transaction's write queue is sealed: Commit has begun or the ledger was discarded")

// Queue appends step to the transaction's write phase, after every write queued so far: a route
// joined to the ledger queues each of its own writes here at the point it makes it, so the batch
// carries the transaction's statements, the document operations' and the route's, in the order
// they were made. It refuses a ledger no caller joined and a sealed one (ErrQueueSealed).
func (l *Ledger) Queue(step WriteStep) error {
	if l.tx == nil {
		return errUnjoined
	}
	if l.sealed {
		return ErrQueueSealed
	}
	l.queue = append(l.queue, step)
	return nil
}

// queueFor is the ledger whose write queue a write made in tx joins: ctx's, when it queues tx's
// writes (writer) and its write phase has not begun. Nil means the write runs at once: in a
// transaction no ledger queues (a durable writer's own, a test's), or while the write phase's
// batch is being built (batchFor).
func queueFor(ctx context.Context, tx pgx.Tx) *Ledger {
	ledger := ledgerFrom(ctx)
	if ledger == nil || ledger.writer == nil || ledger.sealed || ledger.writer != tx {
		return nil
	}
	return ledger
}

// batchFor is the write phase's batch of the ledger that queues tx's writes while runQueue builds
// it, nil at any other time.
func batchFor(ctx context.Context, tx pgx.Tx) *store.WriteBatch {
	ledger := ledgerFrom(ctx)
	if ledger == nil || ledger.writer != tx {
		return nil
	}
	return ledger.batch
}

// write runs step in tx: queued on ctx's ledger (queueFor), so its statements join the write
// phase's batch in the order the transaction made its writes; on that batch at once while the
// write phase builds it (batchFor); or else at once, as a batch of its own (store.RunWrite), where
// nothing queues it. Every write a document operation makes goes through it.
func write(ctx context.Context, tx pgx.Tx, step WriteStep) error {
	if ledger := queueFor(ctx, tx); ledger != nil {
		ledger.queue = append(ledger.queue, step)
		return nil
	}
	if batch := batchFor(ctx, tx); batch != nil {
		return step(batch)
	}
	return store.RunWrite(ctx, tx, step)
}

// execStep is a write step of one statement whose result nothing reads, its failure wrapped as
// what failed.
func execStep(what, sql string, args ...any) WriteStep {
	return func(b *store.WriteBatch) error {
		b.Queue(what, sql, args...)
		return nil
	}
}

// writeExec writes one statement whose result nothing reads (write, execStep).
func writeExec(ctx context.Context, tx pgx.Tx, what, sql string, args ...any) error {
	return write(ctx, tx, execStep(what, sql, args...))
}

// queueWrites marks ctx so the writes document code makes in tx queue (write) on a ledger of their
// own, for a background pass that decides everything before it writes anything: it does its reads
// and its work under the returned context, then runs the ledger's queue (runQueue) and commits.
// The ledger joins nothing and runs no document operation.
func queueWrites(ctx context.Context, tx pgx.Tx) (context.Context, *Ledger) {
	ledger := &Ledger{writer: tx}
	return withLedger(ctx, ledger), ledger
}

// runQueue runs the write phase. Each queued step puts its statements on one batch, in order, and
// then, when the caller passes it, after runs with a context under which the writes it makes in
// the ledger's transaction join the same batch (write, batchFor): Commit's settlement credit. The
// batch then goes to the server in one round trip (store.WriteBatch.Send), so the statements run
// back to back, and its results are read in order. The first statement or callback that fails -
// or a step that fails before it has queued its statements, once those queued before it have run
// - stops it, and runQueue returns that error as it came, so a caller still tells a typed error -
// a unique violation, ErrDocumentTooLarge, a guarded update's refusal - apart with errors.Is and
// errors.As. It seals the queue first.
func (l *Ledger) runQueue(ctx context.Context, after func(context.Context) error) error {
	l.sealed = true
	queue := l.queue
	l.queue = nil
	var batch store.WriteBatch
	for _, step := range queue {
		if err := step(&batch); err != nil {
			return batch.SendFailed(ctx, l.writer, err)
		}
	}
	if after != nil {
		l.batch = &batch
		err := after(withLedger(ctx, l))
		l.batch = nil
		if err != nil {
			return batch.SendFailed(ctx, l.writer, err)
		}
	}
	return batch.Send(ctx, l.writer)
}

// eventSink is where an event a document operation appends goes once appended: ctx's ledger, which
// publishes it after its commit (Ledger.Commit) and whose settlement reads it back, in the order
// the appends ran. It is taken when the operation runs, so a step appending later reaches the same
// ledger whatever context the write phase runs under.
func eventSink(ctx context.Context) func(model.Event) {
	ledger := ledgerFrom(ctx)
	return func(event model.Event) {
		if ledger != nil {
			ledger.events = append(ledger.events, event)
		}
	}
}

// Now is the transaction's now(): the time it started, which Postgres holds fixed for the whole
// transaction, read once. A row whose column defaults to now() is written with it explicitly by a
// queued insert, so a reader in the read phase knows the value the row will carry.
func (l *Ledger) Now(ctx context.Context) (time.Time, error) {
	if l.writer == nil {
		return time.Time{}, errUnjoined
	}
	if l.now == nil {
		now, err := readNow(ctx, l.writer)
		if err != nil {
			return time.Time{}, err
		}
		l.now = &now
	}
	return *l.now, nil
}

// transactionNow is tx's now(): through the ledger that queues tx's writes (Ledger.Now), which
// reads it once, or read at once.
func transactionNow(ctx context.Context, tx pgx.Tx) (time.Time, error) {
	if ledger := queueFor(ctx, tx); ledger != nil {
		return ledger.Now(ctx)
	}
	return readNow(ctx, tx)
}

// readNow reads the transaction's now() through q.
func readNow(ctx context.Context, q Queryer) (time.Time, error) {
	var now time.Time
	if err := q.QueryRow(ctx, `select now()`).Scan(&now); err != nil {
		return time.Time{}, fmt.Errorf("read the transaction's time: %w", err)
	}
	return now, nil
}

// NextVersion numbers the next version of artifactID this transaction writes and dates it: one
// past the document's latest version, counting the versions this transaction has numbered and not
// yet written, at the transaction's now() (Now). The caller holds the document owner's lock,
// which every writer of a version takes first, so no other transaction numbers one meanwhile. A
// route that queues a version insert of its own numbers it here, so the document service's
// versions in the same transaction follow it.
func (l *Ledger) NextVersion(ctx context.Context, artifactID string) (int, time.Time, error) {
	if l.writer == nil {
		return 0, time.Time{}, errUnjoined
	}
	number, numbered := l.numbers[artifactID]
	if !numbered {
		latest, now, err := latestStoredVersion(ctx, l.writer, artifactID)
		if err != nil {
			return 0, time.Time{}, err
		}
		number = latest
		if l.now == nil {
			l.now = &now
		}
	}
	number++
	if l.numbers == nil {
		l.numbers = make(map[string]int)
	}
	l.numbers[artifactID] = number
	return number, *l.now, nil
}

// pendingVersion is a version this transaction has queued and not yet written, as latestVersion
// reads a stored one.
type pendingVersion struct {
	version          model.Version
	markdown         string
	docUpdateVersion int64
}

// PendingVersionMarkdown is the markdown of version number of artifactID when it is the version
// this transaction last numbered and queued (writeVersionTx) and has not written yet: a route that
// reads the version it just made - the diff a named version's event carries - takes it from here,
// since the table holds the row only once Commit has run the queue. False when this transaction
// queued no such version.
func (l *Ledger) PendingVersionMarkdown(artifactID string, number int) (string, bool) {
	if l.sealed {
		return "", false
	}
	pending, queued := l.latest[artifactID]
	if !queued || pending.version.Number != number {
		return "", false
	}
	return pending.markdown, true
}

// queuedVersion records pending as the latest version of artifactID, which this transaction has
// queued and not yet written (latestVersion, PendingVersionMarkdown).
func (l *Ledger) queuedVersion(artifactID string, pending pendingVersion) {
	if l.latest == nil {
		l.latest = make(map[string]pendingVersion)
	}
	l.latest[artifactID] = pending
}

// queuedAuthors records authors as pending authors of artifactID this transaction has queued the
// write of (upsertPendingAuthors): until its write phase has run, doc_pending_authors holds no
// row of theirs, and the versions this transaction writes read them from here instead
// (transactionPendingAuthors). A queued delete of them (deleteCapturedPendingAuthors) is queued
// after the insert, so it takes the rows back out in the write phase; nothing of this transaction
// reads R between the two.
func (l *Ledger) queuedAuthors(artifactID string, authors []pendingAuthor) {
	if l.owedAuthors == nil {
		l.owedAuthors = make(map[string]map[string]model.Actor, 1)
	}
	owed := l.owedAuthors[artifactID]
	if owed == nil {
		owed = make(map[string]model.Actor, len(authors))
		l.owedAuthors[artifactID] = owed
	}
	for _, author := range authors {
		owed[actorKey(author.actor)] = author.actor
	}
}

// QueueVersion queues the insert of a document version the caller writes itself, outside the
// document service - an upload's, which lists its uploader alone, and a new issue's first version
// of its seeded spec - and returns the row as the insert will leave it. It is named when it has a
// summary. It is the version write every other version goes through (queueVersionInsert), run on
// this ledger whatever context the caller holds: the route that queues a version of its own passes
// the request's, not the one Join returned. It refuses a ledger no caller joined, and a sealed one
// (ErrQueueSealed) as Queue does: its insert would run after the statements it was meant to
// precede, or in no transaction at all.
func (l *Ledger) QueueVersion(ctx context.Context, artifactID, markdown string, authors []model.Actor, summary *string) (model.Version, error) {
	if l.writer == nil {
		return model.Version{}, errUnjoined
	}
	if l.sealed {
		return model.Version{}, ErrQueueSealed
	}
	return queueVersionInsert(withLedger(ctx, l), l.writer, artifactID, markdown, authors, summary != nil, summary, nil)
}

// pendingRoom is a document's stored history (doc_updates) as the writes this transaction has
// queued to it leave it. A transaction that queues a write to it holds its document lock from
// before it read the history to the end, so nothing else changes it meanwhile, and these are
// exact: head is the version the history stands at; recovered, the ceiling of an interrupted prune
// whose recovery the transaction queued, above which the table's rows are deleted first; rebuilt
// says a queued rebuild deleted every row the table holds; appended counts the rows queued since,
// appends and a rebuild's seed, and changed says one of them changed the document's content.
type pendingRoom struct {
	head      int64
	recovered *int64
	rebuilt   bool
	appended  int64
	changed   bool
}

// pendingRoomOf is room's pendingRoom in the ledger ctx queues tx's writes on, nil when there is
// no such ledger or it has queued no write to room's history.
func pendingRoomOf(ctx context.Context, tx pgx.Tx, room string) *pendingRoom {
	if ledger := queueFor(ctx, tx); ledger != nil {
		return ledger.rooms[room]
	}
	return nil
}

// PendingAnchor is an anchored ask or comment row the transaction writes in its queue - a row it
// creates, or one whose anchor or open state it changes - as the row will stand once written. The
// table holds none of that until Commit, so the transaction's anchor refreshes, which read the
// open anchors of a document (openAnchoredMarks), take it from here instead
// (RegisterPendingAnchor).
type PendingAnchor struct {
	// ID is the row's id, and Kind the mark it anchors: MarkAsk for an ask, MarkComment or
	// MarkSuggestion for a comment.
	ID   string
	Kind MarkKind
	// Anchor is the row's anchor; its ArtifactID names the document it anchors.
	Anchor model.Anchor
	// Open says whether a refresh refreshes the row: an open ask, an unresolved comment.
	Open bool
	// Event prepares the row's anchor-refresh event (ask.anchor_refreshed,
	// comment.anchor_refreshed). A refresh that moves the row's anchor calls it in the read phase,
	// in tx, to read what the event needs, and calls the builder it returns while the write phase
	// builds its batch, with the anchor refreshed to. That is before any queued statement has run,
	// so the builder uses only what the read phase read and does only the light work of a step.
	// Nil reads the row from the table in the read phase, as the refresh reads a stored row, which
	// only a row the table already holds can be.
	Event func(ctx context.Context, tx pgx.Tx) (func(refreshed model.Anchor) (model.Event, error), error)
}

// RegisterPendingAnchor records row, an anchored ask or comment row the caller has queued a write
// of, so every anchor refresh this transaction runs on row.Anchor.ArtifactID reads it as it will
// stand: a new open row is refreshed after the stored ones, a stored row the transaction changes is
// read as changed, and one it closes (Open false) is no longer refreshed. A later registration of
// the same row replaces the earlier one in its place.
func (l *Ledger) RegisterPendingAnchor(row PendingAnchor) {
	artifactID := row.Anchor.ArtifactID
	if l.anchors == nil {
		l.anchors = make(map[string][]*PendingAnchor)
	}
	for index, registered := range l.anchors[artifactID] {
		if registered.ID == row.ID {
			l.anchors[artifactID][index] = &row
			return
		}
	}
	l.anchors[artifactID] = append(l.anchors[artifactID], &row)
}

// pendingAnchorsOf is the anchored rows registered on ctx's ledger for artifactID
// (RegisterPendingAnchor), none once its write phase has begun, when the table holds them.
func pendingAnchorsOf(ctx context.Context, artifactID string) []*PendingAnchor {
	ledger := ledgerFrom(ctx)
	if ledger == nil || ledger.sealed {
		return nil
	}
	return ledger.anchors[artifactID]
}

// RegisterPendingArtifact records refKey as the reference key of artifactID, an artifacts row this
// transaction creates and has not written yet: its insert is queued with the transaction's other
// writes, or runs later in its write phase, while the document's own work names the artifact's id
// from the start, so a read of the document's reference here takes it from the ledger rather than
// from the row no read of this transaction can see (documentRefKey). The key is the one the row
// is written with, so it answers the same before and after the write phase has run.
func (l *Ledger) RegisterPendingArtifact(artifactID, refKey string) {
	if l.artifacts == nil {
		l.artifacts = make(map[string]string)
	}
	l.artifacts[artifactID] = refKey
}

// pendingRefKey is the reference key registered for artifactID on ctx's ledger
// (RegisterPendingArtifact), false when none is and the artifacts row is the only place to read it.
func pendingRefKey(ctx context.Context, artifactID string) (string, bool) {
	ledger := ledgerFrom(ctx)
	if ledger == nil {
		return "", false
	}
	refKey, pending := ledger.artifacts[artifactID]
	return refKey, pending
}

// pendingApproval is a document's open approval request as the writes this transaction has queued
// leave it: ask is the row as moved to the transaction's version (moveApprovalAsk), edited_at
// included, and closed says a queued answer closes it (carryApproval).
type pendingApproval struct {
	ask    *model.Ask
	closed bool
}

// recordPendingApproval records pending as artifactID's open approval request on the ledger ctx
// queues tx's writes on, when there is one: the caller has queued the write that leaves it so.
func recordPendingApproval(ctx context.Context, tx pgx.Tx, artifactID string, pending pendingApproval) {
	ledger := queueFor(ctx, tx)
	if ledger == nil {
		return
	}
	if ledger.approvals == nil {
		ledger.approvals = make(map[string]*pendingApproval)
	}
	ledger.approvals[artifactID] = &pending
}

// recordPendingReview records review as the newest review of artifactID on the ledger ctx queues
// tx's writes on: the caller has queued the insert that writes it (QueueReview), and a read of
// artifact_reviews before the write phase still answers the review before this one.
func recordPendingReview(ctx context.Context, tx pgx.Tx, artifactID string, review model.ArtifactReview) {
	ledger := queueFor(ctx, tx)
	if ledger == nil {
		return
	}
	if ledger.reviews == nil {
		ledger.reviews = make(map[string]model.ArtifactReview)
	}
	ledger.reviews[artifactID] = review
}

// PendingReview is the review of artifactID this transaction has queued and not yet written
// (recordPendingReview): a review it queued is newer than every stored one, so a read of the
// document's newest review before Commit takes it from here. False when this transaction queued
// none, and once its write phase has begun, when the table holds it.
func (l *Ledger) PendingReview(artifactID string) (model.ArtifactReview, bool) {
	if l == nil || l.sealed {
		return model.ArtifactReview{}, false
	}
	review, queued := l.reviews[artifactID]
	return review, queued
}

type ledgerVersion struct {
	artifactID string
	version    model.Version
	// capture is whom the version listed (authorCapture): the pending authors its commit deletes
	// and the in-flight credits it marks consumed.
	capture authorCapture
}

// ledgerSeed is a document SeedText wrote in this transaction: the actor who wrote its first
// text, and the tree it wrote with that tree's rendering and its blocks' byte ranges. Credit
// registers the actor against every ask block the tree introduced (registerAskAuthors) once the
// transaction has committed, and the seed's version cuts its mentions' excerpts from the
// rendering.
type ledgerSeed struct {
	actor    model.Actor
	rendered renderedTree
}

type ledgerContextKey struct{}

// errUnjoined refuses a transactional operation called outside Join: its writes would reach no
// ledger, so their authors would never be credited or released.
var errUnjoined = errors.New("docs: the operation needs a transaction joined with Service.Join")

// Join joins the document operations run with the returned context to tx, recording what they
// produce in the returned ledger and queueing their writes for its Commit.
func (s *Service) Join(ctx context.Context, tx pgx.Tx) (context.Context, *Ledger) {
	ledger := &Ledger{service: s, tx: tx, writer: tx}
	return withLedger(ctx, ledger), ledger
}

// inTransaction reports whether the operations recording into l run inside an open database
// transaction that this ledger knows about: one a caller joined, or a writer's own, a
// settlement's or the block-id backfill's. Such an operation fails on a failed room rather than
// waiting for its recovery (awaitRoomRecovery). An operation that runs inside a transaction
// without joining it is invisible here and still waits, which is the hang this rule exists to
// stop, so a handler joins (Docs.Join) and passes the context Join returned.
func (l *Ledger) inTransaction() bool {
	return l != nil && (l.tx != nil || l.ownTx)
}

func withLedger(ctx context.Context, ledger *Ledger) context.Context {
	return context.WithValue(ctx, ledgerContextKey{}, ledger)
}

func ledgerFrom(ctx context.Context) *Ledger {
	ledger, _ := ctx.Value(ledgerContextKey{}).(*Ledger)
	return ledger
}

func txFromContext(ctx context.Context) (pgx.Tx, bool) {
	ledger := ledgerFrom(ctx)
	if ledger == nil || ledger.tx == nil {
		return nil, false
	}
	return ledger.tx, true
}

// Commit runs the transaction's write phase (its queue, one batch with the settlement credit's
// statements after it) and commits it, then credits, releases and publishes what its document
// operations recorded (see Ledger), and last publishes the events they appended, so a caller's
// own events, published after Commit returns, follow them. A statement of the write phase that
// fails, or a callback that refuses its result, stops it: the transaction rolls back, nothing
// reaches a room, and Commit returns the first such error unchanged. When the commit itself
// returns an error its outcome is unknown, so the rooms the transaction wrote are failed and
// reload the durable document.
func (l *Ledger) Commit(ctx context.Context) error {
	if err := l.commit(ctx); err != nil {
		return err
	}
	l.publish()
	l.publishEvents()
	for _, copy := range l.copies {
		l.service.scheduleSettle(copy)
	}
	return nil
}

// publishEvents publishes the events this ledger's document operations appended, in the order
// they appended them.
func (l *Ledger) publishEvents() {
	for _, event := range l.events {
		l.service.events.Publish(event)
	}
}

// commit is Commit up to the publish. Another transaction can run between the two, and tests
// call them apart to hold that window open.
func (l *Ledger) commit(ctx context.Context) error {
	if err := l.runQueue(ctx, l.recordSettlementCredit); err != nil {
		// The transaction rolls back, so its writes reached neither the store nor a room: what it
		// leaves behind is what a transaction that never committed leaves (Discard).
		_ = l.tx.Rollback(context.Background())
		l.Discard()
		return err
	}
	captures := make([]roomCapture, 0, len(l.versions)+len(l.flushes)+len(l.order))
	for _, written := range l.versions {
		captures = append(captures, roomCapture{room: written.artifactID, capture: written.capture})
	}
	for _, flush := range l.flushes {
		captures = append(captures, flushCapture(flush))
	}
	// Each write leaves its room a shadow (roomShadow), installed by the commit under its state's
	// lock, after the flushes' classes are marked stored.
	for _, artifactID := range l.order {
		write := l.live[artifactID]
		if shadow := write.newShadow(); shadow != nil {
			captures = append(captures, roomCapture{room: artifactID, capture: authorCapture{state: write.state}, shadow: shadow})
		}
	}
	// The cursors before the commit, not after it: this transaction holds the document's advisory
	// lock until the commit drops it, and the writers queued on that lock take it the instant it
	// goes. Recorded after the commit, a browser's append would read the head this commit wrote
	// against a cursor still standing one version behind it and be refused (ErrRoomBehindHead),
	// which fails a healthy room over this process's own write and drops the browser's edit with
	// it. Nothing can read a cursor standing at a version this commit has not yet made durable
	// and write durable state from it: every durable writer of the document takes that lock
	// first, and the stale-room evictor, which reads the cursor to decide whether to close the
	// room, closes none this write is open on (documentChanged, hasOpenLiveWrite). A commit that
	// fails fails these rooms (fail), so the instance holding such a cursor is dropped with them.
	l.takeRoomCursors()
	err := l.service.commitConsuming(ctx, l.tx, captures, func(error) {
		// The transaction has ended whichever way the commit went, so a rebuild's room reads the
		// history the commit left from here.
		l.endRebuilds()
	})
	if err == nil && l.service.afterLiveCommit != nil {
		for _, artifactID := range l.order {
			l.service.afterLiveCommit(artifactID)
		}
	}
	l.versions = nil
	if err != nil {
		l.fail(err)
		l.flushes = nil
		return err
	}
	for _, flush := range l.flushes {
		if flush.storedContentChange() {
			l.service.scheduleSettleAfterAppend(flush.room)
		}
	}
	l.flushes = nil
	l.creditRooms()
	return nil
}

// takeRoomCursors records on each room instance a write was made on, and on the room instance its
// publish will reach, the versions this commit is about to make durable, before the commit that
// does (commit) and the publish that applies them to the room (publish). The room does not hold
// them yet, and nothing in that window renders from it: the write holds its writer slot until the
// publish has returned, which defers its room's settlement (settleRoomWithin's supersession
// check) and holds off every other transaction's write and version (awaitLiveWriter). What does
// run in that window is a browser's append queued on the document's advisory lock, which takes it
// the instant this commit drops it: refused there, this process's own committed write would fail
// its own room and drop the browser's edit (TestSettlementBetweenCommitAndPublishWaitsForTheWrite,
// TestABrowserAppendQueuedBehindThisProcessesWriteIsNotRefused). The stale-room evictor reads the
// cursor, so a room left standing at the head here is also a room this process's own change does
// not close (documentChanged, whose other guard is hasOpenLiveWrite).
//
// The two instances are one and the same for a warm write whose room is still resident. They are
// not for a cold write, which has no cursor of its own at all (coldFork): the room it publishes
// into is one that loaded, or is loading, while the write ran, and advancing that room here is
// the only thing that keeps it at the head across this window - a room still loading has no
// document for ygo to hand out, so the version goes to the record its load leaves the instance
// (tookResidentVersion, roomLoad). They are not one for a write whose room was replaced since its
// fork either, where the fork's cursor is the gone instance's. A room that loaded at or past
// these versions takes nothing in - advanceRoomCursor only advances by the version that follows
// the one it stands at - and the publish records them again on the room it reaches
// (publishLiveUpdate).
//
// The room updates the transaction flushed (flushThrough) are already in the instance they were
// made in, and only there: they advance that instance's cursor and reach no other, as a repair's
// append does (advanceRoomCursor). A flush and a write of one instance interleave their versions
// (an operation's flush, its append, the next operation's flush), so the instance takes one run
// of both, in version order (RoomFlush.appended): a cursor advances only by the version after
// the one it stands at. A write whose room is not the flushed instance - a cold write, or one
// whose room was replaced since its fork - takes in its own versions alone. Which of the two a
// write is is decided in one place, the loop over the writes below: a flush of that write's
// document made on the instance it wrote takes the write's versions in with its own, and a
// flush no write of this transaction shares an instance with takes in its own alone.
func (l *Ledger) takeRoomCursors() {
	taken := make(map[*RoomFlush]bool, len(l.flushes))
	for _, artifactID := range l.order {
		write := l.live[artifactID]
		if flush := l.flushFor(artifactID); flush != nil && flush.cursor == write.point.cursor {
			taken[flush] = true
			advanceRoomCursorThrough(flush.cursor, flush.appended(write.versions))
		} else {
			advanceRoomCursorThrough(write.point.cursor, write.versions)
		}
		for _, version := range write.versions {
			l.service.tookResidentVersion(artifactID, version)
		}
	}
	for _, flush := range l.flushes {
		if !taken[flush] {
			advanceRoomCursorThrough(flush.cursor, flush.appended(nil))
		}
	}
}

// roomCapture is a version's capture with the document it versions, the room updates a
// transaction flushed (flush), or the shadow a live write leaves its room (shadow); the last two
// hold only the state, which the commit locks.
type roomCapture struct {
	room    string
	capture authorCapture
	flush   *RoomFlush
	shadow  *roomShadow
}

// flushCapture is the roomCapture of flush, which locks the state flush took its updates from.
func flushCapture(flush *RoomFlush) roomCapture {
	return roomCapture{room: flush.room, capture: authorCapture{state: flush.state}, flush: flush}
}

// commitConsuming commits tx, which wrote the versions captures lists and the room updates their
// flushes stored, and once it has committed marks the in-flight credits each version listed
// consumed and each flushed update stored, its credit held and landed (RoomFlush.markStored), frees
// a shadow left with nothing to measure (dropDrainedShadowLocked), and installs each live write's
// shadow (installShadowLocked), in captures' order. Each capture's state is locked (state.mu) from
// before the commit until then: an append queued behind the document's advisory lock, which tx
// holds until it commits, takes that lock the instant the commit drops it, and must find its
// credit consumed when it reads it (UpdateCredit.take), or it writes an author a version listed
// to the pending authors; and a durable writer that takes it must find the updates this commit
// stored marked, or it stores them again, and the write's shadow installed, or it stores a class
// measured without the write (flushThrough). The states are locked in their documents' sorted
// order, so two transactions versioning the same two documents cannot deadlock. ended runs once
// the commit returns, with its error and the states still locked; a capture with no state locks
// nothing. A commit that fails marks nothing: the room's persistence stores the flushed updates
// later.
func (s *Service) commitConsuming(ctx context.Context, tx pgx.Tx, captures []roomCapture, ended func(error)) error {
	sorted := slices.Clone(captures)
	slices.SortStableFunc(sorted, func(a, b roomCapture) int { return strings.Compare(a.room, b.room) })
	locked := make([]roomCapture, 0, len(sorted))
	holds := func(state *roomState) bool {
		return slices.ContainsFunc(locked, func(held roomCapture) bool { return held.capture.state == state })
	}
	for _, written := range sorted {
		if state := written.capture.state; state != nil && !holds(state) {
			state.mu.Lock()
			locked = append(locked, written)
		}
	}
	err := tx.Commit(ctx)
	var unlanded []*UpdateCredit
	if err == nil {
		for _, written := range captures {
			written.capture.consumeLocked()
			if written.flush != nil {
				unlanded = append(unlanded, written.flush.markStored(holds)...)
				if state := written.capture.state; state != nil {
					state.dropDrainedShadowLocked()
				}
			}
			if written.shadow != nil {
				s.installShadowLocked(written.room, written.capture.state, written.shadow)
			}
		}
	}
	if ended != nil {
		ended(err)
	}
	for _, held := range locked {
		s.unlockState(held.room, held.capture.state)
	}
	for _, credit := range unlanded {
		credit.landed()
	}
	return err
}

// Discard drops what a transaction that did not commit left behind: its queued writes, which never
// ran, its live writes, which no room ever saw, and the rooms its rebuilds held. A version it
// wrote marked nothing it read, so its authors stay where they were. After Commit it does nothing.
func (l *Ledger) Discard() {
	l.sealed = true
	l.queue = nil
	for _, artifactID := range l.order {
		l.service.finishLiveWrite(l.live[artifactID])
	}
	l.versions = nil
	l.flushes = nil
	l.endRebuilds()
}

func (l *Ledger) holdRebuild(artifactID string) {
	l.rebuilds = append(l.rebuilds, artifactID)
}

func (l *Ledger) endRebuilds() {
	for _, artifactID := range l.rebuilds {
		l.service.rebuilding.Delete(artifactID)
	}
	l.rebuilds = nil
}

// recordVersion records a version this transaction wrote, with whom it listed (capture), which
// its commit takes out of the pending authors (recordSettlementCredit, commitConsuming). The
// version holds every change the transaction's write to the document has made so far, so the
// authors it lists leave the write's credits: a change the write makes after it credits its author
// again (creditLiveWrite), and that author is pending once the transaction commits.
func (l *Ledger) recordVersion(artifactID string, version model.Version, capture authorCapture) {
	l.versions = append(l.versions, ledgerVersion{artifactID: artifactID, version: version, capture: capture})
	if write := l.liveWriteFor(artifactID); write != nil {
		for _, author := range version.Authors {
			delete(write.credits, actorKey(author))
			delete(write.creditedBy, actorKey(author))
		}
	}
}

// WroteVersion records version, of artifactID, which the caller wrote itself in this transaction,
// outside the document service, over the transaction's own write to the document - an upload,
// whose version lists its uploader alone. Once this transaction commits, it has cleared every
// author whose change the write's last read of the room held, whether its replacement removed
// that edit or kept it (uploadCapture): each pending author row whose writing update that room
// held, and each in-flight credit observed by then. The write held the document's advisory lock
// from before that read (ReplaceText), so nothing landed between them. A row another process's
// room stored, and a credit observed after the read, stay pending for the next version. An upload
// that changed nothing has no write to hold and nothing to clear.
func (l *Ledger) WroteVersion(artifactID string, version model.Version) {
	write := l.liveWriteFor(artifactID)
	if write == nil || len(write.updates) == 0 {
		return
	}
	l.recordVersion(artifactID, version, uploadCapture(write))
}

func (l *Ledger) liveWriteFor(artifactID string) *liveWrite {
	if l == nil {
		return nil
	}
	return l.live[artifactID]
}

// LostOps reports which operations of this transaction's write to artifactID the live document
// no longer held once the write was published, which the caller reads after Commit. The second
// return is false when no verdict was reached - the transaction wrote nothing to that document,
// its publish failed and the room is reloading, or the room holds a tree past the schema's depth
// bound, which no read can serve - which the caller reports as undetermined rather than as
// survival (LEGION-269).
func (l *Ledger) LostOps(artifactID string) ([]int, bool) {
	write := l.liveWriteFor(artifactID)
	if write == nil || !write.lostVerdict {
		return nil, false
	}
	return write.lost, true
}

func (l *Ledger) addLiveWrite(write *liveWrite) {
	if l.live == nil {
		l.live = make(map[string]*liveWrite)
	}
	l.live[write.artifactID] = write
	l.order = append(l.order, write.artifactID)
	write.flushed = l.flushFor(write.artifactID)
}

// recordSettlementCredit records, in the transaction that wrote the documents, who owes what once
// it commits. It first deletes the pending authors each version this transaction wrote takes out
// (deleteCapturedPendingAuthors), then records, in one statement per write, each write's authors no
// version of the transaction lists (recordVersion), each at the append that recorded that author's
// latest change (liveWrite.creditedBy), and each write's and seed's latest edit source on the
// document's pending-settlement row. Deleting first means the write's own authors are recorded
// after the delete that would otherwise take them out. Both commit or roll back with the content,
// before the request context can be canceled after commit. A transaction whose every write was a
// server lifecycle write (withLifecycleWrite) credited no one, so it records nothing here. Commit
// runs it as its write phase's last part (runQueue), so its statements join that batch after every
// queued write: everything they write is known in memory by then.
func (l *Ledger) recordSettlementCredit(ctx context.Context) error {
	for _, written := range l.versions {
		if err := deleteCapturedPendingAuthors(ctx, l.tx, written.artifactID, written.capture); err != nil {
			return err
		}
	}
	released := l.releasedAuthors()
	for artifactID, seed := range l.seeds {
		if _, listed := released[artifactID][actorKey(seed.actor)]; listed {
			continue
		}
		if err := recordLatestEditSource(ctx, l.tx, artifactID, &seed.actor); err != nil {
			return err
		}
	}
	for _, artifactID := range l.order {
		write := l.live[artifactID]
		if write.actor == nil {
			continue
		}
		pending := pendingAuthorsOf(write.credits, func(key string) persistence.Version { return write.creditedBy[key] })
		if err := upsertPendingAuthors(ctx, l.tx, artifactID, pending); err != nil {
			return err
		}
		if err := recordLatestEditSource(ctx, l.tx, artifactID, write.actor); err != nil {
			return err
		}
	}
	return nil
}

// releasedAuthors is, per artifact, the keys of the authors this transaction's own versions
// listed, which a seed of the same transaction does not record as the latest edit source again.
func (l *Ledger) releasedAuthors() map[string]map[string]struct{} {
	released := make(map[string]map[string]struct{}, len(l.versions))
	for _, written := range l.versions {
		keys := released[written.artifactID]
		if keys == nil {
			keys = make(map[string]struct{}, len(written.version.Authors))
			released[written.artifactID] = keys
		}
		for _, actor := range written.version.Authors {
			keys[actorKey(actor)] = struct{}{}
		}
	}
	return released
}

// creditRooms records, once the transaction has committed and before its live writes publish,
// each seed's and write's latest edit source in its room, which the settlement it arms names on
// its events, and registers the ask blocks each introduced to its author. The authors themselves
// are already recorded in the database (recordSettlementCredit).
func (l *Ledger) creditRooms() {
	for artifactID, seed := range l.seeds {
		state := l.service.lockState(artifactID)
		state.creditSeq.Add(1)
		state.lastActor = new(seed.actor)
		state.unsettled = true
		state.registerAskAuthors(askBlockIDs(seed.rendered.tree), seed.actor)
		l.service.unlockState(artifactID, state)
	}
	for _, artifactID := range l.order {
		write := l.live[artifactID]
		if write.actor == nil {
			continue
		}
		state := l.service.lockState(artifactID)
		state.creditSeq.Add(1)
		state.lastActor = write.actor
		state.unsettled = true
		if write.actor != nil {
			// The carried-forward author of a renamed id is registered before the ids this
			// write's own before/after diff adds, so a rename's more specific registration is
			// never overwritten by the less specific one applyLive's diff would otherwise give
			// it (registerAskAuthors, registerCarriedAskAuthors).
			state.registerCarriedAskAuthors(write.carriedAskAuthors)
			state.registerAskAuthors(write.addedAskBlockIDs, *write.actor)
		}
		l.service.unlockState(artifactID, state)
	}
}

// seeded records that this transaction seeded artifactID's first text as actor (SeedText), with
// the rendering of the tree it wrote, whose ask blocks credit registers to actor once the
// transaction commits.
func (l *Ledger) seeded(artifactID string, rendered renderedTree, actor model.Actor) {
	if l.seeds == nil {
		l.seeds = make(map[string]ledgerSeed)
	}
	l.seeds[artifactID] = ledgerSeed{actor: actor, rendered: rendered}
}

// writtenRendering is the rendering of the tree this transaction last wrote to artifactID: the one
// its live write's latest operation recorded (recordLiveOperation, for applyLive and applyMargin
// alike), else the tree it seeded (SeedText); none when it holds neither.
func (l *Ledger) writtenRendering(artifactID string) renderedTree {
	if l == nil {
		return renderedTree{}
	}
	if write := l.liveWriteFor(artifactID); write != nil && write.tree != nil {
		return renderedTree{tree: write.tree, markdown: write.markdown, offsets: write.offsets}
	}
	return l.seeds[artifactID].rendered
}

// publish applies the committed transaction's live writes to their rooms and broadcasts them.
// Each update is already durable, so the room's own persistence of it is suppressed. A room that
// cannot take its update is failed, and reloads the durable document on its next access.
func (l *Ledger) publish() {
	for _, artifactID := range l.order {
		if write := l.live[artifactID]; !write.finished {
			l.service.publishLiveWrite(write)
		}
	}
}

// fail fails the rooms a transaction wrote when its commit returned an error. The commit may
// have gone through, so the rooms cannot tell whether they should hold the writes, or the room
// updates the transaction flushed, whose versions their cursors already took in (takeRoomCursors);
// failed, they reload the durable document, whichever way the commit went.
func (l *Ledger) fail(cause error) {
	for _, flush := range l.flushes {
		if write := l.live[flush.room]; len(flush.versions) > 0 && (write == nil || len(write.updates) == 0) {
			l.service.failRoom(flush.room, fmt.Errorf("commit flushed document updates: %w", cause))
		}
	}
	for _, artifactID := range l.order {
		write := l.live[artifactID]
		if write.finished {
			continue
		}
		if len(write.updates) > 0 {
			l.service.failRoom(artifactID, fmt.Errorf("commit live document write: %w", cause))
		}
		l.service.finishLiveWrite(write)
	}
}
