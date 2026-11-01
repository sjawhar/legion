package main

import (
	"context"
	"encoding/json"
	"net"
	"net/http"
	"net/http/httptest"
	"os"
	"reflect"
	"strings"
	"syscall"
	"testing"
	"time"
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
// The replacement watches the real bucket through the handle it replaces and feeds nothing, so the
// registry's own copy stays as the stopped watcher left it, and the replacement's first read makes
// that copy servable again: what breaks is the per-key read alone, never the view the lists join.
func breakInterestReads(t *testing.T, registry *store.Registry) {
	t.Helper()
	field := reflect.ValueOf(registry).Elem().FieldByName("watcher")
	watcher := reflect.NewAt(field.Type(), unsafe.Pointer(field.UnsafeAddr())).Elem()
	stopped := watcher.Interface().(*kvwatch.Watcher)
	current := stopped.KV()
	stopped.Stop()
	broken := bus.NewKeyValue(brokenPipeReadKV{KeyValue: current.Legacy()}, current.WatchHandle(), current.Streams())
	replacement := kvwatch.New("interest registry", broken, feedsNothing{})
	watcher.Set(reflect.ValueOf(replacement))
	t.Cleanup(replacement.Stop)
	replacement.Start()
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	if err := replacement.WaitComplete(ctx); err != nil {
		t.Fatalf("the watcher reading through the broken handle never completed a read of the bucket: %v", err)
	}
}

// An interest read is answered only by its named answers: no key, a record this build cannot
// decode, or a key the bucket refuses. Any other error is a read that did not answer, whatever it
// names, so a raw socket error from a broken connection is the bus-unavailable 503 with
// Retry-After: 1 on GET /v1/registry/<session_id>, never a session answered with no topics.
//
// GET /v1/sessions reads no record at all: it joins the interest copy (store.Registry.Cached), so
// the same broken reads leave it answering its 200 list. A session the copy holds nothing for is
// listed with no topics, and that is not the answer this test forbids on the read-through route:
// the copy can be served (kvwatch.Watcher.Servable), which means a read of the whole bucket
// completed and found that session no interest record, rather than a read that failed saying
// nothing.
//
// The session has subscribed to nothing, so the interest cache holds no record for it and every
// per-key read of it reaches the bucket. The negative control is both requests asked before the
// reads break: each answers 200 naming the session.
func TestAnInterestReadFailingWithARawSocketErrorIsTheBusNotAnswering(t *testing.T) {
	const missed = "ses_interest_read_broken_pipe"
	registry, sessions := setupSessionsTest(t, nil, map[string]int{missed: 1})
	state := &listenerDeps{registry: registry, sessions: sessions}
	get := func(handler http.HandlerFunc, path string) *httptest.ResponseRecorder {
		recorder := httptest.NewRecorder()
		handler.ServeHTTP(recorder, httptest.NewRequest(http.MethodGet, path, nil))
		return recorder
	}
	list := sessionsHandler(registry, sessions)
	entry := sessionEntryHandler(state)
	for _, route := range []struct {
		path    string
		handler http.HandlerFunc
	}{
		{"/v1/sessions", list},
		{"/v1/registry/" + missed, entry},
	} {
		recorder := get(route.handler, route.path)
		if recorder.Code != http.StatusOK || !strings.Contains(recorder.Body.String(), `"session_id":"`+missed+`"`) {
			t.Fatalf("GET %s with the bus answering: status %d, body %s; want 200 naming %s", route.path, recorder.Code, recorder.Body.String(), missed)
		}
	}

	breakInterestReads(t, registry)
	t.Run("/v1/registry/"+missed, func(t *testing.T) {
		recorder := get(entry, "/v1/registry/"+missed)
		if recorder.Code != http.StatusServiceUnavailable {
			t.Fatalf("GET /v1/registry/%s with the interest read failing on a broken pipe: status %d, body %s; want 503", missed, recorder.Code, recorder.Body.String())
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
	t.Run("/v1/sessions", func(t *testing.T) {
		recorder := get(list, "/v1/sessions")
		if recorder.Code != http.StatusOK {
			t.Fatalf("GET /v1/sessions with the interest read failing on a broken pipe: status %d, body %s; want the 200 the copies answer, since the listing reads no record",
				recorder.Code, recorder.Body.String())
		}
		var rows []sessionInfo
		if err := json.Unmarshal(recorder.Body.Bytes(), &rows); err != nil {
			t.Fatalf("decode the session list %s: %v", recorder.Body.String(), err)
		}
		if len(rows) != 1 || rows[0].SessionID != missed || len(rows[0].Topics) != 0 {
			t.Fatalf("GET /v1/sessions with the interest read failing on a broken pipe listed %+v, want %s alone with no topics", rows, missed)
		}
	})
}
