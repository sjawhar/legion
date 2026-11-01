package api

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"os"
	"strings"
	"time"

	"github.com/sjawhar/envoy/internal/dispatch/accesslog"
	"github.com/sjawhar/envoy/internal/dispatch/admission"
)

// An answer given before a request's body is read reaches some clients as a failed connection
// rather than as the answer (INFRA-258). An HTTP/1.1 client that sends its whole
// body before it reads the answer - WebKit and Firefox uploading a file - is still sending when
// net/http, finding the body unread, writes the answer and closes the connection; the close resets
// it, and the client drops the answer with it. So every layer's early answer - admission's 429
// OVERLOADED, the keyed layer's KEY_INVALID, the closed-issue refusal of an upload, LOGIN_CHANGED,
// a refused path parameter, a handler's refusal made before it decodes - waits until the body is
// off the connection, up to the route's own limit (apiRoute.bodyLimit) and within the bounds the
// request's body arrives under (transferBounds). A write the server has no room for reads its
// body into a spool of its own before it waits for that room (drainedBody.readWhole), so its
// 429 has nothing left to wait for; every other early answer reads what is left of the body and
// discards it, spooling none of it. The keyed layer reads a repeat's body itself when it finds
// the key held by a first request still running, and judges the repeat once that body has
// arrived (answerDrainedRepeat).

// transferBounds is what a transfer on a request's connection must keep to, its body arriving and
// its answer going out alike: no silence longer than idle, and, once it has had its grace, an
// average of floor bytes a second or better. The bound is on the silence and on the rate, never on
// the transfer's length, so a body or an answer that keeps moving is carried however long it takes
// - a 26 MiB upload over a slow uplink takes minutes - while one that stops is cut within idle of
// its last bytes and one that dribbles for ever is cut at the floor.
//
// One bound covers every layer that reads a request's body - the read before admission
// (drainedBody.readWhole), the keyed layer's spool (readKeyedBody), the handler's own decode
// (decodeJSON, an upload's multipart parse) and the drain an early answer waits behind
// (drainBeforeAnswer) - because what it bounds is the connection the body arrives on, which they
// share. The admission units are not what it bounds: a request holds none of them while its body
// arrives, having either not asked for them yet (readWhole) or given them back (drainedBody.park).
type transferBounds struct {
	idle  time.Duration
	grace time.Duration
	floor int64
}

// defaultTransferBounds is what the deployment runs under.
//
// The idle bound is the read's own deadline (defaultReadDeadline, 20 s): long enough to outlast a
// pause a client's own stack imposes - a retransmission burst, a handover between mobile cells -
// and short enough that a connection nobody is sending on, or reading from, costs about what a
// read that uses its whole deadline costs.
//
// The floor (32 KiB/s) is judged only after a minute's grace, so nothing is cut for starting
// slowly: under it a whole upload (apiRoute.bodyLimit, 26 MiB) still has a quarter of an hour, and
// a 1 Mbit/s uplink, which carries that upload in about three and a half minutes, is four times
// above it. What the floor ends is the client that holds a request open for ever by sending or
// reading just enough to push the idle bound out again.
var defaultTransferBounds = transferBounds{idle: 20 * time.Second, grace: time.Minute, floor: 32 << 10}

// orDefault is b where a test set one (server.transfer) and defaultTransferBounds everywhere else,
// so a body or an answer that stalls in a test is the test's own choice rather than twenty seconds
// of wall clock.
func (b transferBounds) orDefault() transferBounds {
	if b.idle <= 0 {
		return defaultTransferBounds
	}
	return b
}

// movingDeadline is the deadline a transfer keeps on its connection: set at its first bytes and
// pushed out at every read or write after them (arm), taken off at its end (lift). One type serves
// both directions - the read deadline a request's body arrives under (drainedBody) and the write
// deadline its answer goes out under (boundedAnswer) - differing only in which deadline set is. A
// writer with no connection of its own (a test's recorder) bounds nothing: set answers
// http.ErrNotSupported, which nothing here reads.
type movingDeadline struct {
	set     func(time.Time) error
	bounds  transferBounds
	armed   bool
	started time.Time
	// moved is how many bytes of the transfer have moved, which the floor is judged on.
	moved int64
}

// arm gives the bytes about to move until the idle bound from now to move, pushing the deadline out
// from wherever it stood, so a transfer that keeps moving is never cut and one that stops is cut
// within the idle bound of its last bytes. A transfer that has had its grace and is moving slower
// than the floor is given no time at all: the deadline is now, so the read or write about to run
// fails as the idle bound fails one. Both are a deadline on the connection, which net/http sees
// and closes the connection over, rather than an error only this layer knows.
func (d *movingDeadline) arm() {
	now := time.Now()
	if !d.armed {
		d.armed, d.started = true, now
	} else if d.tooSlow(now) {
		_ = d.set(now)
		return
	}
	_ = d.set(now.Add(d.bounds.idle))
}

// tooSlow reports whether the transfer has moved its bytes slower than the floor allows, once it
// has had its grace. A transfer with no floor (a test's) is never too slow.
func (d *movingDeadline) tooSlow(now time.Time) bool {
	elapsed := now.Sub(d.started)
	return d.bounds.floor > 0 && elapsed > d.bounds.grace &&
		d.moved*int64(time.Second) < d.bounds.floor*int64(elapsed)
}

// lift takes the bound off, which a transfer does at its end. Lifted, the connection carries no
// deadline, which is what it carries between requests on this server, whose http.Server sets
// neither ReadTimeout nor WriteTimeout (cmd/dispatch): a deadline left in place there would end
// the read net/http keeps on an idle connection, and cancel the request's context under its
// answer, or fail a later answer on the same keep-alive connection.
func (d *movingDeadline) lift() {
	if d.armed {
		_ = d.set(time.Time{})
	}
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
// reading past it, read from the limit the route's own row carries (apiRoute.BodyLimit, upload),
// so a route the table makes an upload is answered as an upload without naming it anywhere else.
func bodyTooLarge(route apiRoute) *apiError {
	if route.BodyLimit == maxUploadRequestBytes {
		return errUploadTooLarge
	}
	return errJSONTooLarge
}

// drainBeforeAnswer serves a route whose requests carry a body - every route but a GET - so that
// whatever layer answers a request, and however little of its body that layer read, the answer
// is written only once the rest of the body is read and discarded: at most the route's limit and
// one byte more, in io.Discard's buffer, and within the bound the body arrives under
// (movingDeadline.arm). A body that runs past the limit is answered as the limit answers it, 413 with
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
// tell it to send all of it. A body already read whole into its spool before the request asked
// admission for room (drainedBody.readWhole) has nothing left to drain, and the layers below
// read it from there. A request whose body failed and whose handler answered nothing is
// answered here (refuseUnreadBody).
func (s *server) drainBeforeAnswer(route apiRoute, handler http.HandlerFunc) http.HandlerFunc {
	limit, tooLarge := route.bodyLimit(), bodyTooLarge(route)
	return func(w http.ResponseWriter, r *http.Request) {
		if r.Body == nil || r.Body == http.NoBody {
			handler(w, r)
			return
		}
		control := http.NewResponseController(w)
		body := &drainedBody{
			ReadCloser:     r.Body,
			movingDeadline: movingDeadline{set: control.SetReadDeadline, bounds: s.transfer.orDefault()},
			ctx:            r.Context(),
			length:         r.ContentLength,
			limit:          limit,
			askedContinue:  expectsContinue(r),
		}
		defer body.close()
		r.Body = body
		answer := &drainingWriter{
			ResponseWriter: w,
			body:           body,
			tooLarge:       tooLarge,
		}
		handler(answer, r)
		answer.refuseUnreadBody()
	}
}

// drainedBody is a request's body: the bound every layer that reads it shares, the spool it is
// read into before its request asks admission for room, the admission units its reads park while
// it arrives, what has arrived of it, whether it ended, and how.
type drainedBody struct {
	io.ReadCloser
	movingDeadline
	// ticket is the request's admission hold, which admitted hands to the body on the request it
	// was given (the drain sits outside admission, serveRoute); nil for a request admission did
	// not count or has not admitted yet, whose body parks nothing.
	ticket *admission.Ticket
	// refuse cancels the handler's own context with the refusal that would not let the request
	// take its units back (resume), which admitted gives the body beside the ticket; nil for a
	// request admission did not count. The handler then writes nothing and commits nothing,
	// whether or not it read that refusal from the Read that ended its body, and its answer is
	// the refusal (admission.WriteUnserved).
	refuse context.CancelCauseFunc
	// ctx is the request's own context, which the units are taken back under (resume). It is
	// the one outside the handler's, so a refusal of those units does not cancel it.
	ctx context.Context
	// length is how many bytes the client said its body holds (Content-Length), or -1 where it
	// said nothing: the body has ended once that many have arrived, whether or not the layer
	// reading it goes on to read the end after them.
	length int64
	// limit is the route's own body limit (apiRoute.bodyLimit): the spool reads one byte past
	// it and no more, as every other reader of a body stops there.
	limit int64
	// askedContinue is a client that asked to be told to continue before it sends its body
	// (Expect: 100-continue). It has sent nothing, and the first read of the body is what tells
	// it to send all of it, so nothing reads such a body before a layer has had its say
	// (readWhole, drainingWriter.begin).
	askedContinue bool
	// spool is the body read whole off the connection before the request asked for room
	// (readWhole), which every layer below then reads from; held is where those bytes are kept,
	// and read how many of them the layers have taken.
	spool io.Reader
	held  *spooledBody
	read  int64
	// parked is set while the admission units are given back, touched once anything has read
	// the body, ended once it has ended, and answered once the answer has begun.
	parked   bool
	touched  bool
	ended    bool
	answered bool
	// failure is what the read that ended the body on anything but its end answered: the bound
	// it arrives under passed, its client went away, its connection broke.
	failure error
	// spoilt is the spool's own failure - a temporary file Dispatch could not write or read back
	// - which is the server's fault rather than the request's and is answered as one.
	spoilt error
	// refusal is the 429 of a body that arrived and found the server with no room to take its
	// units back; the answer carries it (drainingWriter.begin).
	refusal *admission.Refusal
}

// readWhole reads the body off the connection and into the spool, up to the route's limit and
// one byte more, before its request asks admission for a unit (admitted), and reports whether it
// arrived. The request holds no unit while it reads, and once the body is off the connection
// net/http's own read of that connection runs, so a client that hangs up while its request waits
// for room cancels the request there and then instead of being answered a 429 nobody reads at
// the end of its wait.
//
// A client that asked to be told to continue has sent nothing and is read nothing: the first
// read would tell it to send its whole body, which a layer about to refuse it never asks for
// (TestAnEarlyAnswerToAClientWaitingToContinueAsksForNoBody). A body that did not arrive is
// answered by the answer's writer (refuseUnreadBody), and nothing of the request runs.
func (b *drainedBody) readWhole() bool {
	if b.ended || b.askedContinue {
		return true
	}
	held, err := spoolBody(b, b.limit)
	b.held = held
	if err != nil {
		var spill *spillError
		if errors.As(err, &spill) {
			b.spoilt = err
		}
		return false
	}
	spool, err := held.reader()
	if err != nil {
		b.spoilt = fmt.Errorf("reread a request's body from its spool: %w", err)
		return false
	}
	b.spool = spool
	return true
}

// close gives up the spool's temporary file, if it took one, once the request is answered.
func (b *drainedBody) close() { b.held.close() }

// oversize reports whether the body runs past the route's limit, which is what the spool's one
// byte past the limit tells it.
func (b *drainedBody) oversize() bool { return b.moved > b.limit }

// park gives the request's admission units back while its body arrives, since what a client
// still sending holds is the connection and not the server's room to work: the units an admitted
// request holds are otherwise held until its handler returns (admitted), so clients trickling
// MaxWriteLimit bodies - unauthenticated, since a body is read before any handler resolves its
// caller - held every write unit and had every other write on the process refused 429 OVERLOADED.
// A body read before its request asked for room parks nothing, since the request held no unit
// while it arrived (readWhole) and has ended by the time it holds one; neither does one that has
// ended, whose request runs on the units it took back (resume).
func (b *drainedBody) park() {
	if b.parked || b.ended || b.ticket == nil {
		return
	}
	b.parked = true
	b.ticket.Park()
}

// resume takes the parked units back, now the body has ended and the request goes on to run on
// them, and answers what its reader is told. The units come back at the place the request
// arrived, ahead of every request that arrived after it, and the queue costs it one wait over
// its whole life (admission.Ticket.Resume). A request whose answer has already begun takes
// nothing back: its work is over, and the units it parked are another request's. ctx's error is
// its client having gone, which the handler it ends answers as it answers any other gone client
// (admission.WriteUnserved).
//
// A refusal is the request's answer, kept for the answer's writer to give as a 429 with the body
// already read, and the handler is cancelled under it with the refusal as its cause (refuse).
// Returning the refusal from the read that ended the body is not enough on its own: an upload's
// multipart parse reads the body through a bufio.Reader, which holds back the error that came
// with the last bytes once it has read the closing boundary from them, so the handler went on to
// store the file and commit the artifact row and only then had its 201 replaced by the 429, and a
// client that obeyed Retry-After uploaded the file twice. Cancelled, the handler's next statement
// and its commit fail whether it reads that refusal or not, so nothing of the request is written.
func (b *drainedBody) resume() error {
	if !b.parked || b.answered {
		return nil
	}
	b.parked = false
	err := b.ticket.Resume(b.ctx)
	var refusal *admission.Refusal
	if errors.As(err, &refusal) {
		b.refusal = refusal
		if b.refuse != nil {
			b.refuse(refusal)
		}
	}
	return err
}

// complete reports whether every byte the client said its body holds has arrived, which ends the
// body whether or not the layer reading it reads the end after them: a JSON decode stops at its
// value's last byte and a multipart parse at its closing boundary, and the request must have its
// units back before its handler runs on. A body sent without a Content-Length ends at its end.
func (b *drainedBody) complete() bool {
	return b.length >= 0 && b.moved >= b.length
}

// clientGone reports whether the read that failed this body failed because its client went away
// rather than because the body itself could not be read. A client that went away cancels the
// request's context, as does the bound's own deadline, since net/http cancels the request of any
// connection whose read fails (connReader.handleReadError): the two are told apart by the
// failure, the deadline's being the bound's. Every other failure - a chunked body whose framing
// net/http cannot read, a connection that broke under a client still there - leaves that context
// alone, and such a client is owed the answer its body never let the server give.
func (b *drainedBody) clientGone() bool {
	return errors.Is(b.ctx.Err(), context.Canceled) && !errors.Is(b.failure, os.ErrDeadlineExceeded)
}

func (b *drainedBody) Read(p []byte) (int, error) {
	b.touched = true
	if b.spool != nil {
		// The body is off the connection and in the spool the request read it into before it
		// asked for room (readWhole): every layer below reads it from there, and what they
		// read tells a handler's own answer to a body past the limit from an early answer the
		// limit replaces (drainingWriter.begin).
		n, err := b.spool.Read(p)
		b.read += int64(n)
		return n, err
	}
	if b.ended {
		// Past the body's end the reads are net/http's own and the layer that reads the end
		// after the last byte the client promised: the bound is off a body that ended and
		// stands where one failed (end), and arming it again would leave a deadline on a
		// connection with no body left to arrive on it.
		return b.ReadCloser.Read(p)
	}
	b.park()
	b.arm()
	n, err := b.ReadCloser.Read(p)
	b.moved += int64(n)
	if err == nil && !b.complete() {
		return n, nil
	}
	return n, b.end(err)
}

// end records the body's end - err, or nil where the last byte it promised has arrived - and
// answers what its reader sees. A body that ended takes its parked units back, and a request the
// server has no room for is told so there, so no handler runs on units nobody holds. A body that
// failed keeps its bound, since net/http reads on after the answer itself: it discards up to
// 256 KiB more of a body whose Content-Length leaves that little unread before it writes the
// answer's header, and again when it closes the body once the handler returns. With the deadline
// those reads end where the request's own did and the connection closes; lifted, a client that
// stopped sending would hold the connection open after its answer, and withhold the answer itself
// where its body ran past the route's limit.
func (b *drainedBody) end(err error) error {
	b.ended = true
	if err != nil && !errors.Is(err, io.EOF) {
		b.failure = err
		return err
	}
	b.lift()
	if refused := b.resume(); refused != nil {
		return refused
	}
	return err
}

// spooledBodyInMemory is how much of a spooled body is held in memory; the rest of an upload's
// body waits in a temporary file, as the multipart parser's own spill does, so a 25 MiB upload
// costs no more memory than the megabyte a JSON body costs.
const spooledBodyInMemory = 1 << 20

// spooledBody is a request's body read whole before the work that needs it runs: the room a
// write waits for (drainedBody.readWhole) and the digest a keyed write is judged by
// (readKeyedBody). Its first spooledBodyInMemory bytes are in memory, and the rest, if any, in a
// temporary file.
type spooledBody struct {
	head  []byte
	spill *os.File
	size  int64
}

// spillError is a spool's temporary file failing: Dispatch's own fault (a full disk, a temporary
// directory it cannot write), answered as a 5xx, where a body the client did not finish sending
// is the request's and answered 400.
type spillError struct{ err error }

func (e *spillError) Error() string {
	return "spool a request's body to a temporary file: " + e.err.Error()
}

func (e *spillError) Unwrap() error { return e.err }

// spillWriter is a spooled body's temporary file, whose write failures are spillErrors.
type spillWriter struct{ file *os.File }

func (w spillWriter) Write(p []byte) (int, error) {
	n, err := w.file.Write(p)
	if err != nil {
		return n, &spillError{err: err}
	}
	return n, nil
}

// spoolBody reads body into a spooledBody, at most one byte past limit, the route's own
// (apiRoute.bodyLimit), so what it holds says whether the body runs past that limit. A body that
// fits in spooledBodyInMemory never touches the disk: a route whose limit is no more than that
// holds even its one byte past the limit in memory.
func spoolBody(body io.Reader, limit int64) (*spooledBody, error) {
	read := &spooledBody{}
	source := io.LimitReader(body, limit+1)
	inMemory := int64(spooledBodyInMemory)
	if limit <= inMemory {
		inMemory = limit + 1
	}
	var head bytes.Buffer
	n, err := io.CopyN(&head, source, inMemory)
	read.head, read.size = head.Bytes(), n
	switch {
	case err != nil && !errors.Is(err, io.EOF):
		return read, err
	case err == nil && n <= limit:
		// The head is full and the limit not reached: the rest waits in a temporary file.
		spill, err := os.CreateTemp("", "dispatch-request-body-*")
		if err != nil {
			return read, &spillError{err: err}
		}
		read.spill = spill
		rest, err := io.Copy(spillWriter{file: spill}, source)
		read.size += rest
		if err != nil {
			return read, err
		}
	}
	return read, nil
}

// reader is the body again, from its first byte.
func (b *spooledBody) reader() (io.Reader, error) {
	if b.spill == nil {
		return bytes.NewReader(b.head), nil
	}
	if _, err := b.spill.Seek(0, io.SeekStart); err != nil {
		return nil, err
	}
	return io.MultiReader(bytes.NewReader(b.head), b.spill), nil
}

// close removes the body's temporary file, if it has one.
func (b *spooledBody) close() {
	if b == nil || b.spill == nil {
		return
	}
	name := b.spill.Name()
	_ = b.spill.Close()
	_ = os.Remove(name)
}

// drainingWriter is the answer's writer: its first header, write or flush drains the body first
// (drainBeforeAnswer).
type drainingWriter struct {
	http.ResponseWriter
	body     *drainedBody
	tooLarge *apiError
	begun    bool
	replaced bool
}

// begin drains what is left of the body, once, before the first byte of the answer, whose status
// is status. The drain reads under the bound the body arrives under, whatever is left of it
// (movingDeadline.arm), and parks the request's admission units while it does, as every other
// read of the body does: a read that fails - the bound passed, the client gone, a connection that
// broke - leaves the answer to be written as it was, and the connection to close after it, the
// rest of the body unread. Only a body that ended lifts that bound (movingDeadline.lift); one
// that failed or ran past the limit keeps it for net/http's own reads after the answer.
//
// A body already read whole off the connection before its request asked for room
// (drainedBody.readWhole) has nothing left to drain. Where it ran past the route's limit and no
// layer below read it, the limit answers it here as it answers a body the drain itself finds
// past the limit: the layer that answered never saw the body, and no such request is taken
// whoever answers it. Where a layer did read it, its own answer to a body past the limit - a
// handler's 413 - stands as it does for a body read off the connection.
//
// A body that arrived and found the server with no room to take its units back is answered the
// 429 that refused it, in place of whatever the layer above made of that refusal: the body is
// off the connection and the client may send the whole request again after Retry-After. Nothing
// of the request ran, because the refusal cancelled the handler as well (drainedBody.resume),
// and whatever that handler then tried to write - the refusal itself through
// admission.WriteUnserved, or an answer to the error its own reader gave it - is this one 429.
func (w *drainingWriter) begin(status int) {
	if w.begun {
		return
	}
	w.begun = true
	body := w.body
	body.answered = true
	if body.refusal != nil {
		clear(w.ResponseWriter.Header())
		admission.WriteRefused(w.ResponseWriter, body.refusal)
		w.replaced = true
		return
	}
	if body.spool != nil {
		if body.oversize() && body.read == 0 && status != http.StatusUnauthorized {
			w.answerTooLarge()
		}
		return
	}
	if body.ended || body.oversize() || (body.askedContinue && !body.touched) {
		return
	}
	_, err := io.CopyN(io.Discard, body, body.limit-body.moved+1)
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
	w.answerTooLarge()
}

// answerTooLarge replaces the answer a layer gave with the route's own answer to a body past its
// limit, 413 and none of that layer's headers.
func (w *drainingWriter) answerTooLarge() {
	clear(w.ResponseWriter.Header())
	writeError(w.ResponseWriter, w.tooLarge.code, w.tooLarge.status, w.tooLarge.message)
	w.replaced = true
}

// refuseUnreadBody answers a request whose body did not arrive and whose handler wrote nothing,
// which is what a handler does when the failed read cancelled the request's context under it,
// and what the request does when its body did not arrive for the read that comes before
// admission (drainedBody.readWhole): net/http reads a handler that wrote nothing as a 200 with
// an empty body, and a client that trickled its body would read that as its write having been
// taken. A handler that answered, however early, answered for itself (begin).
//
// A body the bound cut off, and one whose framing could not be read, are refused as the keyed
// layer refuses a body it could not read, and the connection closes after the answer, the rest
// of the body unread: that client is still there to read the refusal. A client that went away is
// written nothing, since nobody is left to read it, and its line carries 499 rather than the 200
// of a handler that wrote nothing. A body that arrived and found no room to take its units back
// is the 429 that refused it. A spool Dispatch could not write is none of these: the request was
// good, the server was not, so it is the 500 a server fault always is.
func (w *drainingWriter) refuseUnreadBody() {
	body := w.body
	if w.begun || (body.failure == nil && body.refusal == nil && body.spoilt == nil) {
		return
	}
	w.begun = true
	switch {
	case body.refusal != nil:
		admission.WriteRefused(w.ResponseWriter, body.refusal)
	case body.spoilt != nil:
		slog.Error("dispatch: API could not spool a request's body", "error", body.spoilt)
		writeError(w.ResponseWriter, codeInternal, http.StatusInternalServerError, "internal server error")
	case body.clientGone():
		accesslog.ClientGone(body.ctx)
	default:
		w.ResponseWriter.Header().Set("Connection", "close")
		writeError(w.ResponseWriter, "REQUEST_BODY_UNREADABLE", http.StatusBadRequest,
			"the request's body did not arrive: nothing was run")
	}
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

// boundedAnswer is a read's answer written under bounds of its own: its first byte arms a write
// deadline, pushed out at every byte after it and lifted when the handler returns
// (movingDeadline), past which a Write fails. A handler whose answer is larger than the sockets
// between it and its client buffer blocks in Write until the client reads, and a pinned read
// blocked there holds its request-pool connection - and the admission unit around it - for as
// long as that client cares to take. Past the bound the write fails, the handler returns, the pin
// and the unit go back and net/http closes the connection.
//
// The bound is the silence between the answer's bytes and the rate they keep, not the answer's
// length, as it is for the body a request arrives with (drainedBody): a client reading a large
// answer over a slow downlink is answered all of it however long that takes, where the read's own
// deadline cut it off at twenty seconds, and one that stops reading is cut within the idle bound.
// Only a read is wrapped (serveRoute), and the websockets are mounted outside it, so no
// long-lived connection is bounded here.
type boundedAnswer struct {
	http.ResponseWriter
	movingDeadline
	// control is the connection's own controller, which set writes the deadline through and
	// Flush flushes through.
	control *http.ResponseController
}

// answerSlice is how much of one Write goes to net/http at a time. The bound is armed before
// each slice and the slice counted after it, so an answer written in one call - WriteJSON
// encodes the whole answer in one Write, and a version held in its row is served in one
// (serveFileVersion) - is bounded as one written in many: the idle bound is on the silence
// between its slices rather than on the whole call, and the floor on the rate they keep. Handed
// to net/http whole, a 25 MiB answer had to leave the server inside one idle bound, which cut
// off every client below about 1.3 MB/s, where the rule is twenty seconds of silence and
// 32 KiB/s after the first minute.
const answerSlice = 64 << 10

func (a *boundedAnswer) WriteHeader(status int) {
	a.arm()
	a.ResponseWriter.WriteHeader(status)
}

func (a *boundedAnswer) Write(p []byte) (int, error) {
	written := 0
	for {
		slice := p[written:]
		if len(slice) > answerSlice {
			slice = slice[:answerSlice]
		}
		a.arm()
		n, err := a.ResponseWriter.Write(slice)
		written += n
		a.moved += int64(n)
		if err != nil || written >= len(p) {
			return written, err
		}
	}
}

func (a *boundedAnswer) Flush() {
	a.arm()
	_ = a.control.Flush()
}

// Unwrap is the connection's own writer, for http.ResponseController.
func (a *boundedAnswer) Unwrap() http.ResponseWriter {
	return a.ResponseWriter
}
