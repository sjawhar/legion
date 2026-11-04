package api

import (
	"bytes"
	"cmp"
	"context"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"os"
	"strconv"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/sjawhar/envoy/internal/dispatch/admission"
	"github.com/sjawhar/envoy/internal/dispatch/files"
)

// The file store's two touchpoints on the artifact routes: an upload's object write before the
// transaction (putUploadedFile) and a version's download (serveFileVersion). storeArtifact and
// getArtifactVersion (artifacts.go) call them; the rows they read and write are theirs.

// putUploadedFile writes an upload's bytes to the file store under sha before storeArtifact opens
// its transaction, and answers the request itself when the upload is refused or the store refuses
// it. With no store, or for a document, it writes nothing. It reports whether the upload may go on.
//
// The object goes first: the bytes (up to maxArtifactBlobSize) cross the network to a store whose
// wait nothing here bounds, and the transaction takes a pooled connection and the owner's row
// lock, which every other writer of that issue queues behind. So the refusals that transaction
// makes are made first, from the pool and locking nothing (refuseUploadBeforeItsObject; an issue
// that is closed or missing is refused before the body is read, refuseUploadToAClosedIssue):
// nothing deletes an object, and one a refused upload left would be named by no row. Deleting it
// on refusal instead is not safe, since the store is keyed by the bytes' hash and another version,
// or another upload in flight, can name the same object. The transaction's own checks still
// decide: an upload this check passes that a concurrent write then makes refusable (another
// upload taking its name with another kind, the issue closing), or whose insert fails, leaves an
// object no row names, which a later upload of the same bytes reuses. The row records the hash
// the object is keyed by and no bytes. A write the request's own context cut off - the refusal
// of the units its body asked back, or a client that went away - is the request's, not the
// store's, and is answered as admission.WriteUnserved answers it, with no ERROR line.
func (s *server) putUploadedFile(w http.ResponseWriter, r *http.Request, kind, sha string, input artifactUploadInput, target artifactTarget) bool {
	if kind == "doc" || s.deps.Files == nil {
		return true
	}
	if err := s.refuseUploadBeforeItsObject(r.Context(), target, input.name, kind); err != nil {
		s.writeHandlerError(w, r, err)
		return false
	}
	if err := s.deps.Files.Put(r.Context(), sha, input.contentType, input.content); err != nil {
		if admission.WriteUnserved(w, r.Context(), err) {
			return false
		}
		slog.Error("dispatch: store an uploaded file", "sha256", sha, "size", len(input.content), "error", err)
		writeError(w, "FILE_STORE_UNAVAILABLE", http.StatusBadGateway, "the file store did not accept the upload")
		return false
	}
	return true
}

// refuseUploadBeforeItsObject answers, from the pool and locking nothing, the refusal
// storeArtifact's transaction would make of a file or image upload to target under name: a
// project that does not exist, an artifact the name or slug finds whose kind is not kind, or a new
// artifact whose name or reference key is past its bound. It reads as that transaction reads, so
// the two refuse alike whenever nothing writes the owner's artifacts between them.
func (s *server) refuseUploadBeforeItsObject(ctx context.Context, target artifactTarget, name, kind string) error {
	pool := s.deps.Store.Pool
	if target.IssueKey == nil && target.Session == "" {
		if _, err := s.loadProject(ctx, pool, target.Project); err != nil {
			return err
		}
	}
	artifact, err := lookupArtifactByNameOrSlug(ctx, pool, target, name)
	if err == nil {
		if artifact.Kind != kind {
			return artifactKindMismatch(name, artifact, kind)
		}
		return nil
	}
	if !errors.Is(err, pgx.ErrNoRows) {
		return err
	}
	if refusal := newArtifactNameRefusal(name); refusal != nil {
		return refusal
	}
	slug, err := s.nextArtifactSlug(ctx, pool, target, name)
	if err != nil {
		return err
	}
	return newArtifactKeyRefusal(target, slug)
}

// serveFileVersion answers a file version's download from what its row holds: a row holding
// bytes is served from the row, whatever store is configured; a row the backfill cleared, or that
// an upload wrote with a store, names its object by hash and is streamed from the store.
//
// The headers wait for the object to open, so a store that fails answers 502 and never a 200 cut
// short; a body that fails after that, or does not hash to the row's hash, aborts the response
// mid-stream rather than ending it as if whole.
//
// The read's own deadline (downloadDeadline) stops at those headers, so what bounds everything
// after them is the transfer bounds (transferBounds), each end of the copy under its own. The
// answer's writes are armed before each of them (boundedAnswer), so a reader that stops, or
// falls below the floor, has its write fail with os.ErrDeadlineExceeded. The store's reads are
// armed with the same idle bound and judged by the same floor over the time they themselves
// take (storeBody), so a store that stops sending mid-object, or dribbles below the floor, ends
// the read rather than holding this handler, its goroutine, its admission unit and the store's
// connection until the client hangs up. The time those reads take is left out of the answer's
// floor (leaveOutOfAnswerFloor), since waiting on the store is no doing of the reader's: judged
// together, every download that took longer than the grace at a slow store's pace was cut and
// its reader blamed for it. A reader that keeps to the bound is sent the whole file however
// long that takes.
//
// The ways a copy ends badly are told apart and logged once each: a write the bound failed is
// the reader's doing, INFO; a store that stopped sending, and one below the floor, are the
// store's, ERROR; a deadline that passed before the answer began, INFO; any other failure of
// the store's read, ERROR. Each aborts the connection, so the client sees a cut connection
// rather than a clean 200 short of its Content-Length and the access line marks the request
// aborted. A client that went away is answered nothing, having nobody left to read it, and a
// write that failed any other way is such a client: net/http cancels the request of a
// connection whose read fails.
//
// A version's bytes never change, so a browser keeps them for a year without asking again: the
// pictures a conversation shows inline are fetched once. `private`, since the route is
// authenticated and no shared cache may hold them. A document version is not served here and
// keeps its headers.
func (s *server) serveFileVersion(w http.ResponseWriter, r *http.Request, artifactID string, number int, content []byte, contentType, sha *string, size *int64) {
	headers := func(length int64) {
		w.Header().Set("Content-Type", cmp.Or(deref(contentType), "application/octet-stream"))
		if sha != nil {
			w.Header().Set("ETag", *sha)
		}
		w.Header().Set("Cache-Control", "private, max-age=31536000, immutable")
		w.Header().Set("Content-Disposition", "attachment")
		w.Header().Set("X-Content-Type-Options", "nosniff")
		w.Header().Set("Content-Length", strconv.FormatInt(length, 10))
		w.WriteHeader(http.StatusOK)
	}
	body, length := io.Reader(bytes.NewReader(content)), int64(len(content))
	// A row-held version is copied from its bytes; the store's object is read under a context
	// of its own, which the store's own bounds end (storeBody) without cancelling the
	// request's. stored is that body, which the cut lines read the store's pace from, and nil
	// for a row-held version.
	reads, stored := r.Context(), (*storeBody)(nil)
	if content == nil {
		if s.deps.Files == nil {
			slog.Error("dispatch: a file version holds no bytes and no file store is configured", "artifact", artifactID, "version", number, "sha256", deref(sha))
			writeError(w, "FILE_STORE_UNAVAILABLE", http.StatusServiceUnavailable, "this file's bytes are in a store this server is not configured to read")
			return
		}
		if sha == nil {
			slog.Error("dispatch: a file version holds no bytes and records no hash to read them by", "artifact", artifactID, "version", number)
			writeError(w, "FILE_MISSING", http.StatusInternalServerError, "this file's bytes cannot be found")
			return
		}
		var stalled context.CancelCauseFunc
		reads, stalled = context.WithCancelCause(r.Context())
		defer stalled(nil)
		object, err := s.deps.Files.Get(reads, *sha)
		if err != nil {
			// A deadline that passed inside the open, or a client that left under it, is the
			// request's own end and not the store's: 503 REQUEST_TIMEOUT, or nothing at all,
			// with no ERROR line, as an upload's object write answers one (putUploadedFile).
			if admission.WriteUnserved(w, r.Context(), err) {
				return
			}
			if errors.Is(err, files.ErrNotFound) {
				// The row names an object the bucket does not hold: not an outage a retry
				// mends, a restore from the bucket's versioning.
				slog.Error("dispatch: a file version's object is missing from the file store", "artifact", artifactID, "version", number, "sha256", *sha, "error", err)
				writeError(w, "FILE_MISSING", http.StatusInternalServerError, "this file's bytes cannot be found")
				return
			}
			slog.Error("dispatch: read a file version from the file store", "artifact", artifactID, "version", number, "sha256", *sha, "error", err)
			writeError(w, "FILE_STORE_UNAVAILABLE", http.StatusBadGateway, "the file store did not return this file")
			return
		}
		defer object.Body.Close()
		// The row's recorded size is the file's length; an object of another length is not the
		// file, however it got there, and is refused before a header is written. The body's
		// reader checks the hash as the bytes go and holds the last of them back until it has,
		// so a wrong body leaves the client short of Content-Length rather than whole.
		if size != nil && object.Size != *size {
			slog.Error("dispatch: a file version's object is not the size the row records", "artifact", artifactID, "version", number, "sha256", *sha, "row_size", *size, "object_size", object.Size)
			writeError(w, "FILE_MISSING", http.StatusInternalServerError, "this file's bytes cannot be found")
			return
		}
		stored = newStoreBody(object.Body, s.transfer.orDefault(), stalled)
		// The answer's floor judges the reader, so the time these reads spend waiting on the
		// store is left out of it; the store's pace is judged by the floor stored carries.
		leaveOutOfAnswerFloor(r.Context(), func() time.Duration { return stored.reading })
		body, length = stored, object.Size
	}
	headers(length)
	answer := &downloadWriter{ResponseWriter: w}
	written, err := io.Copy(answer, body)
	if err == nil {
		return
	}
	switch {
	case errors.Is(answer.failure, os.ErrDeadlineExceeded):
		// The reader stopped reading, or fell below the floor, and the bytes it was sent are
		// short of the Content-Length it was promised. INFO, since it is the reader's doing and
		// no fault of the server's, and once per download, the write that failed being the last
		// one made.
		slog.Info("dispatch: a file version's download was cut: its reader stopped or fell below the floor",
			"artifact", artifactID, "version", number, "sha256", deref(sha), "written", written, "size", length)
	case errors.Is(context.Cause(reads), errStoreStalled):
		slog.Error("dispatch: a file version's download was cut: the file store stopped sending",
			"artifact", artifactID, "version", number, "sha256", deref(sha), "written", written, "size", length,
			"error", context.Cause(reads))
	case errors.Is(context.Cause(reads), errStoreTooSlow):
		// The store sent below the floor over the time these reads took, which is the store's
		// doing and not the reader's: the answer's own floor leaves that time out.
		slog.Error("dispatch: a file version's download was cut: the file store sent below the floor",
			"artifact", artifactID, "version", number, "sha256", deref(sha), "written", written, "size", length,
			"rate", stored.rate(), "error", context.Cause(reads))
	case admission.DeadlinePassed(r.Context()):
		// The deadline fired before the headers went out (servedAsRead stops it there), so
		// the work before the answer - the row's read, the store's open - took the five
		// minutes downloadDeadline allows it.
		slog.Info("dispatch: a file version's download ran past its deadline before its answer began",
			"artifact", artifactID, "version", number, "sha256", deref(sha), "written", written, "size", length)
	case r.Context().Err() != nil:
		return // The client went away; nothing to tell it.
	default:
		slog.Error("dispatch: stream a file version from the file store", "artifact", artifactID, "version", number, "sha256", deref(sha), "error", err)
	}
	panic(http.ErrAbortHandler)
}

// errStoreStalled is the cause a download's read context carries when one read of the file
// store's body went the whole idle bound without finishing, and errStoreTooSlow the cause it
// carries when the store's bytes over the time those reads took fell below the floor
// (storeBody). Both are the store's fault rather than the reader's and are logged at ERROR,
// where a reader that stopped or fell below the floor is logged at INFO.
var (
	errStoreStalled = errors.New("the file store did not finish a read")
	errStoreTooSlow = errors.New("the file store sent below the floor")
)

// storeBody is a store object's body read under bounds of its own, the transfer bounds the
// answer's writes are armed with (transferBounds) measured over the time these reads take
// rather than over the download's length. Each Read arms a timer at the idle bound that ends
// the context the object was opened under, with errStoreStalled as the cause, and stops it once
// the read answers; a store whose bytes over that reading time fall below the floor, once the
// reading time is past the grace, ends that context with errStoreTooSlow. A store that stops
// sending mid-object, or dribbles, therefore fails this read rather than leaving the handler
// blocked in it until its client hangs up, which held the handler's goroutine, its admission
// unit and the store's own connection: enough patient clients during a bucket incident filled
// the read class and every read was then refused 429 OVERLOADED. Bounds with no floor (a
// test's) cut nothing on the rate, as they cut nothing on the answer's.
//
// The idle bound is per read of this body rather than per byte the store sends: the body is the
// store's files.VerifyingReader, whose Read keeps reading until it holds more than the caller
// asked for (files/store.go), so one read of io.Copy's 32 KiB buffer waits for about 32 KiB of
// new bytes and a store below about 1.6 KB/s fails it without ever going silent. What the floor
// is for is the store that stays above that and still cannot finish the file.
//
// One timer serves every read of the object, rearmed before each and stopped after, so a 25 MiB
// file costs one timer rather than one per 32 KiB read. A cancellation is final, so a timer
// that fires in the microseconds between its read answering and the stop ends a copy whose next
// read was about to be armed again.
//
// Nothing here is locked: the reads and the floor's judgment are the one goroutine's that
// io.Copy runs the download on, and the timer's goroutine only cancels the context.
type storeBody struct {
	body   io.Reader
	bounds transferBounds
	cancel context.CancelCauseFunc
	stall  *time.Timer
	// read is how many bytes these reads have taken from the store and reading how long they
	// spent waiting for them: the store's own pace, which its floor judges and which the
	// answer's floor leaves out of the reader's (leaveOutOfAnswerFloor).
	read    int64
	reading time.Duration
}

// newStoreBody reads body under bounds, ending the object's context with cancel when one read
// goes the idle bound without finishing or the store's pace falls below the floor.
func newStoreBody(body io.Reader, bounds transferBounds, cancel context.CancelCauseFunc) *storeBody {
	bounded := &storeBody{body: body, bounds: bounds, cancel: cancel}
	bounded.stall = time.AfterFunc(bounds.idle, func() {
		cancel(fmt.Errorf("%w within %v", errStoreStalled, bounds.idle))
	})
	bounded.stall.Stop()
	return bounded
}

func (b *storeBody) Read(p []byte) (int, error) {
	b.stall.Reset(b.bounds.idle)
	began := time.Now()
	n, err := b.body.Read(p)
	b.stall.Stop()
	b.reading += time.Since(began)
	b.read += int64(n)
	if b.bounds.belowFloor(b.read, b.reading) {
		b.cancel(fmt.Errorf("%w: %d bytes in %v", errStoreTooSlow, b.read, b.reading.Round(time.Millisecond)))
	}
	return n, err
}

// rate is the store's pace in bytes a second, which the cut line reports; a body no read has
// waited on has none.
func (b *storeBody) rate() int64 {
	if b.reading <= 0 {
		return 0
	}
	return b.read * int64(time.Second) / int64(b.reading)
}

// downloadWriter is the answer a version is copied into: it keeps the failure of the write that
// failed, since io.Copy reports a write that failed and a read of the file that failed as one
// error and the two are answered differently (serveFileVersion).
type downloadWriter struct {
	http.ResponseWriter
	failure error
}

func (d *downloadWriter) Write(p []byte) (int, error) {
	n, err := d.ResponseWriter.Write(p)
	if err != nil {
		d.failure = err
	}
	return n, err
}
