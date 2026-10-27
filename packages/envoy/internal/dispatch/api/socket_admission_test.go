package api

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"

	gws "github.com/gorilla/websocket"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"
	"github.com/reearth/ygo/encoding"

	"github.com/sjawhar/envoy/internal/dispatch/auth"
	"github.com/sjawhar/envoy/internal/dispatch/docs"
	"github.com/sjawhar/envoy/internal/dispatch/events"
	"github.com/sjawhar/envoy/internal/dispatch/identity"
	"github.com/sjawhar/envoy/internal/dispatch/pmdoc"
	"github.com/sjawhar/envoy/internal/dispatch/store"
	"github.com/sjawhar/envoy/internal/dispatch/store/storetest"
)

// sentStatement is one statement a store's connection sent, with its arguments.
type sentStatement struct {
	sql  string
	args []any
}

// statementCounter records every statement the connections of one store send, a pipeline's one
// by one (pgx.QueryTracer and pgx.BatchTracer), so a test counts the statements of some text a
// request sent. It is installed on the store's connection configuration, and every pool the store
// opens later is opened from a copy of it, so it sees the shared pool and the rooms pool alike.
type statementCounter struct {
	mu   sync.Mutex
	sent []sentStatement
}

func (c *statementCounter) record(statements ...sentStatement) {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.sent = append(c.sent, statements...)
}

func (c *statementCounter) TraceQueryStart(ctx context.Context, _ *pgx.Conn, data pgx.TraceQueryStartData) context.Context {
	c.record(sentStatement{sql: data.SQL, args: data.Args})
	return ctx
}

func (c *statementCounter) TraceQueryEnd(context.Context, *pgx.Conn, pgx.TraceQueryEndData) {}

func (c *statementCounter) TraceBatchStart(ctx context.Context, _ *pgx.Conn, data pgx.TraceBatchStartData) context.Context {
	for _, queued := range data.Batch.QueuedQueries {
		c.record(sentStatement{sql: queued.SQL, args: queued.Arguments})
	}
	return ctx
}

func (c *statementCounter) TraceBatchQuery(context.Context, *pgx.Conn, pgx.TraceBatchQueryData) {}

func (c *statementCounter) TraceBatchEnd(context.Context, *pgx.Conn, pgx.TraceBatchEndData) {}

// mark is where the statements recorded from now on start, for a later count since it.
func (c *statementCounter) mark() int {
	c.mu.Lock()
	defer c.mu.Unlock()
	return len(c.sent)
}

// count is how many statements sent since mark contain text and name first as their first
// argument.
func (c *statementCounter) count(since int, text, first string) int {
	c.mu.Lock()
	defer c.mu.Unlock()
	count := 0
	for _, statement := range c.sent[since:] {
		if strings.Contains(statement.sql, text) && len(statement.args) > 0 && fmt.Sprint(statement.args[0]) == first {
			count++
		}
	}
	return count
}

// The statements a socket's admission sends for the document's issue (docs.Service.issueOpen) and,
// with cookie identity, for the caller's session (store.PgSessionStore.CurrentSessionGeneration,
// which identity.CookieIdentity.Login reads once per resolution of the caller).
const (
	issueReadText = "coalesce(i.closed_at is null, true)"
	actorReadText = "from user_sessions where login"
	signedInLogin = "alice@d.example"
)

// A browser opens a document's socket on every page load and every reconnect, and an eviction or a
// deploy reconnects every browser of a room at once. Admitting one reads what it needs once: the
// document's issue once and the caller once, whether the room is warm or cold and with the
// Hocuspocus authentication frame the provider sends once the socket is open, and it walks no list
// of rooms to decide the caps.
func TestASocketOpenReadsTheIssueAndTheCallerOnce(t *testing.T) {
	ctx := context.Background()
	database := storetest.Open(t)
	counter := &statementCounter{}
	config := database.Pool.Config()
	config.ConnConfig.Tracer = counter
	opened, err := pgxpool.NewWithConfig(ctx, config)
	if err != nil {
		t.Fatalf("open a counted pool on the test database: %v", err)
	}
	pool := store.NewPool(opened)
	t.Cleanup(pool.Close)
	counted := &store.Store{Pool: pool}

	sessions := store.NewPgSessionStore(counted.Pool)
	cookieIdentity := identity.CookieIdentity{SigningKey: "signing-key", Sessions: sessions}
	broker := events.NewBroker()
	documentService := docs.New(docs.Deps{Store: counted, Events: broker, Identity: cookieIdentity, Settle: time.Hour})
	t.Cleanup(func() { _ = documentService.Shutdown(context.Background()) })
	scans := documentService.CountRoomScansForTest()
	deps, err := NewDeps(DepsInput{
		Store: counted, Identity: cookieIdentity, AgentTokens: sharedAgentTokens(t, "agent-token"),
		Docs: documentService, Events: broker,
		// No heartbeat resolves the caller again while the test counts.
		StreamHeartbeat: time.Hour,
	})
	if err != nil {
		t.Fatalf("new API dependencies: %v", err)
	}
	handler := http.NewServeMux()
	Register(handler, deps)
	generation, err := sessions.EnsureSession(ctx, signedInLogin)
	if err != nil {
		t.Fatalf("establish alice's session: %v", err)
	}
	cookie, err := http.ParseSetCookie(auth.IssueSessionCookie(signedInLogin, generation, "signing-key", true))
	if err != nil {
		t.Fatalf("parse session cookie: %v", err)
	}
	request := func(target string, body any) *httptest.ResponseRecorder {
		t.Helper()
		encoded, err := json.Marshal(body)
		if err != nil {
			t.Fatalf("encode request: %v", err)
		}
		req := httptest.NewRequest(http.MethodPost, target, bytes.NewReader(encoded))
		req.AddCookie(cookie)
		req.Header.Set("Content-Type", "application/json")
		response := httptest.NewRecorder()
		handler.ServeHTTP(response, req)
		return response
	}
	if response := request("/api/v1/projects", map[string]string{"key": "TEST", "name": "Test"}); response.Code != http.StatusCreated {
		t.Fatalf("create project: status=%d body=%s", response.Code, response.Body.String())
	}
	created := request("/api/v1/issues", map[string]string{"project": "TEST", "title": "Socket admission", "spec": "Before.\n"})
	if created.Code != http.StatusCreated {
		t.Fatalf("create issue: status=%d body=%s", created.Code, created.Body.String())
	}
	issue := decodeBody[struct {
		PrimaryArtifactID string `json:"primary_artifact_id"`
	}](t, created)
	room := issue.PrimaryArtifactID
	// The issue's creation may have loaded the document's room; the first socket opens it cold.
	if err := documentService.Evict(ctx, room); err != nil {
		t.Fatalf("evict the document's room: %v", err)
	}

	server := httptest.NewServer(handler)
	t.Cleanup(server.Close)
	schemaVersion := strconv.Itoa(pmdoc.SchemaVersion())
	url := "ws" + strings.TrimPrefix(server.URL, "http") + "/ws/doc/" + room + "?schema_version=" + schemaVersion
	header := http.Header{"Cookie": []string{cookie.Name + "=" + cookie.Value}}
	for _, open := range []string{"a cold open", "a warm open"} {
		since := counter.mark()
		scansBefore := scans.Load()
		connection, response, err := gws.DefaultDialer.Dial(url, header)
		if err != nil {
			t.Fatalf("%s: connect the document socket: response=%v err=%v", open, response, err)
		}
		t.Cleanup(func() { _ = connection.Close() })
		if scope := socketScope(t, connection, room, schemaVersion); scope != "read-write" {
			t.Fatalf("%s: the socket is authenticated %q, want read-write", open, scope)
		}
		if reads := counter.count(since, issueReadText, room); reads != 1 {
			t.Errorf("%s read the document's issue %d times, want once", open, reads)
		}
		if reads := counter.count(since, actorReadText, signedInLogin); reads != 1 {
			t.Errorf("%s resolved the caller %d times, want once", open, reads)
		}
		if scanned := scans.Load() - scansBefore; scanned != 0 {
			t.Errorf("%s walked the rooms %d times to decide the caps, want none", open, scanned)
		}
	}
}

// socketScope sends the Hocuspocus authentication frame a provider sends once its socket is open,
// carrying token as the dashboard carries its schema version, and returns the scope the server's
// authenticated reply names: "readonly" or "read-write".
func socketScope(t *testing.T, connection *gws.Conn, room, token string) string {
	t.Helper()
	frame := encoding.EncodeBytes(func(encoder *encoding.Encoder) {
		encoder.WriteVarString(room)
		encoder.WriteVarUint(2) // Hocuspocus authentication message.
		encoder.WriteVarUint(0) // Token authentication payload.
		encoder.WriteVarString(token)
	})
	if err := connection.WriteMessage(gws.BinaryMessage, frame); err != nil {
		t.Fatalf("send the Hocuspocus authentication frame: %v", err)
	}
	_ = connection.SetReadDeadline(time.Now().Add(10 * time.Second))
	defer func() { _ = connection.SetReadDeadline(time.Time{}) }()
	for {
		_, message, err := connection.ReadMessage()
		if err != nil {
			t.Fatalf("read the Hocuspocus authentication reply: %v", err)
		}
		decoder := encoding.NewDecoder(message)
		if _, err := decoder.ReadVarString(); err != nil {
			t.Fatalf("read the Hocuspocus document name: %v", err)
		}
		if kind, err := decoder.ReadVarUint(); err != nil || kind != 2 {
			continue
		}
		subtype, err := decoder.ReadVarUint()
		if err != nil || subtype != 2 {
			t.Fatalf("Hocuspocus authentication reply subtype = %d (%v), want authenticated", subtype, err)
		}
		scope, err := decoder.ReadVarString()
		if err != nil {
			t.Fatalf("read the Hocuspocus authentication scope: %v", err)
		}
		return scope
	}
}
