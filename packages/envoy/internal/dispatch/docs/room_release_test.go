package docs

import (
	"context"
	"fmt"
	"net/http"
	"net/http/httptest"
	"slices"
	"strings"
	"sync"
	"testing"
	"time"

	gws "github.com/gorilla/websocket"
	"github.com/reearth/ygo/crdt"

	"github.com/sjawhar/envoy/internal/dispatch/events"
	"github.com/sjawhar/envoy/internal/dispatch/identity"
	"github.com/sjawhar/envoy/internal/dispatch/model"
	"github.com/sjawhar/envoy/internal/dispatch/pmdoc"
	"github.com/sjawhar/envoy/internal/dispatch/store"
	"github.com/sjawhar/envoy/internal/dispatch/store/storetest"
)

// The room cap counts live rooms, and the service forgets a document's state once its room has
// gone: touching more documents than the cap since the service started - opened in the editor,
// read and written through the API - leaves a document nobody has opened openable, and once the
// rooms the editor opened go idle the service's per-document maps hold only the rooms still live.
// Before, the service kept state for every document it had touched and counted it against the
// cap, so the document socket answered every document not touched since the last restart 503.
//
// The editor opens its documents a batch at a time, each batch idling out before the next, as
// documents are opened over a day: what fills the cap is how many were touched, not how many are
// open at once. The documents the API writes stay live here, since a room an Apply opens with no
// peer is never idle-evicted (LEGION-484); TestRoomsTheAPIOpenedReleaseTheCapOnceIdle covers those.
func TestDocumentsOpenAfterMoreThanTheRoomCapWereTouched(t *testing.T) {
	service, database := newRoomReleaseService(t)
	const written, read, opened = 100, 100, maxLiveRooms
	ids := createDocuments(t, database, written+read+opened+1)
	writtenIDs, readIDs, openedIDs := ids[:written], ids[written:written+read], ids[written+read:len(ids)-1]
	untouched := ids[len(ids)-1]
	httpServer := httptest.NewServer(http.HandlerFunc(service.ServeHTTP))
	t.Cleanup(httpServer.Close)

	for _, id := range writtenIDs {
		appendThroughAPI(t, service, id)
	}
	for _, id := range readIDs {
		if _, err := service.Text(context.Background(), id); err != nil {
			t.Fatalf("read document %s: %v", id, err)
		}
	}
	for batch := range slices.Chunk(openedIDs, 200) {
		openInEditor(t, service, httpServer.URL, batch)
		waitFor(t, 30*time.Second, "the rooms the editor opened to go idle", func() bool {
			return len(service.srv.Rooms()) == written
		})
	}
	if connection, err := dialDocument(httpServer.URL, untouched); err != nil {
		t.Errorf("open a document nobody touched after %d were: %v, want it admitted", len(ids)-1, err)
	} else {
		leaveDocument(connection)
	}
	waitFor(t, 30*time.Second, "the untouched document's room to go idle", func() bool {
		return len(service.srv.Rooms()) == written
	})
	assertPerDocumentStateFollowsLiveRooms(t, service)
}

// Rooms the API opens - an agent's write, a read that warms the room - with no peer ever joining
// are evicted on the idle schedule too, so touching more documents than the cap through the API
// alone leaves a new document openable and the service's per-document maps empty once they idle.
func TestRoomsTheAPIOpenedReleaseTheCapOnceIdle(t *testing.T) {
	t.Skip("LEGION-484: ygo never idle-stamps a room an Apply opened with no peer; #1723 pins the ygo release whose Apply stamps it, and deletes this skip")
	service, database := newRoomReleaseService(t)
	ids := createDocuments(t, database, maxLiveRooms+51)
	touched, untouched := ids[:len(ids)-1], ids[len(ids)-1]
	httpServer := httptest.NewServer(http.HandlerFunc(service.ServeHTTP))
	t.Cleanup(httpServer.Close)

	for index, id := range touched {
		if index%2 == 0 {
			appendThroughAPI(t, service, id)
			continue
		}
		if err := service.docView(context.Background(), id, func(*crdt.Doc) {}); err != nil {
			t.Fatalf("warm read of document %s: %v", id, err)
		}
	}
	waitFor(t, 30*time.Second, "the rooms the API opened to go idle", func() bool {
		return len(service.srv.Rooms()) == 0
	})
	connection, err := dialDocument(httpServer.URL, untouched)
	if err != nil {
		t.Fatalf("open a document nobody touched after %d were: %v, want it admitted", len(touched), err)
	}
	leaveDocument(connection)
	waitFor(t, 30*time.Second, "the untouched document's room to go idle", func() bool {
		return len(service.srv.Rooms()) == 0
	})
	assertPerDocumentStateFollowsLiveRooms(t, service)
}

// newRoomReleaseService is a document service whose rooms idle out a second after their last
// peer leaves rather than a minute, so a test can watch a thousand of them go.
func newRoomReleaseService(t *testing.T) (*Service, *store.Store) {
	t.Helper()
	database := storetest.Open(t)
	service := New(Deps{
		Store:     database,
		Events:    events.NewBroker(),
		Identity:  identity.HeaderIdentity{Header: "X-Dispatch-User", AllowedLogins: map[string]struct{}{"alice": {}}},
		ServerURL: "https://dispatch.example",
		Settle:    20 * time.Millisecond,
	})
	// ygo's idle sweeper reads it when the first room starts it, so it is set before any room.
	service.srv.RoomIdleTimeout = time.Second
	t.Cleanup(func() {
		if err := service.Shutdown(context.Background()); err != nil {
			t.Errorf("shutdown document service: %v", err)
		}
	})
	return service, database
}

// createDocuments creates n documents on issue DOC-1 holding one paragraph, in a few statements:
// each with that paragraph's Proof state stored and a first version matching it, so loading one
// owes no settlement.
func createDocuments(t *testing.T, database *store.Store, n int) []string {
	t.Helper()
	tree, err := pmdoc.Parse("Room cap probe.")
	if err != nil {
		t.Fatalf("parse seed document: %v", err)
	}
	markdown, err := pmdoc.Render(tree)
	if err != nil {
		t.Fatalf("render seed document: %v", err)
	}
	seed := crdt.New()
	fragment := seed.GetXmlFragment(fragmentName)
	seed.Transact(func(txn *crdt.Transaction) {
		if err := pmdoc.Update(txn, fragment, tree); err != nil {
			t.Errorf("write seed document: %v", err)
		}
	})
	createDocument(t, database, "") // the DOC project and issue DOC-1
	rows, err := database.Pool.Query(context.Background(), `
		with created as (
			insert into artifacts (issue_key, project_key, slug, name, kind, is_primary, created_by)
			select 'DOC-1', 'DOC', 'room-release-' || g, 'document.md', 'doc', false, '{"kind":"user","id":"alice"}'
			from generate_series(1, $1) g
			returning id
		), seeded as (
			insert into doc_updates (artifact_id, version, update, content_changed)
			select id, 1, $2, true from created
		)
		insert into artifact_versions (artifact_id, number, markdown, authors, doc_update_version)
		select id, 1, $3, '[{"kind":"user","id":"alice"}]', 1 from created
		returning artifact_id::text
	`, n, crdt.EncodeStateAsUpdateV1(seed, nil), markdown)
	if err != nil {
		t.Fatalf("create documents: %v", err)
	}
	defer rows.Close()
	var ids []string
	for rows.Next() {
		var id string
		if err := rows.Scan(&id); err != nil {
			t.Fatalf("scan created document: %v", err)
		}
		ids = append(ids, id)
	}
	if err := rows.Err(); err != nil {
		t.Fatalf("create documents: %v", err)
	}
	return ids
}

// appendThroughAPI appends a paragraph as the edit route does: an edit joined to a transaction,
// published to the room once it commits.
func appendThroughAPI(t *testing.T, service *Service, id string) {
	t.Helper()
	ctx := context.Background()
	tx, err := service.store.Pool.Begin(ctx)
	if err != nil {
		t.Fatalf("begin edit: %v", err)
	}
	defer tx.Rollback(ctx)
	joined, ledger := service.Join(ctx, tx)
	defer ledger.Discard()
	ops := []model.EditOp{{Op: "insert", After: "end", Markdown: "Room cap probe."}}
	if _, err := service.ApplyOps(joined, id, ops, model.Actor{Kind: "session", ID: "agent"}, nil); err != nil {
		t.Fatalf("edit document %s: %v", id, err)
	}
	if err := ledger.Commit(ctx); err != nil {
		t.Fatalf("commit edit of document %s: %v", id, err)
	}
}

// openInEditor opens each document as a browser does, waits for its room to be live, and leaves.
func openInEditor(t *testing.T, service *Service, serverURL string, ids []string) {
	t.Helper()
	work := make(chan string)
	failures := make(chan error, len(ids))
	var workers sync.WaitGroup
	for range 8 {
		workers.Add(1)
		go func() {
			defer workers.Done()
			for id := range work {
				connection, err := dialDocument(serverURL, id)
				if err != nil {
					failures <- fmt.Errorf("open document %s: %w", id, err)
					continue
				}
				deadline := time.Now().Add(10 * time.Second)
				for service.srv.GetDoc(id) == nil && time.Now().Before(deadline) {
					time.Sleep(time.Millisecond)
				}
				if service.srv.GetDoc(id) == nil {
					failures <- fmt.Errorf("document %s's room never went live", id)
				}
				leaveDocument(connection)
			}
		}()
	}
	for _, id := range ids {
		work <- id
	}
	close(work)
	workers.Wait()
	close(failures)
	var refused []error
	for err := range failures {
		refused = append(refused, err)
	}
	if len(refused) > 0 {
		t.Errorf("%d of %d editor opens failed; the first: %v", len(refused), len(ids), refused[0])
	}
}

// dialDocument opens id's document socket as a browser does; a refusal names the status it
// answered.
func dialDocument(serverURL, id string) (*gws.Conn, error) {
	connection, response, err := gws.DefaultDialer.Dial(
		"ws"+strings.TrimPrefix(serverURL, "http")+"/ws/doc/"+id,
		http.Header{"X-Dispatch-User": []string{"alice"}},
	)
	if err != nil {
		status := "no response"
		if response != nil {
			status = response.Status
		}
		return nil, fmt.Errorf("%s: %w", status, err)
	}
	return connection, nil
}

// leaveDocument closes connection as a browser tab does, with a close frame, and waits for the
// server's close in reply.
func leaveDocument(connection *gws.Conn) {
	deadline := time.Now().Add(5 * time.Second)
	_ = connection.WriteControl(gws.CloseMessage, gws.FormatCloseMessage(gws.CloseNormalClosure, ""), deadline)
	_ = connection.SetReadDeadline(deadline)
	for {
		if _, _, err := connection.NextReader(); err != nil {
			break
		}
	}
	_ = connection.Close()
}

// assertPerDocumentStateFollowsLiveRooms checks that every map the service keeps per document holds
// the live rooms at most: room state for each live room and nothing else, and nothing in the maps
// that last only while an operation runs.
func assertPerDocumentStateFollowsLiveRooms(t *testing.T, service *Service) {
	t.Helper()
	live := map[string]bool{}
	for _, room := range service.srv.Rooms() {
		live[room] = true
	}
	states, detached := 0, 0
	service.rooms.Range(func(key, _ any) bool {
		states++
		if !live[key.(string)] {
			detached++
		}
		return true
	})
	if detached > 0 || states != len(live) {
		t.Errorf("the service keeps state for %d documents, %d of them with no live room, want state for the %d live rooms alone",
			states, detached, len(live))
	}
	for name, registry := range map[string]*sync.Map{
		"conditional edit gates": &service.conditionalGates,
		"preloads":               &service.preloads,
		"rebuilds":               &service.rebuilding,
	} {
		registry.Range(func(key, _ any) bool {
			t.Errorf("the service's %s keep document %s", name, key)
			return true
		})
	}
	service.suppressMu.Lock()
	suppressed := len(service.suppressed)
	service.suppressMu.Unlock()
	if suppressed != 0 {
		t.Errorf("the service holds persistence-suppression slots for %d documents, want none", suppressed)
	}
	persistence := service.persistence.(*PgVersioned)
	persistence.locksMu.Lock()
	locks := len(persistence.locks)
	persistence.locksMu.Unlock()
	if locks != 0 {
		t.Errorf("the document store keeps a room lock for %d documents nothing holds, want none", locks)
	}
}
