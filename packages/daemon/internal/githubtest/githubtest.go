// Package githubtest holds the fake GitHub server the daemon's tests share. A test that drives
// code which reads GitHub stands one up: an httptest server answering the handful of paths that
// code asks for, closed when the test ends. What every such fake does the same way — the server
// itself, the record it keeps of each request, the bearer gate, the JSON content type and the
// close — is Server's; what the test proves stays in the test's own handler: its switch, its
// default arm, and the state it keeps and reads. Unexpected is the default arm of a fake for
// which any request it did not plan for is a failure.
package githubtest

import (
	"net/http"
	"net/http/httptest"
	"testing"
)

// Spec is what a fake GitHub server is made of beyond its own handler.
type Spec struct {
	// Token, when set, is the bearer every request must carry; one that does not gets 401.
	Token string
	// Record, when set, is called with each request before the handler, from the server's own
	// goroutine, so a recorder a test reads keeps a lock of its own.
	Record func(*http.Request)
	// Handle is the caller's own switch, its default arm included.
	Handle http.HandlerFunc
}

// Server starts a fake GitHub server and returns its URL; it is closed when the test ends.
func Server(t *testing.T, spec Spec) string {
	t.Helper()
	return Serve(t, spec).URL
}

// Serve is Server for a fake that needs the server itself rather than its URL — its client, its
// handler, or a close of its own beside the one Serve registers.
func Serve(t *testing.T, spec Spec) *httptest.Server {
	t.Helper()
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if spec.Record != nil {
			spec.Record(r)
		}
		if spec.Token != "" && r.Header.Get("Authorization") != "Bearer "+spec.Token {
			http.Error(w, `{"message":"Bad credentials"}`, http.StatusUnauthorized)
			return
		}
		w.Header().Set("Content-Type", "application/json")
		spec.Handle(w, r)
	}))
	t.Cleanup(server.Close)
	return server
}

// Unexpected answers a request the fake did not expect: it fails the test naming the method and
// path, and answers 404.
func Unexpected(t *testing.T, w http.ResponseWriter, r *http.Request) {
	t.Helper()
	t.Errorf("unexpected GitHub request %s %s", r.Method, r.URL.Path)
	w.WriteHeader(http.StatusNotFound)
}
