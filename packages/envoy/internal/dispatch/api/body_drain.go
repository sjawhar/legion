package api

import (
	"io"
	"net/http"
	"time"
)

// bodyDrainTimeout bounds how long the server reads a body it is about to answer without using:
// long enough for a body up to an upload's limit to arrive over a slow link, short enough that a
// client that stops sending cannot hold the connection for good.
const bodyDrainTimeout = 60 * time.Second

// drainRequestBody reads and discards what is left of r's body, at most limit bytes (the route's
// own body limit) and for at most bodyDrainTimeout, so a request answered before its handler read
// its body - a refusal - still has its answer read. Without the drain the server answers and
// closes the connection while the client is still sending, the unread bytes reset the connection,
// and WebKit and Firefox report a failed connection instead of the answer (INFRA-258 reproduced
// it: 3 of 4 early answers in WebKit, 1 of 2 in Firefox). A body past limit is not drained to its
// end: the server then closes the connection after the answer, as it does for any body past a
// route's limit. INFRA-258 adds a helper of the same name; whichever change merges second keeps
// one.
func drainRequestBody(w http.ResponseWriter, r *http.Request, limit int64) {
	if r.Body == nil || r.Body == http.NoBody {
		return
	}
	// A writer that cannot set the deadline (a test's recorder) has no connection to hold.
	_ = http.NewResponseController(w).SetReadDeadline(time.Now().Add(bodyDrainTimeout))
	_, _ = io.Copy(io.Discard, http.MaxBytesReader(w, r.Body, limit))
}
