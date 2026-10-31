package api

import (
	"errors"
	"net/http"
	"net/http/httptest"
	"net/url"
	"testing"
	"time"

	"github.com/sjawhar/envoy/internal/dispatch/admission"
	"github.com/sjawhar/envoy/internal/dispatch/docs"
	"github.com/sjawhar/envoy/internal/dispatch/store"
)

// requestPoolAcquires is how many acquires the request pool's tracer has counted.
func requestPoolAcquires(t *testing.T, pool *store.Pool) uint64 {
	t.Helper()
	for _, stats := range pool.Stats() {
		if stats.Name == store.RequestsPool {
			return stats.Acquire.Count
		}
	}
	t.Fatal("the request pool reports no stats")
	return 0
}

// awaitQuietRequestPool returns once the request pool holds no connection and has counted no
// acquire for a while, so the work the seed's writes left running (a settlement, a durable
// append) is not counted against the read measured next.
func awaitQuietRequestPool(t *testing.T, pool *store.Pool) {
	t.Helper()
	deadline := time.Now().Add(10 * time.Second)
	last, quietSince := requestPoolAcquires(t, pool), time.Now()
	for time.Since(quietSince) < 300*time.Millisecond {
		if time.Now().After(deadline) {
			t.Fatal("the request pool never went quiet after the seed")
		}
		time.Sleep(20 * time.Millisecond)
		if now := requestPoolAcquires(t, pool); now != last || pool.Stat().AcquiredConns() != 0 {
			last, quietSince = now, time.Now()
		}
	}
}

// pinnedReadSeed is an issue shaped like the load harness's seed: a spec with a requirement table,
// plain and anchored comments, an open ask, a route, and a reference to a second issue.
type pinnedReadSeed struct {
	key, other, artifactID string
}

func seedPinnedReadIssue(t *testing.T, handler http.Handler) pinnedReadSeed {
	t.Helper()
	spec := "# Spec\n\nRevision token r0\n\n| Requirement | State |\n| --- | --- |\n" +
		"| Requirement TEST-1-R1 holds | open |\n| Requirement TEST-1-R2 holds | open |\n"
	issue := createInteractionIssue(t, handler, "TEST", "Pinned read", spec)
	other := createReferenceAPIIssue(t, handler, "TEST")
	for _, body := range []string{"First plain comment", "Depends on dispatch://" + other.Key, "Third plain comment", "Fourth plain comment"} {
		createThreadComment(t, handler, issue.Key, map[string]any{"body": body}, "alice")
	}
	for _, quote := range []string{"Requirement TEST-1-R1 holds", "Requirement TEST-1-R2 holds"} {
		createThreadComment(t, handler, issue.Key, map[string]any{
			"body":   "Anchored on " + quote,
			"anchor": map[string]string{"artifact": "spec", "quote": quote},
		}, "alice")
	}
	createAnswerListAsk(t, handler, issue.Key, "Which requirement first?")
	if patched := dispatchRequest(t, handler, http.MethodPatch, "/api/v1/issues/"+issue.Key, map[string]any{"route": "role:reviewer"}, "alice"); patched.Code != http.StatusOK {
		t.Fatalf("route the issue: status=%d body=%s", patched.Code, patched.Body.String())
	}
	return pinnedReadSeed{key: issue.Key, other: other.Key, artifactID: issue.PrimaryArtifactID}
}

// hotReadPaths are the production route mix's hot reads, each asked of seed.
func hotReadPaths(seed pinnedReadSeed) []string {
	return []string{
		"/api/v1/issues/" + seed.key,
		"/api/v1/artifacts/" + seed.artifactID + "/text",
		"/api/v1/issues/" + seed.key + "/events",
		"/api/v1/issues/" + seed.key + "/references",
		"/api/v1/references?to=" + url.QueryEscape("dispatch://"+seed.other),
		"/api/v1/references?from=" + url.QueryEscape("dispatch://"+seed.key),
		"/api/v1/issues?project=TEST",
		"/api/v1/issues/" + seed.key + "/comments",
		"/api/v1/inbox",
		"/api/v1/search?q=Requirement",
		"/api/v1/me/replies",
		"/api/v1/me/state",
		"/api/v1/agents",
	}
}

// Every hot read takes one request-pool connection for the whole request: its statements, a
// dozen for an issue read, run one after another on the connection its first statement took,
// rather than each waiting for a connection of its own.
func TestEachHotReadTakesOneRequestPoolConnection(t *testing.T) {
	listener := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		_, _ = w.Write([]byte(`[{"session_id":"ses-lg-0001","title":"reviewer","last_seen":100,"roles":["reviewer"]}]`))
	}))
	t.Cleanup(listener.Close)
	handler, database, _ := newTestServer(t, testServerOptions{envoyURL: listener.URL})
	seed := seedPinnedReadIssue(t, handler)
	for _, path := range hotReadPaths(seed) {
		awaitQuietRequestPool(t, database.Pool)
		before := requestPoolAcquires(t, database.Pool)
		response := dispatchRequest(t, handler, http.MethodGet, path, nil, "alice")
		acquires := requestPoolAcquires(t, database.Pool) - before
		if response.Code != http.StatusOK {
			t.Errorf("GET %s: status=%d body=%s, want 200", path, response.Code, response.Body.String())
			continue
		}
		if acquires != 1 {
			t.Errorf("GET %s took %d request-pool connections, want 1", path, acquires)
		}
		if held := database.Pool.Stat().AcquiredConns(); held != 0 {
			t.Errorf("GET %s left %d request-pool connections taken once it answered, want 0", path, held)
		}
	}
}

// An issue read needs only the one connection it pins, so a request pool of one connection answers
// it, in one checkout.
func TestAnIssueReadIsAnsweredByAOneConnectionRequestPool(t *testing.T) {
	handler, database, _ := newTestServer(t, testServerOptions{requestPoolSize: 1})
	seed := seedPinnedReadIssue(t, handler)
	awaitQuietRequestPool(t, database.Pool)

	before := requestPoolAcquires(t, database.Pool)
	response := dispatchRequest(t, handler, http.MethodGet, "/api/v1/issues/"+seed.key, nil, "alice")
	if response.Code != http.StatusOK {
		t.Fatalf("issue read on a one-connection request pool: status=%d body=%s, want 200", response.Code, response.Body.String())
	}
	if acquires := requestPoolAcquires(t, database.Pool) - before; acquires != 1 {
		t.Fatalf("issue read on a one-connection request pool took %d checkouts, want 1", acquires)
	}
}

// serveTestRoute serves one request to handler mounted as a route of method, as register mounts a
// table route of that method (serveRoute), and answers the response.
func serveTestRoute(t *testing.T, s *server, method string, handler http.HandlerFunc) *httptest.ResponseRecorder {
	t.Helper()
	route := apiRoute{routeRow: routeRow{Method: method, Pattern: "/api/v1/pinned-test", Auth: authAny, Handler: handler}}
	served := s.serveRoute(route, nil, admission.New(admission.DefaultLimits()), 5*time.Second)
	response := httptest.NewRecorder()
	served(response, httptest.NewRequest(method, "/api/v1/pinned-test", nil))
	return response
}

// A read that still nests - a statement while its own cursor is open on the pinned connection -
// is refused at once with ErrNestedAcquire, as on a pool connection of its own, rather than
// waiting on its own connection or interleaving two statements on it.
func TestAReadThatNestsOnItsPinnedConnectionIsRefused(t *testing.T) {
	_, database, _, s := newRegisteredTestServer(t, testServerOptions{})
	var nested error
	response := serveTestRoute(t, s, http.MethodGet, func(w http.ResponseWriter, r *http.Request) {
		rows, err := database.Pool.Query(r.Context(), "select generate_series(1, 3)")
		if err != nil {
			t.Errorf("open the read's cursor: %v", err)
			return
		}
		defer rows.Close()
		if !rows.Next() {
			t.Errorf("read the cursor's first row: %v", rows.Err())
			return
		}
		var one int
		nested = database.Pool.QueryRow(r.Context(), "select 1").Scan(&one)
		w.WriteHeader(http.StatusNoContent)
	})
	if response.Code != http.StatusNoContent {
		t.Fatalf("status=%d body=%s, want the handler's 204", response.Code, response.Body.String())
	}
	if !errors.Is(nested, store.ErrNestedAcquire) {
		t.Fatalf("a statement under the read's open cursor: %v, want ErrNestedAcquire", nested)
	}
	if held := database.Pool.Stat().AcquiredConns(); held != 0 {
		t.Fatalf("%d request-pool connections taken after the read answered, want 0", held)
	}
}

// A write is not pinned: each of its statements gives its connection back, so once one has run the
// write holds none and takes a document's writer gate. A read holds its pinned connection from its
// first statement to its end, so the gate refuses it, since a gate holder may be waiting for a
// connection that read holds.
func TestTheWriterGateAdmitsAWriteAndRefusesAPinnedRead(t *testing.T) {
	handler, database, deps, s := newRegisteredTestServer(t, testServerOptions{})
	issue := createInteractionIssue(t, handler, "TEST", "Gated", "A spec")
	gate := func(r *http.Request) error {
		var one int
		if err := database.Pool.QueryRow(r.Context(), "select 1").Scan(&one); err != nil {
			return err
		}
		_, release, err := deps.Docs.AcquireDocumentWriter(r.Context(), issue.PrimaryArtifactID)
		if err == nil {
			release()
		}
		return err
	}
	for _, served := range []struct {
		method string
		want   error
	}{
		{http.MethodPost, nil},
		{http.MethodGet, docs.ErrGateWhileHoldingConnection},
	} {
		var got error
		serveTestRoute(t, s, served.method, func(w http.ResponseWriter, r *http.Request) {
			got = gate(r)
			w.WriteHeader(http.StatusNoContent)
		})
		if !errors.Is(got, served.want) {
			t.Errorf("%s: the writer gate after a statement answered %v, want %v", served.method, got, served.want)
		}
	}
	if held := database.Pool.Stat().AcquiredConns(); held != 0 {
		t.Fatalf("%d request-pool connections taken after both requests answered, want 0", held)
	}
}
