package docs

import (
	"context"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	gws "github.com/gorilla/websocket"
)

func TestLastPeerDisconnectSettlesDocumentBeforeEviction(t *testing.T) {
	service, artifactID := newTestService(t)
	service.settle = time.Hour
	seedServiceText(t, service, artifactID, "before")
	httpServer := httptest.NewServer(http.HandlerFunc(service.ServeHTTP))
	t.Cleanup(httpServer.Close)
	connection, response, err := gws.DefaultDialer.Dial(
		"ws"+strings.TrimPrefix(httpServer.URL, "http")+"/ws/doc/"+artifactID,
		http.Header{"X-Dispatch-User": []string{"alice"}},
	)
	if err != nil {
		t.Fatalf("connect live document: response=%#v err=%v", response, err)
	}

	editLiveTree(t, service, artifactID, replaceRun("before", "after"))
	if err := connection.Close(); err != nil {
		t.Fatalf("disconnect after edit: %v", err)
	}
	waitForDocumentVersion(t, service.store, artifactID, 2)
	if got, err := service.Text(context.Background(), artifactID); err != nil || got != "after\n" {
		t.Fatalf("reopened text = %q (%v), want settled edit", got, err)
	}
	var events int
	if err := service.store.Pool.QueryRow(context.Background(), `
		select count(*) from events where issue_key = 'DOC-1' and type = 'artifact.version'
	`).Scan(&events); err != nil {
		t.Fatalf("count artifact version events: %v", err)
	}
	if events != 1 {
		t.Fatalf("artifact.version events = %d, want 1", events)
	}
}
