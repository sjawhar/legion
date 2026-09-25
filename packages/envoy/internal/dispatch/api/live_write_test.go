package api

import (
	"context"
	"errors"
	"net/http"
	"net/http/httptest"
	"reflect"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	gws "github.com/gorilla/websocket"
	"github.com/jackc/pgx/v5"
	"github.com/reearth/ygo/crdt"
	"github.com/reearth/ygo/encoding"
	"github.com/reearth/ygo/persistence"
	ygsync "github.com/reearth/ygo/sync"

	"github.com/sjawhar/envoy/internal/dispatch/docs"
	"github.com/sjawhar/envoy/internal/dispatch/identity"
	"github.com/sjawhar/envoy/internal/dispatch/model"
	"github.com/sjawhar/envoy/internal/dispatch/pmdoc"
	"github.com/sjawhar/envoy/internal/dispatch/store"
)

// These tests hold an API transaction open between its live document write and a forced
// failure, and check that nothing outside the transaction ever sees the write: not the room,
// not a browser, not a version, not the durable document. While the handler is held the test
// reads only the resident room and a database connection of its own, because the handler's
// pool is full of work queued behind the transaction's locks (store.Open sizes it to the CPU
// count, 4 on CI's runner).

func TestEditArtifactRollbackNeverReachesTheRoom(t *testing.T) {
	f := newHeldWriteFixture(t, 50*time.Millisecond)
	f.connectPlainSocket(t)
	responses := f.startFailingEdit(t)
	waitForBeforeApply(t, f.failure)
	if _, err := f.docs.ApplyOps(context.Background(), f.issue.PrimaryArtifactID, []model.EditOp{{Op: "replace", Find: "before", With: "before"}}, model.Actor{Kind: "user", ID: "alice"}, nil); err != nil {
		t.Fatalf("apply live update before transactional edit: %v", err)
	}
	waitForDocumentUpdate(t, f.persistence)
	f.waitForLockWaiter(t)
	f.assertArtifactKeyShareLockAvailable(t)
	// A settlement pending when the transactional write starts must not version it either
	// before or after the rollback.
	f.docs.ScheduleSettlement(f.issue.PrimaryArtifactID)
	closeTestGate(f.failure.releaseBeforeApply)
	waitForPostApply(t, f.failure)
	waitForDocumentUpdate(t, f.persistence)
	f.assertLiveText(t, "before\n")
	f.assertVersionCount(t, 1)
	closeTestGate(f.failure.release)
	f.expectFailedResponse(t, responses)
	f.assertLiveText(t, "before\n")
	f.assertVersionCount(t, 1)
	f.assertDurableText(t, "before\n")
}

func TestBrowserTypingDuringAHeldEditKeepsOnlyItsOwnText(t *testing.T) {
	f := newHeldWriteFixture(t, 50*time.Millisecond)
	peer := f.connectPeer(t)
	responses := f.startFailingEdit(t)
	closeTestGate(f.failure.releaseBeforeApply)
	waitForPostApply(t, f.failure)
	peer.barrier(t)
	// The browser's append waits on the transaction's room lock and its settlements wait on
	// that append, so they are still coming when the handler gives up.
	const typed = "Typed while the edit was open."
	peer.appendParagraph(t, typed)
	f.waitForLiveText(t, typed)
	f.assertLiveTextLacks(t, "after")
	closeTestGate(f.failure.release)
	f.expectFailedResponse(t, responses)
	f.assertNoVersionHolds(t, "after")
	f.assertDurableText(t, "before\n\n"+typed+"\n")
	peer.assertTextLacks(t, "after")
	// The browser's append gets past the released room lock and its paragraph settles.
	f.waitForVersionHolding(t, typed)
}

func TestBrowserTypingNextToAHeldEditKeepsItsText(t *testing.T) {
	f := newHeldWriteFixture(t, 50*time.Millisecond)
	peer := f.connectPeer(t)
	responses := f.startFailingEdit(t)
	closeTestGate(f.failure.releaseBeforeApply)
	waitForPostApply(t, f.failure)
	peer.barrier(t)
	// The browser types at the end of the text it holds. Had the room broadcast the held edit,
	// the typed text would hang off the edit's items and vanish with them.
	peer.appendToFirstParagraph(t, " more")
	f.waitForLiveText(t, " more")
	closeTestGate(f.failure.release)
	f.expectFailedResponse(t, responses)
	f.assertNoVersionHolds(t, "after")
	f.assertDurableText(t, "before more\n")
}

func TestReconnectingBrowserCannotBringBackAHeldEdit(t *testing.T) {
	f := newHeldWriteFixture(t, 50*time.Millisecond)
	peer := f.connectPeer(t)
	responses := f.startFailingEdit(t)
	closeTestGate(f.failure.releaseBeforeApply)
	waitForPostApply(t, f.failure)
	peer.barrier(t)
	closeTestGate(f.failure.release)
	f.expectFailedResponse(t, responses)
	// The browser reconnects with the document it kept, as the SPA's provider does, and its
	// sync sends the room whatever the room lacks.
	peer.reconnect(t)
	peer.barrier(t)
	f.assertLiveTextLacks(t, "after")
	f.assertNoVersionHolds(t, "after")
	f.assertDurableText(t, "before\n")
	peer.assertTextLacks(t, "after")
}

func TestCommentQueuedBehindAHeldEditAnchorsOnlyCommittedText(t *testing.T) {
	f := newHeldWriteFixture(t, time.Hour)
	f.connectPlainSocket(t)
	responses := f.startFailingEdit(t)
	closeTestGate(f.failure.releaseBeforeApply)
	waitForPostApply(t, f.failure)
	comments := make(chan *httptest.ResponseRecorder, 1)
	go func() {
		comments <- sessionRequest(t, f.handler, http.MethodPost, "/api/v1/issues/"+f.issue.Key+"/comments", map[string]any{
			"body": "on the edit", "anchor": map[string]any{"artifact": "spec", "quote": "after"}, "actor": sessionActor(),
		})
	}()
	f.waitForLockWaiter(t)
	closeTestGate(f.failure.release)
	f.expectFailedResponse(t, responses)
	comment := awaitResponse(t, comments)
	if comment.Code == http.StatusCreated {
		t.Fatalf("comment anchored on a rolled-back edit: status=%d body=%s", comment.Code, comment.Body.String())
	}
	f.assertNoVersionHolds(t, "after")
	f.assertDurableText(t, "before\n")
}

func TestSuggestionAcceptRollbackNeverReachesTheRoom(t *testing.T) {
	f := newHeldWriteFixture(t, 50*time.Millisecond)
	f.connectPlainSocket(t)
	earlierResponse := sessionRequest(t, f.handler, http.MethodPost, "/api/v1/issues/"+f.issue.Key+"/comments", map[string]any{
		"body": "earlier", "anchor": map[string]any{"artifact": "spec", "quote": "before"}, "actor": sessionActor(),
	})
	if earlierResponse.Code != http.StatusCreated {
		t.Fatalf("create earlier anchored comment: status=%d body=%s", earlierResponse.Code, earlierResponse.Body.String())
	}
	earlier := decodeBody[model.Comment](t, earlierResponse)
	suggestion := sessionRequest(t, f.handler, http.MethodPost, "/api/v1/issues/"+f.issue.Key+"/comments", map[string]any{
		"body":       "replace it",
		"anchor":     map[string]any{"artifact": "spec", "quote": "before"},
		"suggestion": map[string]string{"replace_with": "after"},
		"actor":      sessionActor(),
	})
	if suggestion.Code != http.StatusCreated {
		t.Fatalf("create suggestion: status=%d body=%s", suggestion.Code, suggestion.Body.String())
	}
	comment := decodeBody[model.Comment](t, suggestion)
	drainDocumentUpdates(f.persistence)
	closeTestGate(f.failure.releaseBeforeApply)
	responses := make(chan *httptest.ResponseRecorder, 1)
	go func() {
		responses <- dispatchRequest(t, f.handler, http.MethodPost, "/api/v1/comments/"+comment.ID+"/accept", map[string]any{}, "alice")
	}()
	waitForPostApply(t, f.failure)
	waitForDocumentUpdate(t, f.persistence)
	f.assertLiveText(t, "before\n")
	closeTestGate(f.failure.release)
	if response := awaitResponse(t, responses); response.Code != http.StatusInternalServerError {
		t.Fatalf("accept with forced post-apply failure: status=%d body=%s", response.Code, response.Body.String())
	}
	reloaded := dispatchRequest(t, f.handler, http.MethodGet, "/api/v1/comments/"+earlier.ID, nil, "alice")
	if reloaded.Code != http.StatusOK {
		t.Fatalf("read earlier comment after rollback: status=%d body=%s", reloaded.Code, reloaded.Body.String())
	}
	if comment := decodeBody[struct {
		Comment model.Comment `json:"comment"`
	}](t, reloaded).Comment; comment.Anchor == nil || comment.Anchor.Orphaned {
		t.Fatalf("earlier comment after rollback = %#v, want its original anchor", comment)
	}
	events := dispatchRequest(t, f.handler, http.MethodGet, "/api/v1/issues/"+f.issue.Key+"/events", nil, "alice")
	if events.Code != http.StatusOK || strings.Contains(events.Body.String(), `"type":"comment.anchor_refreshed"`) {
		t.Fatalf("rollback anchor events = status=%d body=%s, want no committed cascade event", events.Code, events.Body.String())
	}
	f.assertLiveText(t, "before\n")
	f.assertNoVersionHolds(t, "after")
	f.assertDurableText(t, "before\n")
}

func TestCommentProjectionFailureNeverReachesTheRoom(t *testing.T) {
	for _, action := range []string{"reply", "resolve"} {
		t.Run(action, func(t *testing.T) {
			f := newHeldWriteFixture(t, time.Hour, func(failure *postApplyFailureDocs) {
				failure.failProjectMark = true
				failure.projectMarkPasses = 1
			})
			peer := f.connectPeer(t)
			rootResponse := sessionRequest(t, f.handler, http.MethodPost, "/api/v1/issues/"+f.issue.Key+"/comments", map[string]any{
				"body": "root", "anchor": map[string]any{"artifact": "spec", "quote": "before"}, "actor": sessionActor(),
			})
			if rootResponse.Code != http.StatusCreated {
				t.Fatalf("create anchored root: status=%d body=%s", rootResponse.Code, rootResponse.Body.String())
			}
			root := decodeBody[model.Comment](t, rootResponse)
			projected := loadMarkProjection(t, f.database, f.issue.PrimaryArtifactID, root.ID)
			closeTestGate(f.failure.releaseBeforeApply)
			responses := make(chan *httptest.ResponseRecorder, 1)
			go func() {
				if action == "reply" {
					responses <- dispatchRequest(t, f.handler, http.MethodPost, "/api/v1/issues/"+f.issue.Key+"/comments", map[string]any{
						"body": "reply", "reply_to": root.ID,
					}, "alice")
					return
				}
				responses <- sessionRequest(t, f.handler, http.MethodPost, "/api/v1/comments/"+root.ID+"/resolve", map[string]any{"actor": sessionActor()})
			}()
			waitForPostApply(t, f.failure)
			peer.barrier(t)
			peer.assertProjection(t, root.ID, projected)
			closeTestGate(f.failure.release)
			if response := awaitResponse(t, responses); response.Code != http.StatusInternalServerError {
				t.Fatalf("%s projection failure: status=%d body=%s", action, response.Code, response.Body.String())
			}
			peer.barrier(t)
			peer.assertProjection(t, root.ID, projected)
			if durable, _ := findMarkProjection(t, f.database, f.issue.PrimaryArtifactID, root.ID); !reflect.DeepEqual(durable, projected) {
				t.Fatalf("durable projection of %s = %#v, want %#v", root.ID, durable, projected)
			}
			f.assertLiveText(t, "before\n")
		})
	}
}

func TestDocumentUploadRollbackNeverReachesTheRoom(t *testing.T) {
	f := newHeldWriteFixture(t, time.Hour)
	f.connectPlainSocket(t)
	closeTestGate(f.failure.releaseBeforeApply)
	responses := make(chan *httptest.ResponseRecorder, 1)
	go func() {
		responses <- multipartRequest(t, f.handler, "/api/v1/issues/"+f.issue.Key+"/artifacts", map[string]string{
			"name": "spec.md",
		}, "spec.md", "text/markdown", []byte("after"), "alice")
	}()
	waitForPostApply(t, f.failure)
	f.assertLiveText(t, "before\n")
	closeTestGate(f.failure.release)
	if response := awaitResponse(t, responses); response.Code != http.StatusInternalServerError {
		t.Fatalf("upload with forced post-replace failure: status=%d body=%s", response.Code, response.Body.String())
	}
	f.assertLiveText(t, "before\n")
	f.assertDurableText(t, "before\n")
}

// heldWriteFixture is an issue whose document edits fail after their live write, behind gates
// the test opens.
type heldWriteFixture struct {
	settle      time.Duration
	handler     http.Handler
	docs        *docs.Service
	failure     *postApplyFailureDocs
	persistence *recordingVersionedStore
	database    *store.Store
	probe       *pgx.Conn
	wsURL       string
	sockets     *servedSockets
	headers     http.Header
	issue       struct {
		Key               string `json:"key"`
		PrimaryArtifactID string `json:"primary_artifact_id"`
	}
}

func newHeldWriteFixture(t *testing.T, settle time.Duration, configure ...func(*postApplyFailureDocs)) *heldWriteFixture {
	t.Helper()
	f := &heldWriteFixture{settle: settle, headers: http.Header{"X-Dispatch-User": []string{"alice"}}}
	f.handler, f.database = newInteractionHandler(t, func(database *store.Store) docs.API {
		f.persistence = &recordingVersionedStore{
			PgVersioned: docs.NewPgVersioned(database),
			updates:     make(chan struct{}, 8),
		}
		f.docs = docs.New(docs.Deps{
			Store:       database,
			Persistence: f.persistence,
			Identity: identity.HeaderIdentity{
				Header: "X-Dispatch-User", AllowedLogins: map[string]struct{}{"alice": {}},
			},
			Settle: settle,
		})
		t.Cleanup(func() { _ = f.docs.Shutdown(context.Background()) })
		f.failure = &postApplyFailureDocs{
			API:                f.docs,
			beforeApply:        make(chan struct{}),
			releaseBeforeApply: make(chan struct{}),
			applied:            make(chan struct{}),
			release:            make(chan struct{}),
		}
		for _, change := range configure {
			change(f.failure)
		}
		// Runs before the service shuts down: a failed assertion must not leave a request
		// holding its transaction at a gate.
		t.Cleanup(func() {
			closeTestGate(f.failure.releaseBeforeApply)
			closeTestGate(f.failure.release)
		})
		return f.failure
	})
	f.issue = createInteractionIssue(t, f.handler, "TEST", "Held edit", "before")
	f.sockets = &servedSockets{finished: make(map[string]chan struct{})}
	documentServer := httptest.NewServer(f.sockets.serve(f.docs.ServeHTTP))
	t.Cleanup(documentServer.Close)
	f.wsURL = "ws" + strings.TrimPrefix(documentServer.URL, "http") + "/ws/doc/" + f.issue.PrimaryArtifactID
	probe, err := pgx.ConnectConfig(context.Background(), f.database.Pool.Config().ConnConfig.Copy())
	if err != nil {
		t.Fatalf("connect database probe: %v", err)
	}
	t.Cleanup(func() { _ = probe.Close(context.Background()) })
	f.probe = probe
	return f
}

// connectPlainSocket keeps the room resident, so the test reads it without the database.
func (f *heldWriteFixture) connectPlainSocket(t *testing.T) {
	t.Helper()
	connection, response, err := gws.DefaultDialer.Dial(f.wsURL, f.headers)
	if err != nil {
		t.Fatalf("connect live document: response=%#v err=%v", response, err)
	}
	t.Cleanup(func() { _ = connection.Close() })
	drainDocumentUpdates(f.persistence)
}

func (f *heldWriteFixture) connectPeer(t *testing.T) *syncedPeer {
	t.Helper()
	peer := &syncedPeer{wsURL: f.wsURL, sockets: f.sockets, headers: f.headers, artifactID: f.issue.PrimaryArtifactID, doc: crdt.New()}
	peer.connect(t)
	t.Cleanup(peer.close)
	drainDocumentUpdates(f.persistence)
	return peer
}

// startFailingEdit posts an edit replacing "before" with "after" that fails after its live
// write. Cleanup opens every gate and waits for the request, so a failed assertion cannot leave
// its transaction open.
func (f *heldWriteFixture) startFailingEdit(t *testing.T) <-chan *httptest.ResponseRecorder {
	t.Helper()
	responses := make(chan *httptest.ResponseRecorder, 1)
	done := make(chan struct{})
	go func() {
		defer close(done)
		responses <- sessionRequest(t, f.handler, http.MethodPost, "/api/v1/artifacts/"+f.issue.PrimaryArtifactID+"/edits", map[string]any{
			"ops": []map[string]string{{"op": "replace", "find": "before", "with": "after"}}, "actor": sessionActor(),
		})
	}()
	t.Cleanup(func() {
		closeTestGate(f.failure.releaseBeforeApply)
		closeTestGate(f.failure.release)
		select {
		case <-done:
		case <-time.After(5 * time.Second):
			t.Error("transactional edit did not exit after releasing test gates")
		}
	})
	return responses
}

func (f *heldWriteFixture) expectFailedResponse(t *testing.T, responses <-chan *httptest.ResponseRecorder) {
	t.Helper()
	response := awaitResponse(t, responses)
	if response.Code != http.StatusInternalServerError {
		t.Fatalf("edit with forced post-apply failure: status=%d body=%s", response.Code, response.Body.String())
	}
}

func (f *heldWriteFixture) liveText(t *testing.T) string {
	t.Helper()
	text, err := f.docs.Text(context.Background(), f.issue.PrimaryArtifactID)
	if err != nil {
		t.Fatalf("read live document: %v", err)
	}
	return text
}

func (f *heldWriteFixture) assertLiveText(t *testing.T, want string) {
	t.Helper()
	if got := f.liveText(t); got != want {
		t.Fatalf("live document = %q, want %q", got, want)
	}
}

func (f *heldWriteFixture) assertLiveTextLacks(t *testing.T, text string) {
	t.Helper()
	if got := f.liveText(t); strings.Contains(got, text) {
		t.Fatalf("live document = %q, holds %q", got, text)
	}
}

func (f *heldWriteFixture) waitForLiveText(t *testing.T, want string) {
	t.Helper()
	deadline := time.Now().Add(5 * time.Second)
	for !strings.Contains(f.liveText(t), want) {
		if time.Now().After(deadline) {
			t.Fatalf("live document never showed %q", want)
		}
		time.Sleep(10 * time.Millisecond)
	}
}

// waitOutSettlement gives a settlement armed by now time to run: three settle intervals, when
// the fixture settles at all within the test.
func (f *heldWriteFixture) waitOutSettlement() {
	if f.settle < time.Second {
		time.Sleep(3 * f.settle)
	}
}

// versions lists the document's versions as "number: markdown".
func (f *heldWriteFixture) versions(t *testing.T) []string {
	t.Helper()
	rows, err := f.probe.Query(context.Background(), `
		select number, coalesce(markdown, '') from artifact_versions where artifact_id = $1 order by number
	`, f.issue.PrimaryArtifactID)
	if err != nil {
		t.Fatalf("read document versions: %v", err)
	}
	defer rows.Close()
	var versions []string
	for rows.Next() {
		var number int
		var markdown string
		if err := rows.Scan(&number, &markdown); err != nil {
			t.Fatalf("scan document version: %v", err)
		}
		versions = append(versions, strconv.Itoa(number)+": "+strconv.Quote(markdown))
	}
	if err := rows.Err(); err != nil {
		t.Fatalf("read document versions: %v", err)
	}
	return versions
}

func (f *heldWriteFixture) waitForVersionHolding(t *testing.T, text string) {
	t.Helper()
	deadline := time.Now().Add(5 * time.Second)
	for {
		for _, version := range f.versions(t) {
			if strings.Contains(version, text) {
				return
			}
		}
		if time.Now().After(deadline) {
			t.Fatalf("no version holds %q: %q", text, f.versions(t))
		}
		time.Sleep(20 * time.Millisecond)
	}
}

// assertVersionCount waits out settlement, then counts the document's versions.
func (f *heldWriteFixture) assertVersionCount(t *testing.T, want int) {
	t.Helper()
	f.waitOutSettlement()
	if versions := f.versions(t); len(versions) != want {
		t.Fatalf("document versions = %q, want %d", versions, want)
	}
}

// assertNoVersionHolds waits out settlement, then checks that no version of the document holds
// text.
func (f *heldWriteFixture) assertNoVersionHolds(t *testing.T, text string) {
	t.Helper()
	f.waitOutSettlement()
	for _, version := range f.versions(t) {
		if strings.Contains(version, text) {
			t.Fatalf("version %s holds the rolled-back %q", version, text)
		}
	}
}

// assertDurableText renders the document the store would load, update by update.
func (f *heldWriteFixture) assertDurableText(t *testing.T, want string) {
	t.Helper()
	deadline := time.Now().Add(5 * time.Second)
	for {
		got := f.durableText(t)
		if got == want {
			return
		}
		if time.Now().After(deadline) {
			t.Fatalf("durable document = %q, want %q", got, want)
		}
		time.Sleep(20 * time.Millisecond)
	}
}

func (f *heldWriteFixture) durableText(t *testing.T) string {
	t.Helper()
	rows, err := f.probe.Query(context.Background(), `
		select update from doc_updates where artifact_id = $1 order by version
	`, f.issue.PrimaryArtifactID)
	if err != nil {
		t.Fatalf("read durable document updates: %v", err)
	}
	defer rows.Close()
	document := crdt.New()
	for rows.Next() {
		var update []byte
		if err := rows.Scan(&update); err != nil {
			t.Fatalf("scan durable document update: %v", err)
		}
		if err := crdt.ApplyUpdateV1(document, update, nil); err != nil {
			t.Fatalf("apply durable document update: %v", err)
		}
	}
	if err := rows.Err(); err != nil {
		t.Fatalf("read durable document updates: %v", err)
	}
	return renderDocument(t, document)
}

// waitForLockWaiter returns once a session of this database waits on a lock: a request queued
// behind the held transaction.
func (f *heldWriteFixture) waitForLockWaiter(t *testing.T) {
	t.Helper()
	deadline := time.Now().Add(5 * time.Second)
	for {
		var waiting int
		if err := f.probe.QueryRow(context.Background(), `
			select count(*) from pg_stat_activity
			where datname = current_database() and wait_event_type = 'Lock'
		`).Scan(&waiting); err != nil {
			t.Fatalf("inspect database locks: %v", err)
		}
		if waiting > 0 {
			return
		}
		if time.Now().After(deadline) {
			t.Fatal("no request queued behind the held transaction")
		}
		time.Sleep(5 * time.Millisecond)
	}
}

func (f *heldWriteFixture) assertArtifactKeyShareLockAvailable(t *testing.T) {
	t.Helper()
	tx, err := f.probe.Begin(context.Background())
	if err != nil {
		t.Fatalf("begin artifact lock probe: %v", err)
	}
	defer tx.Rollback(context.Background())
	var lockedID string
	if err := tx.QueryRow(context.Background(), `
		select id::text from artifacts where id = $1 for key share nowait
	`, f.issue.PrimaryArtifactID).Scan(&lockedID); err != nil {
		t.Fatalf("settlement holds artifact lock while waiting for issue lock: %v", err)
	}
}

func renderDocument(t *testing.T, document *crdt.Doc) string {
	t.Helper()
	tree, err := pmdoc.Read(document.GetXmlFragment("prosemirror"))
	if err != nil {
		t.Fatalf("read document tree: %v", err)
	}
	markdown, err := pmdoc.Render(tree)
	if err != nil {
		t.Fatalf("render document tree: %v", err)
	}
	return markdown
}

// servedSockets lets a test wait until the document server is done with one of its
// connections. ygo runs a peer's disconnect before its handler returns: the room's eviction when
// the peer was its last, and the last-peer settlement.
type servedSockets struct {
	mu       sync.Mutex
	finished map[string]chan struct{}
	next     atomic.Int64
}

// socketHeader names a connection so servedSockets can say when the server let it go.
const socketHeader = "X-Test-Socket"

func (s *servedSockets) serve(next http.HandlerFunc) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		done := make(chan struct{})
		s.mu.Lock()
		s.finished[r.Header.Get(socketHeader)] = done
		s.mu.Unlock()
		defer close(done)
		next(w, r)
	}
}

func (s *servedSockets) done(id string) <-chan struct{} {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.finished[id]
}

// syncedPeer is a writable document connection that behaves as the SPA's provider does: it
// applies every update the room sends, and reconnects with the document it kept.
type syncedPeer struct {
	wsURL      string
	sockets    *servedSockets
	headers    http.Header
	artifactID string
	doc        *crdt.Doc

	mu         sync.Mutex
	connection *gws.Conn
	socketID   string
	readerDone chan struct{}
	synced     chan struct{}
}

// syncedPeerLocal tags the peer's own transactions. It must remain non-zero sized because ygo
// compares origins by interface equality.
type syncedPeerLocal struct{ _ byte }

func (p *syncedPeer) connect(t *testing.T) {
	t.Helper()
	socketID := strconv.FormatInt(p.sockets.next.Add(1), 10)
	headers := p.headers.Clone()
	headers.Set(socketHeader, socketID)
	connection, response, err := gws.DefaultDialer.Dial(p.wsURL+"?schema_version="+strconv.Itoa(pmdoc.SchemaVersion()), headers)
	if err != nil {
		t.Fatalf("connect browser peer: response=%#v err=%v", response, err)
	}
	p.mu.Lock()
	p.connection = connection
	p.socketID = socketID
	p.readerDone = make(chan struct{})
	p.synced = make(chan struct{}, 16)
	readerDone, synced := p.readerDone, p.synced
	p.mu.Unlock()
	go p.read(connection, readerDone, synced)
	p.barrier(t)
}

// read applies every sync frame the room sends. It answers the room's sync step 1 with the
// updates the room lacks, and signals synced for each sync step 2 it applies.
func (p *syncedPeer) read(connection *gws.Conn, done chan<- struct{}, synced chan<- struct{}) {
	defer close(done)
	for {
		_, message, err := connection.ReadMessage()
		if err != nil {
			return
		}
		decoder := encoding.NewDecoder(message)
		if _, err := decoder.ReadVarString(); err != nil {
			return
		}
		if kind, err := decoder.ReadVarUint(); err != nil || kind != 0 {
			continue
		}
		payload := decoder.RemainingBytes()
		kind, _, err := ygsync.ReadSyncMessage(payload)
		if err != nil {
			return
		}
		reply, err := ygsync.ApplySyncMessage(p.doc, payload, nil)
		if err != nil {
			return
		}
		if kind == ygsync.MsgSyncStep1 && reply != nil {
			if p.write(connection, reply) != nil {
				return
			}
		}
		if kind == ygsync.MsgSyncStep2 {
			select {
			case synced <- struct{}{}:
			default:
			}
		}
	}
}

func (p *syncedPeer) write(connection *gws.Conn, syncMessage []byte) error {
	frame := encoding.EncodeBytes(func(encoder *encoding.Encoder) {
		encoder.WriteVarString(p.artifactID)
		encoder.WriteVarUint(0) // Hocuspocus sync message.
	})
	p.mu.Lock()
	defer p.mu.Unlock()
	return connection.WriteMessage(gws.BinaryMessage, append(frame, syncMessage...))
}

// barrier returns once the room answered a sync step 1 sent now. The room writes to one
// connection in order, so every update it sent before the answer has been applied.
func (p *syncedPeer) barrier(t *testing.T) {
	t.Helper()
	p.mu.Lock()
	connection, synced, done := p.connection, p.synced, p.readerDone
	p.mu.Unlock()
	for len(synced) > 0 {
		<-synced
	}
	if err := p.write(connection, ygsync.EncodeSyncStep1(p.doc)); err != nil {
		t.Fatalf("send browser peer sync step 1: %v", err)
	}
	select {
	case <-synced:
	case <-done:
		t.Fatal("browser peer connection closed before the room answered its sync")
	case <-time.After(5 * time.Second):
		t.Fatal("room did not answer the browser peer's sync")
	}
}

func (p *syncedPeer) reconnect(t *testing.T) {
	t.Helper()
	p.mu.Lock()
	socketID := p.socketID
	p.mu.Unlock()
	p.close()
	// A browser's reconnect reaches a server that has let its old connection go, and with it the
	// room that connection emptied. A dial before then races that teardown, and the server can
	// drop the new connection in the window.
	select {
	case <-p.sockets.done(socketID):
	case <-time.After(10 * time.Second):
		t.Fatal("the document server did not let go of the browser's closed connection")
	}
	p.connect(t)
}

func (p *syncedPeer) close() {
	p.mu.Lock()
	connection, done := p.connection, p.readerDone
	p.mu.Unlock()
	if connection == nil {
		return
	}
	_ = connection.Close()
	<-done
}

// edit runs change on the peer's document tree in one local transaction and sends the update it
// produced, as a keystroke does.
func (p *syncedPeer) edit(t *testing.T, change func(*pmdoc.Node) error) {
	t.Helper()
	fragment := p.doc.GetXmlFragment("prosemirror")
	origin := &syncedPeerLocal{}
	var update []byte
	unsubscribe := p.doc.OnUpdate(func(encoded []byte, updateOrigin any) {
		if updateOrigin == origin {
			update = append([]byte(nil), encoded...)
		}
	})
	var editErr error
	p.doc.Transact(func(txn *crdt.Transaction) {
		tree, err := pmdoc.ReadInTransaction(txn, fragment)
		if err != nil {
			editErr = err
			return
		}
		if editErr = change(tree); editErr != nil {
			return
		}
		editErr = pmdoc.Update(txn, fragment, tree)
	}, origin)
	unsubscribe()
	if editErr != nil || update == nil {
		t.Fatalf("browser peer edit: update=%d bytes err=%v", len(update), editErr)
	}
	p.mu.Lock()
	connection := p.connection
	p.mu.Unlock()
	if err := p.write(connection, ygsync.EncodeUpdate(update)); err != nil {
		t.Fatalf("send browser peer update: %v", err)
	}
}

func (p *syncedPeer) appendParagraph(t *testing.T, text string) {
	t.Helper()
	typed, err := pmdoc.Parse(text)
	if err != nil {
		t.Fatalf("parse browser paragraph: %v", err)
	}
	p.edit(t, func(tree *pmdoc.Node) error {
		tree.Children = append(tree.Children, typed.Children...)
		return nil
	})
}

// appendToFirstParagraph types text at the end of the first paragraph's last text run.
func (p *syncedPeer) appendToFirstParagraph(t *testing.T, text string) {
	t.Helper()
	p.edit(t, func(tree *pmdoc.Node) error {
		if len(tree.Children) == 0 || len(tree.Children[0].Children) == 0 {
			return errors.New("browser peer document has no text to type after")
		}
		runs := tree.Children[0].Children
		runs[len(runs)-1].Text += text
		return nil
	})
}

// assertProjection checks the peer's copy of markID's margin projection against want.
func (p *syncedPeer) assertProjection(t *testing.T, markID string, want map[string]any) {
	t.Helper()
	got, _ := p.doc.GetMap("marks").Get(markID)
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("browser peer projection of %s = %#v, want %#v", markID, got, want)
	}
}

func (p *syncedPeer) assertTextLacks(t *testing.T, text string) {
	t.Helper()
	if got := renderDocument(t, p.doc); strings.Contains(got, text) {
		t.Fatalf("browser peer document = %q, holds %q", got, text)
	}
}

// recordingVersionedStore signals every update it appends, classified or not.
type recordingVersionedStore struct {
	*docs.PgVersioned
	updates chan struct{}
}

func (s *recordingVersionedStore) AppendUpdateWithClass(ctx context.Context, room string, update []byte, contentChanged bool) (persistence.Version, error) {
	version, err := s.PgVersioned.AppendUpdateWithClass(ctx, room, update, contentChanged)
	if err == nil {
		s.signal()
	}
	return version, err
}

func (s *recordingVersionedStore) AppendUpdate(ctx context.Context, room string, update []byte) (persistence.Version, error) {
	version, err := s.PgVersioned.AppendUpdate(ctx, room, update)
	if err == nil {
		s.signal()
	}
	return version, err
}

func (s *recordingVersionedStore) AppendUpdateTx(ctx context.Context, tx pgx.Tx, room string, update []byte, contentChanged bool) (persistence.Version, error) {
	version, err := s.PgVersioned.AppendUpdateTx(ctx, tx, room, update, contentChanged)
	if err == nil {
		s.signal()
	}
	return version, err
}

// signal records that an update reached persistence without ever blocking the writer.
func (s *recordingVersionedStore) signal() {
	select {
	case s.updates <- struct{}{}:
	default:
	}
}

func drainDocumentUpdates(store *recordingVersionedStore) {
	for {
		select {
		case <-store.updates:
		default:
			return
		}
	}
}

func waitForDocumentUpdate(t *testing.T, store *recordingVersionedStore) {
	t.Helper()
	select {
	case <-store.updates:
	case <-time.After(time.Second):
		t.Fatal("document mutation did not reach persistence")
	}
}

// postApplyFailureDocs fails a document write after the real write returned, holding it first
// at beforeApply (when set) and then at release.
type postApplyFailureDocs struct {
	docs.API
	beforeApply        chan struct{}
	releaseBeforeApply chan struct{}
	applied            chan struct{}
	release            chan struct{}
	failProjectMark    bool
	projectMarkPasses  int
}

func (d *postApplyFailureDocs) ApplyOps(ctx context.Context, artifactID string, ops []model.EditOp, actor model.Actor, precondition *model.EditPrecondition) (int, error) {
	d.waitBeforeApply()
	applied, err := d.API.ApplyOps(ctx, artifactID, ops, actor, precondition)
	if err != nil {
		return 0, err
	}
	close(d.applied)
	<-d.release
	return applied, errors.New("forced post-apply failure")
}

func (d *postApplyFailureDocs) AcceptSuggestion(ctx context.Context, artifactID, markID, replacement string, actor model.Actor) error {
	d.waitBeforeApply()
	if err := d.API.AcceptSuggestion(ctx, artifactID, markID, replacement, actor); err != nil {
		return err
	}
	close(d.applied)
	<-d.release
	return errors.New("forced post-apply failure")
}

func (d *postApplyFailureDocs) ProjectMark(ctx context.Context, artifactID, markID string, record docs.MarkRecord, actor model.Actor) error {
	if !d.failProjectMark {
		return d.API.ProjectMark(ctx, artifactID, markID, record, actor)
	}
	if d.projectMarkPasses > 0 {
		d.projectMarkPasses--
		return d.API.ProjectMark(ctx, artifactID, markID, record, actor)
	}
	d.waitBeforeApply()
	if err := d.API.ProjectMark(ctx, artifactID, markID, record, actor); err != nil {
		return err
	}
	close(d.applied)
	<-d.release
	return errors.New("forced post-projection failure")
}

func (d *postApplyFailureDocs) ReplaceText(ctx context.Context, artifactID, markdown string, actor model.Actor) (string, error) {
	d.waitBeforeApply()
	_, err := d.API.ReplaceText(ctx, artifactID, markdown, actor)
	if err != nil {
		return "", err
	}
	close(d.applied)
	<-d.release
	return "", errors.New("forced post-apply failure")
}

func (d *postApplyFailureDocs) waitBeforeApply() {
	if d.beforeApply == nil {
		return
	}
	close(d.beforeApply)
	<-d.releaseBeforeApply
}

func closeTestGate(gate chan struct{}) {
	if gate == nil {
		return
	}
	select {
	case <-gate:
		return
	default:
		close(gate)
	}
}

func waitForBeforeApply(t *testing.T, docs *postApplyFailureDocs) {
	t.Helper()
	select {
	case <-docs.beforeApply:
	case <-time.After(time.Second):
		t.Fatal("document mutation did not reach pre-apply gate")
	}
}

func waitForPostApply(t *testing.T, docs *postApplyFailureDocs) {
	t.Helper()
	select {
	case <-docs.applied:
	case <-time.After(time.Second):
		t.Fatal("document mutation did not complete")
	}
}
