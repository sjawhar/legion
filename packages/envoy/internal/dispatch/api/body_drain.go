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
// it, and the client drops the answer with it. So every layer's early answer - admission's 429
// OVERLOADED, the keyed layer's KEY_INVALID, the closed-issue refusal of an upload, LOGIN_CHANGED,
// a refused path parameter, a handler's refusal made before it decodes - waits until the body is
// off the connection, read and discarded, never spooled, up to the route's own limit
// (apiRoute.bodyLimit) and within the bound the request's body arrives under (defaultBodyReadLimit).
// The keyed layer reads a repeat's body itself when it finds the key held by a first request still
// running, and judges the repeat once that body has arrived (answerDrainedRepeat).

// defaultBodyReadLimit is the longest a request's whole body may take to arrive, counted from the
// first read of it, whichever layer reads it: the keyed layer's spool (readKeyedBody), the
// handler's own decode (decodeJSON, an upload's multipart parse), and the drain an early answer
// waits behind (drainBeforeAnswer). A minute carries a whole upload (apiRoute.bodyLimit, 26 MiB) over
// a link of 450 KB/s or more, and a JSON body (maxJSONRequestBytes, 1 MiB) over one of 17 KB/s;
// past it a read of the body fails. It is one bound for the request, not one per layer, because
// what it bounds is what the request holds while its body arrives: an admitted request holds its
// class's units until its handler returns (admitted), so twelve clients trickling a byte at a time
// would otherwise hold every write unit for as long as they cared to, and every other write on the
// process would be refused 429 OVERLOADED. A body still arriving at the bound is not waited for:
// the layer reading it answers as it answers an unreadable body, the connection closes after that
// answer, and the units go back.
const defaultBodyReadLimit = time.Minute

// bodyReadTime is how long a request's body has to arrive (defaultBodyReadLimit; bodyReadLimit in
// a test).
func (s *server) bodyReadTime() time.Duration {
	if s.bodyReadLimit > 0 {
		return s.bodyReadLimit
	}
	return defaultBodyReadLimit
}

// expectsContinue reports whether r's client asked to be told to continue before it sends its body
// (Expect: 100-continue). net/http tells it so at the body's first read, so a layer that answers
// such a request without reading the body asks the client for none of it.
func expectsContinue(r *http.Request) bool {
	return strings.EqualFold(strings.TrimSpace(r.Header.Get("Expect")), "100-continue")
}

// errUploadTooLarge is an upload route's answer to a body past its limit (apiRoute.bodyLimit), and
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
// one byte more, in io.Discard's buffer, and within the bound the body arrives under
// (drainedBody.arm). A body that runs past the limit is answered as the limit answers it, 413 with
// the route's own refusal, in place of the answer the layer gave, since no such request is taken
// whoever answers it; the connection then closes, as net/http closes one whose body runs past what
// it will discard. The one answer the limit does not replace is a 401: a caller that resolves to
// nobody is refused before any limit is judged
// (TestUnauthenticatedLargeIssueBodyIsRejectedBeforeDecoding), and a page whose session
// lapsed under a write reads that 401 as the cue to sign in again. A body that stops arriving - its
// time up, its client gone - leaves the answer as the layer gave it, and the connection closes
// after it. A body a layer already read to its end, or past the limit itself (its handler's 413),
// is left as it is, and so is the body of a client that asked to be told to continue before
// sending it (Expect: 100-continue) and was never told: it sends nothing, and the first read would
// tell it to send all of it. A request whose body failed and whose handler answered nothing is
// answered here (refuseUnreadBody).
func (s *server) drainBeforeAnswer(method, pattern string, limit int64, handler http.HandlerFunc) http.HandlerFunc {
	tooLarge := bodyTooLarge(method, pattern)
	return func(w http.ResponseWriter, r *http.Request) {
		if r.Body == nil || r.Body == http.NoBody {
			handler(w, r)
			return
		}
		body := &drainedBody{ReadCloser: r.Body, bound: http.NewResponseController(w), within: s.bodyReadTime()}
		r.Body = body
		answer := &drainingWriter{
			ResponseWriter: w,
			body:           body,
			limit:          limit,
			tooLarge:       tooLarge,
			askedContinue:  expectsContinue(r),
		}
		handler(answer, r)
		answer.refuseUnreadBody()
	}
}

// drainedBody is a request's body: the one read bound every layer that reads it shares, what was
// read of it, whether it ended, and whether the read that ended it failed.
type drainedBody struct {
	io.ReadCloser
	// bound is the request's connection, on which arm sets the read deadline; within is how long
	// the whole body has to arrive from the first read of it (bodyReadTime).
	bound   *http.ResponseController
	within  time.Duration
	armed   bool
	read    int64
	touched bool
	ended   bool
	// failed is a body that ended on anything but its end: the bound it arrives under passed, its
	// client went away, its connection broke.
	failed bool
}

// arm bounds the reads of this body to within from now, once however many layers read it: a read
// deadline on the request's connection (http.ResponseController.SetReadDeadline), past which a
// read fails. One bound covers the keyed layer's spool, the handler's decode and the drain behind
// an early answer, rather than one each, so the request holds its admission units and its
// connection for that long and no longer (defaultBodyReadLimit). A writer with no connection of
// its own (a test's recorder) bounds nothing.
func (b *drainedBody) arm() {
	if b.armed {
		return
	}
	b.armed = true
	_ = b.bound.SetReadDeadline(time.Now().Add(b.within))
}

// lift takes the bound off, which Read does only once the body has reached its end. Lifted, the
// connection has no read deadline, which is what it has between requests on this server, whose
// http.Server sets no ReadTimeout (cmd/dispatch): a deadline left in place there would end the
// read net/http keeps on an idle connection once the body has ended, and cancel the request's
// context under its answer. A body that failed or that nothing read to its end keeps the bound,
// since net/http reads on after the answer itself: it discards up to 256 KiB more of a body whose
// Content-Length leaves that little unread before it writes the answer's header, and again when it
// closes the body once the handler returns. With the deadline those reads end where the request's
// own did and the connection closes; lifted, a client that stopped sending would hold the
// connection open after its answer, and withhold the answer itself where its body ran past the
// route's limit.
func (b *drainedBody) lift() {
	if b.armed {
		_ = b.bound.SetReadDeadline(time.Time{})
	}
}

func (b *drainedBody) Read(p []byte) (int, error) {
	b.touched = true
	b.arm()
	n, err := b.ReadCloser.Read(p)
	b.read += int64(n)
	if err != nil {
		b.ended = true
		if errors.Is(err, io.EOF) {
			b.lift()
		} else {
			b.failed = true
		}
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
	begun         bool
	replaced      bool
}

// begin drains what is left of the body, once, before the first byte of the answer, whose status
// is status. The drain reads under the bound the body arrives under, whatever is left of it
// (drainedBody.arm): a read that fails - the request's time up, the client gone, a connection
// that broke - leaves the answer to be written as it was, and the connection to close after it,
// the rest of the body unread. Only a body that ended lifts that bound (drainedBody.lift); one
// that failed or ran past the limit keeps it for net/http's own reads after the answer.
func (w *drainingWriter) begin(status int) {
	if w.begun {
		return
	}
	w.begun = true
	body := w.body
	if body.ended || body.read > w.limit || (w.askedContinue && !body.touched) {
		return
	}
	_, err := io.CopyN(io.Discard, body, w.limit-body.read+1)
	switch {
	case errors.Is(err, io.EOF):
		// The body ended inside the limit: the answer stands.
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

// refuseUnreadBody answers a request whose body failed - the bound it arrives under passed, its
// client stopped sending - and whose handler wrote nothing, which is what a handler does when the
// failed read cancelled the request's context under it: net/http reads a handler that wrote
// nothing as a 200 with an empty body, and a client that trickled its body would read that as its
// write having been taken. It is refused instead as the keyed layer refuses a body it could not
// read, and the connection closes after the answer, the rest of the body unread. A handler that
// answered, however early, answered for itself (begin).
func (w *drainingWriter) refuseUnreadBody() {
	if w.begun || !w.body.failed {
		return
	}
	w.begun = true
	w.ResponseWriter.Header().Set("Connection", "close")
	writeError(w.ResponseWriter, "REQUEST_BODY_UNREADABLE", http.StatusBadRequest,
		"the request's body did not arrive: nothing was run")
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
