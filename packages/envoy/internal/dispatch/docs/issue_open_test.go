package docs

import (
	"context"
	"errors"
	"strconv"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/reearth/ygo/crdt"
	"github.com/reearth/ygo/persistence"

	"github.com/sjawhar/envoy/internal/dispatch/docs/docstest"
	"github.com/sjawhar/envoy/internal/dispatch/pmdoc"
	"github.com/sjawhar/envoy/internal/dispatch/store"
	"github.com/sjawhar/envoy/internal/dispatch/store/storetest"
)

// Closing or reopening an issue changes every process's rooms of its documents (plan
// D5). The trigger migration 0149 puts on issues.closed_at notifies dispatch_issue_open_changed
// from the transaction that wrote it, whichever process wrote it; each process's room sync reads
// the issue and closes its rooms of the issue's documents (issueOpenChanged), so their browsers
// reconnect with the scope the issue now allows, which ygo fixes when a browser connects. A
// browser's append on a closed issue is refused by the append gate, which reads the issue itself,
// whether the notification reached the room's process or not, unless the room took the update no
// later than closeMargin after the close committed.

// writeIssueClosed sets issueKey's closed_at in database as PATCH /api/v1/issues/{key} does when it
// closes or reopens the issue: a commit whose notification reaches every process listening.
func writeIssueClosed(t *testing.T, database *store.Store, issueKey string, closed bool) {
	t.Helper()
	statement := `update issues set closed_at = null where key = $1`
	if closed {
		statement = `update issues set closed_at = now() where key = $1`
	}
	if _, err := database.Pool.Exec(context.Background(), statement, issueKey); err != nil {
		t.Fatalf("set issue %s closed=%t: %v", issueKey, closed, err)
	}
}

// setIssueClosedHere writes issueKey's closed_at (writeIssueClosed) and hands service the
// notification that commit sends, at once and on the test's goroutine, for a test that runs no
// room sync.
func setIssueClosedHere(t *testing.T, service *Service, issueKey string, closed bool) {
	t.Helper()
	writeIssueClosed(t, service.store, issueKey, closed)
	service.issueOpenChanged(store.WithTransactionTracking(context.Background()), issueKey)
}

// connectWithScope connects login's browser to artifactID's room through the document server at
// serverURL, authenticates it as the dashboard's editor does, and fails unless the room granted
// scope ("read-write" or "readonly") and the browser holds the stored document text.
func connectWithScope(t *testing.T, serverURL, artifactID, login, scope, text string) *docstest.Peer {
	t.Helper()
	peer := connectPeer(t, serverURL, artifactID, login)
	granted, err := peer.Authenticate(strconv.Itoa(pmdoc.SchemaVersion()), roomWait(t))
	if err != nil {
		t.Fatalf("authenticate %s's browser: %v", login, err)
	}
	if granted != scope {
		t.Fatalf("%s's browser was granted %q, want %q", login, granted, scope)
	}
	if err := peer.WaitForText(peerMarkdown, text, roomWait(t)); err != nil {
		t.Fatalf("%s's browser: %v", login, err)
	}
	return peer
}

// requireEnds fails unless peer's socket is closed by its room within roomWait.
func requireEnds(t *testing.T, peer *docstest.Peer, what string) {
	t.Helper()
	select {
	case <-peer.Ended:
	case <-time.After(roomWait(t)):
		t.Fatalf("%s: the browser's socket was still open after %v", what, roomWait(t))
	}
}

// roomMarkdown is what service's resident room of artifactID renders, "" with none resident.
func roomMarkdown(t *testing.T, service *Service, artifactID string) string {
	t.Helper()
	live := service.srv.GetDoc(artifactID)
	if live == nil {
		return ""
	}
	tree, err := treeOf(live)
	if err != nil {
		t.Fatalf("read the room's document: %v", err)
	}
	markdown, err := renderTree(tree)
	if err != nil {
		t.Fatalf("render the room's document: %v", err)
	}
	return markdown
}

// requireKeystrokeDropped types over the first paragraph in peer's read-only browser and fails if
// service's room took it or the store moved past head. The room answers peer's messages in order,
// so once it has answered the request for its document that follows the keystroke, it has dealt
// with the keystroke; the answers already received are drained first, so the one waited for is
// that request's.
func requireKeystrokeDropped(t *testing.T, service *Service, peer *docstest.Peer, artifactID string, head int64) {
	t.Helper()
	for drained := false; !drained; {
		select {
		case <-peer.Answers:
		default:
			drained = true
		}
	}
	typeOver(t, peer, "before", "typed on a closed issue")
	if err := peer.AskForDocument(); err != nil {
		t.Fatalf("ask the room for its document: %v", err)
	}
	select {
	case <-peer.Answers:
	case <-time.After(roomWait(t)):
		t.Fatal("the room never answered the request for its document")
	}
	if markdown := roomMarkdown(t, service, artifactID); strings.Contains(markdown, "typed on a closed issue") {
		t.Fatalf("the room took a read-only browser's keystroke on the closed issue: it holds %q", markdown)
	}
	if moved := storedHead(t, service.store, artifactID); moved != head {
		t.Fatalf("the document moved from update %d to %d after the close: an update on a closed issue was stored", head, moved)
	}
}

// Two processes on one database each hold a room of the document with a browser in it. The issue
// closes through task B's pool, as a PATCH on task B commits it: both rooms close, task B's
// included, since the process that writes the change hears it like every other; each browser
// reconnects read-only, and a keystroke there is neither taken nor stored. The issue reopens through
// task A: both rooms close again, each browser reconnects read-write, and its edit is stored.
func TestClosingAnIssueOnOneProcessClosesEveryProcessesRoomsAndReopeningMakesThemWritable(t *testing.T) {
	database := storetest.Open(t)
	artifactID := createDocument(t, database, "before")
	taskA, serverA := newTestServiceInstance(t, database)
	taskB, serverB := newTestServiceInstance(t, storetest.SecondProcess(t, database))
	seedServiceText(t, taskA, artifactID, "before")
	waitForLandedAppends(t, taskA, artifactID)
	runRoomSync(t, newRoomSync(taskA))
	runRoomSync(t, newRoomSync(taskB))

	alice := connectWithScope(t, serverA.URL, artifactID, "alice", "read-write", "before\n")
	bob := connectWithScope(t, serverB.URL, artifactID, "bob", "read-write", "before\n")

	writeIssueClosed(t, taskB.store, "DOC-1", true)
	requireEnds(t, alice, "closing the issue through task B left alice's browser on task A connected")
	requireEnds(t, bob, "closing the issue through task B left bob's browser on task B connected")
	alice = connectWithScope(t, serverA.URL, artifactID, "alice", "readonly", "before\n")
	bob = connectWithScope(t, serverB.URL, artifactID, "bob", "readonly", "before\n")
	closedHead := storedHead(t, database, artifactID)
	requireKeystrokeDropped(t, taskA, alice, artifactID, closedHead)
	requireKeystrokeDropped(t, taskB, bob, artifactID, closedHead)

	writeIssueClosed(t, taskA.store, "DOC-1", false)
	requireEnds(t, alice, "reopening the issue through task A left alice's read-only browser on task A connected")
	requireEnds(t, bob, "reopening the issue through task A left bob's read-only browser on task B connected")
	alice = connectWithScope(t, serverA.URL, artifactID, "alice", "read-write", "before\n")
	typeOver(t, alice, "before", "reopened by alice")
	waitFor(t, roomWait(t), "alice's edit on the reopened issue to be stored", func() bool {
		return strings.Contains(storedMarkdown(t, database, artifactID), "reopened by alice")
	})
	bob = connectWithScope(t, serverB.URL, artifactID, "bob", "read-write", "reopened by alice\n")
	typeOver(t, bob, "reopened by alice", "reopened by bob")
	waitFor(t, roomWait(t), "bob's edit on the reopened issue to be stored", func() bool {
		return strings.Contains(storedMarkdown(t, database, artifactID), "reopened by bob")
	})
}

// A keystroke the room took while the issue was open is stored, even when its append reaches the
// gate only after the close committed: the room's persistence is still storing an earlier update,
// or waits on the document's lock behind a settlement or a compaction, and the close's notification
// closes the room meanwhile, which hands the room's queued updates to the gate (CloseRoom flushes
// it). The room's observer took the update before the close, which the gate tells by comparing the
// update's age with how long ago the issue closed (gateAppend). Here task A's persistence is
// held while the issue closes through task B; alice's socket closes, the hold runs out, her
// keystroke is stored, and her browser comes back read-only holding it.
func TestAKeystrokeTakenBeforeTheIssueClosedIsStoredWhenItsAppendReachesTheGateAfter(t *testing.T) {
	database := storetest.Open(t)
	artifactID := createDocument(t, database, "before")
	taskA, serverA := newTestServiceInstance(t, database)
	taskA.hooks = newTestHooks()
	taskB, _ := newTestServiceInstance(t, storetest.SecondProcess(t, database))
	seedServiceText(t, taskA, artifactID, "before")
	waitForLandedAppends(t, taskA, artifactID)
	runRoomSync(t, newRoomSync(taskA))
	runRoomSync(t, newRoomSync(taskB))
	alice := connectWithScope(t, serverA.URL, artifactID, "alice", "read-write", "before\n")
	waitForLandedAppends(t, taskA, artifactID)
	head := storedHead(t, database, artifactID)

	const hold = 3 * time.Second
	held := time.Now().Add(hold)
	if err := taskA.HoldRoomPersistence(artifactID, hold); err != nil {
		t.Fatalf("hold task A's persistence: %v", err)
	}
	typeOver(t, alice, "before", "typed before the close")
	waitFor(t, roomWait(t), "task A's room to take alice's keystroke", func() bool {
		return strings.Contains(roomMarkdown(t, taskA, artifactID), "typed before the close")
	})
	writeIssueClosed(t, taskB.store, "DOC-1", true)
	requireEnds(t, alice, "closing the issue through task B left alice's browser on task A connected")
	if moved := storedHead(t, database, artifactID); moved != head || time.Now().After(held) {
		t.Fatalf("the close was meant to take effect while task A held alice's keystroke unstored, but the head moved from %d to %d or the hold ran out first", head, moved)
	}
	// The room's close waits for its persistence to store what it holds (CloseRoom's flush), so
	// once the room is gone the keystroke's append has been decided.
	waitFor(t, roomWait(t), "task A's room to finish closing", func() bool {
		return taskA.srv.GetDoc(artifactID) == nil
	})
	if stored := storedMarkdown(t, database, artifactID); !strings.Contains(stored, "typed before the close") {
		t.Fatalf("the keystroke alice typed while the issue was open was dropped by its close: the store holds %q", stored)
	}
	connectWithScope(t, serverA.URL, artifactID, "alice", "readonly", "typed before the close\n")
}

// A browser's update whose append waits for the document's lock - behind a settlement, a
// compaction or another process's write - can find, once it has the lock, a stored history its
// room no longer holds a prefix of: another process wrote the document past the room's cursor, and
// a compaction folded every row into that write's (another process's room retiring on the issue's
// close compacts as it exits). A room closing for its issue takes in no catch-up meanwhile (its
// close holds the room's gate, holdOpen), and the catch-up of any other room reports the history
// replaced. The fold keeps every item the update builds on, so the update is stored
// (storedHistoryHolds). Refused as a replaced history, it failed the room and was dropped, and its
// browser's resend reached a room that is read-only once the issue closed: the keystroke was lost.
// Here a transaction holds the document's lock while alice types, so her append waits on it; the
// issue closes, and the close queues on the lock behind the append (it records the room's latest
// edit source first, setRoomIssueOpen); the transaction writes the document and folds its history
// before it lets go.
func TestAKeystrokeTakenBeforeTheIssueClosedIsStoredWhenACompactionFoldedTheHistoryItsRoomHeld(t *testing.T) {
	ctx := context.Background()
	database := storetest.Open(t)
	artifactID := createDocument(t, database, "before")
	taskA, serverA := newTestServiceInstance(t, database)
	taskB, _ := newTestServiceInstance(t, storetest.SecondProcess(t, database))
	seedServiceText(t, taskA, artifactID, "before")
	waitForLandedAppends(t, taskA, artifactID)
	runRoomSync(t, newRoomSync(taskA))
	var failed atomic.Bool
	taskA.beforeFailedEviction = func(string) { failed.Store(true) }
	alice := connectWithScope(t, serverA.URL, artifactID, "alice", "read-write", "before\n")
	waitForLandedAppends(t, taskA, artifactID)
	head := storedHead(t, database, artifactID)

	holder, err := database.Pool.Begin(ctx)
	if err != nil {
		t.Fatalf("begin the transaction that holds the document's lock: %v", err)
	}
	defer holder.Rollback(ctx)
	if err := lockDocumentRoom(ctx, holder, artifactID); err != nil {
		t.Fatalf("hold the document's lock: %v", err)
	}
	typeOver(t, alice, "before", "typed before the close")
	if waiting := storetest.WaitForLockWaiters(t, holder, 1, roomWait(t)); waiting < 1 {
		t.Fatal("alice's append never came to wait on the document's lock")
	}
	writeIssueClosed(t, taskB.store, "DOC-1", true)
	if waiting := storetest.WaitForLockWaiters(t, holder, 2, roomWait(t)); waiting < 2 {
		t.Fatal("task A's close of its room never came to wait on the document's lock behind alice's append")
	}

	foldHistoryPastRoom(t, holder, artifactID, head)
	if taskA.srv.GetDoc(artifactID) == nil {
		t.Fatal("task A's room closed before its append reached the store, so nothing here is tested")
	}
	if err := holder.Commit(ctx); err != nil {
		t.Fatalf("release the document's lock: %v", err)
	}

	requireEnds(t, alice, "closing the issue left alice's browser on task A connected")
	waitFor(t, roomWait(t), "task A's room to finish closing", func() bool {
		return taskA.srv.GetDoc(artifactID) == nil
	})
	if stored := storedMarkdown(t, database, artifactID); !strings.Contains(stored, "typed before the close") {
		t.Fatalf("the keystroke alice typed while the issue was open was dropped: the store holds %q", stored)
	}
	if failed.Load() {
		t.Fatal("the keystroke's append failed task A's room")
	}
}

// foldHistoryPastRoom writes artifactID as another process would, in holder, which holds the
// document's lock, past head, the cursor of the room a test holds, and then folds the history as
// Compact does with compactKeep: one row, the head, holding the state of every row through it. No
// row at or below head remains, and the fold keeps every item of the history, so the room's
// browser updates still build on it (storedHistoryHolds).
func foldHistoryPastRoom(t *testing.T, holder pgx.Tx, artifactID string, head int64) {
	t.Helper()
	ctx := context.Background()
	stored, err := documentThrough(ctx, holder, artifactID, persistence.Version(head))
	if err != nil {
		t.Fatalf("read the stored document: %v", err)
	}
	before := stored.StateVector()
	written := stored.GetMap("written-by-task-b")
	stored.Transact(func(txn *crdt.Transaction) {
		written.Set(txn, "key", "value")
	})
	writtenAt := head + 1
	if _, err := holder.Exec(ctx, `insert into doc_updates (artifact_id, version, update, content_changed) values ($1, $2, $3, false)`,
		artifactID, writtenAt, crdt.EncodeStateAsUpdateV1(stored, before)); err != nil {
		t.Fatalf("write the document as task B: %v", err)
	}
	state, misread, err := foldThrough(ctx, holder, artifactID, persistence.Version(writtenAt))
	if err != nil {
		t.Fatalf("fold the document's history: %v", err)
	}
	if misread != nil {
		t.Fatalf("the folded history does not read back as the rows it folds, so a compaction would leave them: %v", misread)
	}
	if _, err := holder.Exec(ctx, `update doc_updates set update = $3 where artifact_id = $1 and version = $2`, artifactID, writtenAt, state); err != nil {
		t.Fatalf("fold the document's history into task B's write: %v", err)
	}
	if _, err := holder.Exec(ctx, `delete from doc_updates where artifact_id = $1 and version < $2`, artifactID, writtenAt); err != nil {
		t.Fatalf("delete the folded rows: %v", err)
	}
}

// The notification may not reach a process in time: one wait between its listener's redials can
// last up to 45 seconds (pglisten.RedialBackoff). Here no room sync runs at all, so nothing tells
// the room the issue closed, and the issue closed longer ago than closeMargin before alice types:
// the gate refuses her browser's append (ErrIssueClosed), which is not stored, and the room is
// closed for its issue rather than failed, so the browser reconnects read-only. (A keystroke the
// room takes within closeMargin of the close is stored instead: the close's notification may not
// have reached the room yet.)
func TestABrowserAppendOnAClosedIssueIsRefusedWithoutItsNotification(t *testing.T) {
	database := storetest.Open(t)
	artifactID := createDocument(t, database, "before")
	service, server := newTestServiceInstance(t, database)
	seedServiceText(t, service, artifactID, "before")
	waitForLandedAppends(t, service, artifactID)
	var failed atomic.Bool
	service.beforeFailedEviction = func(string) { failed.Store(true) }
	alice := connectWithScope(t, server.URL, artifactID, "alice", "read-write", "before\n")

	closedAgo := closeMargin + 30*time.Second
	if _, err := database.Pool.Exec(context.Background(),
		`update issues set closed_at = now() - make_interval(secs => $2) where key = $1`, "DOC-1", closedAgo.Seconds()); err != nil {
		t.Fatalf("close issue DOC-1 %v ago: %v", closedAgo, err)
	}
	head := storedHead(t, database, artifactID)
	typeOver(t, alice, "before", "typed after the close")
	requireEnds(t, alice, "the gate refused alice's keystroke on the closed issue and left her browser connected")
	if moved := storedHead(t, database, artifactID); moved != head {
		t.Fatalf("the document moved from update %d to %d: the gate stored a browser's update on a closed issue", head, moved)
	}
	if failed.Load() {
		t.Fatal("the refusal failed the room rather than closing it for its issue")
	}
	connectWithScope(t, server.URL, artifactID, "alice", "readonly", "before\n")
}

// A close notified while the listener has no connection reaches nobody and is never sent again:
// the issue-open lane reads the issue of every resident room once each LISTEN stands
// (recheckResidentIssues), so the room closes on the reconnect, with no keystroke to make the
// gate notice.
func TestAnIssueClosedWhileTheListenerWasDownClosesItsRoomsOnTheReconnect(t *testing.T) {
	database := storetest.Open(t)
	artifactID := createDocument(t, database, "before")
	service, server := newTestServiceInstance(t, database)
	seedServiceText(t, service, artifactID, "before")
	waitForLandedAppends(t, service, artifactID)
	hub := newListenerHub(service)
	up := make(chan struct{})
	var dials atomic.Int32
	dial := hub.Dial
	hub.Dial = func(ctx context.Context) (*pgx.Conn, error) {
		if dials.Add(1) == 1 {
			return nil, errors.New("the listener hub has no connection")
		}
		select {
		case <-up:
		case <-ctx.Done():
			return nil, ctx.Err()
		}
		return dial(ctx)
	}
	startRoomSync(t, hub, newRoomSync(service))
	alice := connectWithScope(t, server.URL, artifactID, "alice", "read-write", "before\n")

	writeIssueClosed(t, database, "DOC-1", true)
	close(up)
	requireEnds(t, alice, "the issue closed while the listener was down, and the reconnect left alice's browser connected")
	connectWithScope(t, server.URL, artifactID, "alice", "readonly", "before\n")
}
