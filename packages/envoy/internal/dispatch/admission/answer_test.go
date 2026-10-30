package admission

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/sjawhar/envoy/internal/dispatch/accesslog"
	"github.com/sjawhar/envoy/internal/dispatch/store"
	"github.com/sjawhar/envoy/internal/logtest"
)

// expiredRequest is a request context whose own deadline has passed.
func expiredRequest(t *testing.T) context.Context {
	t.Helper()
	ctx, cancel := WithDeadline(context.Background(), time.Nanosecond)
	t.Cleanup(cancel)
	<-ctx.Done()
	return ctx
}

func cancelledRequest() context.Context {
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	return ctx
}

func assertRetryable(t *testing.T, w *httptest.ResponseRecorder, status int, code string) {
	t.Helper()
	if w.Code != status || !strings.Contains(w.Body.String(), `"code":"`+code+`"`) {
		t.Fatalf("status=%d body=%s, want %d %s", w.Code, w.Body.String(), status, code)
	}
	if got := w.Header().Get("Retry-After"); got != "1" {
		t.Fatalf("Retry-After = %q, want 1", got)
	}
	if got := w.Header().Get("Cache-Control"); got != "no-store" {
		t.Fatalf("Cache-Control = %q, want no-store", got)
	}
}

func TestWriteUnservedTellsTheCausesApartInOrder(t *testing.T) {
	busy := fmt.Errorf("read issue: %w", fmt.Errorf("requests pool: %w: %w", store.ErrPoolBusy, context.DeadlineExceeded))
	commitCutOff := fmt.Errorf("commit the write: %w: %w", store.ErrCommitTimeout, context.DeadlineExceeded)
	for _, test := range []struct {
		name string
		ctx  context.Context
		err  error
		code string // empty: nothing written
		ok   bool
	}{
		{"the read's deadline passed", expiredRequest(t), context.DeadlineExceeded, CodeRequestTimeout, true},
		{"the read's deadline passed, whatever the handler failed with", expiredRequest(t), busy, CodeRequestTimeout, true},
		{"the client went away", cancelledRequest(), context.Canceled, "", true},
		{"the client went away while the pool was busy", cancelledRequest(), busy, "", true},
		{"the pool was busy", context.Background(), busy, CodeDatabaseBusy, true},
		// A deadline of its own on a live request says nothing about a pool: the commit that
		// ran out its bound carries one, and its outcome is unknown where a busy pool's is
		// that nothing ran, so its caller answers it rather than this layer.
		{"a commit ran out its bound", context.Background(), commitCutOff, "", false},
		{"another deadline ran out on a live request", context.Background(), fmt.Errorf("call: %w", context.DeadlineExceeded), "", false},
		{"anything else", context.Background(), errors.New("boom"), "", false},
	} {
		t.Run(test.name, func(t *testing.T) {
			w := httptest.NewRecorder()
			if ok := WriteUnserved(w, test.ctx, test.err); ok != test.ok {
				t.Fatalf("WriteUnserved = %v, want %v", ok, test.ok)
			}
			if test.code == "" {
				if w.Body.Len() != 0 || len(w.Header()) != 0 {
					t.Fatalf("wrote status=%d headers=%v body=%s, want nothing", w.Code, w.Header(), w.Body.String())
				}
				return
			}
			assertRetryable(t, w, http.StatusServiceUnavailable, test.code)
		})
	}
}

// A request whose client went away is answered nothing, and its access-log line says so: 499,
// nginx's client-closed code, where a handler that writes nothing is otherwise logged as the 200
// of a request served. The write that rolled back because its client left was read as a success.
func TestARequestWhoseClientWentAwayIsLogged499(t *testing.T) {
	logged := &bytes.Buffer{}
	logtest.SetDefault(t, slog.NewTextHandler(logged, nil))
	handler := accesslog.Handler("/api/v1/issues/{key}", http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if !WriteUnserved(w, r.Context(), fmt.Errorf("patch issue: %w", context.Canceled)) {
			t.Error("WriteUnserved did not answer a request whose client went away")
		}
	}))
	request := httptest.NewRequest(http.MethodPatch, "/api/v1/issues/TEST-1", nil).WithContext(cancelledRequest())
	handler.ServeHTTP(httptest.NewRecorder(), request)

	line := logged.String()
	if !strings.Contains(line, "msg=\""+accesslog.Message+"\"") || !strings.Contains(line, "status=499") {
		t.Fatalf("the access-log line of a request whose client went away:\n%s\nwant one carrying status=499", line)
	}
}

// A refusal is 429 Too Many Requests, not 503: nothing ran, the server is up, and the client is
// told to slow down and retry after Retry-After.
func TestWriteRefusedAnswersTooManyRequestsOverloaded(t *testing.T) {
	w := httptest.NewRecorder()
	WriteRefused(w, &Refusal{Class: Write, Reason: QueueFull})
	assertRetryable(t, w, http.StatusTooManyRequests, CodeOverloaded)
}
