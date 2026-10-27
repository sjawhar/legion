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

	"github.com/sjawhar/envoy/internal/dispatch/docs/docstest"
	"github.com/sjawhar/envoy/internal/dispatch/pmdoc"
	"github.com/sjawhar/envoy/internal/dispatch/store"
	"github.com/sjawhar/envoy/internal/dispatch/store/storetest"
)

// Closing or reopening an issue changes every process's rooms of its documents (INFRA-556, plan
// D5). The trigger migration 0121 puts on issues.closed_at notifies dispatch_issue_open_changed
// from the transaction that wrote it, whichever process wrote it; each process's room sync reads
// the issue and closes its rooms of the issue's documents (issueOpenChanged), so their browsers
// reconnect with the scope the issue now allows, which ygo fixes when a browser connects. A
// browser's append on a closed issue is refused by the append gate, which reads the issue itself,
// whether the notification reached the room's process or not.

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

// The notification may not reach a process in time: its listener can be redialing for up to thirty
// seconds (pglisten.RedialBackoff), and even a live one hears the close a moment after its commit.
// Here no room sync runs at all, so nothing tells the room the issue closed: the gate refuses its
// browser's next append (ErrIssueClosed), which is not stored, and the room is closed for its issue
// rather than failed, so the browser reconnects read-only.
func TestABrowserAppendOnAClosedIssueIsRefusedWithoutItsNotification(t *testing.T) {
	database := storetest.Open(t)
	artifactID := createDocument(t, database, "before")
	service, server := newTestServiceInstance(t, database)
	seedServiceText(t, service, artifactID, "before")
	waitForLandedAppends(t, service, artifactID)
	var failed atomic.Bool
	service.beforeFailedEviction = func(string) { failed.Store(true) }
	alice := connectWithScope(t, server.URL, artifactID, "alice", "read-write", "before\n")

	writeIssueClosed(t, database, "DOC-1", true)
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
