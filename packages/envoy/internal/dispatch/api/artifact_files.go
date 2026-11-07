package api

import (
	"context"
	"errors"
	"io"
	"log/slog"
	"mime"
	"net/http"
	"strconv"

	"github.com/jackc/pgx/v5"
	"github.com/sjawhar/envoy/internal/dispatch/files"
)

// The file store's two touchpoints on the artifact routes: an upload's object write before the
// transaction (putUploadedFile) and a version's read (serveVersionBytes, under the download
// headers of serveFileVersion or the page headers of serveFilePage). storeArtifact,
// getArtifactVersion and getArtifactVersionPage (artifacts.go) call them; the rows they read and
// write are theirs.

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
// the object is keyed by and no bytes.
func (s *server) putUploadedFile(w http.ResponseWriter, r *http.Request, kind, sha string, input artifactUploadInput, target artifactTarget) bool {
	if kind == "doc" || s.deps.Files == nil {
		return true
	}
	if err := s.refuseUploadBeforeItsObject(r.Context(), target, input.name, kind); err != nil {
		s.writeHandlerError(w, err)
		return false
	}
	if err := s.deps.Files.Put(r.Context(), sha, input.contentType, input.content); err != nil {
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

// serveFileVersion answers a file version's download under the download headers: the file's own
// stored type, and an attachment the browser saves.
//
// A version's bytes never change, so a browser keeps them for a year without asking again: the
// pictures a conversation shows inline are fetched once. `private`, since the route is
// authenticated and no shared cache may hold them. A document version is not served here and
// keeps its headers.
func (s *server) serveFileVersion(w http.ResponseWriter, r *http.Request, artifactID string, number int, content []byte, contentType, sha *string, size *int64) {
	s.serveVersionBytes(w, r, artifactID, number, content, sha, size, downloadHeaders(w, contentType, sha))
}

// downloadContentSecurityPolicy is the policy every file version's download carries, the GET's
// answer and the HEAD's alike. `sandbox` with no token gives a document drawn from these bytes
// an opaque origin, so it is same-origin with nothing and holds no cookie and no storage of
// Dispatch's, and `default-src 'none'` leaves it no source for anything at all; the two
// together are what a raw host sends (GitHub's does). It is the second half of the rule below,
// and the half that holds when the first is wrong: a type this route refuses to send is a type
// no engine draws, and a type it does send that an engine draws anyway is drawn with no origin
// of Dispatch's and no way out of the frame. A policy on a subresource is ignored by the engine
// that fetched it, so an `<img src>` of these bytes, a `fetch` of them and a reader's own
// download are answered exactly as before.
const downloadContentSecurityPolicy = "sandbox; default-src 'none'"

// typesAnEngineDrawsAsADocument are the stored types a browser draws as a document rather than
// saving: a page's own type, the two XML dialects an engine renders, and SVG, whose script runs
// in the document it is drawn as. A version stored under one of them is downloaded as
// `application/octet-stream` (downloadContentType), because `Content-Disposition: attachment`
// is advice an engine may decline - WebKit draws an attachment it believes it can render, which
// the page route met (A4, dispatch://INFRA-682) - and a document this route drew would run on
// Dispatch's own origin, holding the reader's cookie: it could then grant its own page any data
// file it liked, which R9 gives to no one but the person at their browser.
//
// No picture is one of them. An image is made from sniffed PNG, JPEG, GIF or WebP alone and is
// stored under the type its bytes are (artifactKind), so the `<img src>` the web app draws a
// conversation's pictures with asks for a type this map does not hold and is answered it.
var typesAnEngineDrawsAsADocument = map[string]bool{
	"text/html":             true,
	"application/xhtml+xml": true,
	"image/svg+xml":         true,
	"text/xml":              true,
	"application/xml":       true,
}

// downloadContentType is the type a file version is downloaded under: the type it is stored
// under, and `application/octet-stream` for a type an engine draws as a document, for a version
// that records no type, and for a type that does not parse - one an engine may still read a
// drawable type out of, which is reason enough not to repeat it back.
func downloadContentType(storedType string) string {
	mediaType, _, err := mime.ParseMediaType(storedType)
	if err != nil || typesAnEngineDrawsAsADocument[mediaType] {
		return "application/octet-stream"
	}
	return storedType
}

// downloadHeaders is the download answer's headers, written once with the body's length:
// serveFileVersion hands it to serveVersionBytes, which calls it after the last refusal and
// before the first byte, and a HEAD calls it itself on the length of the version's bytes, having
// read none of them (getArtifactVersion). Both answers carry the same headers, Content-Length
// included.
func downloadHeaders(w http.ResponseWriter, contentType, sha *string) func(length int64) {
	return func(length int64) {
		w.Header().Set("Content-Type", downloadContentType(deref(contentType)))
		if sha != nil {
			w.Header().Set("ETag", *sha)
		}
		w.Header().Set("Cache-Control", "private, max-age=31536000, immutable")
		w.Header().Set("Content-Disposition", "attachment")
		w.Header().Set("X-Content-Type-Options", "nosniff")
		w.Header().Set("Content-Security-Policy", downloadContentSecurityPolicy)
		w.Header().Set("Content-Length", strconv.FormatInt(length, 10))
		w.WriteHeader(http.StatusOK)
	}
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
//     the page asks Dispatch for nothing and reports nothing through them.
//   - `frame-src 'none'` stops a nested frame, which would carry a document this policy does not
//     cover.
//   - `form-action 'none'` stops a form from posting the reader's data anywhere.
//   - `base-uri 'none'` stops a `<base>` element from pointing the page's relative addresses at
//     another host.
//
// `frame-ancestors 'self'` lets only Dispatch's own pages frame this one. What is left is what a
// page that draws needs: its own inline script, style, pictures, media, fonts and workers, from
// the response itself, from `data:` and from `blob:`. No directive stops the page from moving its
// own frame to another address, carrying what it holds in that address, and a WebRTC connection
// is outside `connect-src`: the spec's decision "What a page can still do" (dispatch://INFRA-682)
// accepts both, since the page gets only the files a person granted it.
const pageContentSecurityPolicy = "sandbox allow-scripts; default-src 'none'; script-src 'unsafe-inline' 'unsafe-eval' blob:; style-src 'unsafe-inline'; img-src data: blob:; font-src data:; media-src data: blob:; worker-src blob:; connect-src 'none'; frame-src 'none'; form-action 'none'; base-uri 'none'; frame-ancestors 'self'"

// serveFilePage answers one version of a page file as HTML. It reads the version's bytes the two
// ways serveFileVersion reads them, from the row or from the file store by hash, and refuses the
// same way; only the headers differ.
func (s *server) serveFilePage(w http.ResponseWriter, r *http.Request, artifactID string, number int, content []byte, sha *string, size *int64) {
	s.serveVersionBytes(w, r, artifactID, number, content, sha, size, pageHeaders(w, r, sha))
}

// pageHeaders is the page answer's headers, written once with the body's length: serveFilePage
// hands it to serveVersionBytes, which calls it after the last refusal and before the first
// byte, and a HEAD calls it itself on the length of the version's bytes, having read none of
// them (getArtifactVersionPage). Both answers carry the same headers, Content-Length included.
//
// The answer is `inline` to a request whose Sec-Fetch-Dest is `iframe`, the frame a view block
// inserts, and that answer alone is `text/html`. Every other request, and a request that sends
// no Sec-Fetch-Dest, gets `attachment` AND `application/octet-stream`: a reader who opens the
// address in a tab saves the file, and the browser runs none of it. The type carries that rule
// rather than the disposition alone, because `Content-Disposition` is advice an engine may
// decline - WebKit displays an attachment it believes it can render, so a page served as
// `text/html` was drawn in a tab there while Chromium and Firefox downloaded it (A4,
// dispatch://INFRA-682). A type no browser renders leaves no engine that choice, and `nosniff`
// stops one guessing `text/html` back from the bytes. `Vary` names the header so no cache gives
// one of those answers to the other request. `no-cache` makes the browser ask again on each
// load, under the version's ETag, so a later change to these headers reaches every page, and
// an ask that brings the tag back is answered 304 from the row (pageNotModified).
func pageHeaders(w http.ResponseWriter, r *http.Request, sha *string) func(length int64) {
	return func(length int64) {
		writePageHeaders(w, r, sha)
		w.Header().Set("Content-Length", strconv.FormatInt(length, 10))
		w.WriteHeader(http.StatusOK)
	}
}

// framePageRequest is the one test of "this is the frame a view block inserted": the request
// whose Sec-Fetch-Dest is `iframe`. It decides the two headers that differ between the page's
// two answers and whether a revalidation may be answered 304, so the inline answer and the
// answer a cache may reuse are the same request shape by construction.
func framePageRequest(r *http.Request) bool {
	return r.Header.Get("Sec-Fetch-Dest") == "iframe"
}

// writePageHeaders writes every page header but Content-Length, which the 200 sets from the
// body it is about to write and the 304 has none of.
func writePageHeaders(w http.ResponseWriter, r *http.Request, sha *string) {
	disposition, contentType := "attachment", "application/octet-stream"
	if framePageRequest(r) {
		disposition, contentType = "inline", "text/html; charset=utf-8"
	}
	w.Header().Set("Content-Type", contentType)
	w.Header().Set("Content-Disposition", disposition)
	w.Header().Set("X-Content-Type-Options", "nosniff")
	w.Header().Set("Referrer-Policy", "no-referrer")
	w.Header().Set("Cache-Control", "private, no-cache")
	if tag := entityTag(sha); tag != "" {
		w.Header().Set("ETag", tag)
	}
	w.Header().Set("Vary", "Sec-Fetch-Dest")
	w.Header().Set("Content-Security-Policy", pageContentSecurityPolicy)
}

// entityTag is a version's entity tag: the hash of its bytes, quoted as RFC 9110 8.8.3 spells
// an entity tag, which is what a browser sends back in If-None-Match. A version that records no
// hash has none, and is answered without the header. It is strong, as the hash of the bytes a
// version will always have is.
func entityTag(sha *string) string {
	if sha == nil {
		return ""
	}
	return `"` + *sha + `"`
}

// pageNotModified answers a page request whose If-None-Match holds the version's entity tag,
// and reports whether it did. The page route is `no-cache`, so a browser revalidates every
// frame it starts; without this each revalidation downloaded the whole page again, up to 5 MiB
// of it.
//
// Only the frame's own answer is ever revalidated. A browser that had fetched the page into a
// frame was observed reusing that stored answer for a later navigation to the same address in a
// tab - `Vary: Sec-Fetch-Dest` did not keep the two apart - so a 304 to a tab handed it the
// frame's `text/html` and `inline` and drew the page on Dispatch's own address, which is the one
// thing A4 forbids (dispatch://INFRA-682). The attachment answer is a download and revalidating
// it saves a reader nothing, so this refuses every request but the frame's and the guarantee
// stops depending on a browser honouring `Vary`.
//
// The 304 carries every header the 200 would carry but Content-Length, and no body: a cache
// replaces the headers it stored with the ones a 304 brings (RFC 9111 4.3.4) and keeps the
// rest, so a 304 that left out the policy or the disposition would leave the frame running
// under the ones stored when the page was first fetched - which is the thing `no-cache` is here
// to prevent.
func pageNotModified(w http.ResponseWriter, r *http.Request, sha *string) bool {
	tag := entityTag(sha)
	if tag == "" || !framePageRequest(r) || r.Header.Get("If-None-Match") != tag {
		return false
	}
	writePageHeaders(w, r, sha)
	w.WriteHeader(http.StatusNotModified)
	return true
}

// answerHead answers a HEAD from the version's row alone, under the headers of its route, and
// reports whether it did. A version the row holds the bytes of has its length in stored, read
// by octet_length from the stored value's header; a version a file store holds has it in
// recorded, the size the row keeps. A row with neither is answered false, and its route falls
// through to the read of the bytes, which keeps the file store's own refusals.
func answerHead(stored, recorded *int64, headers func(length int64)) bool {
	length := stored
	if length == nil {
		length = recorded
	}
	if length == nil {
		return false
	}
	headers(*length)
	return true
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
