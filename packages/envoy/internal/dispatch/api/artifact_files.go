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
// after them is the answer's transfer bound (boundedAnswer, transferBounds) - on both sides of
// the copy. The answer's writes are armed before each of them, so a reader that stops, or falls
// below the floor, has its write fail with os.ErrDeadlineExceeded; the store's reads are armed
// with the same idle bound (storeBody), so a store that stops sending mid-object ends the read
// rather than holding this handler, its goroutine, its admission unit and the store's
// connection until the client hangs up. A reader that keeps to the bound is sent the whole file
// however long that takes.
//
// The ways a copy ends badly are told apart and logged once each: a write the bound failed is
// the reader's doing, INFO; a store that stopped sending is the store's, ERROR; a deadline that
// passed before the answer began, INFO; any other failure of the store's read, ERROR. Each
// aborts the connection, so the client sees a cut connection rather than a clean 200 short of
// its Content-Length and the access line marks the request aborted. A client that went away is
// answered nothing, having nobody left to read it, and a write that failed any other way is
// such a client: net/http cancels the request of a connection whose read fails.
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
	// of its own, which its stall bound ends (storeBody) without cancelling the request's.
	reads := r.Context()
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
		body, length = newStoreBody(object.Body, s.transfer.orDefault().idle, stalled), object.Size
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
	case admission.DeadlinePassed(r.Context()):
		// The deadline fired before the headers went out (servedAsRead stops it there), so
		// the work before the answer - the row's read, the store's open - took the five
		// minutes the row allows it.
		slog.Info("dispatch: a file version's download ran past its deadline before its answer began",
			"artifact", artifactID, "version", number, "sha256", deref(sha), "written", written, "size", length)
	case r.Context().Err() != nil:
		return // The client went away; nothing to tell it.
	default:
		slog.Error("dispatch: stream a file version from the file store", "artifact", artifactID, "version", number, "sha256", deref(sha), "error", err)
	}
	panic(http.ErrAbortHandler)
}

// errStoreStalled is the cause a download's read context carries when the file store stopped
// sending (storeBody): the one end of a download that is the store's fault rather than the
// reader's, logged at ERROR where a reader that stopped is logged at INFO.
var errStoreStalled = errors.New("the file store sent nothing")

// storeBody is a store object's body read under the download's idle bound, the same bound the
// answer's writes are armed with (transferBounds.idle): each Read arms a timer that ends the
// context the object was opened under, with errStoreStalled as the cause, and stops it once the
// read answers. A store that stops sending mid-object therefore fails this read rather than
// leaving the handler blocked in it until its client hangs up, which held the handler's
// goroutine, its admission unit and the store's own connection: enough patient clients during a
// bucket incident filled the read class and every read was then refused 429 OVERLOADED.
//
// One timer serves every read of the object, rearmed before each and stopped after, so a 25 MiB
// file costs one timer rather than one per 32 KiB read. A cancellation is final, so a timer
// that fires while its read is answering ends a copy that was about to end anyway.
type storeBody struct {
	body  io.Reader
	idle  time.Duration
	stall *time.Timer
}

// newStoreBody reads body under idle, ending the object's context with cancel when the store
// goes that long without sending.
func newStoreBody(body io.Reader, idle time.Duration, cancel context.CancelCauseFunc) *storeBody {
	bounded := &storeBody{body: body, idle: idle}
	bounded.stall = time.AfterFunc(idle, func() { cancel(fmt.Errorf("%w for %v", errStoreStalled, idle)) })
	bounded.stall.Stop()
	return bounded
}

func (b *storeBody) Read(p []byte) (int, error) {
	b.stall.Reset(b.idle)
	n, err := b.body.Read(p)
	b.stall.Stop()
	return n, err
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
