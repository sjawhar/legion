package docs

import (
	"context"
	"errors"
	"fmt"
	"net/http/httptest"
	"slices"
	"strconv"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/reearth/ygo/crdt"
	"github.com/reearth/ygo/persistence"

	"github.com/sjawhar/envoy/internal/dispatch/docs/docstest"
	"github.com/sjawhar/envoy/internal/dispatch/events"
	"github.com/sjawhar/envoy/internal/dispatch/model"
	"github.com/sjawhar/envoy/internal/dispatch/pmdoc"
	"github.com/sjawhar/envoy/internal/dispatch/store"
	"github.com/sjawhar/envoy/internal/dispatch/store/storetest"
)

// Closing or reopening an issue changes every process's rooms of its documents (plan
// D5). The trigger migration 20261101181500 puts on issues.closed_at notifies
// dispatch_issue_open_changed from the transaction that wrote it, whichever process wrote it; each
// process's room sync reads the issue and closes its rooms of the issue's documents
// (issueOpenChanged), so their browsers reconnect with the scope the issue now allows, which ygo
// fixes when a browser connects. A browser's append on a closed issue is refused by the append
// gate, which reads the issue itself, whether the notification reached the room's process or not,
// unless the room took the update no later than closeMargin after the close committed.

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

// waitPastCloseMargin waits on the database's clock until it stands more than closeMargin past
// the moment issueKey closed (issues.closed_at), and fails the test unless it does. Inside the
// margin the gate admits a browser's update whatever its age (observedBeforeClose), so a test of
// the age term must put the close that far behind first. It waits in the database, in one
// statement, rather than sleeping on this process's clock: the two terms the gate compares are
// read on the database's clock and this one, and only the database's says when the margin is out.
func waitPastCloseMargin(t *testing.T, database *store.Store, issueKey string) {
	t.Helper()
	ctx := context.Background()
	if _, err := database.Pool.Exec(ctx, `
		select pg_sleep(greatest(0::double precision,
			$2::double precision - extract(epoch from clock_timestamp() - closed_at)::double precision))
		from issues where key = $1
	`, issueKey, (closeMargin + 50*time.Millisecond).Seconds()); err != nil {
		t.Fatalf("wait out issue %s's close margin: %v", issueKey, err)
	}
	var micros *int64
	if err := database.Pool.QueryRow(ctx, `
		select (extract(epoch from clock_timestamp() - closed_at) * 1000000)::bigint from issues where key = $1
	`, issueKey).Scan(&micros); err != nil {
		t.Fatalf("read how long ago issue %s closed: %v", issueKey, err)
	}
	if micros == nil {
		t.Fatalf("issue %s is open, so no close margin applies and nothing waits it out", issueKey)
	}
	if closedFor := time.Duration(*micros) * time.Microsecond; closedFor <= closeMargin {
		t.Fatalf("the database's clock stands %s past issue %s's close, want more than the gate's close margin of %s",
			closedFor.Round(time.Millisecond), issueKey, closeMargin)
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

// residentRoomMarkdown is what service's resident room of artifactID renders, "" with none resident.
func residentRoomMarkdown(t *testing.T, service *Service, artifactID string) string {
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
	if markdown := residentRoomMarkdown(t, service, artifactID); strings.Contains(markdown, "typed on a closed issue") {
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
// update's age with how long ago the issue closed (gateAppend). Here task A's appends wait at a
// gate the test holds (gatedAppends) while the issue closes through task B; alice's socket closes,
// the test opens the gate, her keystroke is stored, and her browser comes back read-only holding
// it. The gate is an event, not a wait: the append stays unstored for exactly as long as the test
// needs, whatever the race detector costs the run, and the test reads back that the keystroke is
// unstored rather than that the head stands still, since a browser's connect makes a durable
// append of its own that changes no content and lands whenever it lands. The gate opens only once
// the close is further behind than closeMargin on the database's clock (waitPastCloseMargin): the
// margin alone admits an update of any age, so until it is out the gate's age term, the one this
// guards, decides nothing.
func TestAKeystrokeTakenBeforeTheIssueClosedIsStoredWhenItsAppendReachesTheGateAfter(t *testing.T) {
	database := storetest.Open(t)
	artifactID := createDocument(t, database, "before")
	taskA, appends := newGatedServiceInstance(t, database)
	serverA := newTestServer(t, taskA)
	taskB, _ := newTestServiceInstance(t, storetest.SecondProcess(t, database))
	seedServiceText(t, taskA, artifactID, "before")
	waitForLandedAppends(t, taskA, artifactID)
	runRoomSync(t, newRoomSync(taskA))
	runRoomSync(t, newRoomSync(taskB))
	alice := connectWithScope(t, serverA.URL, artifactID, "alice", "read-write", "before\n")
	waitForLandedAppends(t, taskA, artifactID)

	appends.hold()
	// Registered here, so it runs before every cleanup registered above it: a test that fails
	// with the gate held leaves neither the room's close nor the service's shutdown waiting on an
	// append that never comes.
	t.Cleanup(appends.release)
	typeOver(t, alice, "before", "typed before the close")
	waitFor(t, roomWait(t), "task A's room to take alice's keystroke and its append to reach the held gate", func() bool {
		return appends.waiting.Load() > 0 &&
			strings.Contains(residentRoomMarkdown(t, taskA, artifactID), "typed before the close")
	})
	writeIssueClosed(t, taskB.store, "DOC-1", true)
	requireEnds(t, alice, "closing the issue through task B left alice's browser on task A connected")
	if stored := storedMarkdown(t, database, artifactID); strings.Contains(stored, "typed before the close") {
		t.Fatalf("the close was meant to take effect while task A held alice's keystroke at the gate, but the store already holds %q", stored)
	}
	// The gate's margin covers the latency of the close's notification, so it admits any update
	// while the close is that recent: the wait puts the close behind it, and the keystroke is
	// then stored for its age alone, the term this test guards.
	waitPastCloseMargin(t, database, "DOC-1")
	// The room's close waits for its persistence to store what it holds (CloseRoom's flush), so
	// once the gate is open and the room is gone the keystroke's append has been decided.
	appends.release()
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
	// The connect stores nothing: alice's copy holds nothing the room lacks, and a transaction
	// that changes nothing commits no update (ygo calls no update observer for one,
	// reearth/ygo#350), so the head read below is the head her keystroke is appended on.
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
// Compact does with a keep of one: one row, the head, holding the state of every row through it. No
// row at or below head remains, and the fold keeps every item of the history, so the room's
// browser updates still build on it (storedHistoryHolds).
func foldHistoryPastRoom(t *testing.T, holder pgx.Tx, artifactID string, head int64) {
	t.Helper()
	ctx := context.Background()
	through := persistence.Version(head)
	stored, err := documentThrough(artifactID, through, storedUpdates(ctx, holder, artifactID, through), maxUpdateItems)
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
	folding := persistence.Version(writtenAt)
	state, _, misread, err := fold(storedUpdates(ctx, holder, artifactID, folding), maxUpdateItems)
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
	// The connect stored nothing: alice's copy held nothing her room lacked, and a transaction
	// that changes nothing commits no update, so this head stands still unless the gate stores
	// her keystroke.
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

// newHeldAppendsService is newTestServiceInstance with the test hooks on, so a test can hold the
// room's browser appends back (HoldRoomPersistence) and have the keystrokes it makes meanwhile
// queue into one batch.
func newHeldAppendsService(t *testing.T, database *store.Store) (*Service, *httptest.Server) {
	t.Helper()
	service := New(Deps{
		Store:     database,
		Events:    events.NewBroker(),
		Identity:  headerIdentity(database),
		ServerURL: "https://dispatch.example",
		Settle:    time.Hour,
		TestHooks: true,
	})
	t.Cleanup(func() {
		if err := service.Shutdown(context.Background()); err != nil {
			t.Errorf("shutdown document service: %v", err)
		}
	})
	return service, newTestServer(t, service)
}

// One batch carries the keystrokes a room queued (roomBatchThrough), and an issue can close in
// the middle of such a run. The gate cuts the run at the first update the room took later than
// closeMargin after the close (observedBeforeClose, gateAppend): the prefix the room took in
// time is stored, exactly as those updates would have been stored one transaction each, and the
// rest stays queued and is refused ErrIssueClosed on its own call, which closes the room for its
// issue. Refusing the whole batch would lose the edits made while the issue was open; storing it
// whole would store the edits made after the close.
func TestABatchStraddlingTheIssuesCloseStoresWhatTheRoomTookInTime(t *testing.T) {
	database := storetest.Open(t)
	artifactID := createDocument(t, database, "before")
	service, server := newHeldAppendsService(t, database)
	seedServiceText(t, service, artifactID, "before")
	waitForLandedAppends(t, service, artifactID)
	var failed atomic.Bool
	service.beforeFailedEviction = func(string) { failed.Store(true) }
	alice := connectWithScope(t, server.URL, artifactID, "alice", "read-write", "before\n")
	head := storedHead(t, database, artifactID)

	// The room's appends are held until well after both keystrokes, so the first one's call
	// reads the queue with the second in it and runs one batch of the two.
	queued := queueAKeystrokeEachSideOfTheClose(t, database, service, alice, artifactID, "before", 3*closeMargin)
	// The batch of the first keystroke runs through the second: without that this would be two
	// appends of one update each, not one run the close cuts.
	if through, _, _ := service.roomBatchThrough(artifactID, queued[0], false); through != queued[1].seq {
		t.Fatalf("the batch of the first keystroke runs through position %d, not the second keystroke's %d: the two are not one run", through, queued[1].seq)
	}

	requireEnds(t, alice, "the gate refused the keystroke alice made after the close and left her browser connected")
	waitFor(t, roomWait(t), "the batch's admitted prefix to be stored", func() bool {
		return storedHead(t, database, artifactID) > head
	})
	if moved := storedHead(t, database, artifactID); moved != head+1 {
		t.Fatalf("the document moved from update %d to %d, want one stored update: the batch's admitted prefix", head, moved)
	}
	stored := storedMarkdown(t, database, artifactID)
	if !strings.Contains(stored, "typed in time") {
		t.Fatalf("the store reads %q, without the keystroke the room took while the issue was open: the batch was refused whole", stored)
	}
	if strings.Contains(stored, "typed after the close") {
		t.Fatalf("the store reads %q, holding the keystroke the room took after the close margin was out", stored)
	}
	if failed.Load() {
		t.Fatal("the refusal of the batch's rest failed the room rather than closing it for its issue")
	}
	connectWithScope(t, server.URL, artifactID, "alice", "readonly", stored)
}

// queueAKeystrokeEachSideOfTheClose closes issue DOC-1 and has alice type two keystrokes over
// from: one the room takes while the close is still within closeMargin, which the gate admits,
// and one it takes after the margin is out, which it refuses. The room's own appends are held for
// hold, so both are still queued when it returns, one run of the two for a batch or a joined
// flush to take, and it answers their classes in the room's commit order. No room sync runs in
// these tests, so nothing tells the room its issue closed in between: the gate is what notices.
func queueAKeystrokeEachSideOfTheClose(t *testing.T, database *store.Store, service *Service, alice *docstest.Peer, artifactID, from string, hold time.Duration) []*queuedClass {
	t.Helper()
	if err := service.HoldRoomPersistence(artifactID, hold); err != nil {
		t.Fatalf("hold the room's appends: %v", err)
	}
	writeIssueClosed(t, database, "DOC-1", true)
	typeOver(t, alice, from, "typed in time")
	waitPastCloseMargin(t, database, "DOC-1")
	// Half a margin past the margin: the gate judges the second keystroke's age on this process's
	// clock against the close on the database's, so the two need only run at about the same rate.
	time.Sleep(closeMargin / 2)
	typeOver(t, alice, "typed in time", "typed after the close")
	waitFor(t, roomWait(t), "both keystrokes to be queued on the room", func() bool {
		state := service.lockExistingState(artifactID)
		if state == nil {
			return false
		}
		defer service.unlockState(artifactID, state)
		return len(state.updateClasses) == 2
	})
	state := service.lockExistingState(artifactID)
	queued := slices.Clone(state.updateClasses)
	service.unlockState(artifactID, state)
	return queued
}

// requireStillQueuedAndUnstored fails unless artifactID's room still holds queued, the classes a
// flush read, in order, with none of them marked stored: a transaction whose flush failed and
// rolled back has acknowledged no keystroke to its browser as saved and has dropped none from the
// room's pending state, so each is still the room's own call's to store or refuse.
func requireStillQueuedAndUnstored(t *testing.T, service *Service, artifactID string, queued []*queuedClass) {
	t.Helper()
	for index, class := range queued {
		if class.stored.Load() {
			t.Fatalf("keystroke %d is marked stored by a transaction that stored nothing of the run: the room drops it from its queue though no row holds it", index+1)
		}
	}
	state := service.lockExistingState(artifactID)
	if state == nil {
		t.Fatal("the room keeps no state after the failed flush, so the keystrokes it queued are gone")
	}
	still := slices.Clone(state.updateClasses)
	service.unlockState(artifactID, state)
	if !slices.Equal(still, queued) {
		t.Fatalf("the room's queue holds %d classes after the failed flush, want the %d it read: none of them is dropped", len(still), len(queued))
	}
}

// A transaction that flushes the room's queue before its own write (Ledger.flushThrough: a
// snapshot's or a named version's capture, a repair, the block-id backfill) meets the same gate
// the room's own batch does, and the close can cut its run the same way. The flush then fails with
// ErrIssueClosed and records nothing: its transaction rolls back and stores neither the prefix the
// gate admitted nor the writer's own update, which the writer derives from a room state holding
// the keystroke the gate refused. The run is left exactly as the flush found it - neither
// keystroke marked stored, neither dropped from the room's queue - and the room's own batch
// decides them under the prefix rule (appendRoomTx): the one taken in time is stored, the one
// taken after the margin refused, which closes the room for its issue.
func TestAJoinedFlushTheCloseCutsFailsAndMarksNothing(t *testing.T) {
	database := storetest.Open(t)
	artifactID := createDocument(t, database, "before")
	service, server := newHeldAppendsService(t, database)
	seedServiceText(t, service, artifactID, "before")
	waitForLandedAppends(t, service, artifactID)
	alice := connectWithScope(t, server.URL, artifactID, "alice", "read-write", "before\n")
	head := storedHead(t, database, artifactID)

	// The room's own appends are held, so both keystrokes are still queued when the joined
	// transaction below flushes them, about two margins in; the hold ends two margins later.
	queued := queueAKeystrokeEachSideOfTheClose(t, database, service, alice, artifactID, "before", 4*closeMargin)

	ctx := context.Background()
	tx, err := database.Pool.Begin(ctx)
	if err != nil {
		t.Fatalf("begin the flushing transaction: %v", err)
	}
	defer tx.Rollback(ctx)
	joined, ledger := service.Join(ctx, tx)
	defer ledger.Discard()
	if err := lockDocumentRoom(joined, tx, artifactID); err != nil {
		t.Fatalf("lock the document: %v", err)
	}
	if err := ledger.flushThrough(joined, tx, artifactID, roomPoint{cursor: queued[1].cursor, seq: queued[1].seq}); !errors.Is(err, ErrIssueClosed) {
		t.Fatalf("the flush of a run the close cuts = %v, want ErrIssueClosed", err)
	}
	if flush := ledger.flushFor(artifactID); flush != nil && len(flush.classes) != 0 {
		t.Fatalf("the failed flush records %d classes at versions %v, want none: its transaction stores nothing of the run",
			len(flush.classes), flush.versions)
	}
	// The caller's transaction rolls back, as the request that met the error does, and the prefix
	// the gate admitted goes with it.
	if err := tx.Rollback(ctx); err != nil {
		t.Fatalf("roll the flushing transaction back: %v", err)
	}
	ledger.Discard()
	if moved := storedHead(t, database, artifactID); moved != head {
		t.Fatalf("the document moved from update %d to %d, want neither keystroke stored: the failed flush left a row behind", head, moved)
	}
	requireStillQueuedAndUnstored(t, service, artifactID, queued)

	// The room's own call stores what it took in time and refuses the rest, which closes the room
	// for its issue and stores nothing more.
	requireEnds(t, alice, "the room's own call refused the keystroke alice made after the close and left her browser connected")
	if moved := storedHead(t, database, artifactID); moved != head+1 {
		t.Fatalf("the document stands at update %d after the room's own call, want %d: the keystroke it took in time", moved, head+1)
	}
	stored := storedMarkdown(t, database, artifactID)
	if !strings.Contains(stored, "typed in time") {
		t.Fatalf("the store reads %q, without the keystroke the room took while the issue was open", stored)
	}
	if strings.Contains(stored, "typed after the close") {
		t.Fatalf("the store reads %q, holding the keystroke the room took after the close margin was out", stored)
	}
}

// A writer of a closed issue's documents flushes the room before its own write, and the close can
// cut that run: WriteIssueAskStates writes each withdrawn ask's state into the document, on the
// issue's row and owner-verified, through a live write whose Ledger.flushThrough stores the room's
// queued keystrokes first. The gate admits the keystroke the room took in time and refuses the one
// it took after the margin, and the write must fail there rather than carry on: its own update is
// derived from a room that holds the refused keystroke, so stored on top of the admitted prefix it
// would store a document the store refused the makings of. Nothing of the run is stored, no ask
// state with it, and both keystrokes are left to the room's own call.
func TestWritingIssueAskStatesOnARunTheCloseCutsStoresNothing(t *testing.T) {
	database := storetest.Open(t)
	artifactID := createDocument(t, database, "before")
	service, server := newHeldAppendsService(t, database)
	seedServiceText(t, service, artifactID, "Context\n\n"+regionBlock)
	// Settlement indexes the ask the write withdraws below and writes its state into the block,
	// so what the store holds from here on is what alice's browser connects on.
	settleCurrentGeneration(t, service, artifactID)
	region := blockAskOn(t, service, artifactID, "region")
	waitForLandedAppends(t, service, artifactID)
	alice := connectWithScope(t, server.URL, artifactID, "alice", "read-write", storedMarkdown(t, database, artifactID))
	head := storedHead(t, database, artifactID)

	queued := queueAKeystrokeEachSideOfTheClose(t, database, service, alice, artifactID, "Context", 4*closeMargin)

	closer := model.Actor{Kind: "user", ID: "carol"}
	err := joinedWrite(service, func(ctx context.Context) error {
		tx, _ := txFromContext(ctx)
		// The issue route holds the issue's row before it writes any document.
		if _, err := tx.Exec(ctx, `select 1 from issues where key = 'DOC-1' for no key update`); err != nil {
			return fmt.Errorf("lock the issue: %w", err)
		}
		_, err := service.WriteIssueAskStates(ctx, "DOC-1", []model.Ask{withdrawnAsk(region, closer)}, closer, false)
		return err
	})
	if !errors.Is(err, ErrIssueClosed) {
		t.Fatalf("the ask-state write over a run the close cuts = %v, want ErrIssueClosed", err)
	}
	if moved := storedHead(t, database, artifactID); moved != head {
		t.Fatalf("the document moved from update %d to %d, want nothing stored: the failed write stored the run's admitted prefix, its own update, or both", head, moved)
	}
	if stored := storedMarkdown(t, database, artifactID); strings.Contains(stored, `state="resolved"`) || strings.Contains(stored, "typed in time") {
		t.Fatalf("the store reads %q, want the document as the write found it: a write over a run the close cuts stores nothing", stored)
	}
	requireStillQueuedAndUnstored(t, service, artifactID, queued)

	// The room's own call decides the two under the batch path's prefix rule, as it would had no
	// writer joined them at all.
	requireEnds(t, alice, "the room's own call refused the keystroke alice made after the close and left her browser connected")
	if moved := storedHead(t, database, artifactID); moved != head+1 {
		t.Fatalf("the document stands at update %d after the room's own call, want %d: the keystroke it took in time", moved, head+1)
	}
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
