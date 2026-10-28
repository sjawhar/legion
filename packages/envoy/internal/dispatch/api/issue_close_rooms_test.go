package api

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"net"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"time"

	gws "github.com/gorilla/websocket"
	"github.com/jackc/pgx/v5"

	"github.com/sjawhar/envoy/internal/dispatch/docs"
	"github.com/sjawhar/envoy/internal/dispatch/model"
	"github.com/sjawhar/envoy/internal/dispatch/store"
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
// every process's document rooms (migration 0137's trigger, docs.Service.issueOpenChanged): the
// route makes no call of its own after it, so nothing about the request - a client that has hung
// up included - can keep a room taking edits from the browsers connected to it.
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
