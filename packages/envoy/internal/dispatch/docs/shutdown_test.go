package docs

import (
	"context"
	"encoding/json"
	"errors"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/reearth/ygo/crdt"
	"github.com/reearth/ygo/persistence"

	"github.com/sjawhar/envoy/internal/dispatch/docs/docstest"
	"github.com/sjawhar/envoy/internal/dispatch/events"
	"github.com/sjawhar/envoy/internal/dispatch/identity"
	"github.com/sjawhar/envoy/internal/dispatch/model"
	"github.com/sjawhar/envoy/internal/dispatch/pmdoc"
	"github.com/sjawhar/envoy/internal/dispatch/store"
	"github.com/sjawhar/envoy/internal/dispatch/store/storetest"
)

// A deploy usually finds someone with a document open, its tab connected to the room with what it
// last typed owed a settlement. Shutdown settles the room while it is still loaded and only then
// closes the editor's connection: ygo's CloseRoom evicts the room as it closes it, and a settlement
// does not load a room during shutdown, so a room closed first would leave the edit unversioned
// until someone next opened the document.
func TestShutdownSettlesARoomWithAnEditorConnectedBeforeClosingIt(t *testing.T) {
	database := storetest.Open(t)
	artifactID := createDocument(t, database, "before")
	service := newShutdownTestService(t, database, nil)
	seedServiceText(t, service, artifactID, "before")
	settleCurrentGeneration(t, service, artifactID)
	httpServer := httptest.NewServer(http.HandlerFunc(service.ServeHTTP))
	t.Cleanup(httpServer.Close)
	editor := connectPeer(t, httpServer.URL, artifactID)
	waitForPeerDocument(t, editor, "before\n")
	typeOver(t, editor, "before", "after")
	waitForSettlementOwed(t, service, artifactID)

	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	if err := service.Shutdown(ctx); err != nil {
		t.Fatalf("shutdown with an editor connected: %v", err)
	}
	select {
	case <-editor.Ended:
	case <-time.After(5 * time.Second):
		t.Fatal("the editor's connection was still open 5s after Shutdown returned")
	}
	if number, markdown, authors := latestDocumentVersion(t, database, artifactID); number != 2 || markdown != "after\n" ||
		len(authors) != 1 || authors[0] != (model.Actor{Kind: "user", ID: "alice"}) {
		t.Fatalf("the document's latest version is %d holding %q by %v, want version 2 holding the editor's edit, %q, credited to alice",
			number, markdown, authors, "after\n")
	}
	if owed, err := settlementPending(context.Background(), database.Pool, artifactID); err != nil || owed {
		t.Fatalf("the document still owes its settlement (%v) after the shutdown that ran it", err)
	}
}

// An editor whose next update always reaches its room before the last one is stored - a client
// typing faster than its appends land - leaves that room an append outstanding for as long as it
// types. Shutdown waits only for the appends queued before it began, so the room it types into is
// left to resume and every other room's owed settlement still runs.
func TestAnEditorThatNeverStopsTypingHoldsNoOtherRoomsSettlementAtShutdown(t *testing.T) {
	database := storetest.Open(t)
	quiet := createIssueDocument(t, database, 1, "quiet")
	typed := createIssueDocument(t, database, 2, "typed")
	keyboard := &typist{room: typed, recorded: make(chan chan struct{}, 1)}
	service := newShutdownTestService(t, database, &typingStore{VersionedStore: NewPgVersioned(database), typist: keyboard})
	keyboard.service = service
	keyboard.observeRecording(service)
	seedServiceText(t, service, quiet, "quiet")
	settleCurrentGeneration(t, service, quiet)
	seedServiceText(t, service, typed, "typed")
	settleCurrentGeneration(t, service, typed)
	editLiveTree(t, service, quiet, replaceRun("quiet", "quiet, edited"))
	waitForSettlementOwed(t, service, quiet)
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	if err := service.waitForDurableAppends(ctx, quiet); err != nil {
		t.Fatalf("wait for the quiet document's edit to be stored: %v", err)
	}
	defer keyboard.stop()
	keyboard.start(t)

	shutdownCtx, cancelShutdown := context.WithTimeout(context.Background(), 15*time.Second)
	defer cancelShutdown()
	shutdown := make(chan error, 1)
	go func() { shutdown <- service.Shutdown(shutdownCtx) }()
	// The typist stops once the quiet document is versioned or Shutdown has returned: while it
	// types, ygo's own shutdown waits on the commits it keeps making.
	deadline := time.After(10 * time.Second)
	var shutdownErr error
	returned := false
wait:
	for {
		select {
		case shutdownErr = <-shutdown:
			returned = true
			break wait
		case <-deadline:
			break wait
		case <-time.After(10 * time.Millisecond):
			if number, _, _ := latestDocumentVersion(t, database, quiet); number == 2 {
				break wait
			}
		}
	}
	keystrokes := keyboard.stop()
	if !returned {
		shutdownErr = <-shutdown
	}
	t.Logf("the typist made %d keystrokes; Shutdown returned %v", keystrokes, shutdownErr)
	if number, markdown, _ := latestDocumentVersion(t, database, quiet); number != 2 || markdown != "quiet, edited\n" {
		t.Errorf("the quiet document's latest version is %d holding %q, want version 2 holding %q: an editor typing in another room held its settlement",
			number, markdown, "quiet, edited\n")
	}
	if owed, err := settlementPending(context.Background(), database.Pool, quiet); err != nil || owed {
		t.Errorf("the quiet document still owes its settlement (%v) after the shutdown that ran it", err)
	}
	if shutdownErr != nil {
		t.Errorf("shutdown with an editor typing = %v, want nil", shutdownErr)
	}
}

// A browser leaving a room settles what it was owed at once (settleLastPeer), on ygo's disconnect
// goroutine, in place of the timer it stops. Shutdown joins that settlement as it joins a timer's:
// it reports which documents settled, and stops ygo, only once the settlement has returned, and the
// process does not close the store under it.
func TestShutdownJoinsTheSettlementItsLastBrowserLeavingStarted(t *testing.T) {
	database := storetest.Open(t)
	artifactID := createDocument(t, database, "before")
	service := newShutdownTestService(t, database, nil)
	pause := pauseRepairCommits(service)
	t.Cleanup(pause.let)
	seedServiceText(t, service, artifactID, "before")
	settleCurrentGeneration(t, service, artifactID)
	httpServer := httptest.NewServer(http.HandlerFunc(service.ServeHTTP))
	t.Cleanup(httpServer.Close)
	browser := connectPeer(t, httpServer.URL, artifactID)
	waitForPeerDocument(t, browser, "before\n")
	// A block with no id arms the room's settlement, which has to stamp one.
	editLiveTree(t, service, artifactID, appendUnidentifiedBlocks(t, "added"))
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	if err := service.waitForPendingUpdates(ctx, artifactID); err != nil {
		t.Fatalf("wait for the edit to reach persistence: %v", err)
	}
	if err := service.waitForDurableAppends(ctx, artifactID); err != nil {
		t.Fatalf("wait for the edit to be stored: %v", err)
	}

	pause.armed.Store(true)
	browser.Close()
	select {
	case <-pause.held:
	case <-time.After(10 * time.Second):
		t.Fatal("the settlement the browser's leaving owed never stamped the room")
	}
	shutdownCtx, cancelShutdown := context.WithTimeout(context.Background(), 20*time.Second)
	defer cancelShutdown()
	shutdown := make(chan error, 1)
	go func() { shutdown <- service.Shutdown(shutdownCtx) }()
	// Shutdown's own settlement of the room waits on the owner row the held one holds until the
	// drain budget ends; a Shutdown still running a little after that is waiting on the held
	// settlement.
	select {
	case err := <-shutdown:
		t.Errorf("Shutdown returned (%v) while the settlement the browser's leaving started was still committing its stamp", err)
		shutdown <- err
	case <-time.After(ShutdownDrainBudget + 2*time.Second):
	}
	pause.let()
	select {
	case err := <-shutdown:
		if err != nil {
			t.Fatalf("shutdown under the browser's settlement: %v", err)
		}
	case <-time.After(10 * time.Second):
		t.Fatal("Shutdown never returned once the settlement the browser's leaving started could go on")
	}
	// The stamp went with the shutdown's gate, so the document is left to resume, its edit stored.
	if owed, err := settlementPending(context.Background(), database.Pool, artifactID); err != nil || !owed {
		t.Fatalf("the document owes no settlement (%v) after a shutdown that settled nothing", err)
	}
	waitForPersistedProofText(t, database, artifactID, "before\n\nadded\n")
}

// An append a room queued before the signal can be slow to store - waiting on its document's lock
// or a slow write - and only that room's settlement can wait on it. Every other room drains, reads
// what it owes and settles on its own, so the quiet document's edit is versioned while the held
// room's append is still outstanding, and the held room alone is left to resume.
func TestShutdownSettlesEveryOtherRoomWhileOneRoomsQueuedAppendIsHeld(t *testing.T) {
	database := storetest.Open(t)
	quiet := createIssueDocument(t, database, 1, "quiet")
	held := createIssueDocument(t, database, 2, "held")
	appends := &heldStore{VersionedStore: NewPgVersioned(database), room: held, entered: make(chan struct{}), release: make(chan struct{})}
	t.Cleanup(appends.let)
	service := newShutdownTestService(t, database, appends)
	seedServiceText(t, service, quiet, "quiet")
	settleCurrentGeneration(t, service, quiet)
	seedServiceText(t, service, held, "held")
	settleCurrentGeneration(t, service, held)
	editLiveTree(t, service, quiet, replaceRun("quiet", "quiet, edited"))
	waitForSettlementOwed(t, service, quiet)
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	if err := service.waitForDurableAppends(ctx, quiet); err != nil {
		t.Fatalf("wait for the quiet document's edit to be stored: %v", err)
	}
	appends.armed.Store(true)
	editLiveTree(t, service, held, replaceRun("held", "held, edited"))
	select {
	case <-appends.entered:
	case <-time.After(10 * time.Second):
		t.Fatal("the held room's append never reached the store")
	}

	shutdownCtx, cancelShutdown := context.WithTimeout(context.Background(), 15*time.Second)
	defer cancelShutdown()
	shutdown := make(chan error, 1)
	go func() { shutdown <- service.Shutdown(shutdownCtx) }()
	var shutdownErr error
	select {
	case shutdownErr = <-shutdown:
	case <-time.After(ShutdownDrainBudget + 5*time.Second):
		t.Fatal("Shutdown did not return once its drain budget ended")
	}
	appends.let()
	if number, markdown, _ := latestDocumentVersion(t, database, quiet); number != 2 || markdown != "quiet, edited\n" {
		t.Errorf("the quiet document's latest version is %d holding %q, want version 2 holding %q: another room's held append spent its settlement's budget",
			number, markdown, "quiet, edited\n")
	}
	if owed, err := settlementPending(context.Background(), database.Pool, quiet); err != nil || owed {
		t.Errorf("the quiet document still owes its settlement (%v) after the shutdown that ran it", err)
	}
	// The held append lands once let go, and the pending-settlement row it writes resumes the
	// held document's settlement in the next process.
	waitForSettlementOwed(t, service, held)
	if number, _, _ := latestDocumentVersion(t, database, held); number != 1 {
		t.Errorf("the held document's latest version is %d, want 1 with its settlement left to the next process", number)
	}
	if !errors.Is(shutdownErr, context.DeadlineExceeded) {
		t.Errorf("Shutdown returned %v, want the held room's append the drain budget did not see through", shutdownErr)
	}
}

// A settlement that has to write into its room - stamping a block id here - is refused that write
// once Shutdown begins closing the room. Shutdown leaves the document to resume from its
// pending-settlement row, says so at WARN, and counts no settlement failure, which three of would
// fail the room.
func TestShutdownLeavesASettlementThatMustWriteIntoItsRoomToResume(t *testing.T) {
	database := storetest.Open(t)
	artifactID := createDocument(t, database, "before")
	service := newShutdownTestService(t, database, nil)
	seedServiceText(t, service, artifactID, "before")
	settleCurrentGeneration(t, service, artifactID)
	// A block with no id arms the room's settlement, which has to stamp one.
	editLiveTree(t, service, artifactID, appendUnidentifiedBlocks(t, "added"))
	waitForSettlementOwed(t, service, artifactID)
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	if err := service.waitForPendingUpdates(ctx, artifactID); err != nil {
		t.Fatalf("wait for the edit to reach persistence: %v", err)
	}
	if err := service.waitForDurableAppends(ctx, artifactID); err != nil {
		t.Fatalf("wait for the edit to be stored: %v", err)
	}
	logs := &lockedLog{}
	previous := slog.Default()
	slog.SetDefault(slog.New(slog.NewTextHandler(logs, &slog.HandlerOptions{Level: slog.LevelWarn})))
	t.Cleanup(func() { slog.SetDefault(previous) })

	if err := service.Shutdown(ctx); err != nil {
		t.Fatalf("shutdown with a settlement that must stamp its room: %v", err)
	}
	state := service.room(artifactID)
	state.mu.Lock()
	failures := state.settleFailures
	state.mu.Unlock()
	if failures != 0 {
		t.Errorf("the refused write counted %d settlement failures, want none", failures)
	}
	output := logs.String()
	if !strings.Contains(output, `msg="dispatch: skip shutdown document settlement that has to write into its room" room=`+artifactID) {
		t.Errorf("Shutdown did not log the settlement it left because its room refused the write:\n%s", output)
	}
	if strings.Contains(output, `msg="dispatch: settle document"`) {
		t.Errorf("Shutdown logged the refused write as a settlement failure:\n%s", output)
	}
	if owed, err := settlementPending(context.Background(), database.Pool, artifactID); err != nil || !owed {
		t.Errorf("the document owes no settlement (%v), want it left to resume", err)
	}
}

// newShutdownTestService is a document service whose settlements wait an hour, so a test's
// Shutdown is the one that runs them, and whose browsers sign in as alice. appends, when set,
// stands in for the store's own document persistence.
func newShutdownTestService(t *testing.T, database *store.Store, appends VersionedStore) *Service {
	t.Helper()
	service := New(Deps{
		Store: database, Persistence: appends, Events: events.NewBroker(),
		Identity: identity.HeaderIdentity{Header: "X-Dispatch-User", AllowedLogins: map[string]struct{}{"alice": {}}},
		Settle:   time.Hour,
	})
	t.Cleanup(func() { _ = service.Shutdown(context.Background()) })
	return service
}

// waitForPeerDocument asks the room for its document until the peer's copy renders want.
func waitForPeerDocument(t *testing.T, peer *docstest.Peer, want string) {
	t.Helper()
	deadline := time.After(10 * time.Second)
	for peerMarkdown(peer) != want {
		if err := peer.AskForDocument(); err != nil {
			t.Fatalf("ask the room for the document: %v", err)
		}
		select {
		case <-peer.Answers:
		case <-peer.Ended:
			t.Fatal("the peer's connection closed before the room sent the document")
		case <-deadline:
			t.Fatalf("the peer's copy renders %q, want %q", peerMarkdown(peer), want)
		}
	}
}

// peerMarkdown renders the peer's copy of the document, read under its lock while the room's
// updates apply, or "" while the copy holds no document it can render.
func peerMarkdown(peer *docstest.Peer) string {
	fragment := peer.Doc.GetXmlFragment(fragmentName)
	var rendered string
	peer.Doc.Transact(func(txn *crdt.Transaction) {
		tree, err := pmdoc.ReadInTransaction(txn, fragment)
		if err != nil {
			return
		}
		rendered, _ = pmdoc.Render(tree)
	}, nil)
	return rendered
}

// typeOver types replacement over old, the first paragraph's whole text, in one keystroke's
// transaction, and sends the room the update it made.
func typeOver(t *testing.T, peer *docstest.Peer, old, replacement string) {
	t.Helper()
	fragment := peer.Doc.GetXmlFragment(fragmentName)
	var changeErr error
	if _, err := peer.Send(func(txn *crdt.Transaction) {
		tree, err := pmdoc.ReadInTransaction(txn, fragment)
		if err != nil {
			changeErr = err
			return
		}
		if len(tree.Children) == 0 || len(tree.Children[0].Children) != 1 || tree.Children[0].Children[0].Text != old {
			changeErr = errors.New("the first paragraph does not hold only " + old)
			return
		}
		tree.Children[0].Children[0].Text = replacement
		changeErr = pmdoc.Update(txn, fragment, tree)
	}); err != nil || changeErr != nil {
		t.Fatalf("type over %q: %v %v", old, err, changeErr)
	}
}

// waitForSettlementOwed waits for the document's pending-settlement row.
func waitForSettlementOwed(t *testing.T, service *Service, artifactID string) {
	t.Helper()
	waitFor(t, 10*time.Second, "the document to owe a settlement", func() bool {
		owed, err := settlementPending(context.Background(), service.store.Pool, artifactID)
		return err == nil && owed
	})
}

// latestDocumentVersion is the number, markdown and authors of the document's latest version.
func latestDocumentVersion(t *testing.T, database *store.Store, artifactID string) (int, string, []model.Actor) {
	t.Helper()
	var number int
	var markdown string
	var authors []byte
	if err := database.Pool.QueryRow(context.Background(), `
		select number, coalesce(markdown, ''), authors
		from artifact_versions where artifact_id = $1 order by number desc limit 1
	`, artifactID).Scan(&number, &markdown, &authors); err != nil {
		t.Fatalf("read the document's latest version: %v", err)
	}
	var actors []model.Actor
	if err := json.Unmarshal(authors, &actors); err != nil {
		t.Fatalf("decode the latest version's authors: %v", err)
	}
	return number, markdown, actors
}

// typist types into one room as a browser whose next keystroke always reaches the room before its
// last one is stored: while it types, each of the room's appends waits for the room to record the
// next keystroke before it lands (typingStore), so the room always has one outstanding.
type typist struct {
	room    string
	service *Service
	typing  atomic.Bool
	count   atomic.Int64
	// recorded carries the channel the next keystroke closes once the room's update observers have
	// recorded it.
	recorded chan chan struct{}
	// mu holds the typist's own copy of the document while a keystroke edits it.
	mu  sync.Mutex
	doc *crdt.Doc
}

// observeRecording registers, on the typist's room as it loads, an observer behind the service's
// own and ahead of ygo's persistence observer, which reports each keystroke the room has recorded.
func (p *typist) observeRecording(service *Service) {
	load := service.srv.OnLoadDocument
	service.srv.OnLoadDocument = func(ctx context.Context, room string, doc *crdt.Doc) error {
		if err := load(ctx, room, doc); err != nil {
			return err
		}
		if room == p.room {
			doc.OnUpdate(func(_ []byte, origin any) {
				if origin != "typist" {
					return
				}
				select {
				case recorded := <-p.recorded:
					close(recorded)
				default:
				}
			})
		}
		return nil
	}
}

// start types the first keystroke and returns once a few have been stored.
func (p *typist) start(t *testing.T) {
	t.Helper()
	room := p.service.srv.GetDoc(p.room)
	if room == nil {
		t.Fatal("the typist's room is not loaded")
	}
	p.doc = crdt.New()
	if err := crdt.ApplyUpdateV1(p.doc, crdt.EncodeStateAsUpdateV1(room, nil), nil); err != nil {
		t.Fatalf("copy the typist's room: %v", err)
	}
	p.typing.Store(true)
	if err := p.keystroke(); err != nil {
		t.Fatalf("the typist's first keystroke: %v", err)
	}
	waitFor(t, 10*time.Second, "the typist's keystrokes to be stored", func() bool { return p.count.Load() >= 5 })
}

// next types the next keystroke while the typist types, and returns once the room has recorded it.
func (p *typist) next() {
	if !p.typing.Load() {
		return
	}
	recorded := make(chan struct{})
	p.recorded <- recorded
	go func() { _ = p.keystroke() }()
	select {
	case <-recorded:
	case <-time.After(time.Second):
		// A keystroke the room did not take; the chain of appends ends with it.
		select {
		case <-p.recorded:
		default:
		}
	}
}

// keystroke appends a character to the first paragraph in the typist's copy and applies the change
// to the room as a browser's update.
func (p *typist) keystroke() error {
	room := p.service.srv.GetDoc(p.room)
	if room == nil {
		return errors.New("the typist's room is not loaded")
	}
	p.mu.Lock()
	synced := p.doc.StateVector()
	fragment := p.doc.GetXmlFragment(fragmentName)
	var editErr error
	p.doc.Transact(func(txn *crdt.Transaction) {
		tree, err := pmdoc.ReadInTransaction(txn, fragment)
		if err != nil {
			editErr = err
			return
		}
		runs := tree.Children[0].Children
		runs[len(runs)-1].Text += "."
		editErr = pmdoc.Update(txn, fragment, tree)
	})
	update := crdt.EncodeStateAsUpdateV1(p.doc, synced)
	p.mu.Unlock()
	if editErr != nil {
		return editErr
	}
	p.count.Add(1)
	return crdt.ApplyUpdateV1(room, update, "typist")
}

// stop ends the typing and reports how many keystrokes were made.
func (p *typist) stop() int64 {
	p.typing.Store(false)
	return p.count.Load()
}

// typingStore stores a document's updates as the store does, but holds each of the typist's room's
// appends until the typist's next keystroke is in the room.
type typingStore struct {
	VersionedStore
	typist *typist
}

func (s *typingStore) AppendUpdateWithClass(ctx context.Context, room string, update []byte, contentChanged bool) (persistence.Version, error) {
	if room == s.typist.room {
		s.typist.next()
	}
	return s.VersionedStore.(classifiedUpdateStore).AppendUpdateWithClass(ctx, room, update, contentChanged)
}

// heldStore stores a document's updates as the store does, but holds the held room's next append,
// once armed, until the test lets it go.
type heldStore struct {
	VersionedStore
	room    string
	armed   atomic.Bool
	entered chan struct{}
	release chan struct{}
	once    sync.Once
}

func (s *heldStore) let() { s.once.Do(func() { close(s.release) }) }

func (s *heldStore) AppendUpdateWithClass(ctx context.Context, room string, update []byte, contentChanged bool) (persistence.Version, error) {
	if room == s.room && s.armed.CompareAndSwap(true, false) {
		close(s.entered)
		<-s.release
	}
	return s.VersionedStore.(classifiedUpdateStore).AppendUpdateWithClass(ctx, room, update, contentChanged)
}
