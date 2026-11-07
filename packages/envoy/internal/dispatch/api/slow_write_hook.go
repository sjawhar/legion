package api

import (
	"context"
	"net/http"
	"strings"
	"sync/atomic"
	"time"
)

// slowWriteRecord is what the slow-write hook did on one route, held beside the route's armed
// delay so a test can ask afterwards whether the hook really ran. A disarm leaves the record
// standing - only the delay goes - because what a test wants to know once it has disarmed a route
// is what the hook did while it was armed, and whether it did anything since.
//
// The count is of runs that entered the hook's delay branch, not of the time they waited there: a
// hook that stored a zero delay instead of deleting the disarmed route would still enter the
// branch, and a zero timer fires at once, so a record of time waited would read zero and let that
// pass. A record of entries reads one and does not. The time waited is kept because it is worth
// printing when such an assertion fails, but nothing is judged by it.
type slowWriteRecord struct {
	runs   atomic.Int64 // runs that entered the hook's delay branch
	waited atomic.Int64 // what they waited there altogether, in nanoseconds
}

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
		// Only the delay goes: the route's slowWriteRecord outlives the disarm, so what the hook
		// did while the route was armed is still there to be read.
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
// the key is released and nothing is written. Either way the run is recorded against its route
// (waitSlowWrite), which is how a test tells a delay that ran from one that never did.
func (s *server) slowedByTestHook(handler http.HandlerFunc) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		route := r.Method + " " + r.URL.Path
		if delay, armed := s.slowWrites.Load(route); armed {
			if !s.waitSlowWrite(r.Context(), route, delay.(time.Duration)) {
				writeError(w, "SLOW_WRITE_CUT_OFF", http.StatusServiceUnavailable,
					"the slow-write test hook's wait was cut off before the handler ran")
				return
			}
		}
		handler(w, r)
	}
}

// waitSlowWrite waits delay out on ctx - the handler's own context - and counts this run against
// route, answering whether the wait ran out. False is a context that ended inside the delay, whose
// run never reaches its handler. The run is counted either way, and the time it spent waiting is
// added either way too: a run cut off inside the delay entered the branch just as one that waited
// it out did, and a test that drove the hook into that branch should see it.
func (s *server) waitSlowWrite(ctx context.Context, route string, delay time.Duration) bool {
	held, _ := s.slowWriteRuns.LoadOrStore(route, &slowWriteRecord{})
	record := held.(*slowWriteRecord)
	entered := time.Now()
	waited := waitFor(ctx, delay)
	record.runs.Add(1)
	record.waited.Add(int64(time.Since(entered)))
	return waited
}

// slowWriteRunsOn answers what the hook did on route: how many runs entered its delay branch and
// what they waited there altogether. A route the hook never delayed answers no runs and no time,
// so a caller reads the same thing whether the route was never armed or was armed and the hook
// never reached.
func (s *server) slowWriteRunsOn(route string) (runs int64, waited time.Duration) {
	held, kept := s.slowWriteRuns.Load(route)
	if !kept {
		return 0, 0
	}
	record := held.(*slowWriteRecord)
	return record.runs.Load(), time.Duration(record.waited.Load())
}
