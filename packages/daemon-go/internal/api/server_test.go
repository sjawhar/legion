package api

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgconn"
)

type fakeSource struct {
	state State
	err   error
}

func (f fakeSource) State(_ context.Context, _ pgx.Tx) (State, error) {
	if f.err != nil {
		return State{}, f.err
	}
	return f.state, nil
}

func serve(t *testing.T, src StateSource, method, target string) *httptest.ResponseRecorder {
	t.Helper()
	return serveWithTransactions(t, src, &fakeStateTransactions{tx: &fakeTx{}}, method, target)
}

func serveWithTransactions(t *testing.T, src StateSource, txs *fakeStateTransactions, method, target string) *httptest.ResponseRecorder {
	t.Helper()
	server := NewServer("127.0.0.1", 8437, Options{State: src, StateTransactions: txs})
	recorder := httptest.NewRecorder()
	server.Handler.ServeHTTP(recorder, httptest.NewRequest(method, target, nil))
	return recorder
}

type fakeStateTransactions struct {
	tx   *fakeTx
	opts pgx.TxOptions
}

func (f *fakeStateTransactions) BeginTx(_ context.Context, opts pgx.TxOptions) (pgx.Tx, error) {
	f.opts = opts
	return f.tx, nil
}

type fakeTx struct {
	committed  bool
	rolledBack bool
}

func (t *fakeTx) Begin(context.Context) (pgx.Tx, error) { return t, nil }
func (t *fakeTx) Commit(context.Context) error {
	t.committed = true
	return nil
}
func (t *fakeTx) Rollback(context.Context) error {
	t.rolledBack = true
	return nil
}
func (*fakeTx) CopyFrom(context.Context, pgx.Identifier, []string, pgx.CopyFromSource) (int64, error) {
	return 0, errors.New("unexpected copy")
}
func (*fakeTx) SendBatch(context.Context, *pgx.Batch) pgx.BatchResults { return nil }
func (*fakeTx) LargeObjects() pgx.LargeObjects                         { return pgx.LargeObjects{} }
func (*fakeTx) Prepare(context.Context, string, string) (*pgconn.StatementDescription, error) {
	return nil, errors.New("unexpected prepare")
}
func (*fakeTx) Exec(context.Context, string, ...any) (pgconn.CommandTag, error) {
	return pgconn.CommandTag{}, errors.New("unexpected exec")
}
func (*fakeTx) Query(context.Context, string, ...any) (pgx.Rows, error) {
	return nil, errors.New("unexpected query")
}
func (*fakeTx) QueryRow(context.Context, string, ...any) pgx.Row { return nil }
func (*fakeTx) Conn() *pgx.Conn                                  { return nil }

func TestServerBindsTheConfiguredAddressOnly(t *testing.T) {
	server := NewServer("127.0.0.1", 8437, Options{State: fakeSource{}})
	if server.Addr != "127.0.0.1:8437" {
		t.Fatalf("addr = %q, want 127.0.0.1:8437", server.Addr)
	}
	if server.ReadHeaderTimeout == 0 {
		t.Fatal("ReadHeaderTimeout is unset: a slow-header client would hold the daemon's listener")
	}
}

func TestStateRouteServesTheRecord(t *testing.T) {
	src := fakeSource{state: State{
		Daemon:    DaemonInfo{Project: "legion", SchemaVersion: 1, Boots: 7},
		Admission: Admission{Cap: 2},
	}}

	recorder := serve(t, src, http.MethodGet, "/legion/v1/state")

	if recorder.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200; body %s", recorder.Code, recorder.Body)
	}
	if contentType := recorder.Header().Get("Content-Type"); contentType != "application/json" {
		t.Fatalf("content-type = %q, want application/json", contentType)
	}
	var got State
	if err := json.Unmarshal(recorder.Body.Bytes(), &got); err != nil {
		t.Fatalf("decode body %s: %v", recorder.Body, err)
	}
	if got.Daemon.Project != "legion" || got.Daemon.Boots != 7 || got.Admission.Cap != 2 {
		t.Fatalf("state = %+v, want the source's project, boots and cap", got)
	}
	// The plugin's reader is strict: a nil Go slice or map on the wire is `null`, which its
	// schema refuses. The empty record must still be an array and an object.
	body := recorder.Body.String()
	for _, want := range []string{`"active":[]`, `"waiting":[]`, `"issues":{}`} {
		if !strings.Contains(body, want) {
			t.Fatalf("body %s is missing %s", body, want)
		}
	}
}

func TestStateRouteUsesARepeatableReadOnlyTransaction(t *testing.T) {
	txs := &fakeStateTransactions{tx: &fakeTx{}}

	recorder := serveWithTransactions(t, fakeSource{state: State{}}, txs, http.MethodGet, "/legion/v1/state")

	if recorder.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200; body %s", recorder.Code, recorder.Body)
	}
	if txs.opts.IsoLevel != pgx.RepeatableRead || txs.opts.AccessMode != pgx.ReadOnly {
		t.Fatalf("transaction options = %#v, want repeatable-read read-only", txs.opts)
	}
	if !txs.tx.committed {
		t.Fatal("state transaction was not committed")
	}
}

func TestStateRouteReportsASourceFailure(t *testing.T) {
	// The handler logs the failure through the default logger; keep it out of the test output.
	previous := slog.Default()
	slog.SetDefault(slog.New(slog.NewTextHandler(io.Discard, nil)))
	t.Cleanup(func() { slog.SetDefault(previous) })

	recorder := serve(t, fakeSource{err: errors.New("dial db.internal: refused")}, http.MethodGet, "/legion/v1/state")

	if recorder.Code != http.StatusInternalServerError {
		t.Fatalf("status = %d, want 500; body %s", recorder.Code, recorder.Body)
	}
	var body map[string]string
	if err := json.Unmarshal(recorder.Body.Bytes(), &body); err != nil {
		t.Fatalf("decode body %s: %v", recorder.Body, err)
	}
	if body["error"] == "" {
		t.Fatalf("body = %v, want an error message", body)
	}
}

func TestHealthzAnswersWithoutTheStore(t *testing.T) {
	src := fakeSource{err: errors.New("dial db.internal: refused")}

	recorder := serve(t, src, http.MethodGet, "/healthz")

	if recorder.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200; body %s", recorder.Code, recorder.Body)
	}
}

func TestUnknownPathIsARefusedRoute(t *testing.T) {
	recorder := serve(t, fakeSource{}, http.MethodGet, "/legion/v1/trees")

	if recorder.Code != http.StatusNotFound {
		t.Fatalf("status = %d, want 404", recorder.Code)
	}
	if got := strings.TrimSpace(recorder.Body.String()); got != `{"error":"no route"}` {
		t.Fatalf("body = %s, want {\"error\":\"no route\"}", got)
	}
}
