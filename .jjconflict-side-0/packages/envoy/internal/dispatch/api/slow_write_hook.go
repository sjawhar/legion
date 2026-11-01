package api

import (
	"net/http"
	"strings"
	"time"
)

// armSlowWrite is POST /api/v1/writes/_test/slow, {method, path, delay_ms}: from now on every keyed
// run of that method and exact request path, once its key is claimed, waits delay_ms on its
// handler's context before the handler runs (slowedByTestHook), so a browser test can drive a
// write Dispatch takes longer to finish than the page waits for an answer, without holding the
// request in the browser. It replaces what an earlier call armed for that method and path;
// delay_ms 0 disarms it. Test-only: mounted only under Deps.TestHooksEnabled.
func (s *server) armSlowWrite(w http.ResponseWriter, r *http.Request) {
	if !s.requireAuthenticated(w, r) {
		return
	}
	var input struct {
		Method  string `json:"method"`
		Path    string `json:"path"`
		DelayMS int64  `json:"delay_ms"`
	}
	if err := decodeJSON(r, &input); err != nil {
		s.writeHandlerError(w, err)
		return
	}
	if !keyedMethod(input.Method) || !strings.HasPrefix(input.Path, "/") ||
		input.DelayMS < 0 || input.DelayMS > keyedHandlerLimit.Milliseconds() {
		writeError(w, "INVALID_BODY", http.StatusBadRequest,
			"want {method: POST or PATCH, path: a request path, delay_ms: 0 to disarm, at most the keyed handler's limit}")
		return
	}
	route := input.Method + " " + input.Path
	if input.DelayMS == 0 {
		s.slowWrites.Delete(route)
	} else {
		s.slowWrites.Store(route, time.Duration(input.DelayMS)*time.Millisecond)
	}
	WriteJSON(w, http.StatusOK, map[string]bool{"ok": true})
}

// slowedByTestHook is handler behind the slow-write hook's delay for the request's method and path,
// when armSlowWrite armed one. The delay waits on the handler's own context: the one keyedWrites
// detaches from the client, so a client that gives up does not end it. A context that ends during
// the delay ends the run with the handler never called, answered 503 as an interrupted run is, so
// the key is released and nothing is written.
func (s *server) slowedByTestHook(handler http.HandlerFunc) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		if delay, armed := s.slowWrites.Load(r.Method + " " + r.URL.Path); armed {
			timer := time.NewTimer(delay.(time.Duration))
			select {
			case <-timer.C:
			case <-r.Context().Done():
				timer.Stop()
				writeError(w, "SLOW_WRITE_CUT_OFF", http.StatusServiceUnavailable,
					"the slow-write test hook's wait was cut off before the handler ran")
				return
			}
		}
		handler(w, r)
	}
}
