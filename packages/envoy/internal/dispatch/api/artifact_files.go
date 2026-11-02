package api

import (
	"cmp"
	"errors"
	"io"
	"log/slog"
	"net/http"
	"os"
	"strconv"

	"github.com/sjawhar/envoy/internal/dispatch/admission"
	"github.com/sjawhar/envoy/internal/dispatch/files"
)

// The file store's two touchpoints on the artifact routes: an upload's object write before the
// transaction (putUploadedFile) and a version's download (serveFileVersion). storeArtifact and
// getArtifactVersion (artifacts.go) call them; the rows they read and write are theirs.

// putUploadedFile writes an upload's bytes to the file store under sha before storeArtifact opens
// its transaction, and answers the request itself when the store refuses. With no store, or for
// a document, it writes nothing. It reports whether the upload may go on.
//
// The object goes first: the bytes (up to maxArtifactBlobSize) cross the network to a store whose
// wait nothing here bounds, and the transaction takes a pooled connection and the owner's row
// lock, which every other writer of that issue queues behind. A store that refuses leaves no row
// behind; anything that refuses after it (a closed issue, a kind mismatch, a new name past its
// bound, a failed insert) leaves an object no row names, which a later upload of the same bytes
// reuses. The row records the hash the object is keyed by and no bytes. A write the request's
// own context cut off - the refusal of the units its body asked back, or a client that went away
// - is the request's, not the store's, and is answered as admission.WriteUnserved answers it,
// with no ERROR line.
func (s *server) putUploadedFile(w http.ResponseWriter, r *http.Request, kind, sha string, input artifactUploadInput) bool {
	if kind == "doc" || s.deps.Files == nil {
		return true
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

// serveFileVersion answers a file version's download from what its row holds: a row holding
// bytes is served from the row, whatever store is configured; a row the backfill cleared, or that
// an upload wrote with a store, names its object by hash and is streamed from the store.
//
// The headers wait for the object to open, so a store that fails answers 502 and never a 200 cut
// short; a body that fails after that, or does not hash to the row's hash, aborts the response
// mid-stream rather than ending it as if whole.
//
// The read's own deadline (downloadDeadline) stops at those headers, so what bounds the bytes
// that follow is the answer's transfer bound alone (boundedAnswer): a reader that keeps to it is
// sent the whole file however long that takes, and one that stops, or falls below the floor, has
// its write fail with os.ErrDeadlineExceeded. Such a download is logged once at INFO and the
// connection aborted, as the store's own failures are, so the client sees a cut connection
// rather than a clean 200 short of its Content-Length; a client that went away is answered
// nothing, having nobody left to read it.
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
	if content != nil {
		headers(int64(len(content)))
		written, err := w.Write(content)
		if errors.Is(err, os.ErrDeadlineExceeded) {
			cutDownload(artifactID, number, sha, int64(written), int64(len(content)))
		}
		return
	}
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
	object, err := s.deps.Files.Get(r.Context(), *sha)
	if err != nil {
		if errors.Is(err, files.ErrNotFound) {
			// The row names an object the bucket does not hold: not an outage a retry mends, a
			// restore from the bucket's versioning.
			slog.Error("dispatch: a file version's object is missing from the file store", "artifact", artifactID, "version", number, "sha256", *sha, "error", err)
			writeError(w, "FILE_MISSING", http.StatusInternalServerError, "this file's bytes cannot be found")
			return
		}
		slog.Error("dispatch: read a file version from the file store", "artifact", artifactID, "version", number, "sha256", *sha, "error", err)
		writeError(w, "FILE_STORE_UNAVAILABLE", http.StatusBadGateway, "the file store did not return this file")
		return
	}
	defer object.Body.Close()
	// The row's recorded size is the file's length; an object of another length is not the file,
	// however it got there, and is refused before a header is written. The body's reader checks
	// the hash as the bytes go and holds the last of them back until it has, so a wrong body
	// leaves the client short of Content-Length rather than whole.
	if size != nil && object.Size != *size {
		slog.Error("dispatch: a file version's object is not the size the row records", "artifact", artifactID, "version", number, "sha256", *sha, "row_size", *size, "object_size", object.Size)
		writeError(w, "FILE_MISSING", http.StatusInternalServerError, "this file's bytes cannot be found")
		return
	}
	headers(object.Size)
	answer := &downloadWriter{ResponseWriter: w}
	if _, err := io.Copy(answer, object.Body); err != nil {
		switch {
		case errors.Is(answer.failure, os.ErrDeadlineExceeded):
			cutDownload(artifactID, number, sha, answer.written, object.Size)
		case admission.DeadlinePassed(r.Context()):
			// The deadline fired before the headers went out (servedAsRead stops it there), so
			// the work before the answer - the row's read, the store's open - took the five
			// minutes the row allows it.
			slog.Info("dispatch: a file version's download ran past its deadline before its answer began",
				"artifact", artifactID, "version", number, "sha256", *sha, "written", answer.written, "size", object.Size)
		case r.Context().Err() != nil:
			return // The client went away; nothing to tell it.
		default:
			slog.Error("dispatch: stream a file version from the file store", "artifact", artifactID, "version", number, "sha256", *sha, "error", err)
		}
		panic(http.ErrAbortHandler)
	}
}

// cutDownload is the line a download whose write failed on the answer's transfer bound writes:
// its reader stopped reading, or fell below the floor, and the bytes it was sent are short of the
// Content-Length it was promised. INFO, since it is the reader's doing and no fault of the
// server's, and once per download, the write that failed being the last one made.
func cutDownload(artifactID string, number int, sha *string, written, size int64) {
	slog.Info("dispatch: a file version's download was cut: its reader stopped or fell below the floor",
		"artifact", artifactID, "version", number, "sha256", deref(sha), "written", written, "size", size)
}

// downloadWriter is the answer a store-backed version is copied into: it counts what reached the
// client and keeps the failure of the write that failed, since io.Copy reports a write that
// failed and a read of the object that failed as one error and the two are answered differently
// (serveFileVersion).
type downloadWriter struct {
	http.ResponseWriter
	written int64
	failure error
}

func (d *downloadWriter) Write(p []byte) (int, error) {
	n, err := d.ResponseWriter.Write(p)
	d.written += int64(n)
	if err != nil {
		d.failure = err
	}
	return n, err
}
