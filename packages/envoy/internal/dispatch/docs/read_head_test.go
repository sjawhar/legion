package docs

import (
	"context"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/sjawhar/envoy/internal/dispatch/model"
	"github.com/sjawhar/envoy/internal/dispatch/store"
	"github.com/sjawhar/envoy/internal/dispatch/store/storetest"
)

// A read is never older than the stored head, nor than the room (plan D8): a resident room whose
// cursor stands at the head is read from memory, and one behind it - another process stored an
// update its catch-up has not applied yet - is read from the stored document merged with the
// updates only that room holds, without waiting for that catch-up. The tests here run two
// services on one database, as production's two tasks are, and hold the reading process's
// catch-up (catchUpGate) so its room stays behind for as long as a test reads it.

// catchUpGate holds a service's catch-up of one document after its read of the store and before it
// applies what it read (roomCatchUps.afterRead), while held: the room stays behind the head with
// the catch-up in flight.
type catchUpGate struct {
	room    string
	entered chan struct{}

	mu      sync.Mutex
	release chan struct{}
}

// holdCatchUps installs a gate, open, on service's catch-up of room. It must run before the room
// sync starts, which reads the hook from its own goroutine.
func holdCatchUps(service *Service, room string) *catchUpGate {
	gate := &catchUpGate{room: room, entered: make(chan struct{}, 16)}
	service.catchUps.afterRead = gate.wait
	return gate
}

func (g *catchUpGate) wait(room string) {
	if room != g.room {
		return
	}
	g.mu.Lock()
	release := g.release
	g.mu.Unlock()
	if release == nil {
		return
	}
	select {
	case g.entered <- struct{}{}:
	default:
	}
	<-release
}

// hold makes every later catch-up of the gate's room wait until open.
func (g *catchUpGate) hold() {
	g.mu.Lock()
	defer g.mu.Unlock()
	if g.release == nil {
		g.release = make(chan struct{})
	}
}

// open lets every held catch-up go on, and every later one run.
func (g *catchUpGate) open() {
	g.mu.Lock()
	defer g.mu.Unlock()
	if g.release != nil {
		close(g.release)
		g.release = nil
	}
}

// awaitHeld waits until a catch-up of the gate's room has read the store and is held.
func (g *catchUpGate) awaitHeld(t *testing.T, what string) {
	t.Helper()
	select {
	case <-g.entered:
	case <-time.After(roomWait(t)):
		t.Fatalf("no catch-up of the reader's room read the store after %s within %v", what, roomWait(t))
	}
}

// readPair is one document served by two processes: writer, which holds no room for it to begin
// with, and reader, on a pool of its own, whose room is resident, whose catch-up a test can hold,
// and whose decodes of the stored document (LoadDocument, a cold read's and storedTree's) loads
// counts. A room load reads the store through Load, which it does not count.
type readPair struct {
	database   *store.Store
	artifactID string
	writer     *Service
	reader     *Service
	loads      *loadCountingStore
	gate       *catchUpGate
}

func newReadPair(t *testing.T) readPair {
	t.Helper()
	database := storetest.Open(t)
	artifactID := createDocument(t, database, "# First")
	writer, _ := newTestServiceInstance(t, database)
	second := storetest.SecondProcess(t, database)
	loads := &loadCountingStore{VersionedStore: NewPgVersioned(second)}
	reader := newReadCacheService(t, second, loads)
	seedServiceText(t, writer, artifactID, "The quick brown fox.\n\nSecond.\n")
	settleCurrentGeneration(t, writer, artifactID)

	gate := holdCatchUps(reader, artifactID)
	if err := reader.warmLiveDocument(context.Background(), artifactID); err != nil {
		t.Fatalf("load the reader's room: %v", err)
	}
	// The sweep after the first LISTEN catches the room up, with the gate open.
	runRoomSync(t, newRoomSync(reader))
	// Runs before the room sync stops and the services shut down: a catch-up held then would
	// keep the lane, and so both, from returning.
	t.Cleanup(gate.open)
	waitFor(t, roomWait(t), "the reader's room to stand at the head", func() bool {
		return !roomBehind(t, reader, artifactID)
	})
	return readPair{database: database, artifactID: artifactID, writer: writer, reader: reader, loads: loads, gate: gate}
}

// leaveReaderBehind holds the reader's catch-up, runs write through the writer, and returns once
// the reader's catch-up has read what write stored and is held before applying it: the reader's
// room is behind the stored head.
func (pair readPair) leaveReaderBehind(t *testing.T, what string, write func()) {
	t.Helper()
	pair.gate.hold()
	write()
	pair.gate.awaitHeld(t, what)
	if !roomBehind(t, pair.reader, pair.artifactID) {
		t.Fatalf("the reader's room stands at the head after %s, so nothing here is tested", what)
	}
}

// joinedRead runs read inside a transaction of the reader's pool joined to the reader, on a context
// the pool tracks, as an API request's: a second connection of that pool taken under the
// transaction is refused (store.ErrNestedAcquire), not merely slow.
func (pair readPair) joinedRead(t *testing.T, read func(ctx context.Context) error) error {
	t.Helper()
	ctx := store.WithTransactionTracking(context.Background())
	tx, err := pair.reader.store.Pool.Begin(ctx)
	if err != nil {
		t.Fatalf("begin the read's transaction: %v", err)
	}
	defer tx.Rollback(ctx)
	joined, ledger := pair.reader.Join(ctx, tx)
	defer ledger.Discard()
	return read(joined)
}

// typeIntoTheRoomBehindTheHead has a browser type text into the reader's room while no append of
// it can land: a transaction of its own holds the document's advisory lock, which the append
// takes, so the room holds a paragraph the store has not taken. land lets that append through and
// waits for it; it runs at the test's end when the test does not call it.
func (pair readPair) typeIntoTheRoomBehindTheHead(t *testing.T, text string) (land func()) {
	t.Helper()
	ctx := context.Background()
	locked, err := pair.database.Pool.Begin(ctx)
	if err != nil {
		t.Fatalf("begin the transaction holding the document's lock: %v", err)
	}
	var once sync.Once
	release := func() { once.Do(func() { _ = locked.Rollback(ctx) }) }
	t.Cleanup(release)
	if err := lockDocumentRoom(ctx, locked, pair.artifactID); err != nil {
		t.Fatalf("hold the document's lock: %v", err)
	}
	if err := browserStyleWrite(pair.reader, pair.artifactID, appendBlocks(t, text+"\n"), make(chan struct{})); err != nil {
		t.Fatalf("the browser's edit in the room behind the head: %v", err)
	}
	if stored := storedMarkdown(t, pair.database, pair.artifactID); strings.Contains(stored, text) {
		t.Fatalf("the stored document = %q, want it without the keystroke whose append is held", stored)
	}
	return func() {
		t.Helper()
		release()
		waitForLandedAppends(t, pair.reader, pair.artifactID)
	}
}

// A read on the reader right after a write through the writer, while the reader's catch-up of it is
// held, shows the write: the room is behind the head, so the read takes the stored document, and
// the room stays behind and sound, since no read waits for or forces the catch-up. Both read paths
// do (readDocument through heldTree, and docTree), inside a joined transaction as well as outside
// one. Once the catch-up lands, the same reads come from the room, with no decode of the store.
func TestAReadOfARoomBehindTheStoredHeadReadsTheStoreAndOfACurrentOneReadsTheRoom(t *testing.T) {
	pair := newReadPair(t)
	ctx := context.Background()
	room := pair.reader.srv.GetDoc(pair.artifactID)
	const written = "Second, written on the other process."

	pair.leaveReaderBehind(t, "the writer's edit", func() {
		if _, err := joinedApplyOps(pair.writer, pair.artifactID, []model.EditOp{
			{Op: "replace", Find: "Second.", With: written},
		}, model.Actor{Kind: "session", ID: "session-0123456789abcdef"}, nil); err != nil {
			t.Fatalf("edit through the writer: %v", err)
		}
	})

	before := pair.loads.loads.Load()
	text, token, err := pair.reader.TextWithToken(ctx, pair.artifactID)
	if err != nil || !strings.Contains(text, written) {
		t.Fatalf("the reader's read right after the writer's edit = %q, %v; want the edit, which the store holds", text, err)
	}
	if storedText, storedToken := storedMarkdown(t, pair.database, pair.artifactID), tokenOfStored(t, pair.database, pair.artifactID); text != storedText || token != storedToken {
		t.Fatalf("the reader's read = %q (token %s), want the stored document %q (token %s)", text, token, storedText, storedToken)
	}
	if _, err := pair.reader.BlockForQuote(ctx, pair.artifactID, written); err != nil {
		t.Fatalf("docTree on the reader right after the writer's edit: %v; want the block holding the edit", err)
	}
	if err := pair.joinedRead(t, func(ctx context.Context) error {
		joinedText, err := pair.reader.Text(ctx, pair.artifactID)
		if err == nil && !strings.Contains(joinedText, written) {
			t.Errorf("the reader's joined read right after the writer's edit = %q; want the edit", joinedText)
		}
		if err != nil {
			return err
		}
		_, err = pair.reader.BlockForQuote(ctx, pair.artifactID, written)
		return err
	}); err != nil {
		t.Fatalf("the reader's joined reads right after the writer's edit: %v", err)
	}
	if decoded := pair.loads.loads.Load() - before; decoded != 4 {
		t.Fatalf("the four reads of a room behind the head decoded the stored document %d times, want 4: each reads the store", decoded)
	}
	if !roomBehind(t, pair.reader, pair.artifactID) {
		t.Fatal("the reader's room stands at the head while its catch-up is held: a read forced it")
	}
	if pair.reader.srv.GetDoc(pair.artifactID) != room {
		t.Fatal("a read replaced the reader's room behind the head")
	}
	if err := pair.reader.roomFailure(pair.artifactID); err != nil {
		t.Fatalf("a read failed the reader's room behind the head: %v", err)
	}

	pair.gate.open()
	waitFor(t, roomWait(t), "the reader's room to catch up", func() bool {
		return !roomBehind(t, pair.reader, pair.artifactID)
	})
	before = pair.loads.loads.Load()
	if text, err := pair.reader.Text(ctx, pair.artifactID); err != nil || !strings.Contains(text, written) {
		t.Fatalf("the reader's read once caught up = %q, %v; want the edit", text, err)
	}
	if _, err := pair.reader.BlockForQuote(ctx, pair.artifactID, written); err != nil {
		t.Fatalf("docTree on the reader once caught up: %v", err)
	}
	if err := pair.joinedRead(t, func(ctx context.Context) error {
		_, err := pair.reader.Text(ctx, pair.artifactID)
		return err
	}); err != nil {
		t.Fatalf("the reader's joined read once caught up: %v", err)
	}
	if decoded := pair.loads.loads.Load() - before; decoded != 0 {
		t.Fatalf("reads of a room at the head decoded the stored document %d times, want none: they read the room", decoded)
	}
	if pair.reader.srv.GetDoc(pair.artifactID) != room {
		t.Fatal("the reader's room was replaced rather than caught up")
	}
}

// A read of a room behind the stored head that holds an update of its own the store has not taken
// is older than neither of them: it reads the stored document, which holds what another process
// committed, merged with that update, which only the room holds (storedWithUpdates). The room
// alone would answer without the other process's committed write; the stored document alone would
// answer without text this process has already sent the browser that typed it. Once the room's own
// append has landed, the store holds both and the read is the stored document itself.
func TestAReadOfARoomBehindTheHeadHoldingItsOwnUpdateReadsTheStoreAndThatUpdate(t *testing.T) {
	pair := newReadPair(t)
	ctx := context.Background()
	const written = "Second, written on the other process."
	const typed = "Typed into the room behind the head."

	pair.leaveReaderBehind(t, "the writer's edit", func() {
		if _, err := joinedApplyOps(pair.writer, pair.artifactID, []model.EditOp{
			{Op: "replace", Find: "Second.", With: written},
		}, model.Actor{Kind: "session", ID: "session-0123456789abcdef"}, nil); err != nil {
			t.Fatalf("edit through the writer: %v", err)
		}
	})
	land := pair.typeIntoTheRoomBehindTheHead(t, typed)

	text, err := pair.reader.Text(ctx, pair.artifactID)
	if err != nil {
		t.Fatalf("the reader's read while its room holds an unstored keystroke: %v", err)
	}
	for _, want := range []string{typed, written} {
		if !strings.Contains(text, want) {
			t.Fatalf("the reader's read while its room holds an unstored keystroke = %q, want %q: the read is older than neither the room nor the store", text, want)
		}
	}
	for _, quote := range []string{typed, written} {
		if _, err := pair.reader.BlockForQuote(ctx, pair.artifactID, quote); err != nil {
			t.Fatalf("docTree on the room holding an unstored keystroke, asked for %q: %v; want its block", quote, err)
		}
	}
	if !roomBehind(t, pair.reader, pair.artifactID) {
		t.Fatal("the reader's room stands at the head while its catch-up is held: a read forced it")
	}

	// The append lands once the lock is let go, and the store, which then holds the keystroke and
	// the writer's edit both, is the whole of what the next read of the room behind the head takes.
	land()
	text, err = pair.reader.Text(ctx, pair.artifactID)
	if err != nil {
		t.Fatalf("the reader's read once its append landed: %v", err)
	}
	for _, want := range []string{typed, written} {
		if !strings.Contains(text, want) {
			t.Fatalf("the reader's read once its append landed = %q, want %q", text, want)
		}
	}
}

// A mark a browser on the writer's process placed and stored is verified on the reader at once,
// inside a joined transaction as an anchored comment's is, while the reader's catch-up of it is
// held: the look reads the store, so it neither waits out MarkWait nor answers ErrAnchorMissing.
// A mark stored while VerifyMark waits wakes it through the reader's room once the catch-up
// applies it.
func TestVerifyMarkOnTheOtherProcessSeesAMarkTheWriterStored(t *testing.T) {
	pair := newReadPair(t)
	pair.reader.markWait = roomWait(t)

	pair.leaveReaderBehind(t, "the writer's mark", func() {
		browserMark(t, pair.writer, pair.artifactID, "proofComment", "held-mark", "quick")
		waitForLandedAppends(t, pair.writer, pair.artifactID)
	})
	before := pair.loads.loads.Load()
	started := time.Now()
	var anchored Anchored
	if err := pair.joinedRead(t, func(ctx context.Context) (err error) {
		anchored, err = pair.reader.VerifyMark(ctx, pair.artifactID, MarkComment, "held-mark")
		return err
	}); err != nil || anchored.Quote != "quick" {
		t.Fatalf("VerifyMark on the reader of the mark the writer stored = %q, %v; want %q", anchored.Quote, err, "quick")
	}
	if waited := time.Since(started); waited >= pair.reader.markWait {
		t.Fatalf("VerifyMark took %v, its whole MarkWait: it waited rather than read the store", waited)
	}
	if pair.loads.loads.Load() == before {
		t.Fatal("VerifyMark on a room behind the head did not read the store")
	}
	if !roomBehind(t, pair.reader, pair.artifactID) {
		t.Fatal("the reader's room stands at the head while its catch-up is held")
	}

	pair.gate.open()
	waitFor(t, roomWait(t), "the reader's room to catch up", func() bool {
		return !roomBehind(t, pair.reader, pair.artifactID)
	})
	warmed := make(chan struct{}, 1)
	pair.reader.afterReadWarm = func(string) {
		select {
		case warmed <- struct{}{}:
		default:
		}
	}
	type verified struct {
		anchored Anchored
		err      error
	}
	result := make(chan verified, 1)
	go func() {
		anchored, err := pair.reader.VerifyMark(context.Background(), pair.artifactID, MarkComment, "late-mark")
		result <- verified{anchored, err}
	}()
	select {
	case <-warmed:
	case <-time.After(roomWait(t)):
		t.Fatal("VerifyMark never subscribed to the reader's room")
	}
	browserMark(t, pair.writer, pair.artifactID, "proofComment", "late-mark", "fox")
	waitForLandedAppends(t, pair.writer, pair.artifactID)
	got := <-result
	if got.err != nil || got.anchored.Quote != "fox" {
		t.Fatalf("VerifyMark on the reader, waiting while the writer stored the mark = %q, %v; want %q", got.anchored.Quote, got.err, "fox")
	}
}

// VerifyMark on a room behind the head that holds an unstored update of its own finds a mark the
// other process placed and stored: the look reads the stored document, which holds that mark,
// merged with the room's own update. Reading the room alone would leave the mark unfound until
// the catch-up landed, so the look would wait out its whole MarkWait and answer ErrAnchorMissing,
// while the mark has been durable all along (the comment at VerifyMark).
func TestVerifyMarkOnARoomBehindTheHeadHoldingItsOwnUpdateFindsTheStoredMark(t *testing.T) {
	pair := newReadPair(t)
	pair.reader.markWait = roomWait(t)
	const typed = "Typed into the room behind the head."

	pair.leaveReaderBehind(t, "the writer's mark", func() {
		browserMark(t, pair.writer, pair.artifactID, "proofComment", "held-mark", "quick")
		waitForLandedAppends(t, pair.writer, pair.artifactID)
	})
	land := pair.typeIntoTheRoomBehindTheHead(t, typed)
	defer land()

	started := time.Now()
	var anchored Anchored
	if err := pair.joinedRead(t, func(ctx context.Context) (err error) {
		anchored, err = pair.reader.VerifyMark(ctx, pair.artifactID, MarkComment, "held-mark")
		return err
	}); err != nil || anchored.Quote != "quick" {
		t.Fatalf("VerifyMark on the reader, its room behind the head and holding an unstored keystroke = %q, %v; want %q", anchored.Quote, err, "quick")
	}
	if waited := time.Since(started); waited >= pair.reader.markWait {
		t.Fatalf("VerifyMark took %v, its whole MarkWait: it waited rather than read the store", waited)
	}
	if text, err := pair.reader.Text(context.Background(), pair.artifactID); err != nil || !strings.Contains(text, typed) {
		t.Fatalf("the reader's read beside that VerifyMark = %q, %v; want the room's own unstored keystroke", text, err)
	}
	if !roomBehind(t, pair.reader, pair.artifactID) {
		t.Fatal("the reader's room stands at the head while its catch-up is held")
	}
}

// The token a read answers with while its room is behind the head and holds an unstored update of
// its own is the whole document's: the stored document, holding the other process's committed
// write, merged with that update. It is the token the document still has once the catch-up and
// the room's own append have landed, so the conditional edit the caller passes it back to is
// accepted (currentToken, checkEditPrecondition). A token taken over the room alone would be the
// token of a document that never stood - one without the committed write - and would refuse that
// edit.
func TestTheTokenOfARoomBehindTheHeadHoldingItsOwnUpdateIsTheWholeDocuments(t *testing.T) {
	pair := newReadPair(t)
	ctx := context.Background()
	actor := model.Actor{Kind: "session", ID: "session-0123456789abcdef"}
	const written = "Second, written on the other process."
	const typed = "Typed into the room behind the head."

	pair.leaveReaderBehind(t, "the writer's edit", func() {
		if _, err := joinedApplyOps(pair.writer, pair.artifactID, []model.EditOp{
			{Op: "replace", Find: "Second.", With: written},
		}, actor, nil); err != nil {
			t.Fatalf("edit through the writer: %v", err)
		}
	})
	land := pair.typeIntoTheRoomBehindTheHead(t, typed)

	_, token, err := pair.reader.TextWithToken(ctx, pair.artifactID)
	if err != nil {
		t.Fatalf("the reader's read while its room is behind the head and holds an unstored keystroke: %v", err)
	}

	// The room's append lands and its catch-up applies the writer's edit, so the document that
	// stands is the one that token was taken over, and nothing else has written it.
	land()
	pair.gate.open()
	waitFor(t, roomWait(t), "the reader's room to catch up", func() bool {
		return !roomBehind(t, pair.reader, pair.artifactID)
	})
	outcome, err := joinedApplyOps(pair.reader, pair.artifactID, nil, actor, &model.EditPrecondition{Document: token})
	if err != nil {
		t.Fatalf("a conditional edit on the reader with the token read while its room was behind the head: %v; want it accepted: that token is the document's", err)
	}
	if outcome.Token != token {
		t.Fatalf("the token of the document the conditional edit met = %s, want the token read while the room was behind the head %s", outcome.Token, token)
	}
}

// tokenOfStored is the token a read of the stored document answers with.
func tokenOfStored(t *testing.T, database *store.Store, artifactID string) string {
	t.Helper()
	_, token, err := renderTokenTree(persistedProofTree(t, database, artifactID))
	if err != nil {
		t.Fatalf("render the stored document's token: %v", err)
	}
	return token
}
