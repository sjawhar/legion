package api

import (
	"cmp"
	"errors"
	"io"
	"log/slog"
	"net/http"
	"strconv"

	"github.com/sjawhar/envoy/internal/dispatch/files"
)

// The file store's two touchpoints on the artifact routes: an upload's object write before the
// transaction (putUploadedFile) and a version's read (serveVersionBytes, under the download
// headers of serveFileVersion or the page headers of serveFilePage). storeArtifact,
// getArtifactVersion and getArtifactVersionPage (artifacts.go) call them; the rows they read and
// write are theirs.

// putUploadedFile writes an upload's bytes to the file store under sha before storeArtifact opens
// its transaction, and answers the request itself when the store refuses. With no store, or for
// a document, it writes nothing. It reports whether the upload may go on.
//
// The object goes first: the bytes (up to maxArtifactBlobSize) cross the network to a store whose
// wait nothing here bounds, and the transaction takes a pooled connection and the owner's row
// lock, which every other writer of that issue queues behind. A store that refuses leaves no row
// behind; anything that refuses after it (a closed issue, a kind mismatch, a new name past its
// bound, a failed insert) leaves an object no row names, which a later upload of the same bytes
// reuses. The row records the hash the object is keyed by and no bytes.
func (s *server) putUploadedFile(w http.ResponseWriter, r *http.Request, kind, sha string, input artifactUploadInput) bool {
	if kind == "doc" || s.deps.Files == nil {
		return true
	}
	if err := s.deps.Files.Put(r.Context(), sha, input.contentType, input.content); err != nil {
		slog.Error("dispatch: store an uploaded file", "sha256", sha, "size", len(input.content), "error", err)
		writeError(w, "FILE_STORE_UNAVAILABLE", http.StatusBadGateway, "the file store did not accept the upload")
		return false
	}
	return true
}

// serveFileVersion answers a file version's download under the download headers: the file's own
// stored type, and an attachment the browser saves.
//
// A version's bytes never change, so a browser keeps them for a year without asking again: the
// pictures a conversation shows inline are fetched once. `private`, since the route is
// authenticated and no shared cache may hold them. A document version is not served here and
// keeps its headers.
func (s *server) serveFileVersion(w http.ResponseWriter, r *http.Request, artifactID string, number int, content []byte, contentType, sha *string, size *int64) {
	s.serveVersionBytes(w, r, artifactID, number, content, sha, size, func(length int64) {
		w.Header().Set("Content-Type", cmp.Or(deref(contentType), "application/octet-stream"))
		if sha != nil {
			w.Header().Set("ETag", *sha)
		}
		w.Header().Set("Cache-Control", "private, max-age=31536000, immutable")
		w.Header().Set("Content-Disposition", "attachment")
		w.Header().Set("X-Content-Type-Options", "nosniff")
		w.Header().Set("Content-Length", strconv.FormatInt(length, 10))
		w.WriteHeader(http.StatusOK)
	})
}

// pageContentSecurityPolicy is the policy the page route sends with every answer it makes, the
// inline one and the attachment one. Compare it with the spec (dispatch://INFRA-682/spec) word by
// word: it is one string and the spec is one line.
//
// `sandbox allow-scripts` gives the page an opaque origin. The page is then same-origin with
// nothing, so it holds no cookie and no storage of Dispatch's, and it keeps script. Each `'none'`
// shuts one way out of the frame:
//
//   - `default-src 'none'` leaves no source for anything the directives after it do not name.
//   - `connect-src 'none'` stops fetch, XMLHttpRequest, EventSource, WebSocket and sendBeacon, so
//     the page reports nothing to anyone and asks Dispatch for nothing.
//   - `frame-src 'none'` stops a nested frame, which would carry a document this policy does not
//     cover.
//   - `form-action 'none'` stops a form from posting the reader's data anywhere.
//   - `base-uri 'none'` stops a `<base>` element from pointing the page's relative addresses at
//     another host.
//
// `frame-ancestors 'self'` lets only Dispatch's own pages frame this one. What is left is what a
// page that draws needs: its own inline script, style, pictures, media, fonts and workers, from
// the response itself, from `data:` and from `blob:`.
const pageContentSecurityPolicy = "sandbox allow-scripts; default-src 'none'; script-src 'unsafe-inline' 'unsafe-eval' blob:; style-src 'unsafe-inline'; img-src data: blob:; font-src data:; media-src data: blob:; worker-src blob:; connect-src 'none'; frame-src 'none'; form-action 'none'; base-uri 'none'; frame-ancestors 'self'"

// serveFilePage answers one version of a page file as HTML. It reads the version's bytes the two
// ways serveFileVersion reads them, from the row or from the file store by hash, and refuses the
// same way; only the headers differ.
//
// The answer is `inline` to a request whose Sec-Fetch-Dest is `iframe`, the frame a view block
// inserts. Every other request, and a request that sends no Sec-Fetch-Dest, gets `attachment`: a
// reader who opens the address in a tab saves the file, and the browser runs none of it. `Vary`
// names the header so no cache gives one of those answers to the other request. `no-cache` makes
// the browser ask again on each load, under the version's ETag, so a later change to these
// headers reaches every page.
func (s *server) serveFilePage(w http.ResponseWriter, r *http.Request, artifactID string, number int, content []byte, sha *string, size *int64) {
	disposition := "attachment"
	if r.Header.Get("Sec-Fetch-Dest") == "iframe" {
		disposition = "inline"
	}
	s.serveVersionBytes(w, r, artifactID, number, content, sha, size, func(length int64) {
		w.Header().Set("Content-Type", "text/html; charset=utf-8")
		w.Header().Set("Content-Disposition", disposition)
		w.Header().Set("X-Content-Type-Options", "nosniff")
		w.Header().Set("Referrer-Policy", "no-referrer")
		w.Header().Set("Cache-Control", "private, no-cache")
		if sha != nil {
			w.Header().Set("ETag", *sha)
		}
		w.Header().Set("Vary", "Sec-Fetch-Dest")
		w.Header().Set("Content-Security-Policy", pageContentSecurityPolicy)
		w.Header().Set("Content-Length", strconv.FormatInt(length, 10))
		w.WriteHeader(http.StatusOK)
	})
}

// serveVersionBytes writes a version's bytes under the headers its route sets: a row holding
// bytes is served from the row, whatever store is configured; a row the backfill cleared, or that
// an upload wrote with a store, names its object by hash and is streamed from the store.
//
// headers is called once, with the body's length, after every refusal has passed and before the
// first byte. It waits for the object to open, so a store that fails answers 502 and never a 200
// cut short; a body that fails after that, or does not hash to the row's hash, aborts the
// response mid-stream rather than ending it as if whole.
func (s *server) serveVersionBytes(w http.ResponseWriter, r *http.Request, artifactID string, number int, content []byte, sha *string, size *int64, headers func(length int64)) {
	if content != nil {
		headers(int64(len(content)))
		_, _ = w.Write(content)
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
	if _, err := io.Copy(w, object.Body); err != nil {
		if r.Context().Err() != nil {
			return // The client went away; nothing to tell it.
		}
		slog.Error("dispatch: stream a file version from the file store", "artifact", artifactID, "version", number, "sha256", *sha, "error", err)
		panic(http.ErrAbortHandler)
	}
}
