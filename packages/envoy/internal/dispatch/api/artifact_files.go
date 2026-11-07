package api

import (
	"bytes"
	"cmp"
	"context"
	"errors"
	"io"
	"log/slog"
	"mime"
	"net/http"
	"net/textproto"
	"strconv"
	"strings"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/sjawhar/envoy/internal/contracts"
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

// serveFileVersion answers a file version's download from what its row holds: a row holding
// bytes is served from the row, whatever store is configured; a row the backfill cleared, or that
// an upload wrote with a store, names its object by hash and is streamed from the store.
//
// The answer carries the file's own name (fileDisposition), inline for a type a browser draws
// where the reader is and attachment for every other, and names the bytes by their hash in a
// quoted entity tag, the only form net/http reads back from If-Range and If-None-Match.
//
// A reader may ask for part of a file: every answer says so (`Accept-Ranges: bytes`), since a
// player decides from the first answer it gets whether it may seek. Row bytes are answered by
// http.ServeContent, which writes the length and the status itself and answers Range, 206,
// Content-Range, 416 and the conditional reads. Store bytes are streamed for the whole of a
// file, so one is never held in memory for a reader who asked for all of it: with no Range, as
// they always were, and for an unconditional range that names the whole of the object
// (`wholeObjectRange`, what a player's opening probe asks for), answered 206 with the same
// bytes, the same Content-Range and the same length http.ServeContent would write. Only a
// genuinely partial range reads the object into memory first, since the store hands out one
// stream from the object's start.
//
// A conditional read of store bytes with no Range is answered here as http.ServeContent answers
// one of row bytes: an If-None-Match holding this version's entity tag is 304 with no body
// (`etagMatches`), so a browser that kept the bytes fetches none of them again.
//
// The headers wait for the object to open, so a store that fails answers 502 and never a 200 cut
// short; a body that fails after that, or does not hash to the row's hash, aborts the streamed
// response mid-stream rather than ending it as if whole.
//
// A version's bytes never change, so a browser keeps them for a year without asking again: the
// pictures a conversation shows inline are fetched once. `private`, since the route is
// authenticated and no shared cache may hold them. A document version is not served here and
// keeps its headers.
func (s *server) serveFileVersion(w http.ResponseWriter, r *http.Request, artifactID, name string, number int, content []byte, contentType, sha *string, size *int64) {
	storedType := cmp.Or(deref(contentType), "application/octet-stream")
	headers := func() {
		w.Header().Set("Content-Type", storedType)
		if sha != nil {
			w.Header().Set("ETag", `"`+*sha+`"`)
		}
		w.Header().Set("Cache-Control", "private, max-age=31536000, immutable")
		w.Header().Set("Content-Disposition", fileDisposition(storedType, name))
		w.Header().Set("X-Content-Type-Options", "nosniff")
		w.Header().Set("Accept-Ranges", "bytes")
	}
	if content != nil {
		headers()
		http.ServeContent(w, r, name, time.Time{}, bytes.NewReader(content))
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
	ranged := r.Header.Get("Range")
	if ranged != "" && !wholeObjectRange(r, ranged, object.Size) {
		// A part of the file is answered from the whole of it: the store hands out one stream
		// from the object's start, and the reader that checks the hash must see all of it.
		// files.ReadObject takes it in that reader's own steps, so the object is held once and
		// not twice; it is at most files.MaxObjectSize, which the store's own writes are held
		// to, so the read is bounded.
		whole, err := files.ReadObject(object)
		if err != nil {
			if r.Context().Err() != nil {
				return // The client went away; nothing to tell it.
			}
			// Nothing is written yet, so a body that fails or does not hash to the row's hash
			// is named rather than cut short: the bytes the row names cannot be served.
			slog.Error("dispatch: read a file version from the file store for a ranged request", "artifact", artifactID, "version", number, "sha256", *sha, "error", err)
			writeError(w, "FILE_MISSING", http.StatusInternalServerError, "this file's bytes cannot be found")
			return
		}
		headers()
		http.ServeContent(w, r, name, time.Time{}, bytes.NewReader(whole))
		return
	}
	if ranged == "" && etagMatches(r.Header.Get("If-None-Match"), `"`+*sha+`"`) {
		// The reader holds these bytes already, and a version's never change: the object opened,
		// but no byte of it is read and no body goes out.
		headers()
		w.WriteHeader(http.StatusNotModified)
		return
	}
	headers()
	w.Header().Set("Content-Length", strconv.FormatInt(object.Size, 10))
	status := http.StatusOK
	if ranged != "" {
		w.Header().Set("Content-Range", "bytes 0-"+strconv.FormatInt(object.Size-1, 10)+"/"+strconv.FormatInt(object.Size, 10))
		status = http.StatusPartialContent
	}
	w.WriteHeader(status)
	if _, err := io.Copy(w, object.Body); err != nil {
		if r.Context().Err() != nil {
			return // The client went away; nothing to tell it.
		}
		slog.Error("dispatch: stream a file version from the file store", "artifact", artifactID, "version", number, "sha256", *sha, "error", err)
		panic(http.ErrAbortHandler)
	}
}

// wholeObjectRange reports whether r asks for the whole of a size-byte object in one range and
// on no condition: `bytes=0-` or `bytes=0-<size-1>`, which is how Chrome opens a file it may
// seek in (`<video preload="metadata">`). Those two are streamed, so that opening costs no
// buffer at all. Every other range, and any range carrying a condition, is http.ServeContent's,
// which reads the conditions and answers 416 for a range past the end; it is handed a reader it
// can seek in, so those requests read the object into memory first. They are not rare:
// Safari's own opening probe is `bytes=0-1`, and a seek in any engine is `bytes=N-`. Each costs
// one copy of the object for the one request, at most files.MaxObjectSize. An empty object is
// http.ServeContent's too, since a range over no bytes overlaps nothing.
func wholeObjectRange(r *http.Request, header string, size int64) bool {
	if size <= 0 {
		return false
	}
	for _, condition := range []string{"If-Match", "If-Modified-Since", "If-None-Match", "If-Range", "If-Unmodified-Since"} {
		if r.Header.Get(condition) != "" {
			return false
		}
	}
	spec, found := strings.CutPrefix(header, "bytes=")
	if !found {
		return false
	}
	first, last, found := strings.Cut(spec, "-")
	if !found || textproto.TrimString(first) != "0" {
		return false
	}
	last = textproto.TrimString(last)
	return last == "" || last == strconv.FormatInt(size-1, 10)
}

// etagMatches reports whether an If-None-Match header holds tag, this version's own quoted
// entity tag, read as net/http's checkIfNoneMatch reads that header: a comma-separated list, `*`
// standing for any tag, and the weak comparison a conditional GET uses, under which `W/"x"`
// matches `"x"`. net/http keeps that reading to itself (scanETag, etagWeakMatch) and
// http.ServeContent, the one exported door to it, cannot be handed a store object's stream, so
// the walk is written out here rather than compared as a string, which would answer the whole
// file to a list and to a weak tag.
func etagMatches(header, tag string) bool {
	for rest := textproto.TrimString(header); rest != ""; rest = textproto.TrimString(rest) {
		if rest[0] == ',' {
			rest = rest[1:]
			continue
		}
		if rest[0] == '*' {
			return true
		}
		candidate, remain := scanETag(rest)
		if candidate == "" {
			return false
		}
		if strings.TrimPrefix(candidate, "W/") == strings.TrimPrefix(tag, "W/") {
			return true
		}
		rest = remain
	}
	return false
}

// scanETag reads the entity tag an If-None-Match list opens with and what follows it, as
// net/http's own scanETag does: `"text"` or `W/"text"`, holding only the characters an entity
// tag may hold. It answers an empty tag for anything else, which ends the walk.
func scanETag(header string) (etag, remain string) {
	start := 0
	if strings.HasPrefix(header, "W/") {
		start = 2
	}
	if len(header[start:]) < 2 || header[start] != '"' {
		return "", ""
	}
	for i := start + 1; i < len(header); i++ {
		switch c := header[i]; {
		case c == '"':
			return header[:i+1], header[i+1:]
		case c == 0x21 || (c >= 0x23 && c <= 0x7e) || c >= 0x80:
		default:
			return "", ""
		}
	}
	return "", ""
}

// fileDisposition is a version download's Content-Disposition: inline for a content type a
// browser draws where the reader is (contracts.InlineContentTypes, generated from
// INLINE_CONTENT_TYPES in packages/contracts, whose one reader this is), attachment for every
// other one, with the artifact's own name as the filename either way, so a saved file keeps the
// name Dispatch shows. The stored type is matched without its parameters, since
// `image/png; charset=binary` is the same picture as `image/png`.
//
// The name is given twice, as RFC 6266 says: `filename`, which every client reads and only ASCII
// can express, and `filename*`, RFC 8187's encoding of the whole name, which every browser since
// IE9 prefers.
func fileDisposition(storedType, name string) string {
	mediaType := storedType
	if parsed, _, err := mime.ParseMediaType(storedType); err == nil {
		mediaType = parsed
	}
	disposition := "attachment"
	if _, inline := contracts.InlineContentTypes[mediaType]; inline {
		disposition = "inline"
	}
	// Both parameters are written here rather than by mime.FormatMediaType, which writes one
	// parameter of a name: it leaves a value that is a token unquoted, and for a name holding a
	// character a token may not carry it writes the RFC 2231 extended parameter in place of the
	// plain one, which would leave a client that reads only `filename` with no name at all.
	return disposition +
		`; filename="` + plainFilename(name) + `"` +
		"; filename*=UTF-8''" + percentEncodeFilename(name)
}

// plainFilename is name as the text inside the quotes of a `filename` parameter: a character the
// parameter cannot carry - a control character, or anything outside printable ASCII, which only
// `filename*` can express - written as an underscore, and a quote or a backslash escaped by a
// backslash, which is the whole of RFC 6266's quoted-string. A client that reads `filename*`
// never sees it.
func plainFilename(name string) string {
	plain := make([]byte, 0, len(name))
	for _, char := range name {
		switch {
		case char < 0x20 || char > 0x7e:
			plain = append(plain, '_')
		case char == '"' || char == '\\':
			plain = append(plain, '\\', byte(char))
		default:
			plain = append(plain, byte(char))
		}
	}
	return string(plain)
}

// percentEncodeFilename is name as a `filename*` parameter's value, the text after its charset
// and language: every byte outside RFC 8187's attr-char set percent-encoded. Hand-rolled because
// the standard library's escapes are for URLs, not for this: url.PathEscape leaves a semicolon, a
// comma and a quote through, each of which would end the parameter where the name has not, and
// url.QueryEscape writes a space as a plus sign, which a client reads as a plus sign.
func percentEncodeFilename(name string) string {
	const hexDigits = "0123456789ABCDEF"
	var encoded strings.Builder
	encoded.Grow(len(name))
	for _, char := range []byte(name) {
		switch {
		case char >= 'a' && char <= 'z', char >= 'A' && char <= 'Z', char >= '0' && char <= '9',
			strings.IndexByte("!#$&+-.^_`|~", char) >= 0:
			encoded.WriteByte(char)
		default:
			encoded.WriteByte('%')
			encoded.WriteByte(hexDigits[char>>4])
			encoded.WriteByte(hexDigits[char&0xf])
		}
	}
	return encoded.String()
}
