package api

import (
	"io"
	"net/http"
	"strings"
)

// An answer given before a request's body is read reaches some clients as a failed connection
// rather than as the answer (INFRA-258, FinalReview4 L2). An HTTP/1.1 client that sends its whole
// body before it reads the answer - WebKit and Firefox uploading a file - is still sending when
// net/http, finding the body unread, writes the answer and closes the connection; the close resets
// it, and the client drops the answer with it. So every layer's early answer - the keyed layer's
// KEY_IN_FLIGHT and KEY_INVALID, the closed-issue refusal of an upload, LOGIN_CHANGED, a refused
// path parameter, a handler's refusal made before it decodes - waits until the body is off the
// connection, read and discarded, never spooled, up to the route's own limit (routeBodyLimit).

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
// one byte more, in io.Discard's buffer. A body that runs past the limit is answered as the limit
// answers it, 413 with the route's own refusal, in place of the answer the layer gave, since no
// such request is taken whoever answers it; the connection then closes, as net/http closes one
// whose body runs past what it will discard. The one answer the limit does not replace is a 401:
// a caller that resolves to nobody is refused before any limit is judged
// (TestUnauthenticatedLargeIssueBodyIsRejectedBeforeDecoding), and a page whose session lapsed
// under a write reads that 401 as the cue to sign in again. A body a layer already read to its
// end, or past the limit itself (its handler's 413), is left as it is, and so is the body of a
// client that asked to be told to continue before sending it (Expect: 100-continue) and was never
// told: it sends nothing, and reading would ask it for all of it.
func drainBeforeAnswer(method, pattern string, handler http.HandlerFunc) http.HandlerFunc {
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
			askedContinue:  strings.EqualFold(strings.TrimSpace(r.Header.Get("Expect")), "100-continue"),
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
	begun         bool
	replaced      bool
}

// begin drains what is left of the body, once, before the first byte of the answer, whose status
// is status. A read that fails - the client gone, a connection that broke - leaves the answer to be
// written as it was.
func (w *drainingWriter) begin(status int) {
	if w.begun {
		return
	}
	w.begun = true
	body := w.body
	if body.ended || body.read > w.limit || (w.askedContinue && !body.touched) {
		return
	}
	if _, err := io.CopyN(io.Discard, body, w.limit-body.read+1); err != nil || status == http.StatusUnauthorized {
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
