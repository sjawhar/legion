package main

import (
	"encoding/json"
	"net"
	"net/http"
	"net/http/httptest"
	"os"
	"reflect"
	"strings"
	"syscall"
	"testing"
	"unsafe"

	"github.com/nats-io/nats.go"
	"github.com/sjawhar/envoy/internal/bus"
	"github.com/sjawhar/envoy/internal/kvwatch"
	"github.com/sjawhar/envoy/internal/store"
)

// brokenPipeReadKV is an interest bucket whose reads fail with the raw socket error nats.go v1.50.0
// returns when a request fills its 32 KiB write buffer on a broken connection: natsWriter.flush
// passes the socket's own *net.OpError through (nats.go:2118-2119, 2143-2145), and so do publish
// and requestWithContext (nats.go:4406, context.go:60-62). Every other call goes to the real bucket.
type brokenPipeReadKV struct {
	nats.KeyValue
}

func (kv brokenPipeReadKV) Get(string) (nats.KeyValueEntry, error) {
	return nil, &net.OpError{Op: "write", Net: "tcp", Err: os.NewSyscallError("write", syscall.EPIPE)}
}

// breakInterestReads makes every interest bucket read of registry fail as brokenPipeReadKV does,
// by swapping the handle its watcher hands out, as stallSessionReads does for the session bucket.
func breakInterestReads(t *testing.T, registry *store.Registry) {
	t.Helper()
	field := reflect.ValueOf(registry).Elem().FieldByName("watcher")
	watcher := reflect.NewAt(field.Type(), unsafe.Pointer(field.UnsafeAddr())).Elem()
	broken := bus.KeyValue{KeyValue: brokenPipeReadKV{KeyValue: watcher.Interface().(*kvwatch.Watcher).KV().KeyValue}}
	watcher.Set(reflect.ValueOf(kvwatch.New("interest registry", broken, func(nats.KeyValueEntry) {}, func() {})))
}

// An interest read is answered only by its named answers: no key, a record this build cannot
// decode, or a key the bucket refuses. Any other error is a read that did not answer, whatever it
// names, so a raw socket error from a broken connection is the bus-unavailable 503 with
// Retry-After: 1 on GET /v1/sessions as on GET /v1/registry/<session_id>, never a session listed
// with no topics. The session has subscribed to nothing, so the interest cache holds no record for
// it and every read of it reaches the bucket. The negative control is both requests asked before
// the reads break: each answers 200 listing the session.
func TestAnInterestReadFailingWithARawSocketErrorIsTheBusNotAnswering(t *testing.T) {
	const missed = "ses_interest_read_broken_pipe"
	registry, sessions := setupSessionsTest(t, nil, map[string]int{missed: 1})
	state := &listenerDeps{registry: registry, sessions: sessions}
	get := func(handler http.HandlerFunc, path string) *httptest.ResponseRecorder {
		recorder := httptest.NewRecorder()
		handler.ServeHTTP(recorder, httptest.NewRequest(http.MethodGet, path, nil))
		return recorder
	}
	routes := []struct {
		path    string
		handler http.HandlerFunc
	}{
		{"/v1/sessions", sessionsHandler(registry, sessions)},
		{"/v1/registry/" + missed, sessionEntryHandler(state)},
	}

	for _, route := range routes {
		recorder := get(route.handler, route.path)
		if recorder.Code != http.StatusOK || !strings.Contains(recorder.Body.String(), `"session_id":"`+missed+`"`) {
			t.Fatalf("GET %s with the bus answering: status %d, body %s; want 200 naming %s", route.path, recorder.Code, recorder.Body.String(), missed)
		}
	}

	breakInterestReads(t, registry)
	for _, route := range routes {
		t.Run(route.path, func(t *testing.T) {
			recorder := get(route.handler, route.path)
			if recorder.Code != http.StatusServiceUnavailable {
				t.Fatalf("GET %s with the interest read failing on a broken pipe: status %d, body %s; want 503", route.path, recorder.Code, recorder.Body.String())
			}
			var refusal apiError
			if err := json.Unmarshal(recorder.Body.Bytes(), &refusal); err != nil {
				t.Fatalf("decode the refusal %s: %v", recorder.Body.String(), err)
			}
			if !strings.HasPrefix(refusal.Error, busUnansweredPrefix) || !strings.Contains(refusal.Error, "broken pipe") {
				t.Fatalf("the refusal reads %q, want it to begin %q and name the broken pipe", refusal.Error, busUnansweredPrefix)
			}
			if got := recorder.Header().Get("Retry-After"); got != "1" {
				t.Fatalf("Retry-After = %q, want 1", got)
			}
		})
	}
}
