package api

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"net"
	"net/http"
	"net/http/httptest"
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"

	gws "github.com/gorilla/websocket"
	"github.com/jackc/pgx/v5"
	"github.com/reearth/ygo/crdt"

	"github.com/sjawhar/envoy/internal/dispatch/docs"
	"github.com/sjawhar/envoy/internal/dispatch/model"
	"github.com/sjawhar/envoy/internal/dispatch/pmdoc"
	"github.com/sjawhar/envoy/internal/dispatch/store"
	"github.com/sjawhar/envoy/internal/dispatch/store/storetest"
	"github.com/sjawhar/envoy/internal/pglisten"
)

// runDocumentRoomSync runs documentService's room sync on a listener hub of its own, as
// cmd/dispatch does, until the test ends, and returns once the hub's first LISTEN stands: a
// notification committed before that reaches nobody.
func runDocumentRoomSync(t *testing.T, database *store.Store, documentService *docs.Service) {
	t.Helper()
	hub := &pglisten.Hub{
		Owner: "dispatch test",
		Dial: func(ctx context.Context) (*pgx.Conn, error) {
			return database.Pool.Dedicated(ctx, docs.ListenerApplicationPrefix+documentService.ProcessID())
		},
	}
	run := documentService.RegisterRoomSync(hub)
	listening := make(chan struct{}, 1)
	hub.OnConnected(func() {
		select {
		case listening <- struct{}{}:
		default:
		}
	})
	ctx, cancel := context.WithCancel(context.Background())
	var running sync.WaitGroup
	running.Add(2)
	go func() {
		defer running.Done()
		run(ctx)
	}()
	go func() {
		defer running.Done()
		hub.Run(ctx)
	}()
	t.Cleanup(func() {
		cancel()
		running.Wait()
	})
	select {
	case <-listening:
	case <-time.After(30 * time.Second):
		t.Fatal("the document service's listener hub never listened")
	}
}

// Closing an issue is durable the moment its transaction commits, and the commit itself tells
// every process's document rooms (migration 20261101181500's trigger,
// docs.Service.issueOpenChanged): the route makes no call of its own after it, so nothing about
// the request - a client that has hung up included - can keep a room taking edits from the
// browsers connected to it.
func TestClosingAnIssueClosesItsRoomsFromItsCommit(t *testing.T) {
	var documentService *docs.Service
	handler, database := newInteractionHandler(t, func(database *store.Store) docs.API {
		documentService = docs.New(docs.Deps{
			Store:    database,
			Identity: headerIdentity(database),
			Settle:   time.Hour,
		})
		t.Cleanup(func() { _ = documentService.Shutdown(context.Background()) })
		return documentService
	})
	runDocumentRoomSync(t, database, documentService)
	issue := createInteractionIssue(t, handler, "TEST", "Closing issue", "A spec")
	documentServer := httptest.NewServer(http.HandlerFunc(documentService.ServeHTTP))
	t.Cleanup(documentServer.Close)
	headers := http.Header{"X-Dispatch-User": []string{"alice"}}
	wsURL := "ws" + strings.TrimPrefix(documentServer.URL, "http") + "/ws/doc/" + issue.PrimaryArtifactID
	connection, wsResponse, err := gws.DefaultDialer.Dial(wsURL, headers)
	if err != nil {
		t.Fatalf("connect live document: response=%#v err=%v", wsResponse, err)
	}
	t.Cleanup(func() { _ = connection.Close() })

	body, err := json.Marshal(map[string]string{"status": "done"})
	if err != nil {
		t.Fatalf("encode close request: %v", err)
	}
	request := httptest.NewRequest(http.MethodPatch, "/api/v1/issues/"+issue.Key, bytes.NewReader(body))
	request.Header.Set("Content-Type", "application/json")
	request.Header.Set("X-Dispatch-User", "alice")
	response := httptest.NewRecorder()
	handler.ServeHTTP(response, request)
	if response.Code != http.StatusOK {
		t.Fatalf("close issue: status=%d body=%s", response.Code, response.Body.String())
	}

	waitForDocumentConnectionClose(t, connection)
	if _, err := replaceDocumentText(
		database, documentService, issue.PrimaryArtifactID, "after", model.Actor{Kind: "user", ID: "alice"},
	); !errors.Is(err, docs.ErrIssueClosed) {
		t.Fatalf("write to the closed issue's document = %v, want ErrIssueClosed", err)
	}
}

// slowWithdrawal is how long the close below holds its transaction open after writing the
// withdrawn ask's block, before a browser types: longer than the document gate's closeMargin (two
// seconds, docs/append_gate.go), by a margin no scheduling delay of the test eats.
const slowWithdrawal = 4 * time.Second

// A close's transaction writes closed_at, then withdraws the issue's open asks, writing their blocks
// into the issue's documents under each document's advisory lock, which it holds until it commits.
// A browser's keystroke in a room of such a document on another process waits on that lock, so it
// reaches the append gate only after the close committed. The keystroke was made before the close
// took effect, so it is stored however long the withdrawal took: the gate measures closeMargin from
// the close's commit (the close stamps closed_at again just before it commits), not from the start
// of its transaction. The commit's notification then closes the room, and the browser reconnects
// read-only, holding the keystroke. Here the close holds its transaction open for longer than
// closeMargin after writing the withdrawn block, and the browser types then.
func TestAKeystrokeWaitingOnASlowClosesDocumentLockIsStored(t *testing.T) {
	wrapper := &peerInsertDocs{}
	var documentService *docs.Service
	handler, database := newInteractionHandler(t, func(database *store.Store) docs.API {
		documentService = docs.New(docs.Deps{Store: database, Identity: headerIdentity(database), Settle: 20 * time.Millisecond})
		t.Cleanup(func() { _ = documentService.Shutdown(context.Background()) })
		wrapper.API = documentService
		return wrapper
	})
	runDocumentRoomSync(t, database, documentService)
	issue := createInteractionIssue(t, handler, "TEST", "A slow close", closeDecision)
	awaitIndexedAskBlock(t, handler, issue.PrimaryArtifactID, "close-decision", "Ship it?")
	awaitSettled(t, database, issue.PrimaryArtifactID)

	// The browser's room is on another process, which hears the close from its commit.
	second := storetest.SecondProcess(t, database)
	other := docs.New(docs.Deps{Store: second, Identity: headerIdentity(second), Settle: time.Hour})
	t.Cleanup(func() { _ = other.Shutdown(context.Background()) })
	runDocumentRoomSync(t, second, other)
	peer := connectBrowserPeer(t, other, issue.PrimaryArtifactID)
	wrapper.insert = func() {
		// The close's transaction began before its withdrawal wrote the block, so the keystroke
		// is typed more than closeMargin after that start.
		time.Sleep(slowWithdrawal)
		peer.appendToFirstParagraph(t, " typed during the close")
		waitForDocumentLockWaiter(t, database)
	}
	closed := dispatchRequest(t, handler, http.MethodPatch, "/api/v1/issues/"+issue.Key, map[string]string{"status": "done"}, "alice")
	if closed.Code != http.StatusOK {
		t.Fatalf("close: status=%d body=%s", closed.Code, closed.Body.String())
	}

	select {
	case <-peer.connection().Ended:
	case <-time.After(30 * time.Second):
		t.Fatal("the close left the browser's room on the other process open")
	}
	for deadline := time.Now().Add(10 * time.Second); ; time.Sleep(20 * time.Millisecond) {
		stored := storedDocumentText(t, database, issue.PrimaryArtifactID)
		if strings.Contains(stored, "typed during the close") {
			break
		}
		if time.Now().After(deadline) {
			t.Fatalf("the keystroke typed while the close's write held the document's lock was dropped: the store holds %q", stored)
		}
	}
	peer.reconnect(t)
	scope, err := peer.connection().Authenticate(strconv.Itoa(pmdoc.SchemaVersion()), 10*time.Second)
	if err != nil {
		t.Fatalf("authenticate the reconnected browser: %v", err)
	}
	if scope != "readonly" {
		t.Fatalf("the browser reconnected to the closed issue's document with scope %q, want readonly", scope)
	}
}

// waitForDocumentLockWaiter returns once a session of database waits on a document's advisory lock
// (a room's append, lockRoom), failing the test after ten seconds.
func waitForDocumentLockWaiter(t *testing.T, database *store.Store) {
	t.Helper()
	for deadline := time.Now().Add(10 * time.Second); ; time.Sleep(10 * time.Millisecond) {
		waiting, err := storetest.CountLockWaits(context.Background(), database, "%pg_advisory%lock(hashtext($1))%")
		if err != nil {
			t.Fatal(err)
		}
		if waiting > 0 {
			return
		}
		if time.Now().After(deadline) {
			t.Fatal("no append came to wait on the document's lock")
		}
	}
}

// storedDocumentText renders every stored update of artifactID, as a room loading it would hold it.
func storedDocumentText(t *testing.T, database *store.Store, artifactID string) string {
	t.Helper()
	rows, err := database.Pool.Query(context.Background(), `select update from doc_updates where artifact_id = $1 order by version`, artifactID)
	if err != nil {
		t.Fatalf("read the stored document updates: %v", err)
	}
	updates, err := pgx.CollectRows(rows, pgx.RowTo[[]byte])
	if err != nil {
		t.Fatalf("read the stored document updates: %v", err)
	}
	document := crdt.New()
	for _, update := range updates {
		if err := crdt.ApplyUpdateV1(document, update, nil); err != nil {
			t.Fatalf("apply a stored document update: %v", err)
		}
	}
	return renderDocument(t, document)
}

// waitForDocumentConnectionClose waits for the server to close a connected client's connection.
func waitForDocumentConnectionClose(t *testing.T, connection *gws.Conn) {
	t.Helper()
	connection.SetReadDeadline(time.Now().Add(10 * time.Second))
	for {
		if _, _, err := connection.ReadMessage(); err != nil {
			var networkError net.Error
			if errors.As(err, &networkError) && networkError.Timeout() {
				t.Fatal("document connection remained open after the issue closed")
			}
			return
		}
	}
}
