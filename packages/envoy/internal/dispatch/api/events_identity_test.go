package api

import (
	"bufio"
	"context"
	"io"
	"net/http"
	"net/http/httptest"
	"sync/atomic"
	"testing"
	"time"

	"github.com/sjawhar/envoy/internal/dispatch/docs"
	"github.com/sjawhar/envoy/internal/dispatch/events"
	"github.com/sjawhar/envoy/internal/dispatch/identity"
	"github.com/sjawhar/envoy/internal/dispatch/store/storetest"
)

// endableIdentity names one person until their session ends.
type endableIdentity struct{ ended atomic.Bool }

func (i *endableIdentity) Login(*http.Request) (string, error) {
	if i.ended.Load() {
		return "", identity.ErrNoIdentity
	}
	return "alice@d.example", nil
}

// A person whose session ends (logout, a refresh the sign-in pool refuses, a lost group) stops
// receiving the event stream they opened while signed in: each heartbeat resolves them again.
func TestEventStreamClosesOnceItsPersonNoLongerResolves(t *testing.T) {
	database := storetest.Open(t)
	broker := events.NewBroker()
	documents := docs.New(docs.Deps{Store: database, Events: broker})
	t.Cleanup(func() { _ = documents.Shutdown(context.Background()) })
	person := &endableIdentity{}
	deps, err := NewDeps(DepsInput{
		Store: database, Identity: person, AgentToken: "agent-token", Docs: documents, Events: broker,
		StreamHeartbeat: 20 * time.Millisecond,
	})
	if err != nil {
		t.Fatalf("new API dependencies: %v", err)
	}
	mux := http.NewServeMux()
	Register(mux, deps)
	server := httptest.NewServer(mux)
	t.Cleanup(server.Close)

	response, err := http.Get(server.URL + "/api/v1/events")
	if err != nil {
		t.Fatalf("open event stream: %v", err)
	}
	defer response.Body.Close()
	if response.StatusCode != http.StatusOK {
		t.Fatalf("event stream status %d, want 200", response.StatusCode)
	}
	reader := bufio.NewReader(response.Body)
	if line, err := reader.ReadString('\n'); err != nil {
		t.Fatalf("first heartbeat: %q %v", line, err)
	}

	person.ended.Store(true)
	closed := make(chan error, 1)
	go func() {
		_, err := io.Copy(io.Discard, reader)
		closed <- err
	}()
	select {
	case err := <-closed:
		if err != nil {
			t.Fatalf("stream ended with %v, want a clean close", err)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("the event stream stayed open after its person stopped resolving")
	}
}
