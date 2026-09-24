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
	"testing"
	"time"

	gws "github.com/gorilla/websocket"

	"github.com/sjawhar/envoy/internal/dispatch/docs"
	"github.com/sjawhar/envoy/internal/dispatch/identity"
	"github.com/sjawhar/envoy/internal/dispatch/model"
	"github.com/sjawhar/envoy/internal/dispatch/store"
)

// hangUpDocs is the client disconnecting in the window the refresh runs in: the request's
// context is cancelled as the handler reaches SetIssueClosed, after its transaction committed
// and before the refresh has read the issue's rooms.
type hangUpDocs struct {
	docs.API
	cancel context.CancelFunc
}

func (d *hangUpDocs) SetIssueClosed(ctx context.Context, issueKey string, closed bool) {
	d.cancel()
	d.API.SetIssueClosed(ctx, issueKey, closed)
}

// Closing an issue is durable the moment its transaction commits, so the rooms of its
// documents have to learn about it even if the client that asked is already gone: a room that
// never gets the flag keeps taking edits from the browsers connected to it until something
// else reloads it.
func TestClosingAnIssueClosesItsRoomsAfterTheClientHangsUp(t *testing.T) {
	var documentService *docs.Service
	requestCtx, cancel := context.WithCancel(context.Background())
	t.Cleanup(cancel)
	handler, _ := newInteractionHandler(t, func(database *store.Store) docs.API {
		documentService = docs.New(docs.Deps{
			Store: database,
			Identity: identity.HeaderIdentity{
				Header: "X-Dispatch-User", AllowedLogins: map[string]struct{}{"alice": {}},
			},
			Settle: time.Hour,
		})
		t.Cleanup(func() { _ = documentService.Shutdown(context.Background()) })
		return &hangUpDocs{API: documentService, cancel: cancel}
	})
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
	request := httptest.NewRequest(
		http.MethodPatch, "/api/v1/issues/"+issue.Key, bytes.NewReader(body),
	).WithContext(requestCtx)
	request.Header.Set("Content-Type", "application/json")
	request.Header.Set("X-Dispatch-User", "alice")
	response := httptest.NewRecorder()
	handler.ServeHTTP(response, request)
	if response.Code != http.StatusOK {
		t.Fatalf("close issue: status=%d body=%s", response.Code, response.Body.String())
	}

	waitForDocumentConnectionClose(t, connection)
	if _, err := documentService.ReplaceText(
		context.Background(), issue.PrimaryArtifactID, "after", model.Actor{Kind: "user", ID: "alice"},
	); !errors.Is(err, docs.ErrIssueClosed) {
		t.Fatalf("write to the closed issue's document = %v, want ErrIssueClosed", err)
	}
}

// waitForDocumentConnectionClose waits for the server to close a connected client's room.
func waitForDocumentConnectionClose(t *testing.T, connection *gws.Conn) {
	t.Helper()
	connection.SetReadDeadline(time.Now().Add(time.Second))
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
