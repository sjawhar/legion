package docs

import (
	"context"
	"errors"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	gws "github.com/gorilla/websocket"
	"github.com/reearth/ygo/crdt"

	"github.com/sjawhar/envoy/internal/dispatch/model"
)

// newerContentDocument is a document holding "before" and, after it, an element this build does
// not know (InjectUnknownElementForTest), resident in the service it returns. The service settles
// only when a test calls for it.
func newerContentDocument(t *testing.T) (*Service, string) {
	t.Helper()
	service, artifactID := newTestService(t)
	service.settle = time.Hour
	seedServiceText(t, service, artifactID, "before")
	if err := service.InjectUnknownElementForTest(context.Background(), artifactID); err != nil {
		t.Fatalf("write an element this build does not know: %v", err)
	}
	return service, artifactID
}

// storedHoldsUnknownElement reports whether the document's stored history holds the element
// InjectUnknownElementForTest wrote, and the head version that history stands at.
func storedHoldsUnknownElement(t *testing.T, service *Service, artifactID string) (bool, int64) {
	t.Helper()
	loaded, err := NewPgVersioned(service.store).Load(context.Background(), artifactID)
	if err != nil {
		t.Fatalf("load the stored document: %v", err)
	}
	doc := crdt.New()
	if err := crdt.ApplyUpdateV1(doc, loaded.Update, nil); err != nil {
		t.Fatalf("decode the stored document: %v", err)
	}
	for _, child := range doc.GetXmlFragment(fragmentName).Children() {
		if element, ok := child.(*crdt.YXmlElement); ok && element.NodeName == UnknownElementForTest {
			return true, int64(loaded.Version)
		}
	}
	return false, int64(loaded.Version)
}

// A document holding an element this build does not know is newer content, not a tree outside the
// schema: a newer build wrote it while two builds serve one database. Every read answers
// ErrDocNewerContent, whether a room holds the document or only the store does.
func TestADocumentHoldingAnUnknownElementReadsAsNewerContent(t *testing.T) {
	service, artifactID := newerContentDocument(t)
	reads := []struct {
		name string
		read func() error
	}{
		{"text", func() error {
			_, err := service.Text(context.Background(), artifactID)
			return err
		}},
		{"text with token", func() error {
			_, _, err := service.TextWithToken(context.Background(), artifactID)
			return err
		}},
		{"blocks", func() error {
			_, err := service.Blocks(context.Background(), artifactID)
			return err
		}},
	}
	check := func(t *testing.T, where string) {
		t.Helper()
		for _, read := range reads {
			err := read.read()
			if !errors.Is(err, ErrDocNewerContent) || errors.Is(err, ErrDocSchema) || !strings.Contains(err.Error(), UnknownElementForTest) {
				t.Fatalf("%s %s: %v, want ErrDocNewerContent naming %q and not ErrDocSchema", where, read.name, err, UnknownElementForTest)
			}
		}
	}
	check(t, "resident")
	if err := service.Evict(context.Background(), artifactID); err != nil {
		t.Fatalf("evict the document: %v", err)
	}
	check(t, "stored")
	if cacheHas(service, artifactID) {
		t.Fatal("a document holding newer content was cached")
	}
}

// Settlement skips a document holding newer content as it skips a tree outside the schema: it
// writes no version, records no failure and leaves the live room standing. It never retries the
// read until the room fails, which would answer every read 503.
func TestSettlementSkipsADocumentHoldingNewerContent(t *testing.T) {
	service, artifactID := newerContentDocument(t)
	state := service.room(artifactID)
	state.mu.Lock()
	generation := state.roomGeneration
	state.mu.Unlock()
	service.settleRoom(artifactID, generation)

	var versions int
	if err := service.store.Pool.QueryRow(context.Background(), `
		select count(*) from artifact_versions where artifact_id = $1
	`, artifactID).Scan(&versions); err != nil {
		t.Fatalf("count document versions: %v", err)
	}
	if versions != 1 {
		t.Fatalf("settlement wrote %d versions for a document holding newer content, want 1", versions)
	}
	if _, err := service.Text(context.Background(), artifactID); !errors.Is(err, ErrDocNewerContent) {
		t.Fatalf("read after the settlement: %v, want ErrDocNewerContent", err)
	}
	state.mu.Lock()
	defer state.mu.Unlock()
	if state.failed != nil {
		t.Fatalf("settlement failed the live room: %v", state.failed)
	}
	if state.settleFailures != 0 {
		t.Fatalf("settlement recorded %d failures for a document holding newer content", state.settleFailures)
	}
	if !state.unsettled {
		t.Fatal("settlement released the unsettled state of a document it could not read")
	}
}

// A replacement from markdown repairs a tree outside the schema, but over newer content it would
// delete the element this build cannot read. So it is an ordinary refusal, ErrDocNewerContent, and
// the stored document keeps the element and gains no update, whether a room holds the document or
// a replacement after a restart reads it from the store.
func TestAReplacementOverNewerContentIsRefusedAndKeepsTheElement(t *testing.T) {
	service, artifactID := newerContentDocument(t)
	if err := service.Evict(context.Background(), artifactID); err != nil {
		t.Fatalf("evict the document: %v", err)
	}
	held, head := storedHoldsUnknownElement(t, service, artifactID)
	if !held {
		t.Fatal("the stored document does not hold the element this build does not know")
	}
	replace := func(t *testing.T, service *Service, where string) {
		t.Helper()
		_, err := joinedReplaceText(service, artifactID, "replaced\n", model.Actor{Kind: "user", ID: "alice"})
		if !errors.Is(err, ErrDocNewerContent) || errors.Is(err, ErrDocSchema) {
			t.Fatalf("%s replacement: %v, want ErrDocNewerContent and not ErrDocSchema", where, err)
		}
		if held, after := storedHoldsUnknownElement(t, service, artifactID); !held || after != head {
			t.Fatalf("%s replacement left the stored document holding the element %t at head %d, want true at %d", where, held, after, head)
		}
	}
	replace(t, service, "stored")
	if err := service.warmLiveDocument(context.Background(), artifactID); err != nil {
		t.Fatalf("load the document's room: %v", err)
	}
	replace(t, service, "resident")
	if _, err := service.Text(context.Background(), artifactID); !errors.Is(err, ErrDocNewerContent) {
		t.Fatalf("read after the refused replacements: %v, want ErrDocNewerContent", err)
	}
}

// The document websocket admits no connection to a room holding newer content, since a browser
// editor would drop the element it cannot build and write the result back. It closes the upgrade
// with documentNewerContentCloseCode, which the Hocuspocus provider answers by reconnecting, not
// with documentSchemaCloseCode, which the dashboard reads as the repair state.
func TestDocumentSocketClosesARoomHoldingNewerContentWithItsOwnCode(t *testing.T) {
	for _, resident := range []bool{true, false} {
		name := "stored"
		if resident {
			name = "resident"
		}
		t.Run(name, func(t *testing.T) {
			service, artifactID := newerContentDocument(t)
			if !resident {
				if err := service.Evict(context.Background(), artifactID); err != nil {
					t.Fatalf("evict the document: %v", err)
				}
			}
			httpServer := httptest.NewServer(http.HandlerFunc(service.ServeHTTP))
			t.Cleanup(httpServer.Close)
			headers := http.Header{"X-Dispatch-User": []string{"alice"}}
			wsURL := "ws" + strings.TrimPrefix(httpServer.URL, "http") + "/ws/doc/" + artifactID
			connection, response, err := gws.DefaultDialer.Dial(wsURL, headers)
			if err != nil {
				t.Fatalf("connect a document holding newer content: response=%#v err=%v, want an upgrade the server closes", response, err)
			}
			t.Cleanup(func() { _ = connection.Close() })
			connection.SetReadDeadline(time.Now().Add(time.Second))
			_, message, err := connection.ReadMessage()
			var closed *gws.CloseError
			if !errors.As(err, &closed) || closed.Code != documentNewerContentCloseCode {
				t.Fatalf("first frame = %q (%v), want close %d before any sync", message, err, documentNewerContentCloseCode)
			}
			if _, err := service.Text(context.Background(), artifactID); !errors.Is(err, ErrDocNewerContent) {
				t.Fatalf("read after the refused socket: %v, want ErrDocNewerContent", err)
			}
		})
	}
}
