package api

import (
	"bytes"
	"encoding/json"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/sjawhar/envoy/internal/dispatch/accesslog"
	"github.com/sjawhar/envoy/internal/dispatch/store"
)

// BenchmarkARouteWithAndWithoutItsAccessLogLine measures what the one line per request costs on a
// real route: GET /api/v1/issues/{key} served by the registered handlers against the local
// Postgres (DISPATCH_TEST_DATABASE_URL), once through the mux Register mounts, whose every route
// writes its line, and once through a mux of the same handlers with the line left off. The line
// is written through a slog text handler on io.Discard, so the number is the line's own cost
// without the operator's stderr behind it.
func BenchmarkARouteWithAndWithoutItsAccessLogLine(b *testing.B) {
	setDefaultLogger(b, slog.NewTextHandler(io.Discard, nil))

	logged, database, _, served := newRegisteredTestServer(b, testServerOptions{})
	key := benchmarkIssue(b, logged, database)

	plain := http.NewServeMux()
	for _, route := range served.routes() {
		plain.HandleFunc(route.Method+" "+route.Pattern, trackTransactions(served.refuseUnstorableParameters(route.Pattern, route.Handler)))
	}

	for _, bench := range []struct {
		name    string
		handler http.Handler
	}{
		{"with the access log", logged},
		{"without the access log", plain},
	} {
		b.Run(bench.name, func(b *testing.B) {
			b.ReportAllocs()
			for b.Loop() {
				request := httptest.NewRequest(http.MethodGet, "/api/v1/issues/"+key, nil)
				request.Header.Set("Authorization", "Bearer agent-token")
				response := httptest.NewRecorder()
				bench.handler.ServeHTTP(response, request)
				if response.Code != http.StatusOK {
					b.Fatalf("GET /api/v1/issues/%s = %d, want 200", key, response.Code)
				}
			}
		})
	}
}

// BenchmarkTheAccessLogLineAlone measures the wrapper and the line over a handler that only
// writes a short JSON body, so the cost is the log line's rather than a route's work.
func BenchmarkTheAccessLogLineAlone(b *testing.B) {
	setDefaultLogger(b, slog.NewTextHandler(io.Discard, nil))

	body := bytes.Repeat([]byte("x"), 512)
	answer := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		accesslog.Note(r.Context(), accesslog.KindSession, "session-0123456789abcdef")
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write(body)
	})
	for _, bench := range []struct {
		name    string
		handler http.Handler
	}{
		{"with the access log", accesslog.Handler("/api/v1/issues/{key}", answer)},
		{"without the access log", answer},
	} {
		b.Run(bench.name, func(b *testing.B) {
			b.ReportAllocs()
			for b.Loop() {
				request := httptest.NewRequest(http.MethodGet, "/api/v1/issues/BENCH-1", nil)
				response := httptest.NewRecorder()
				bench.handler.ServeHTTP(response, request)
			}
		})
	}
}

// benchmarkIssue creates one issue through the server under benchmark and answers its key.
func benchmarkIssue(b *testing.B, handler http.Handler, database *store.Store) string {
	b.Helper()
	if _, err := database.Pool.Exec(b.Context(), `insert into projects (key, name) values ('BENCH', 'Bench')`); err != nil {
		b.Fatalf("seed project: %v", err)
	}
	payload, err := json.Marshal(map[string]any{"project": "BENCH", "title": "An issue a benchmark reads"})
	if err != nil {
		b.Fatalf("encode issue: %v", err)
	}
	request := httptest.NewRequest(http.MethodPost, "/api/v1/issues", bytes.NewReader(payload))
	request.Header.Set("Content-Type", "application/json")
	request.Header.Set("X-Dispatch-User", "alice")
	response := httptest.NewRecorder()
	handler.ServeHTTP(response, request)
	if response.Code != http.StatusCreated {
		b.Fatalf("create issue = %d: %s", response.Code, response.Body.String())
	}
	var created struct {
		Key string `json:"key"`
	}
	if err := json.Unmarshal(response.Body.Bytes(), &created); err != nil {
		b.Fatalf("decode created issue: %v", err)
	}
	return created.Key
}
