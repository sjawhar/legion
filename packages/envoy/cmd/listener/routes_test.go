package main

import (
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/sjawhar/envoy/internal/logging"
)

// `envoy-listener routes` prints each /v1 row's operations, and the docs site's API reference is
// generated from them, so a row names exactly what its handler answers: each operation's method
// reaches the handler at the operation's path rather than the /v1 not-found answer, and any other
// method is refused with 405 at that path.
func TestEveryV1RouteAnswersExactlyItsMethods(t *testing.T) {
	// "probe" holds an interest and a live entry, so a documented read of it answers what it holds.
	registry, sessions := setupSessionsTest(t, map[string][]string{"probe": {"notifications.test.>"}}, map[string]int{"probe": 1})
	mux := http.NewServeMux()
	registerV1Routes(mux, &listenerDeps{registry: registry, sessions: sessions}, "test-machine", logging.New("test"))
	const notFound = "{\"error\":\"not found\"}\n"

	documented := map[string]map[string]bool{}
	for _, route := range v1Routes {
		for _, operation := range route.operations {
			if !strings.HasPrefix(operation.Path, route.pattern) && operation.Path != route.pattern {
				t.Errorf("%s %s is documented under pattern %s, which does not serve it", operation.Method, operation.Path, route.pattern)
			}
			path := strings.NewReplacer("{session_id}", "probe", "{role}", "probe").Replace(operation.Path)
			if documented[path] == nil {
				documented[path] = map[string]bool{}
			}
			documented[path][operation.Method] = true
		}
	}
	// DELETE runs last, after every read of what it removes.
	for _, method := range []string{http.MethodGet, http.MethodPost, http.MethodPut, http.MethodPatch, http.MethodDelete} {
		for path, methods := range documented {
			recorder := httptest.NewRecorder()
			mux.ServeHTTP(recorder, httptest.NewRequest(method, path, nil))
			switch {
			case methods[method] && (recorder.Code == http.StatusMethodNotAllowed || recorder.Body.String() == notFound):
				t.Errorf("%s %s is documented, and answered %d %q", method, path, recorder.Code, recorder.Body.String())
			case !methods[method] && recorder.Code != http.StatusMethodNotAllowed:
				t.Errorf("%s %s is not documented, and answered %d %q, want 405", method, path, recorder.Code, recorder.Body.String())
			}
		}
	}
}
