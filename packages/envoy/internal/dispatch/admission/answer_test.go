package admission

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/sjawhar/envoy/internal/dispatch/store"
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

func assertUnavailable(t *testing.T, w *httptest.ResponseRecorder, code string) {
	t.Helper()
	if w.Code != http.StatusServiceUnavailable || !strings.Contains(w.Body.String(), `"code":"`+code+`"`) {
		t.Fatalf("status=%d body=%s, want 503 %s", w.Code, w.Body.String(), code)
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
	for _, test := range []struct {
		name string
		ctx  context.Context
		err  error
		code string // empty: nothing written
		ok   bool
	}{
		{"the request's deadline passed", expiredRequest(t), context.DeadlineExceeded, CodeRequestTimeout, true},
		{"the request's deadline passed, whatever the handler failed with", expiredRequest(t), busy, CodeRequestTimeout, true},
		{"the client went away", cancelledRequest(), context.Canceled, "", true},
		{"the client went away while the pool was busy", cancelledRequest(), busy, "", true},
		{"the pool was busy", context.Background(), busy, CodeDatabaseBusy, true},
		{"another deadline ran out on a live request", context.Background(), fmt.Errorf("call: %w", context.DeadlineExceeded), CodeDatabaseBusy, true},
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
			assertUnavailable(t, w, test.code)
		})
	}
}

func TestWriteRefusedAnswersOverloaded(t *testing.T) {
	w := httptest.NewRecorder()
	WriteRefused(w, &Refusal{Class: Write, Reason: QueueFull})
	assertUnavailable(t, w, CodeOverloaded)
}
