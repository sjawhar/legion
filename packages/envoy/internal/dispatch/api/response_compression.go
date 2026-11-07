package api

import (
	"bufio"
	"compress/gzip"
	"io"
	"net"
	"net/http"
	"strconv"
	"strings"
	"sync"
)

// Dispatch's answers are JSON a page reads whole - an issue's comment list, its event log, a
// document's blocks - and they compress by a factor of ten or more. Under `Dispatch-Trees: 1` the
// dashboard is sent the display tree of every stored text beside the markdown itself
// (display_trees.go), which makes the same answer several times larger, so the bytes a browser
// waits for grow with the trees it asked for. compressAnswers gzips those answers for the callers
// that say they take gzip, and changes nothing else about them: same status, same JSON, same
// headers but Content-Encoding and Vary.
//
// gzip alone: every browser and every Go client takes it, and brotli would cost a dependency and
// a second encoder for an answer nothing holds in a cache.

const (
	// compressedEncoding is the only encoding this layer writes.
	compressedEncoding = "gzip"
	// compressibleMinimum is the smallest answer worth compressing. A gzip stream costs 18 bytes
	// of header and trailer and a compressor's whole state, and an answer under a kilobyte already
	// travels in the one segment it would travel in compressed, so nothing below this is
	// compressed: the keyed layer's refusals, a 404, an ask's answer, the per-request writes a
	// page makes while a person types. The comment lists and event logs this exists for are two
	// orders of magnitude above it.
	compressibleMinimum = 1024
)

// compressAnswers serves one route so that its answer is gzipped when the request says the caller
// takes gzip, the answer is Dispatch's own JSON, and it is at least compressibleMinimum bytes
// long. It is mounted around every row of the route table (register), inside the access log, whose
// line then records the bytes that went on the wire.
//
// Every answer carries `Vary: Accept-Encoding` whether or not it was compressed, beside the
// `Vary: Dispatch-Trees` the same loop adds (varyOnDisplayTrees), so no cache between a browser
// and Dispatch hands a caller that takes neither gzip nor trees an answer written for one that
// takes both.
func compressAnswers(handler http.HandlerFunc) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		w.Header().Add("Vary", "Accept-Encoding")
		if !acceptsGzip(r.Header.Get("Accept-Encoding")) {
			// A caller that sends no Accept-Encoding, or none that names gzip, is written the
			// bytes it was written before this existed, through its own writer.
			handler(w, r)
			return
		}
		answer := &compressingWriter{ResponseWriter: w}
		handler(answer, r)
		// Only a handler that returned has an answer to end: one that panics (an artifact
		// download whose store fails mid-stream aborts with http.ErrAbortHandler) leaves its
		// connection to be dropped rather than its half-written body to be closed as if whole.
		answer.finish()
	}
}

// acceptsGzip reads an Accept-Encoding header and reports whether the caller takes gzip: `gzip`
// named with any weight above zero, or `*` with one where gzip is not named. `gzip;q=0` is a
// refusal and outranks a `*` beside it (RFC 9110 §12.5.3).
func acceptsGzip(header string) bool {
	if header == "" {
		return false
	}
	wildcard := false
	for rest := header; rest != ""; {
		var field string
		field, rest, _ = strings.Cut(rest, ",")
		name, parameters, _ := strings.Cut(field, ";")
		name = strings.TrimSpace(name)
		switch {
		case strings.EqualFold(name, compressedEncoding):
			return !refusedWeight(parameters)
		case name == "*":
			wildcard = !refusedWeight(parameters)
		}
	}
	return wildcard
}

// refusedWeight reports whether the parameters of one Accept-Encoding field carry `q=0`, which is
// how a caller names an encoding to refuse it. A weight this cannot read is no refusal.
func refusedWeight(parameters string) bool {
	for rest := parameters; rest != ""; {
		var parameter string
		parameter, rest, _ = strings.Cut(rest, ";")
		name, value, found := strings.Cut(parameter, "=")
		if !found || !strings.EqualFold(strings.TrimSpace(name), "q") {
			continue
		}
		weight, err := strconv.ParseFloat(strings.TrimSpace(value), 64)
		return err == nil && weight <= 0
	}
	return false
}

// compressibleAnswer reports whether an answer's own headers say its body is Dispatch's JSON, the
// only thing this layer compresses. Every answer Dispatch composes is written through WriteJSON,
// which sets `application/json`; every other media type an API route writes is an upload's own
// bytes, served under the type they were stored as (serveFileVersion) - an image, a PDF, an
// archive, each already compressed or none of this layer's business - so the media type alone
// keeps the downloads out without a list of routes to keep up to date. An answer that already
// carries an encoding is left as the handler wrote it.
func compressibleAnswer(header http.Header) bool {
	if header.Get("Content-Encoding") != "" {
		return false
	}
	mediaType, _, _ := strings.Cut(header.Get("Content-Type"), ";")
	mediaType = strings.TrimSpace(mediaType)
	return strings.EqualFold(mediaType, "application/json") ||
		(len(mediaType) > len("+json") && strings.EqualFold(mediaType[len(mediaType)-len("+json"):], "+json"))
}

// gzipWriters keeps the compressors between answers: a gzip.Writer carries deflate's whole window
// and hash tables, which is hundreds of kilobytes an answer this layer would otherwise allocate
// and drop. A pooled writer keeps the level it was made at, so every writer in here is
// gzip.BestSpeed's.
var gzipWriters sync.Pool

// compressingWriter is the answer's writer while the choice between gzip and the bytes as they
// are is still open. It holds the answer's first compressibleMinimum bytes rather than deciding
// from a Content-Length no JSON answer sets: once that much is held the answer is compressed, and
// an answer that ends below it goes out as it was written, header and all. Nothing but the body
// waits - the status and every header the handler set are written the moment the choice is made.
type compressingWriter struct {
	http.ResponseWriter
	// status is the status the handler gave, and 0 until it gives one.
	status int
	// held is the answer's first bytes, kept until they reach compressibleMinimum or the answer
	// ends.
	held []byte
	// gzip is the compressor, once the answer is being compressed, and nil otherwise.
	gzip *gzip.Writer
	// settled is true once the choice is made: compressing through gzip, or passing every byte
	// straight through.
	settled bool
	// err is the first error a write to the compressor gave, which the Write that caused it and
	// every Write after it answer.
	err error
}

// settle makes the choice, writes the status and the headers under it, and passes on whatever is
// held. Compressing drops the Content-Length the handler may have set, since the length it names
// is the length of bytes that are no longer what goes out, and chunks the answer instead.
func (w *compressingWriter) settle(compress bool) {
	w.settled = true
	if compress {
		w.Header().Del("Content-Length")
		w.Header().Set("Content-Encoding", compressedEncoding)
		if pooled, ok := gzipWriters.Get().(*gzip.Writer); ok {
			pooled.Reset(w.ResponseWriter)
			w.gzip = pooled
		} else {
			// gzip.BestSpeed, not the default 6: a warm read is dominated by encoding and
			// compressing the answer, and the trees this layer now carries make the body
			// larger. On the 103 KB comment list of BenchmarkCompressingAnAnswer, level 1
			// takes 284us and sends 7,698 bytes where level 6 takes 861us and sends 6,882 -
			// a third of the time for a ninth more bytes, and the bytes are the side of the
			// trade with room to give. The level is a constant, so NewWriterLevel cannot
			// refuse it.
			w.gzip, _ = gzip.NewWriterLevel(w.ResponseWriter, gzip.BestSpeed)
		}
	}
	status := w.status
	if status == 0 {
		status = http.StatusOK
	}
	w.ResponseWriter.WriteHeader(status)
	if len(w.held) > 0 {
		held := w.held
		w.held = nil
		w.write(held)
	}
}

// write passes bytes on to the compressor or to the connection, whichever the answer settled on.
func (w *compressingWriter) write(body []byte) (int, error) {
	if w.err != nil {
		return 0, w.err
	}
	if w.gzip == nil {
		return w.ResponseWriter.Write(body)
	}
	written, err := w.gzip.Write(body)
	if err != nil {
		w.err = err
	}
	return written, err
}

func (w *compressingWriter) WriteHeader(status int) {
	if w.settled || status < http.StatusOK {
		// An informational response (100 Continue, 103 Early Hints) is not the answer and is
		// never this layer's to hold; a status after the answer's own is net/http's to complain
		// about, as it would without this layer.
		w.ResponseWriter.WriteHeader(status)
		return
	}
	if w.status == 0 {
		w.status = status
	}
	if !compressibleAnswer(w.Header()) {
		w.settle(false)
	}
}

// Write holds the answer's first bytes until they are worth compressing. A body that arrives in
// one piece, as json.Encoder writes it, is never copied into the held buffer at all.
func (w *compressingWriter) Write(body []byte) (int, error) {
	if w.settled {
		return w.write(body)
	}
	if !compressibleAnswer(w.Header()) {
		w.settle(false)
		return w.write(body)
	}
	if len(w.held)+len(body) >= compressibleMinimum {
		if len(w.held) == 0 {
			w.settle(true)
			return w.write(body)
		}
		w.held = append(w.held, body...)
		w.settle(true)
		return len(body), w.err
	}
	if w.held == nil {
		w.held = make([]byte, 0, compressibleMinimum)
	}
	w.held = append(w.held, body...)
	return len(body), nil
}

// ReadFrom keeps the server's own copy (sendfile, and its buffer reuse) for an answer that is not
// compressed, so an artifact download still streams as it did (accesslog.recorder.ReadFrom).
func (w *compressingWriter) ReadFrom(body io.Reader) (int64, error) {
	if !w.settled && compressibleAnswer(w.Header()) {
		// A compressible body streamed rather than written whole still passes the held buffer,
		// so it is judged by the same threshold: io.Copy through Write, without handing it this
		// ReadFrom again.
		return io.Copy(writeOnly{w}, body)
	}
	if !w.settled {
		w.settle(false)
	}
	if w.gzip != nil {
		return io.Copy(writeOnly{w.gzip}, body)
	}
	if from, ok := w.ResponseWriter.(io.ReaderFrom); ok {
		return from.ReadFrom(body)
	}
	return io.Copy(writeOnly{w.ResponseWriter}, body)
}

// writeOnly hides every method of its writer but Write.
type writeOnly struct{ io.Writer }

// Flush sends what the answer has so far, which makes the choice if it is still open: a handler
// that flushes has bytes it wants on the wire now, and below the threshold they go as they are.
func (w *compressingWriter) Flush() {
	if !w.settled {
		w.settle(compressibleAnswer(w.Header()) && len(w.held) >= compressibleMinimum)
	}
	if w.gzip != nil && w.err == nil {
		if err := w.gzip.Flush(); err != nil {
			w.err = err
		}
	}
	_ = http.NewResponseController(w.ResponseWriter).Flush()
}

// Hijack hands over the connection for a handler that writes its own answer on it, the websocket
// upgrade. Nothing of such an answer is compressed, and whatever the handler held before taking
// the connection goes out as it was written.
func (w *compressingWriter) Hijack() (net.Conn, *bufio.ReadWriter, error) {
	hijacker, ok := w.ResponseWriter.(http.Hijacker)
	if !ok {
		return nil, nil, http.ErrNotSupported
	}
	if !w.settled && (w.status != 0 || len(w.held) > 0) {
		w.settle(false)
	}
	// Past the upgrade the connection's bytes are the handler's frames, not an answer's body.
	w.settled = true
	return hijacker.Hijack()
}

// Unwrap gives http.ResponseController the server's own writer, so a handler that moves its
// connection's deadlines reaches it through this wrapper (accesslog.recorder.Unwrap).
func (w *compressingWriter) Unwrap() http.ResponseWriter { return w.ResponseWriter }

// finish ends the answer: a compressed one is closed, so its trailer goes out and its compressor
// returns to the pool, and one still held is written as it stands.
func (w *compressingWriter) finish() {
	if w.gzip != nil {
		if err := w.gzip.Close(); err != nil && w.err == nil {
			w.err = err
		}
		w.gzip.Reset(io.Discard)
		gzipWriters.Put(w.gzip)
		w.gzip = nil
		return
	}
	if !w.settled {
		w.settle(false)
	}
}
