package docs

import (
	"context"
	"errors"
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

// writeIssueClosedAgo sets issueKey's closed_at in database to closedAgo before now, the row a
// close that far back leaves: the append gate weighs a browser's update against the close's age
// (closeMargin), and back-dating the row puts the close that far behind without the test waiting
// there in real time (waitPastCloseMargin waits instead, where the close must be a real one).
func writeIssueClosedAgo(t *testing.T, database *store.Store, issueKey string, closedAgo time.Duration) {
	t.Helper()
	if _, err := database.Pool.Exec(context.Background(),
		`update issues set closed_at = now() - make_interval(secs => $2) where key = $1`, issueKey, closedAgo.Seconds()); err != nil {
		t.Fatalf("close issue %s %v ago: %v", issueKey, closedAgo, err)
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

	writeIssueClosedAgo(t, database, "DOC-1", closeMargin+30*time.Second)
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

// roomResident is whether service holds a room of artifactID now (ResidentRooms).
func roomResident(service *Service, artifactID string) bool {
	return slices.Contains(service.ResidentRooms(), artifactID)
}

// waitForRoomGone waits for service to hold no room of artifactID, which the close of its issue
// issueKey takes from it (closeRoomForIssue). The close may run on a goroutine of its own - the
// append gate's refusal starts one (refuseClosedIssueAppend) - and ygo waits inside it for a
// caller in the room to leave, so the room can go after the call that closed it returns.
func waitForRoomGone(t *testing.T, service *Service, artifactID, issueKey string) {
	t.Helper()
	waitFor(t, roomWait(t), "the room of "+artifactID+", whose issue "+issueKey+" closed, to stop being resident", func() bool {
		return !roomResident(service, artifactID)
	})
}

// requireRecordClosed fails unless service's record of artifactID's room reads its issue closed,
// read through the pair the service reads it with (lockExistingState, unlockState). A record that
// reads open took no load after the issue closed, so a test that wants the load's own flag under
// the close that follows is testing nothing.
func requireRecordClosed(t *testing.T, service *Service, artifactID string) {
	t.Helper()
	state := service.lockExistingState(artifactID)
	if state == nil {
		t.Fatalf("the service holds no record of %s's room, so no load of it read the issue", artifactID)
	}
	closed := state.closed
	service.unlockState(artifactID, state)
	if !closed {
		t.Fatalf("the record of %s's room reads its issue open: its load did not mark it closed, and the close below meets the record it would have met anyway", artifactID)
	}
}

// requireRecordOpen fails unless service's record of artifactID's room reads its issue open,
// read through the pair requireRecordClosed reads it with (lockExistingState, unlockState). A
// room whose record reads closed refuses every write this service makes into it (allowInject,
// roomClosed) and arms no settlement (scheduleSettleAfterLocked), so a close that marked the
// record of a room it left standing leaves that room serving nothing.
func requireRecordOpen(t *testing.T, service *Service, artifactID string) {
	t.Helper()
	state := service.lockExistingState(artifactID)
	if state == nil {
		t.Fatalf("the service holds no record of %s's room, so nothing here holds the issue's state", artifactID)
	}
	closed := state.closed
	service.unlockState(artifactID, state)
	if closed {
		t.Fatalf("the record of %s's room reads its issue closed, and the issue is open: the room takes no write of this service and arms no settlement", artifactID)
	}
}

// A room that loads after its issue closed writes the record's closed flag itself, and the close
// this process hears afterwards must close that room all the same. Here no room of the document
// is resident - the seed's room is evicted, leaving the process as one that never opened the
// document - the issue closes in the database with nothing notified, as a listener's redial
// leaves it (this test runs no room sync), and alice's browser then loads the room: the load
// reads the issue itself and writes state.closed on the record (onLoadDocument, websocket.go),
// with no close behind it, so the room keeps every browser in it on the admission it connected
// with. The close notified next (issueOpenChanged, the issue-open lane's own handler, called here
// on the test's goroutine as setIssueClosedHere calls it) found the record's flag already
// matching the issue and returned, so nothing closed the room and its browsers held that
// admission until ygo retired the room as idle, 60 to 90 seconds later. A notified close now
// reaches closeRoomForIssue whatever the record holds (setRoomIssueOpen). The record is read back
// before that close (requireRecordClosed): a test that reads only the room resident would pass
// without the load having marked anything, and the flag the load writes is the whole case.
func TestAnIssuesCloseClosesARoomWhoseLoadAlreadyMarkedItsRecordClosed(t *testing.T) {
	database := storetest.Open(t)
	artifactID := createDocument(t, database, "before")
	service, server := newTestServiceInstance(t, database)
	seedServiceText(t, service, artifactID, "before")
	waitForLandedAppends(t, service, artifactID)
	if err := service.Evict(context.Background(), artifactID); err != nil {
		t.Fatalf("evict the room the seed loaded: %v", err)
	}
	if roomResident(service, artifactID) {
		t.Fatalf("the process still holds a room of %s before the issue closed, so the load below is not the first one after the close", artifactID)
	}

	writeIssueClosed(t, database, "DOC-1", true)
	// The admission is the issue's state as this connect reads it (authorize), so a browser
	// arriving after the close is read-only; what the close below takes from it is its socket,
	// which is what a browser admitted writable before the close loses too.
	alice := connectWithScope(t, server.URL, artifactID, "alice", "readonly", "before\n")
	if !roomResident(service, artifactID) {
		t.Fatalf("alice's browser loaded no room of %s, so no load of it read the closed issue", artifactID)
	}
	requireRecordClosed(t, service, artifactID)

	service.issueOpenChanged(store.WithTransactionTracking(context.Background()), "DOC-1")
	waitForRoomGone(t, service, artifactID, "DOC-1")
	requireEnds(t, alice, "the close of issue DOC-1 left alice's browser on the room of "+artifactID+" connected")
}

// The append gate closes such a room too. Bob opens the document while the issue is closed and
// nothing has been notified, as a listener's redial leaves it (this test runs no room sync): his
// connect loads the room, and that load writes the record's closed flag with no close behind it
// (onLoadDocument, websocket.go). The issue then reopens, still unheard here, so the record
// keeps the flag its load wrote while the issue is open - only a close or a reopen this process
// hears rewrites it (setRoomIssueOpen) - and alice, connecting now, is admitted writable: her
// socket's admission and her authentication both read the issue open (authorize,
// authorizeSchemaVersion), and ygo fixes that scope until she reconnects. The issue closes a
// second time, again unheard, and the gate refuses her keystroke's append, the close standing
// further back than closeMargin so the update's age decides it (ErrIssueClosed); the room's
// persistence hands that refusal to refuseClosedIssueAppend (StoreUpdateContext). The gate
// returned on a record already reading closed, which left alice writing into a room nothing
// closed; its close is now a notified one, which closes the room whatever the record holds
// (setRoomIssueOpen), so both browsers' sockets end and they reconnect read-only. The room is
// closed for its issue rather than failed (beforeFailedEviction).
func TestTheAppendGateClosesARoomWhoseLoadAlreadyMarkedItsRecordClosed(t *testing.T) {
	database := storetest.Open(t)
	artifactID := createDocument(t, database, "before")
	service, server := newTestServiceInstance(t, database)
	seedServiceText(t, service, artifactID, "before")
	waitForLandedAppends(t, service, artifactID)
	var failed atomic.Bool
	service.beforeFailedEviction = func(string) { failed.Store(true) }
	if err := service.Evict(context.Background(), artifactID); err != nil {
		t.Fatalf("evict the room the seed loaded: %v", err)
	}
	if roomResident(service, artifactID) {
		t.Fatalf("the process still holds a room of %s before the issue closed, so bob's connect below loads none", artifactID)
	}

	writeIssueClosed(t, database, "DOC-1", true)
	bob := connectWithScope(t, server.URL, artifactID, "bob", "readonly", "before\n")
	requireRecordClosed(t, service, artifactID)

	writeIssueClosed(t, database, "DOC-1", false)
	alice := connectWithScope(t, server.URL, artifactID, "alice", "read-write", "before\n")
	// The reopen reached no room sync, and alice's connect loaded nothing: the room was already
	// resident, so the record still holds the flag bob's load wrote.
	requireRecordClosed(t, service, artifactID)

	// Closed far enough back that the gate judges alice's keystroke by its age alone: inside the
	// margin it stores a browser's update whatever its age (observedBeforeClose).
	writeIssueClosedAgo(t, database, "DOC-1", closeMargin+30*time.Second)
	// Alice's connect stored nothing that changed the document, and the gate stores nothing from
	// here on, so this head stands still unless it takes her keystroke.
	head := storedHead(t, database, artifactID)

	typeOver(t, alice, "before", "typed after the second close")
	requireEnds(t, alice, "the gate refused alice's keystroke on the room whose load marked its record closed and left her browser connected")
	requireEnds(t, bob, "the gate's close of the room whose load marked its record closed left bob's read-only browser connected")
	waitForRoomGone(t, service, artifactID, "DOC-1")
	if moved := storedHead(t, database, artifactID); moved != head {
		t.Fatalf("the document moved from update %d to %d: the gate stored a browser's update on a closed issue", head, moved)
	}
	if failed.Load() {
		t.Fatal("the refusal failed the room rather than closing it for its issue")
	}
	connectWithScope(t, server.URL, artifactID, "alice", "readonly", "before\n")
}

// A room stores one update per keystroke (PersistCoalesceWindow = -1), so a browser typing into
// a room whose issue closed unheard has each keystroke refused on its own (ErrIssueClosed), and
// the room must close once for the burst, however many keystrokes it holds. Here alice's
// appends wait at a gate the test holds (gatedAppends) while she types four times, so all four
// keystrokes are in the room and queued for its persistence before any of them reaches the
// store; opening that gate sends the four to the append gate, which refuses them one after
// another. The first refusal takes the mark of the instance whose append it was and starts the
// close alone (roomInstance.issueCloseStarted, refuseClosedIssueAppend); the three after it
// start nothing.
//
// The mark saves a goroutine and a close pass per refused keystroke, not a close of the wrong
// room: a close that carries an instance closes the room only while the room still holds that
// instance (roomServer.closeOwned), so each close after the first finds the room no longer its
// own, or gone, and closes nothing. What each of them costs instead is a goroutine of its own
// and a pass of the close, and one that reaches a room this instance still holds - the close in
// front of it waiting for a caller inside the room, say - takes the room's record under the
// close gate and writes the record's latest edit source to the document's pending-settlement
// row in a transaction of its own (closeRefusedAppendRoom, closeRecordLocked,
// persistClosedLastActor). The calls the service makes to beforeRefusedAppendClose are the
// closes the refusals started, which the count below takes, so that count reads four without
// the mark and one with it. RoomCloses counts the closes that took a room (roomServer's
// closeHeld), and it reads one either way: nothing loads the room again here, so the three
// closes after the first find the room gone and close nothing. It is the control, and the room
// a browser does load into that gap - which such a close must leave standing, and must write
// nothing of - is the room of the two tests below.
func TestABurstOfRefusedKeystrokesClosesItsRoomOnce(t *testing.T) {
	database := storetest.Open(t)
	artifactID := createDocument(t, database, "before")
	service, appends := newGatedServiceInstance(t, database)
	service.hooks = newTestHooks()
	server := newTestServer(t, service)
	seedServiceText(t, service, artifactID, "before")
	waitForLandedAppends(t, service, artifactID)
	var failed atomic.Bool
	service.beforeFailedEviction = func(string) { failed.Store(true) }
	var started atomic.Int64
	service.beforeRefusedAppendClose = func(string) { started.Add(1) }
	alice := connectWithScope(t, server.URL, artifactID, "alice", "read-write", "before\n")
	waitForLandedAppends(t, service, artifactID)

	writeIssueClosedAgo(t, database, "DOC-1", closeMargin+30*time.Second)
	appends.hold()
	const keystrokes = 4
	typed := "before"
	for stroke := 1; stroke <= keystrokes; stroke++ {
		next := "keystroke " + strconv.Itoa(stroke)
		typeOver(t, alice, typed, next)
		typed = next
	}
	// The room applies each keystroke as it arrives and queues its append behind the gate, in
	// order, so the last of them in the room's own copy says all four are in the room with none
	// of them stored.
	waitFor(t, roomWait(t), "the room to take all four of alice's keystrokes, the last of them "+strconv.Quote(typed), func() bool {
		return strings.Contains(residentRoomMarkdown(t, service, artifactID), typed)
	})
	appends.release()

	requireEnds(t, alice, "the gate refused alice's burst of keystrokes on the closed issue and left her browser connected")
	// Waited for rather than read off the room being gone: ygo takes a room out of its map
	// before it waits for the room's persistence worker, so this close is counted after the
	// room has stopped being resident, and one started behind it later still
	// (waitForRefusedAppendClose).
	waitForRefusedAppendClose(t, service)
	if roomResident(service, artifactID) {
		t.Fatalf("the close the refused keystrokes started left a room of %s resident", artifactID)
	}
	counters := requireRoomCounters(t, service, artifactID)
	if closes := counters.RoomCloses; closes != 1 {
		t.Fatalf("the service closed a room of %s %d times for a burst of %d refused keystrokes, want once", artifactID, closes, keystrokes)
	}
	if begun := started.Load(); begun != 1 {
		t.Fatalf("the refusals of a burst of %d keystrokes on %s started %d closes, want one", keystrokes, artifactID, begun)
	}
	if failed.Load() {
		t.Fatal("the refusals failed the room rather than closing it for its issue")
	}
}

// A close the append gate's refusal started, waiting behind a close already under way, leaves
// alone the room a browser loaded in between. ygo takes a room out of its map before it waits
// for the room's persistence worker (CloseRoom's flush), so a browser can load the room again
// while a close of it runs, and a close queued behind that one on the room's close gate closed
// the room that browser loaded. Here the gate refuses alice's keystroke and the close it starts
// parks before it runs (beforeRefusedAppendClose); the eviction that follows stands for the
// close in front of it, which took alice's room, and bob's connect for the browser that loads
// the room in the gap, a new instance under the same name (instanceOf, GetDoc). The parked close
// is then let go, and it must leave bob's room standing: it closes the room only while the room
// still holds the instance whose append was refused, checked under the close gate
// (closeRefusedAppendRoom, roomServer.closeOwned). Closing by name instead takes bob's room, and
// his socket with it.
//
// The close is waited for rather than slept past: the service counts that goroutine in evictWG,
// the group its Shutdown joins, so the group draining is the close having run
// (waitForRefusedAppendClose).
func TestACloseForARefusedAppendLeavesTheRoomThatLoadedInBetweenAlone(t *testing.T) {
	database := storetest.Open(t)
	artifactID := createDocument(t, database, "before")
	service, server := newTestServiceInstance(t, database)
	seedServiceText(t, service, artifactID, "before")
	waitForLandedAppends(t, service, artifactID)
	alice := connectWithScope(t, server.URL, artifactID, "alice", "read-write", "before\n")
	refused := service.instanceOf(service.srv.GetDoc(artifactID))
	if refused == nil {
		t.Fatalf("the service lists no instance for the room of %s alice is in", artifactID)
	}
	parked, release := parkRefusedAppendClose(service)

	writeIssueClosedAgo(t, database, "DOC-1", closeMargin+30*time.Second)
	typeOver(t, alice, "before", "typed after the close")
	awaitRefusedAppendCloseParked(t, parked)

	if err := service.Evict(context.Background(), artifactID); err != nil {
		t.Fatalf("evict the room whose append was refused: %v", err)
	}
	requireEnds(t, alice, "evicting the room alice typed into left her browser connected")
	bob := connectWithScope(t, server.URL, artifactID, "bob", "readonly", "before\n")
	if !roomResident(service, artifactID) {
		t.Fatalf("bob's browser loaded no room of %s, so the parked close has none to leave alone", artifactID)
	}
	reloaded := service.instanceOf(service.srv.GetDoc(artifactID))
	if reloaded == nil || reloaded == refused {
		t.Fatalf("the room of %s bob loaded holds the instance whose append was refused, so nothing here is tested", artifactID)
	}

	close(release)
	waitForRefusedAppendClose(t, service)
	if !roomResident(service, artifactID) {
		t.Fatalf("the close of the instance whose append was refused took the room of %s that loaded while it waited", artifactID)
	}
	if held := service.instanceOf(service.srv.GetDoc(artifactID)); held != reloaded {
		t.Fatalf("the room of %s holds an instance other than the one bob loaded: that close took his room", artifactID)
	}
	requireRoomAnswers(t, bob, "the close of the refused append's instance left bob on the room that loaded while it waited")
}

// parkRefusedAppendClose makes service park the first close the append gate's refusal starts,
// with the refused instance's mark taken and the close not yet run, so a test can make the room
// load again while that close waits (beforeRefusedAppendClose). It answers the channel the hook
// closes once that close is parked (awaitRefusedAppendCloseParked) and the one the test closes
// to let it go. A close started after the first runs unparked: the staging holds one close, and
// a second waiting on a channel the test never closes would be a close that never returns.
func parkRefusedAppendClose(service *Service) (parked chan struct{}, release chan struct{}) {
	parked, release = make(chan struct{}), make(chan struct{})
	var first atomic.Bool
	service.beforeRefusedAppendClose = func(string) {
		if !first.CompareAndSwap(false, true) {
			return
		}
		close(parked)
		<-release
	}
	return parked, release
}

// awaitRefusedAppendCloseParked waits for the close one of the parking hooks installed for it
// to be parked in that hook (parkRefusedAppendClose, parkCloseAfterRoomClosed), and fails the
// test unless it parks within roomWait: until it is, the close has not reached the window the
// test stages its work in, where the room it names is still the room the test is about to take
// away from it, or the row it has still to write is still unwritten by any other room.
func awaitRefusedAppendCloseParked(t *testing.T, parked <-chan struct{}) {
	t.Helper()
	select {
	case <-parked:
	case <-time.After(roomWait(t)):
		t.Fatalf("the close the gate's refusal started had not parked after %v", roomWait(t))
	}
}

// waitForRefusedAppendClose waits for the close the append gate's refusal started to have run.
// The service registers that goroutine with evictWG, the group its Shutdown joins
// (refuseClosedIssueAppend), so the group draining is the close having returned - the one it
// left alone as much as the one it closed.
func waitForRefusedAppendClose(t *testing.T, service *Service) {
	t.Helper()
	done := make(chan struct{})
	go func() {
		service.evictWG.Wait()
		close(done)
	}()
	select {
	case <-done:
	case <-time.After(roomWait(t)):
		t.Fatalf("the close the gate's refusal started had not finished after %v", roomWait(t))
	}
}

// requireRoomAnswers drains the answers peer already holds, asks its room for the whole document
// and fails unless the room answers within roomWait: the socket is open and a room is there to
// answer on it. The room answers a connection's messages in order, so the answer waited for is
// this request's.
func requireRoomAnswers(t *testing.T, peer *docstest.Peer, what string) {
	t.Helper()
	for drained := false; !drained; {
		select {
		case <-peer.Answers:
		default:
			drained = true
		}
	}
	if err := peer.AskForDocument(); err != nil {
		t.Fatalf("%s: ask the room for its document: %v", what, err)
	}
	select {
	case <-peer.Answers:
	case <-peer.Ended:
		t.Fatalf("%s: the browser's socket was closed", what)
	case <-time.After(roomWait(t)):
		t.Fatalf("%s: the room never answered the request for its document", what)
	}
}

// Such a close writes nothing to the record of the room that loaded in between either. One try
// at the close holds the room's close gate for its check that the room still holds the refused
// instance, for the write to that room's record and for the close (closeRefusedAppendRoom,
// closeRecordLocked, roomServer.closeOwned), and the record is written on the first check that
// passes and on no later one. The gate goes between the tries a room a caller is inside costs
// (closeWhenFree), so a try after the one that wrote can find the name holding another instance
// and close nothing while that write stands; the check of the close here passes on no try at
// all, so it leaves the record as it found it.
// Writing the record first and checking after marked closed the record of whatever room held
// the name by then and then left that room standing - a room serving a browser with a record
// that says its issue is closed. Here the gate refuses alice's keystroke and the close it
// starts parks before it runs (beforeRefusedAppendClose); her room is evicted, which stands for
// the close in front of it; the issue reopens in the database, unheard here (this test runs no
// room sync); and bob's connect loads the room again, a new instance under the same name
// (instanceOf, GetDoc) with a writable browser in it, since his connect reads the issue open.
// That room is one that must go on taking writes.
//
// The parked close is then let go and waited for (evictWG, waitForRefusedAppendClose), and
// three readings say it left bob's room alone. The room is still resident and still holds bob's
// instance: a close that took the room instead fails these, and the ownership check under the
// gate is what keeps it from taking it. Its record reads the issue open (requireRecordOpen):
// this is the reading the record write above that check breaks, since the write it makes is on
// bob's record. And bob's browser still writes, his keystroke moving the stored document
// (storedHead): a record reading closed refuses every write this service makes into the room
// (allowInject, roomClosed) and arms no settlement for what it stores
// (scheduleSettleAfterLocked), while the gate judges a browser's own append by the issue its
// appending transaction reads, so the keystroke is stored under the record write as well and
// this reading is the one the control leaves standing.
func TestACloseForARefusedAppendLeavesTheRecordOfTheRoomThatLoadedInBetweenAlone(t *testing.T) {
	database := storetest.Open(t)
	artifactID := createDocument(t, database, "before")
	service, server := newTestServiceInstance(t, database)
	seedServiceText(t, service, artifactID, "before")
	waitForLandedAppends(t, service, artifactID)
	alice := connectWithScope(t, server.URL, artifactID, "alice", "read-write", "before\n")
	refused := service.instanceOf(service.srv.GetDoc(artifactID))
	if refused == nil {
		t.Fatalf("the service lists no instance for the room of %s alice is in", artifactID)
	}
	parked, release := parkRefusedAppendClose(service)

	// Closed far enough back that the gate judges alice's keystroke by its age alone: inside the
	// margin it stores a browser's update whatever its age (observedBeforeClose).
	writeIssueClosedAgo(t, database, "DOC-1", closeMargin+30*time.Second)
	typeOver(t, alice, "before", "typed after the close")
	awaitRefusedAppendCloseParked(t, parked)

	if err := service.Evict(context.Background(), artifactID); err != nil {
		t.Fatalf("evict the room whose append was refused: %v", err)
	}
	requireEnds(t, alice, "evicting the room alice typed into left her browser connected")
	writeIssueClosed(t, database, "DOC-1", false)
	bob := connectWithScope(t, server.URL, artifactID, "bob", "read-write", "before\n")
	if !roomResident(service, artifactID) {
		t.Fatalf("bob's browser loaded no room of %s, so the parked close has none to leave alone", artifactID)
	}
	reloaded := service.instanceOf(service.srv.GetDoc(artifactID))
	if reloaded == nil || reloaded == refused {
		t.Fatalf("the room of %s bob loaded holds the instance whose append was refused, so nothing here is tested", artifactID)
	}

	close(release)
	waitForRefusedAppendClose(t, service)
	if !roomResident(service, artifactID) {
		t.Fatalf("the close of the instance whose append was refused took the room of %s that loaded while it waited", artifactID)
	}
	if held := service.instanceOf(service.srv.GetDoc(artifactID)); held != reloaded {
		t.Fatalf("the room of %s holds an instance other than the one bob loaded: that close took his room", artifactID)
	}
	requireRecordOpen(t, service, artifactID)
	// Bob's connect makes a durable append of its own that changes no content; it lands before
	// the head is read, so the head stands still unless his keystroke moves it.
	waitForLandedAppends(t, service, artifactID)
	head := storedHead(t, database, artifactID)
	typeOver(t, bob, "before", "typed into the room the close left alone")
	waitFor(t, roomWait(t), "bob's keystroke into the room the close left alone to be stored", func() bool {
		return storedHead(t, database, artifactID) > head
	})
}

// requireRecordLastActor fails unless the record of artifactID's room holds actor as the latest
// edit source no settlement has taken (state.lastActor). A record holding none has nothing for
// the close below to move to the document's pending-settlement row, so the test would prove
// nothing.
func requireRecordLastActor(t *testing.T, service *Service, artifactID string, actor model.Actor) {
	t.Helper()
	state := service.lockExistingState(artifactID)
	if state == nil {
		t.Fatalf("the service holds no record of %s's room, so it holds no latest edit source", artifactID)
	}
	lastActor := state.lastActor
	service.unlockState(artifactID, state)
	if lastActor == nil || *lastActor != actor {
		t.Fatalf("the record of %s's room holds %+v as its latest edit source, want %+v", artifactID, lastActor, actor)
	}
}

// A record a load marked closed can hold a latest edit source no settlement took, and the close
// the append gate's refusal makes moves it to the document's pending-settlement row: the load
// that wrote the record's closed flag wrote that flag alone, with no close behind it to move the
// source (onLoadDocument, closeRefusedAppendRoom, persistLastActor, lastActorPersisted). Here
// alice's edit is recorded as a landed browser edit records it - a durable pending author and
// the record's latest edit source, with no settlement to take it, this service's Settle being
// an hour (recordActor) - the issue then closes with nothing notified, as a listener's redial
// leaves it (this test runs no room sync), and bob's connect loads the room: that load writes
// the record's closed flag and leaves the source the record holds as it is (onLoadDocument).
// The issue reopens, still unheard, so alice's browser is admitted writable; bob then closes
// his tab, the issue closes a second time, far enough back for the update's age to decide it,
// and the gate refuses alice's keystroke (ErrIssueClosed), whose close finds the record already
// reading closed. Alice is the one browser left in the room, so her refused keystroke credits
// her and nobody else (connectedCreditLocked), which is the actor her recorded edit already
// left on the record; with bob still in the room the keystroke would credit nobody and leave
// the record holding none.
//
// Without the move the row keeps the latest edit source of the last append that landed, the
// seed's own user:seed, and the last assertion below fails reading that actor instead of
// alice. The close writes that row after it has closed the room and outside the room's close
// gate, since the write takes the document's advisory lock (closeRefusedAppendRoom,
// persistClosedLastActor), so the read below waits for the close's own goroutine to have
// finished rather than for the room to have gone (evictWG, waitForRefusedAppendClose).
func TestTheAppendGatesCloseMovesTheRecordsLatestEditSourceToThePendingSettlementRow(t *testing.T) {
	ctx := context.Background()
	database := storetest.Open(t)
	artifactID := createDocument(t, database, "before")
	service, server := newTestServiceInstance(t, database)
	seedServiceText(t, service, artifactID, "before")
	waitForLandedAppends(t, service, artifactID)
	if err := service.Evict(ctx, artifactID); err != nil {
		t.Fatalf("evict the room the seed loaded: %v", err)
	}
	editor := model.Actor{Kind: "user", ID: "alice"}
	service.recordActor(t, artifactID, editor)

	writeIssueClosed(t, database, "DOC-1", true)
	bob := connectWithScope(t, server.URL, artifactID, "bob", "readonly", "before\n")
	requireRecordClosed(t, service, artifactID)
	requireRecordLastActor(t, service, artifactID, editor)

	writeIssueClosed(t, database, "DOC-1", false)
	alice := connectWithScope(t, server.URL, artifactID, "alice", "read-write", "before\n")
	// The reopen reached no room sync, and alice's connect loaded nothing: the room was already
	// resident, so the record still holds the flag the load wrote and the source it was holding.
	requireRecordClosed(t, service, artifactID)
	requireRecordLastActor(t, service, artifactID, editor)
	owed, named, err := readOwedSettlement(ctx, service.store.Pool, artifactID)
	if err != nil {
		t.Fatalf("read the document's pending settlement before the close: %v", err)
	}
	if !owed {
		t.Fatalf("the document owes no settlement before the close, so its row is not the one a load left behind")
	}
	if named != nil && *named == editor {
		t.Fatalf("the pending-settlement row already names %+v as the latest editor, so the close below is not what puts her there", editor)
	}

	// Bob closes his tab, so alice is the one browser in the room when she types and the
	// keystroke's credit names her (connectedCreditLocked). The record's source and the
	// keystroke's credit are then the same actor, and which of the two the record holds at the
	// close does not decide the row below.
	bob.Close()
	waitFor(t, roomWait(t), "the record of the room to hold alice's browser alone", func() bool {
		return connectedBrowsers(service, artifactID) == 1
	})

	// Closed far enough back that the gate judges alice's keystroke by its age alone: inside the
	// margin it stores a browser's update whatever its age (observedBeforeClose).
	writeIssueClosedAgo(t, database, "DOC-1", closeMargin+30*time.Second)
	typeOver(t, alice, "before", "typed after the second close")
	requireEnds(t, alice, "the gate refused alice's keystroke on the room whose load marked its record closed and left her browser connected")
	waitForRoomGone(t, service, artifactID, "DOC-1")
	waitForRefusedAppendClose(t, service)
	owed, named, err = readOwedSettlement(ctx, service.store.Pool, artifactID)
	if err != nil {
		t.Fatalf("read the document's pending settlement after the close: %v", err)
	}
	if !owed || named == nil || *named != editor {
		t.Fatalf("the pending-settlement row of %s names %+v as its latest editor (owed=%t), want %+v: the gate's close left the record's source behind",
			artifactID, named, owed, editor)
	}
}

// A close the append gate's refusal started must put no older edit source back on the document's
// pending-settlement row. It reads the source off the room's record under the room's close gate
// and writes it to the row after the close and outside that gate, since that write takes the
// document's advisory lock (closeRefusedAppendRoom, closeRecordLocked, persistClosedLastActor).
// The issue can reopen inside that window and a newer room can load: ygo takes a room out of its
// map before CloseRoom waits for the room's persistence worker, and the gate goes between the
// tries a room a caller is inside costs (closeWhenFree). That room's browser edit writes its own
// source to the same row when its append lands, and the late write put the older source back
// over it, so the settlement the document owes named the person whose edit is not the latest.
//
// Here alice types into a writable room on an open issue: her keystroke lands and leaves her on
// the record as the latest edit source and on the row, with no settlement to take either, this
// service's Settle being an hour (connectedCreditLocked). The issue then closes unheard, as a
// listener's redial leaves it (this test runs no room sync), far enough back for the update's
// age to decide it, and the gate refuses her next keystroke (ErrIssueClosed); the close that
// refusal starts reads alice off the record and closes the room. It is held where it has closed
// the room and has the row still to write: the room server runs afterClose there, inside the
// close gate, once ygo's CloseRoom has returned (roomServer.closeHeld, parkCloseAfterRoomClosed).
//
// While it is held the issue reopens in the database, still unheard, and bob's connect loads the
// room again, a new instance under the same name (instanceOf, GetDoc) with a writable browser in
// it, since his connect reads the issue open. He is the one browser in that room, so his
// keystroke credits him and nobody else (connectedCreditLocked); it is stored, and its append
// writes him to the row. The test reads that row back before it lets the close go: until the row
// names bob there is no older source for the close to put back, and the readings after it would
// hold whether the close wrote or not.
//
// The close is then let go and waited for (evictWG, waitForRefusedAppendClose). The row must
// still name bob: the write reads the room's record under the document's advisory lock and
// commits nothing unless that record still reads closed at the credit sequence the source was
// read at, which bob's load moved off closed and his edit moved past (persistLastActor,
// recordClosedAt). Bob's record must still hold him as its latest edit source and still read the
// issue open: a close whose row write is refused clears neither, since clearing is what a
// written row buys (persistClosedLastActor, lastActorPersisted).
func TestACloseForARefusedAppendLeavesThePendingSettlementRowTheRoomThatLoadedInBetweenWrote(t *testing.T) {
	database := storetest.Open(t)
	artifactID := createDocument(t, database, "before")
	service, server := newTestServiceInstance(t, database)
	seedServiceText(t, service, artifactID, "before")
	waitForLandedAppends(t, service, artifactID)
	alice := connectWithScope(t, server.URL, artifactID, "alice", "read-write", "before\n")
	refused := service.instanceOf(service.srv.GetDoc(artifactID))
	if refused == nil {
		t.Fatalf("the service lists no instance for the room of %s alice is in", artifactID)
	}
	// Alice's connect makes a durable append of its own that changes no content; it lands before
	// the head is read, so the head stands still unless her keystroke moves it.
	waitForLandedAppends(t, service, artifactID)
	head := storedHead(t, database, artifactID)
	alicesEdit := model.Actor{Kind: "user", ID: "alice"}
	typeOver(t, alice, "before", "alice typed")
	waitFor(t, roomWait(t), "alice's keystroke on the open issue to be stored", func() bool {
		return storedHead(t, database, artifactID) > head
	})
	waitForLandedAppends(t, service, artifactID)
	requireRecordLastActor(t, service, artifactID, alicesEdit)
	requirePendingSettlementActor(t, service, artifactID, alicesEdit,
		"alice's keystroke landed on the open issue, before the close reads her off the record")
	parked, release := parkCloseAfterRoomClosed(service, artifactID)

	// Closed far enough back that the gate judges alice's next keystroke by its age alone: inside
	// the margin it stores a browser's update whatever its age (observedBeforeClose).
	writeIssueClosedAgo(t, database, "DOC-1", closeMargin+30*time.Second)
	typeOver(t, alice, "alice typed", "alice typed after the close")
	awaitRefusedAppendCloseParked(t, parked)
	requireEnds(t, alice, "the close the gate's refusal started closed the room and left alice's browser connected")
	if roomResident(service, artifactID) {
		t.Fatalf("a room of %s is resident while the close is parked after it closed the room, so bob loads no new instance", artifactID)
	}
	// The parked close holds a snapshot of the record to write, and that snapshot names alice:
	// the record reads closed, which here only this close can have made it read - the issue
	// closed unheard, the room loaded while it was open, and a close writes that flag under the
	// gate before it closes the room (closeRecordLocked) - and the source it read is the alice
	// the record still holds, nothing having moved it since. A close holding no snapshot, or one
	// holding none of its own, writes no row whatever the guard does, and the readings below
	// would hold on their own.
	requireRecordClosed(t, service, artifactID)
	requireRecordLastActor(t, service, artifactID, alicesEdit)

	writeIssueClosed(t, database, "DOC-1", false)
	bob := connectWithScope(t, server.URL, artifactID, "bob", "read-write", "alice typed\n")
	reloaded := service.instanceOf(service.srv.GetDoc(artifactID))
	if reloaded == nil || reloaded == refused {
		t.Fatalf("the room of %s bob loaded holds the instance whose append was refused, so nothing here is tested", artifactID)
	}
	// Bob's connect makes a durable append of its own that changes no content, and alice's socket
	// leaves the record behind her closed room: both are waited for, so the row his keystroke
	// writes is the last word on it and the keystroke's credit names him alone.
	waitForLandedAppends(t, service, artifactID)
	waitFor(t, roomWait(t), "the record of the room that loaded while the close waited to hold bob's browser alone", func() bool {
		return connectedBrowsers(service, artifactID) == 1
	})
	bobsEdit := model.Actor{Kind: "user", ID: "bob"}
	typeOver(t, bob, "alice typed", "bob typed while the close waited")
	waitFor(t, roomWait(t), "bob's keystroke to write him to the pending-settlement row of "+artifactID, func() bool {
		named := pendingSettlementActor(t, service, artifactID)
		return named != nil && *named == bobsEdit
	})
	requireRecordLastActor(t, service, artifactID, bobsEdit)
	requireRecordOpen(t, service, artifactID)

	close(release)
	waitForRefusedAppendClose(t, service)
	requirePendingSettlementActor(t, service, artifactID, bobsEdit,
		"the close let go once the room that loaded while it waited had written the row")
	requireRecordLastActor(t, service, artifactID, bobsEdit)
	requireRecordOpen(t, service, artifactID)
}

// parkCloseAfterRoomClosed makes service park the first close of artifactID's room that closed
// it, where ygo's CloseRoom has returned and the close has the record's latest edit source still
// to write to the document's pending-settlement row (closeRefusedAppendRoom,
// persistClosedLastActor): the room server runs afterClose there, holding the room's close gate
// (roomServer.closeHeld). It answers the channel the hook closes once that close is parked
// (awaitRefusedAppendCloseParked) and the one the test closes to let it go. The hook the service
// installed, the test hooks' close count, runs on every close as it did (New).
//
// afterClose is documented as a hook that must not block, and this one blocks: nothing waits
// behind the gate it holds here, since a repair that finds the gate held writes nothing and
// returns rather than waiting (roomServer.holdOpen), neither a browser's load nor its keystroke
// takes that gate, and no other close of the room is made. A close after the first runs
// unparked, as does every close of another room: the staging holds one close, and a second
// waiting on a channel the test never closes would be a close that never returns.
func parkCloseAfterRoomClosed(service *Service, artifactID string) (parked chan struct{}, release chan struct{}) {
	parked, release = make(chan struct{}), make(chan struct{})
	counted := service.srv.afterClose
	var first atomic.Bool
	service.srv.afterClose = func(room string) {
		if counted != nil {
			counted(room)
		}
		if room != artifactID || !first.CompareAndSwap(false, true) {
			return
		}
		close(parked)
		<-release
	}
	return parked, release
}

// pendingSettlementActor is the latest editor the document's pending-settlement row names, nil
// where it names none or the document owes no settlement (readOwedSettlement).
func pendingSettlementActor(t *testing.T, service *Service, artifactID string) *model.Actor {
	t.Helper()
	owed, named, err := readOwedSettlement(context.Background(), service.store.Pool, artifactID)
	if err != nil {
		t.Fatalf("read the pending settlement of %s: %v", artifactID, err)
	}
	if !owed {
		return nil
	}
	return named
}

// requirePendingSettlementActor fails unless the document's pending-settlement row names actor as
// its latest editor; where says what the row was read after.
func requirePendingSettlementActor(t *testing.T, service *Service, artifactID string, actor model.Actor, where string) {
	t.Helper()
	if named := pendingSettlementActor(t, service, artifactID); named == nil || *named != actor {
		t.Fatalf("%s: the pending-settlement row of %s names %+v as its latest editor, want %+v", where, artifactID, named, actor)
	}
}
