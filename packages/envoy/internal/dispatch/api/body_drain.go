package api

import (
	"errors"
	"io"
	"net/http"
	"strings"
	"time"
)

// An answer given before a request's body is read reaches some clients as a failed connection
// rather than as the answer (INFRA-258). An HTTP/1.1 client that sends its whole
// body before it reads the answer - WebKit and Firefox uploading a file - is still sending when
// net/http, finding the body unread, writes the answer and closes the connection; the close resets
// it, and the client drops the answer with it. So every layer's early answer - the keyed layer's
// KEY_INVALID, the closed-issue refusal of an upload, LOGIN_CHANGED, a refused path parameter, a
// handler's refusal made before it decodes - waits until the body is off the connection, read and
// discarded, never spooled, up to the route's own limit (routeBodyLimit) and for at most
// bodyDrainLimit. The keyed layer reads a repeat's body itself when it finds the key held by a
// first request still running, and judges the repeat once that body has arrived
// (answerDrainedRepeat).

// bodyDrainLimit is the longest a drain waits for the rest of a request's body, counted from when
// the drain begins: a minute carries a whole upload (routeBodyLimit) over a link of 450 KB/s or
// more. A body still arriving then is not waited for: its answer goes out as it stood, and the
// connection closes after it, so a client that stopped sending does not hold its request open - nor
// whatever the layer that answered it holds - for as long as it cares to.
const bodyDrainLimit = time.Minute

// drainTime is how long a drain waits for a body (bodyDrainLimit; drainReadLimit in a test).
func (s *server) drainTime() time.Duration {
	if s.drainReadLimit > 0 {
		return s.drainReadLimit
	}
	return bodyDrainLimit
}

// readFor bounds a drain of the body of the request w answers to drainTime from now, a read
// deadline on its connection (http.ResponseController.SetReadDeadline), past which a read fails
// and the drain ends; it answers what lifts the bound, which the drain calls only once the body has
// reached its end. Lifted, the connection has no read deadline, which is what it has between
// requests on this server, whose http.Server sets no ReadTimeout (cmd/dispatch): a deadline left in
// place there would end the read net/http keeps on an idle connection once the body has ended, and
// cancel the request's context under its answer. A drain that ends short of the body's end - its
// read failed, or the body ran past the route's limit - leaves the deadline in place, since net/http
// reads on after the answer itself: it discards up to 256 KiB more of a body whose Content-Length
// leaves that little unread before it writes the answer's header, and again when it closes the body
// once the handler returns. With the deadline those reads end where the drain's did and the
// connection closes; lifted, a client that stopped sending would hold the connection open after its
// answer, and withhold the answer itself where its body ran past the limit. A writer with no
// connection of its own (a test's recorder) bounds nothing.
func (s *server) readFor(w http.ResponseWriter) (lift func()) {
	controller := http.NewResponseController(w)
	if err := controller.SetReadDeadline(time.Now().Add(s.drainTime())); err != nil {
		return func() {}
	}
	return func() { _ = controller.SetReadDeadline(time.Time{}) }
}

// expectsContinue reports whether r's client asked to be told to continue before it sends its body
// (Expect: 100-continue). net/http tells it so at the body's first read, so a layer that answers
// such a request without reading the body asks the client for none of it.
func expectsContinue(r *http.Request) bool {
	return strings.EqualFold(strings.TrimSpace(r.Header.Get("Expect")), "100-continue")
}

// errUploadTooLarge is an upload route's answer to a body past its limit (routeBodyLimit), and
// errJSONTooLarge every other route's (decodeJSON).
var (
	errUploadTooLarge = errorf(http.StatusRequestEntityTooLarge, "CAP_EXCEEDED", "artifact blob exceeds 25 MB")
	errJSONTooLarge   = errorf(http.StatusRequestEntityTooLarge, "REQUEST_TOO_LARGE", "request body exceeds %d bytes", maxJSONRequestBytes)
)

// bodyTooLarge is the route's answer to a body past its limit: the one its handler gives on
// reading past it.
func bodyTooLarge(method, pattern string) *apiError {
	if keyedUploads[method+" "+pattern] {
		return errUploadTooLarge
	}
	return errJSONTooLarge
}

// drainBeforeAnswer serves a route whose requests carry a body - every route but a GET - so that
// whatever layer answers a request, and however little of its body that layer read, the answer
// is written only once the rest of the body is read and discarded: at most the route's limit and
// one byte more, in io.Discard's buffer, for at most drainTime (readFor). A body that runs past the
// limit is answered as the limit answers it, 413 with the route's own refusal, in place of the
// answer the layer gave, since no such request is taken whoever answers it; the connection then
// closes, as net/http closes one whose body runs past what it will discard. The one answer the
// limit does not replace is a 401: a caller that resolves to nobody is refused before any limit is
// judged (TestUnauthenticatedLargeIssueBodyIsRejectedBeforeDecoding), and a page whose session
// lapsed under a write reads that 401 as the cue to sign in again. A body that stops arriving - its
// time up, its client gone - leaves the answer as the layer gave it, and the connection closes
// after it. A body a layer already read to its end, or past the limit itself (its handler's 413),
// is left as it is, and so is the body of a client that asked to be told to continue before
// sending it (Expect: 100-continue) and was never told: it sends nothing, and the first read would
// tell it to send all of it.
func (s *server) drainBeforeAnswer(method, pattern string, handler http.HandlerFunc) http.HandlerFunc {
	limit := routeBodyLimit(method, pattern)
	tooLarge := bodyTooLarge(method, pattern)
	return func(w http.ResponseWriter, r *http.Request) {
		if r.Body == nil || r.Body == http.NoBody {
			handler(w, r)
			return
		}
		body := &drainedBody{ReadCloser: r.Body}
		r.Body = body
		handler(&drainingWriter{
			ResponseWriter: w,
			body:           body,
			limit:          limit,
			tooLarge:       tooLarge,
			askedContinue:  expectsContinue(r),
			readFor:        s.readFor,
		}, r)
	}
}

// drainedBody is a request's body that counts what was read of it and whether it ended.
type drainedBody struct {
	io.ReadCloser
	read    int64
	touched bool
	ended   bool
}

func (b *drainedBody) Read(p []byte) (int, error) {
	b.touched = true
	n, err := b.ReadCloser.Read(p)
	b.read += int64(n)
	if err != nil {
		b.ended = true
	}
	return n, err
}

// drainingWriter is the answer's writer: its first header, write or flush drains the body first
// (drainBeforeAnswer).
type drainingWriter struct {
	http.ResponseWriter
	body          *drainedBody
	limit         int64
	tooLarge      *apiError
	askedContinue bool
	readFor       func(http.ResponseWriter) (lift func())
	begun         bool
	replaced      bool
}

// begin drains what is left of the body, once, before the first byte of the answer, whose status
// is status. A read that fails - the drain's time up, the client gone, a connection that broke -
// leaves the answer to be written as it was, and the connection to close after it, the rest of the
// body unread. Only a body that ended lifts the drain's deadline (readFor); one that failed or ran
// past the limit keeps it for net/http's own reads after the answer.
func (w *drainingWriter) begin(status int) {
	if w.begun {
		return
	}
	w.begun = true
	body := w.body
	if body.ended || body.read > w.limit || (w.askedContinue && !body.touched) {
		return
	}
	lift := w.readFor(w.ResponseWriter)
	_, err := io.CopyN(io.Discard, body, w.limit-body.read+1)
	switch {
	case errors.Is(err, io.EOF):
		// The body ended inside the limit: the answer stands.
		lift()
		return
	case err != nil:
		w.ResponseWriter.Header().Set("Connection", "close")
		return
	case status == http.StatusUnauthorized:
		return
	}
	// The body runs past the limit: the answer is the limit's.
	clear(w.ResponseWriter.Header())
	writeError(w.ResponseWriter, w.tooLarge.code, w.tooLarge.status, w.tooLarge.message)
	w.replaced = true
}

func (w *drainingWriter) WriteHeader(status int) {
	w.begin(status)
	if !w.replaced {
		w.ResponseWriter.WriteHeader(status)
	}
}

// Write is the answer's body; a first Write without WriteHeader answers 200, as net/http's does.
func (w *drainingWriter) Write(p []byte) (int, error) {
	w.begin(http.StatusOK)
	if w.replaced {
		return len(p), nil
	}
	return w.ResponseWriter.Write(p)
}

// Flush flushes the answer once the body is drained, so a handler that flushes early cannot send
// the answer ahead of it.
func (w *drainingWriter) Flush() {
	w.begin(http.StatusOK)
	if !w.replaced {
		_ = http.NewResponseController(w.ResponseWriter).Flush()
	}
}

// Unwrap is the connection's own writer, for http.ResponseController.
func (w *drainingWriter) Unwrap() http.ResponseWriter {
	return w.ResponseWriter
}
