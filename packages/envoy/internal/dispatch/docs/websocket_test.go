package docs

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strconv"
	"strings"
	"testing"
	"time"

	gws "github.com/gorilla/websocket"
	"github.com/reearth/ygo/crdt"
	"github.com/reearth/ygo/encoding"
	"github.com/sjawhar/envoy/internal/contracts"
	"github.com/sjawhar/envoy/internal/dispatch/auth"
	"github.com/sjawhar/envoy/internal/dispatch/docs/docstest"
	"github.com/sjawhar/envoy/internal/dispatch/events"
	"github.com/sjawhar/envoy/internal/dispatch/model"
	"github.com/sjawhar/envoy/internal/dispatch/pmdoc"
	"github.com/sjawhar/envoy/internal/dispatch/store/storetest"
)

// connectPeer connects login's browser to artifactID's room through the document server at
// serverURL, with the admission query a current dashboard presents, so the room takes its edits.
func connectPeer(t *testing.T, serverURL, artifactID, login string) *docstest.Peer {
	t.Helper()
	url := "ws" + strings.TrimPrefix(serverURL, "http") + "/ws/doc/" + artifactID +
		"?" + docstest.AdmissionQuery(pmdoc.SchemaVersion())
	return docstest.Dial(t, url, http.Header{"X-Dispatch-User": []string{login}}, artifactID, crdt.New())
}

func TestIssueCloseClosesOpenDocumentConnection(t *testing.T) {
	service, artifactID := newTestService(t)
	seedServiceText(t, service, artifactID, "before")
	httpServer := httptest.NewServer(http.HandlerFunc(service.ServeHTTP))
	t.Cleanup(httpServer.Close)
	headers := http.Header{"X-Dispatch-User": []string{"alice"}}
	wsURL := "ws" + strings.TrimPrefix(httpServer.URL, "http") + "/ws/doc/" + artifactID
	connection, response, err := gws.DefaultDialer.Dial(wsURL, headers)
	if err != nil {
		t.Fatalf("connect live document: response=%#v err=%v", response, err)
	}
	t.Cleanup(func() { _ = connection.Close() })

	if _, err := service.store.Pool.Exec(context.Background(), `update issues set closed_at = now() where key = 'DOC-1'`); err != nil {
		t.Fatalf("close document issue: %v", err)
	}
	service.SetIssueClosed(context.Background(), "DOC-1", true)
	waitForNoLiveDocument(t, service, artifactID)
	connection.SetReadDeadline(time.Now().Add(time.Second))
	for {
		if _, _, err := connection.ReadMessage(); err != nil {
			break
		}
	}
	if err := service.srv.Apply(context.Background(), artifactID, func(_ *crdt.Doc, _ func(func(*crdt.Transaction))) {}); !errors.Is(err, ErrIssueClosed) {
		t.Fatalf("server write after issue close = %v, want ErrIssueClosed", err)
	}
}

func TestClosedColdRoomAuthorizesReadOnly(t *testing.T) {
	service, artifactID := newTestService(t)
	if _, err := service.store.Pool.Exec(context.Background(), `update issues set closed_at = now() where key = 'DOC-1'`); err != nil {
		t.Fatalf("close document issue: %v", err)
	}
	request := httptest.NewRequest(http.MethodGet, "/ws/doc/"+artifactID, nil)
	request.Header.Set("X-Dispatch-User", "alice")
	request.SetPathValue("room", artifactID)
	request = request.WithContext(context.WithValue(request.Context(), connectionContextKey{}, &connectionState{}))
	config, ok := service.authorize(request)
	if !ok || !config.ReadOnly {
		t.Fatalf("cold closed room authorization = %#v, %t; want read-only acceptance", config, ok)
	}
}

// admissionRow is one dashboard bundle's admission: the query its provider sends on the upgrade
// and the Hocuspocus token it sends once connected, with whether each alone is admitted read-write.
type admissionRow struct {
	name                         string
	query, token                 string
	queryWritable, tokenWritable bool
}

// admissionRows covers every shape a bundle presents. An editor built before the editor schema
// existed sends the schema version it cached or fetched and a bare token; one built with it sends
// both versions, in the query and as `<schema>:<editor>`. Only the current pair is writable, and a
// token other than exactly that is read-only even beside the current query, since it overrides it.
func admissionRows() []admissionRow {
	current := strconv.Itoa(pmdoc.SchemaVersion())
	previous := strconv.Itoa(pmdoc.SchemaVersion() - 1)
	editor := strconv.Itoa(contracts.DocumentEditorSchemaVersion)
	staleEditor := strconv.Itoa(contracts.DocumentEditorSchemaVersion - 1)
	currentQuery := docstest.AdmissionQuery(pmdoc.SchemaVersion())
	return []admissionRow{
		{name: "an old bundle with the schema version it cached", query: "schema_version=" + previous, token: previous},
		{name: "an old bundle with the schema version it fetched", query: "schema_version=" + current, token: current},
		{name: "a bundle with another editor schema", query: "schema_version=" + current + "&editor_schema=" + staleEditor, token: current + ":" + staleEditor},
		{name: "a new bundle with a stale schema version", query: "schema_version=" + previous + "&editor_schema=" + editor, token: previous + ":" + editor},
		{name: "no versions", query: "", token: ""},
		{name: "only the editor schema", query: "editor_schema=" + editor, token: ":" + editor},
		{name: "a token without the editor schema", query: currentQuery, queryWritable: true, token: current + ":"},
		{name: "a token with a third part", query: currentQuery, queryWritable: true, token: current + ":" + editor + ":" + editor},
		{name: "a token with a leading space", query: currentQuery, queryWritable: true, token: " " + current + ":" + editor},
		{name: "a current query and an old token", query: currentQuery, queryWritable: true, token: current},
		{name: "the current bundle", query: currentQuery, queryWritable: true, token: docstest.AdmissionToken(pmdoc.SchemaVersion()), tokenWritable: true},
	}
}

// authorizeUpgrade runs the upgrade's admission for a request to artifactID's room with query.
func authorizeUpgrade(t *testing.T, service *Service, artifactID, query string) bool {
	t.Helper()
	request := httptest.NewRequest(http.MethodGet, "/ws/doc/"+artifactID+"?"+query, nil)
	request.Header.Set("X-Dispatch-User", "alice")
	request.SetPathValue("room", artifactID)
	request = request.WithContext(context.WithValue(request.Context(), connectionContextKey{}, &connectionState{}))
	config, ok := service.authorize(request)
	if !ok {
		t.Fatalf("query %q was refused, want an admission", query)
	}
	return config.ReadOnly
}

func TestSchemaVersionAdmissionAtTheUpgradeReadsBothVersions(t *testing.T) {
	service, artifactID := newTestService(t)
	for _, row := range admissionRows() {
		t.Run(row.name, func(t *testing.T) {
			if readOnly := authorizeUpgrade(t, service, artifactID, row.query); readOnly == row.queryWritable {
				t.Fatalf("query %q admitted read-only %t, want %t", row.query, readOnly, !row.queryWritable)
			}
		})
	}
}

// The scope the Hocuspocus authentication reply carries is the provider's signal, and the token's
// answer overrides the upgrade's, so a token other than the current one is read-only after any query.
func TestHocuspocusAuthenticationCommunicatesSchemaReadOnly(t *testing.T) {
	service, artifactID := newTestService(t)
	seedServiceText(t, service, artifactID, "before")
	httpServer := httptest.NewServer(http.HandlerFunc(service.ServeHTTP))
	t.Cleanup(httpServer.Close)
	for _, row := range admissionRows() {
		t.Run(row.name, func(t *testing.T) {
			wsURL := "ws" + strings.TrimPrefix(httpServer.URL, "http") + "/ws/doc/" + artifactID + "?" + row.query
			connection, response, err := gws.DefaultDialer.Dial(wsURL, http.Header{"X-Dispatch-User": []string{"alice"}})
			if err != nil {
				t.Fatalf("connect live document: response=%#v err=%v", response, err)
			}
			defer func() { _ = connection.Close() }()
			want := "readonly"
			if row.tokenWritable {
				want = "read-write"
			}
			if scope := authenticationScope(t, connection, artifactID, row.token); scope != want {
				t.Fatalf("token %q after query %q authenticated %q, want %q", row.token, row.query, scope, want)
			}
		})
	}
}

// authenticationScope sends token as the room's Hocuspocus token over connection and returns the
// scope of the authenticated reply.
func authenticationScope(t *testing.T, connection *gws.Conn, artifactID, token string) string {
	t.Helper()
	if err := connection.WriteMessage(gws.BinaryMessage, docstest.AuthenticationMessage(artifactID, token)); err != nil {
		t.Fatalf("send schema authentication: %v", err)
	}
	connection.SetReadDeadline(time.Now().Add(time.Second))
	for {
		_, message, err := connection.ReadMessage()
		if err != nil {
			t.Fatalf("read schema authentication reply: %v", err)
		}
		decoder := encoding.NewDecoder(message)
		if _, err := decoder.ReadVarString(); err != nil {
			t.Fatalf("read Hocuspocus document name: %v", err)
		}
		kind, err := decoder.ReadVarUint()
		if err != nil {
			t.Fatalf("read Hocuspocus message kind: %v", err)
		}
		if kind != 2 {
			continue
		}
		subtype, err := decoder.ReadVarUint()
		if err != nil {
			t.Fatalf("read Hocuspocus authentication subtype: %v", err)
		}
		scope, err := decoder.ReadVarString()
		if err != nil {
			t.Fatalf("read Hocuspocus authentication scope: %v", err)
		}
		if subtype != 2 {
			t.Fatalf("schema authentication = subtype %d scope %q, want authenticated", subtype, scope)
		}
		return scope
	}
}

// A closed issue's document is read-only to every bundle, the current one included, at the upgrade
// and in the token's answer.
func TestSchemaVersionAdmissionOfAClosedIssueIsReadOnly(t *testing.T) {
	service, artifactID := newTestService(t)
	if _, err := service.store.Pool.Exec(context.Background(), `update issues set closed_at = now() where key = 'DOC-1'`); err != nil {
		t.Fatalf("close document issue: %v", err)
	}
	if !authorizeUpgrade(t, service, artifactID, docstest.AdmissionQuery(pmdoc.SchemaVersion())) {
		t.Fatal("the current query on a closed issue was admitted read-write")
	}
	config, err := service.authorizeSchemaVersion(artifactID, docstest.AdmissionToken(pmdoc.SchemaVersion()))
	if err != nil {
		t.Fatalf("authorize the current token: %v", err)
	}
	if !config.ReadOnly {
		t.Fatalf("the current token on a closed issue = %#v, want read-only", config)
	}
}

func TestLoadFailureMakesDocumentServiceUnavailable(t *testing.T) {
	database := storetest.Open(t)
	artifactID := createDocument(t, database, "before")
	service := New(Deps{
		Store:       database,
		Persistence: failingVersionedStore{VersionedStore: NewPgVersioned(database), loadErr: errors.New("load failed")},
		Events:      events.NewBroker(),
		Identity:    headerIdentity(database),
	})
	t.Cleanup(func() { _ = service.Shutdown(context.Background()) })

	httpServer := httptest.NewServer(http.HandlerFunc(service.ServeHTTP))
	t.Cleanup(httpServer.Close)
	headers := http.Header{"X-Dispatch-User": []string{"alice"}}
	wsURL := "ws" + strings.TrimPrefix(httpServer.URL, "http") + "/ws/doc/" + artifactID
	connection, response, err := gws.DefaultDialer.Dial(wsURL, headers)
	if connection != nil {
		_ = connection.Close()
	}
	if err == nil || response == nil || response.StatusCode != http.StatusServiceUnavailable {
		t.Fatalf("failed-room connection: response=%#v err=%v, want HTTP 503", response, err)
	}
	if _, err := service.Text(context.Background(), artifactID); !errors.Is(err, ErrServiceUnavailable) {
		t.Fatalf("load failure = %v, want ErrServiceUnavailable", err)
	}
}

// A history that does not decode refuses the socket before any upgrade, and a read of it is the
// state a rebuild repairs (ErrDocumentUnloadable) rather than any failed room's ErrServiceUnavailable.
func TestCorruptLoadRefusesTheSocketAndReadsAsUnloadable(t *testing.T) {
	database := storetest.Open(t)
	artifactID := createDocument(t, database, "before")
	service := New(Deps{
		Store:       database,
		Persistence: failingVersionedStore{VersionedStore: NewPgVersioned(database), loadUpdate: []byte{0xff}},
		Events:      events.NewBroker(),
		Identity:    headerIdentity(database),
	})
	t.Cleanup(func() { _ = service.Shutdown(context.Background()) })

	httpServer := httptest.NewServer(http.HandlerFunc(service.ServeHTTP))
	t.Cleanup(httpServer.Close)
	headers := http.Header{"X-Dispatch-User": []string{"alice"}}
	wsURL := "ws" + strings.TrimPrefix(httpServer.URL, "http") + "/ws/doc/" + artifactID
	connection, response, err := gws.DefaultDialer.Dial(wsURL, headers)
	if connection != nil {
		_ = connection.Close()
	}
	if err == nil || response == nil || response.StatusCode != http.StatusServiceUnavailable {
		t.Fatalf("corrupt-room connection: response=%#v err=%v, want HTTP 503", response, err)
	}
	if _, err := service.Text(context.Background(), artifactID); !errors.Is(err, ErrDocumentUnloadable) || errors.Is(err, ErrServiceUnavailable) {
		t.Fatalf("corrupt load = %v, want ErrDocumentUnloadable", err)
	}
}

// A cold socket's admission check hands the room's load the history it decoded, so the history
// is read once - but only while it is still the stored one. An update appended in between raises
// the head, and the load reads the store again rather than open the room without that update.
func TestASocketsPreloadIsServedOnlyWhileTheHistoryHasNotMoved(t *testing.T) {
	service, artifactID := newTestService(t)
	seedServiceText(t, service, artifactID, "before")
	adapter := &servicePersistenceAdapter{store: service.persistence, service: service}
	preload := func(t *testing.T) []byte {
		t.Helper()
		_, loaded, err := service.loadTree(context.Background(), artifactID)
		if err != nil || loaded == nil || len(loaded.Update) == 0 {
			t.Fatalf("load cold document: loaded=%v err=%v, want its durable history", loaded, err)
		}
		service.preloads.Store(artifactID, &preloadedDocument{loaded: *loaded})
		return loaded.Update
	}

	held := preload(t)
	served, err := adapter.LoadDoc(artifactID)
	if err != nil || &served[0] != &held[0] {
		t.Fatalf("load with a current preload served %d bytes (%v), want the preloaded history itself", len(served), err)
	}
	if _, kept := service.preloads.Load(artifactID); kept {
		t.Fatal("a served preload stayed behind for a later load")
	}

	preload(t)
	moved, err := encodeDocumentTree(&pmdoc.Node{Type: "doc", Children: []*pmdoc.Node{{
		Type: "paragraph", Children: []*pmdoc.Node{{Type: "text", Text: "appended"}},
	}}})
	if err != nil {
		t.Fatalf("encode appended update: %v", err)
	}
	if _, err := service.persistence.AppendUpdate(context.Background(), artifactID, moved); err != nil {
		t.Fatalf("append update after the check: %v", err)
	}
	served, err = adapter.LoadDoc(artifactID)
	if err != nil {
		t.Fatalf("load after the history moved: %v", err)
	}
	doc := crdt.New()
	if err := crdt.ApplyUpdateV1(doc, served, nil); err != nil {
		t.Fatalf("decode the loaded history: %v", err)
	}
	if markdown, err := renderDocument(doc); err != nil || !strings.Contains(markdown, "appended") {
		t.Fatalf("load after the history moved served %q (%v), want the appended update", markdown, err)
	}
}

// A browser editor normalizes a tree it cannot represent and writes the result back, so the
// document websocket admits no connection to a room outside the Proof schema: resident or only
// stored, refused by the tree reader or only by the renderer. It completes the upgrade only to
// close with documentSchemaCloseCode before any sync, a code a browser can read, and neither loads
// nor fails the room.
func TestDocumentSocketRefusesARoomOutsideTheSchema(t *testing.T) {
	for _, test := range []struct {
		name    string
		corrupt func(*testing.T, *Service, string)
	}{
		{"a resident room the reader refuses", writeSchemaInvalidElement},
		{"a stored history only the renderer refuses", func(t *testing.T, service *Service, artifactID string) {
			if err := AppendRenderOnlySchemaViolationForTest(context.Background(), service.store, artifactID); err != nil {
				t.Fatal(err)
			}
		}},
	} {
		t.Run(test.name, func(t *testing.T) {
			service, artifactID := newTestService(t)
			seedServiceText(t, service, artifactID, "before")
			test.corrupt(t, service, artifactID)
			httpServer := httptest.NewServer(http.HandlerFunc(service.ServeHTTP))
			t.Cleanup(httpServer.Close)
			headers := http.Header{"X-Dispatch-User": []string{"alice"}}
			wsURL := "ws" + strings.TrimPrefix(httpServer.URL, "http") + "/ws/doc/" + artifactID
			connection, response, err := gws.DefaultDialer.Dial(wsURL, headers)
			if err != nil {
				t.Fatalf("connect outside-schema document: response=%#v err=%v, want an upgrade the server closes", response, err)
			}
			t.Cleanup(func() { _ = connection.Close() })
			connection.SetReadDeadline(time.Now().Add(time.Second))
			_, message, err := connection.ReadMessage()
			var closed *gws.CloseError
			if !errors.As(err, &closed) || closed.Code != documentSchemaCloseCode {
				t.Fatalf("first frame = %q (%v), want close %d before any sync", message, err, documentSchemaCloseCode)
			}
			if _, err := service.Text(context.Background(), artifactID); !errors.Is(err, ErrDocOutsideSchema) {
				t.Fatalf("read after refused socket: %v, want ErrDocOutsideSchema", err)
			}
		})
	}
}

func TestShutdownClosesDocumentPeers(t *testing.T) {
	database := storetest.Open(t)
	artifactID := createDocument(t, database, "before")
	service := New(Deps{
		Store: database, Events: events.NewBroker(),
		Identity: headerIdentity(database),
	})
	t.Cleanup(func() { _ = service.Shutdown(context.Background()) })
	seedServiceText(t, service, artifactID, "before")
	httpServer := httptest.NewServer(http.HandlerFunc(service.ServeHTTP))
	t.Cleanup(httpServer.Close)
	headers := http.Header{"X-Dispatch-User": []string{"alice"}}
	wsURL := "ws" + strings.TrimPrefix(httpServer.URL, "http") + "/ws/doc/" + artifactID
	connection, response, err := gws.DefaultDialer.Dial(wsURL, headers)
	if err != nil {
		t.Fatalf("connect live document: response=%#v err=%v", response, err)
	}
	defer connection.Close()
	waitFor(t, time.Second, "document peer connection", func() bool {
		state := service.room(artifactID)
		state.mu.Lock()
		defer state.mu.Unlock()
		return len(state.connected) == 1
	})
	shutdown := make(chan error, 1)
	go func() { shutdown <- service.Shutdown(context.Background()) }()
	connection.SetReadDeadline(time.Now().Add(time.Second))
	for {
		if _, _, err := connection.ReadMessage(); err != nil {
			break
		}
	}
	if err := <-shutdown; err != nil {
		t.Fatalf("shutdown with document peer: %v", err)
	}
	if got, err := service.Text(context.Background(), artifactID); err != nil || got != "before\n" {
		t.Fatalf("text after closing document peer = %q (%v), want before", got, err)
	}
}

// Shutdown returns at its caller's deadline while an editor is connected and a durable append waits
// on a lock, and the append still reaches the store once the lock is released.
func TestShutdownReturnsAtItsDeadlineWithAnEditorConnectedAndAnAppendLocked(t *testing.T) {
	database := storetest.Open(t)
	artifactID := createDocument(t, database, "before")
	service := New(Deps{
		Store: database, Events: events.NewBroker(),
		Identity: headerIdentity(database),
		Settle:   time.Hour,
	})
	t.Cleanup(func() { _ = service.Shutdown(context.Background()) })
	seedServiceText(t, service, artifactID, "before")
	httpServer := httptest.NewServer(http.HandlerFunc(service.ServeHTTP))
	t.Cleanup(httpServer.Close)
	headers := http.Header{"X-Dispatch-User": []string{"alice"}}
	wsURL := "ws" + strings.TrimPrefix(httpServer.URL, "http") + "/ws/doc/" + artifactID
	connection, response, err := gws.DefaultDialer.Dial(wsURL, headers)
	if err != nil {
		t.Fatalf("connect live document: response=%#v err=%v", response, err)
	}
	defer connection.Close()

	locker, err := database.Pool.Begin(context.Background())
	if err != nil {
		t.Fatalf("begin append lock transaction: %v", err)
	}
	defer locker.Rollback(context.Background())
	if _, err := locker.Exec(context.Background(), `select pg_advisory_xact_lock(hashtext($1))`, artifactID); err != nil {
		t.Fatalf("lock document append: %v", err)
	}
	editLiveTree(t, service, artifactID, replaceRun("before", "after"))
	if !service.hasDurableAppend(artifactID) {
		t.Fatal("durable append finished while its advisory lock was held")
	}
	ctx, cancel := context.WithTimeout(context.Background(), 400*time.Millisecond)
	defer cancel()
	if err := service.Shutdown(ctx); !errors.Is(err, context.DeadlineExceeded) {
		t.Fatalf("shutdown with an editor connected and an append locked = %v, want deadline exceeded", err)
	}
	if err := locker.Commit(context.Background()); err != nil {
		t.Fatalf("release append lock: %v", err)
	}
	// Not waitForPersistedUpdates: Shutdown's CloseRoom evicts this room once the append lands
	// (room.connected), and ygo compacts an evicted room's history to one row (compactKeep),
	// so a row-count check here races the compaction that normally follows eviction. The
	// durable-append signal alone proves the locked write reached the store; the content check
	// below proves what it reached the store as.
	waitCtx, waitCancel := context.WithTimeout(context.Background(), time.Minute)
	defer waitCancel()
	if err := service.waitForDurableAppends(waitCtx, artifactID); err != nil {
		t.Fatalf("wait for the locked append to be stored: %v", err)
	}
	reloaded := New(Deps{Store: database, Events: events.NewBroker(), Settle: time.Hour})
	defer reloaded.Shutdown(context.Background())
	if got, err := reloaded.Text(context.Background(), artifactID); err != nil || got != "after\n" {
		t.Fatalf("text after the shutdown the locked append outlasted = %q (%v), want after", got, err)
	}
}

func TestAppendFailureClosesDocumentConnectionAndReloadsRoom(t *testing.T) {
	database := storetest.Open(t)
	artifactID := createDocument(t, database, "before")
	service := New(Deps{
		Store:       database,
		Persistence: failingVersionedStore{VersionedStore: NewPgVersioned(database), appendErr: errors.New("append failed")},
		Events:      events.NewBroker(),
		Identity:    headerIdentity(database),
		Settle:      time.Hour,
	})
	t.Cleanup(func() { _ = service.Shutdown(context.Background()) })
	seedServiceText(t, service, artifactID, "before")
	httpServer := httptest.NewServer(http.HandlerFunc(service.ServeHTTP))
	t.Cleanup(httpServer.Close)
	headers := http.Header{"X-Dispatch-User": []string{"alice"}}
	wsURL := "ws" + strings.TrimPrefix(httpServer.URL, "http") + "/ws/doc/" + artifactID
	connection, response, err := gws.DefaultDialer.Dial(wsURL, headers)
	if err != nil {
		t.Fatalf("connect live document: response=%#v err=%v", response, err)
	}
	t.Cleanup(func() { _ = connection.Close() })

	editLiveTree(t, service, artifactID, replaceRun("before", "after"))
	connection.SetReadDeadline(time.Now().Add(time.Second))
	for {
		if _, _, err := connection.ReadMessage(); err != nil {
			break
		}
	}
	_ = connection.Close()
	waitForNoLiveDocument(t, service, artifactID)
	if got, err := service.Text(context.Background(), artifactID); err != nil || got != "before\n" {
		t.Fatalf("reloaded text after append failure = %q (%v), want persisted text before", got, err)
	}
}

func TestFailedRoomEvictsAndReloadsOnNextAccess(t *testing.T) {
	database := storetest.Open(t)
	artifactID := createDocument(t, database, "before")
	service := New(Deps{
		Store:       database,
		Persistence: &failingOnceVersionedStore{VersionedStore: NewPgVersioned(database)},
		Events:      events.NewBroker(),
		Identity:    headerIdentity(database),
	})
	t.Cleanup(func() { _ = service.Shutdown(context.Background()) })
	seedServiceText(t, service, artifactID, "before")
	httpServer := httptest.NewServer(http.HandlerFunc(service.ServeHTTP))
	t.Cleanup(httpServer.Close)
	headers := http.Header{"X-Dispatch-User": []string{"alice"}}
	wsURL := "ws" + strings.TrimPrefix(httpServer.URL, "http") + "/ws/doc/" + artifactID

	connection, response, err := gws.DefaultDialer.Dial(wsURL, headers)
	if connection != nil {
		_ = connection.Close()
	}
	if err == nil || response == nil || response.StatusCode != http.StatusServiceUnavailable {
		t.Fatalf("first failed-room access: response=%#v err=%v, want HTTP 503", response, err)
	}
	connection, response, err = gws.DefaultDialer.Dial(wsURL, headers)
	if err != nil {
		t.Fatalf("reloaded room access: response=%#v err=%v", response, err)
	}
	_ = connection.Close()
}

// TestDocumentBearerCannotForgeVerifiedServiceSubject: Actor.Service means "the
// server verified this Kubernetes subject from a projected token", and both
// renderers show it as that. The document websocket takes its actor from a
// caller-supplied header, so a shared-token holder must not be able to put one
// there — nor an owner, which seeds the default assignee.
func TestDocumentBearerCannotForgeVerifiedServiceSubject(t *testing.T) {
	database := storetest.Open(t)
	artifactID := createDocument(t, database, "# First")
	service := New(Deps{
		Store:       database,
		Events:      events.NewBroker(),
		Identity:    headerIdentity(database),
		AgentTokens: sharedAgentTokens(t, "doc-agent-token"),
		ServerURL:   "https://dispatch.example",
		Settle:      20 * time.Millisecond,
	})
	t.Cleanup(func() {
		if err := service.Shutdown(context.Background()); err != nil {
			t.Errorf("shutdown document service: %v", err)
		}
	})
	service.settle = time.Hour
	seedServiceText(t, service, artifactID, "before")

	httpServer := httptest.NewServer(http.HandlerFunc(service.ServeHTTP))
	t.Cleanup(httpServer.Close)
	headers := http.Header{
		"Authorization": []string{"Bearer doc-agent-token"},
		"X-Dispatch-Actor": []string{`{"kind":"session","id":"session-0123456789abcdef",` +
			`"origin":{"host":"forge"},` +
			`"service":"system:serviceaccount:legion:legion-worker","owner":"mallory"}`},
	}
	wsURL := "ws" + strings.TrimPrefix(httpServer.URL, "http") + "/ws/doc/" + artifactID
	connection, response, err := gws.DefaultDialer.Dial(wsURL, headers)
	if err != nil {
		t.Fatalf("connect as document bearer: response=%#v err=%v", response, err)
	}
	t.Cleanup(func() { _ = connection.Close() })
	// A connection is credited with the browser edits made while it is open, and a raw room edit is
	// one, so this change puts the connection's actor on the next version, as the server persists it.
	editLiveTree(t, service, artifactID, replaceRun("before", "after"))

	named, err := namedVersion(t, service, artifactID,
		"checkpoint", model.Actor{Kind: "user", ID: "alice"})
	if err != nil {
		t.Fatalf("name document version: %v", err)
	}
	version := named.Version
	var session *model.Actor
	for index, author := range version.Authors {
		if author.Kind == "session" {
			session = &version.Authors[index]
		}
	}
	if session == nil {
		t.Fatalf("version authors = %#v, want the bearer connection's session actor among them", version.Authors)
	}
	if session.Service != nil {
		t.Fatalf("the header's service reached the persisted author: %q", *session.Service)
	}
	if session.Owner != nil {
		t.Fatalf("the header's owner reached the persisted author: %q", *session.Owner)
	}
	if session.ID != "session-0123456789abcdef" {
		t.Fatalf("author id = %q, want the header's session id", session.ID)
	}
	if session.Origin == nil || session.Origin.Host != "forge" {
		t.Fatalf("author origin = %#v, want the header's origin preserved", session.Origin)
	}
}

// sharedAgentTokens is a DISPATCH_AGENT_TOKEN setting parsed as the server parses it at boot.
func sharedAgentTokens(t *testing.T, setting string) *auth.SharedAgentTokens {
	t.Helper()
	tokens, err := auth.ParseSharedAgentTokens(setting)
	if err != nil {
		t.Fatalf("ParseSharedAgentTokens(%q): %v", setting, err)
	}
	return tokens
}

// The document websocket takes a bearer's session actor only when the bearer is one of the shared
// agent token's values; one byte off, or a value the setting does not list, the same actor is
// refused.
func TestDocumentBearerMustBeTheSharedToken(t *testing.T) {
	service := &Service{agentTokens: sharedAgentTokens(t, "doc-agent-token old-doc-token")}
	for _, test := range []struct {
		authorization string
		admitted      bool
	}{
		{authorization: "Bearer doc-agent-token", admitted: true},
		{authorization: "Bearer old-doc-token", admitted: true},
		{authorization: "Bearer doc-agent-tokem", admitted: false},
		{authorization: "Bearer other-doc-token", admitted: false},
	} {
		request := httptest.NewRequest(http.MethodGet, "/ws/doc/room", nil)
		request.Header.Set("Authorization", test.authorization)
		request.Header.Set("X-Dispatch-Actor", `{"kind":"session","id":"session-0123456789abcdef"}`)
		if _, err := service.requestActor(request); (err == nil) != test.admitted {
			t.Errorf("requestActor(Authorization %q) err = %v, want admitted %t", test.authorization, err, test.admitted)
		}
	}
}

func TestWebsocketRejectsUnauthenticatedConnection(t *testing.T) {
	service, _ := newTestService(t)
	httpServer := httptest.NewServer(http.HandlerFunc(service.ServeHTTP))
	t.Cleanup(httpServer.Close)
	wsURL := "ws" + strings.TrimPrefix(httpServer.URL, "http") + "/room"
	connection, response, err := gws.DefaultDialer.Dial(wsURL, nil)
	if connection != nil {
		_ = connection.Close()
	}
	if err == nil {
		t.Fatal("unauthenticated websocket connection succeeded")
	}
	if response == nil || response.StatusCode != http.StatusUnauthorized {
		t.Fatalf("unauthenticated websocket response = %#v, want HTTP 401", response)
	}
}

func TestUnauthenticatedDocumentRequestsDoNotAllocateRooms(t *testing.T) {
	service, _ := newTestService(t)

	for number := range 1000 {
		response := httptest.NewRecorder()
		service.ServeHTTP(response, httptest.NewRequest(http.MethodGet, fmt.Sprintf("/ws/doc/random-%d", number), nil))
		if response.Code != http.StatusUnauthorized {
			t.Fatalf("request %d status = %d, want %d", number, response.Code, http.StatusUnauthorized)
		}
	}

	rooms := 0
	service.rooms.Range(func(_, _ any) bool {
		rooms++
		return true
	})
	if rooms != 0 {
		t.Fatalf("unauthenticated requests left %d room states, want 0", rooms)
	}
}
