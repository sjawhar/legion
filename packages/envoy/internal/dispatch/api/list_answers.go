package api

import (
	"bytes"
	"compress/gzip"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"hash"
	"io"
	"log/slog"
	"net/http"
	"sync/atomic"

	"github.com/sjawhar/envoy/internal/dispatch/bytelru"
	"github.com/sjawhar/envoy/internal/dispatch/pmdoc"
)

// An issue's comment list and its activity feed are the two answers a dashboard tab reads again
// and again, and under `Dispatch-Trees: 1` each carries the display tree of every stored text
// beside the markdown (display_trees.go), which makes those answers several times larger. Both
// now answer under an ETag, so an unchanged list costs its reader no bytes and no parse, and both
// keep what they last answered here, so an unchanged list costs this server no encode and no
// compression either.
//
// Where this sits against the compression layer: the kept bytes are the bytes that go on the
// wire, compressed, so writing them sets `Content-Encoding` here rather than leaving the layer
// above to compress them a second time. compressAnswers leaves an answer that already carries an
// encoding exactly as the handler wrote it (compressibleAnswer), so nothing compresses twice;
// what this writes for a caller that takes no gzip carries no encoding and that layer leaves it
// alone too, since it only ever compresses what it is given, and it is given the final bytes.
// The compression this does is the compression that layer would have done - one pool of
// compressors, one level, one threshold (gzipListAnswer) - so a caller is sent the bytes it was
// sent before any of this existed.
//
// Each content coding takes an entry of its own rather than one entry serving both: what is kept
// is what goes on the wire, and one shared entry would mean compressing on every gzip read or
// expanding on every read without it, which is the work the cache exists to remove. The cost is
// one body held twice at most, under a bound that holds whole answers anyway.

// listAnswerCacheBytes bounds the answers kept here. 64 MiB holds the warm lists of a few
// thousand issues at the size the trees make them, which is every list a deployment's readers
// touch in a working day; it is the bound the display trees of the same process are held to
// (pmdoc.DisplayCacheBytes), a bound a server carries beside its pools rather than a target to
// fill. The bound is on the kept bytes alone (bytelru).
const listAnswerCacheBytes = 64 << 20

// The two lists kept here, each named by its route, so one issue's comment list and its activity
// feed never share an entry.
const (
	commentListRoute = http.MethodGet + " /api/v1/issues/{key}/comments"
	eventFeedRoute   = http.MethodGet + " /api/v1/issues/{key}/events"
)

// listAnswerKey is everything that decides one list answer's bytes. The ETag carries what the
// answer is made of - for the feed the issue's event sequence and the paging query, for the
// comment list the answer's own bytes - and, for both, the trees variant and the display parser
// version (listETag), so a deploy that reads markdown differently and a caller that changes its
// header both land on keys of their own. Beside it the key names the route and its owner, which
// the ETag of neither list carries, and the content coding the caller takes, which decides
// whether the entry holds the compressed form or the plain one.
type listAnswerKey struct {
	route string
	owner string
	etag  string
	gzip  bool
}

// listAnswerBytes is one answer exactly as it goes on the wire, under the coding its key names.
type listAnswerBytes []byte

// listAnswerCache is the answers one server keeps, with a count of those it has written. The
// count is what a test reads to tell a kept answer from one made again, as it reads
// pmdoc.DisplayTreeBuilds to tell a stored tree from one parsed again; nothing else reads it.
type listAnswerCache struct {
	*bytelru.Cache[listAnswerKey, listAnswerBytes]
	writes atomic.Uint64
}

func newListAnswerCache() *listAnswerCache {
	return &listAnswerCache{Cache: bytelru.New[listAnswerKey, listAnswerBytes](listAnswerCacheBytes)}
}

// answer writes the answer kept under key and reports whether it had one. Every method here
// takes a nil cache, which is a server a test built directly (directServer) rather than one
// register mounted: such a server keeps nothing and makes every answer the long way.
func (c *listAnswerCache) answer(w http.ResponseWriter, key listAnswerKey) bool {
	if c == nil {
		return false
	}
	kept, held := c.Get(key)
	if !held {
		return false
	}
	writeListAnswerBytes(w, key.gzip, kept)
	c.writes.Add(1)
	return true
}

// keep puts the bytes of one answer under its key. The value is never written to again: a key
// carries the answer's ETag, so an entry under it holds the one answer that hashed to it.
func (c *listAnswerCache) keep(key listAnswerKey, body listAnswerBytes) {
	if c == nil {
		return
	}
	c.Put(key, body)
}

// answered is how many answers this cache has written since the process started.
func (c *listAnswerCache) answered() uint64 {
	if c == nil {
		return 0
	}
	return c.writes.Load()
}

// writeListAnswerBytes writes one list answer's final bytes: Dispatch's own JSON, under the gzip
// encoding when that is what the bytes carry.
func writeListAnswerBytes(w http.ResponseWriter, gzipped bool, body listAnswerBytes) {
	w.Header().Set("Content-Type", "application/json")
	if gzipped {
		w.Header().Set("Content-Encoding", compressedEncoding)
	}
	w.WriteHeader(http.StatusOK)
	if _, err := w.Write(body); err != nil {
		slog.Warn("dispatch: write list answer failed", "error", err)
	}
}

// writeListAnswer writes encoded as the answer under key and keeps what it wrote. The caller has
// already compared the ETag, so what arrives here is an answer that goes out whole.
func (s *server) writeListAnswer(w http.ResponseWriter, key listAnswerKey, encoded listAnswerBytes) {
	body, gzipped := encoded, false
	if key.gzip && len(encoded) >= compressibleMinimum {
		compressed, err := gzipListAnswer(encoded)
		if err != nil {
			// A compressor that fails has written nothing the caller has seen, so the answer goes
			// out as the plain JSON it is - what a caller sending no Accept-Encoding is sent -
			// and nothing is kept under a key promising gzip.
			slog.Warn("dispatch: compress list answer failed", "route", key.route, "error", err)
		} else {
			body, gzipped = compressed, true
		}
	}
	if gzipped == key.gzip {
		// The bytes carry the coding the key names, so a reader of this entry can be told what it
		// is being sent from the key alone. An answer under compressibleMinimum is left plain for
		// a caller that takes gzip (compressAnswers leaves it plain too) and is kept under no
		// key, which costs that caller nothing worth keeping: under a kilobyte, a warm read
		// encodes an answer that travels in the one segment it would travel in compressed.
		s.listAnswers.keep(key, body)
	}
	writeListAnswerBytes(w, gzipped, body)
}

// encodeListAnswer is body encoded exactly as WriteJSON writes it, trailing newline and all, so
// the bytes a list answers with are the bytes it answered with before any of them were kept.
func encodeListAnswer(body any) (listAnswerBytes, error) {
	var encoded bytes.Buffer
	if err := json.NewEncoder(&encoded).Encode(body); err != nil {
		return nil, fmt.Errorf("encode the list answer: %w", err)
	}
	return encoded.Bytes(), nil
}

// gzipListAnswer is encoded compressed as the compression layer would have compressed it on its
// way out: the same encoding, the same level and the same pool of compressors (compressAnswers),
// so the kept bytes are the bytes that layer would have sent.
func gzipListAnswer(encoded listAnswerBytes) (listAnswerBytes, error) {
	var compressed bytes.Buffer
	// These answers compress by a factor of ten or more, so a buffer that starts near the result
	// allocates once instead of doubling its way up from nothing.
	compressed.Grow(len(encoded)/8 + 64)
	writer, pooled := gzipWriters.Get().(*gzip.Writer)
	if pooled {
		writer.Reset(&compressed)
	} else {
		// gzip.BestSpeed, the level the compression layer writes at, which its own comment weighs.
		// The level is a constant, so NewWriterLevel cannot refuse it.
		writer, _ = gzip.NewWriterLevel(&compressed, gzip.BestSpeed)
	}
	defer func() {
		writer.Reset(io.Discard)
		gzipWriters.Put(writer)
	}()
	if _, err := writer.Write(encoded); err != nil {
		return nil, fmt.Errorf("compress the list answer: %w", err)
	}
	if err := writer.Close(); err != nil {
		return nil, fmt.Errorf("close the compressed list answer: %w", err)
	}
	return compressed.Bytes(), nil
}

// listAnswerKeyFor is the entry key of one list answer: the route and the owner the request
// names, the answer's own ETag, and whether this caller takes gzip, which decides which form of
// the answer it is sent and which form the entry holds.
func listAnswerKeyFor(r *http.Request, route, owner, etag string) listAnswerKey {
	return listAnswerKey{
		route: route,
		owner: owner,
		etag:  etag,
		gzip:  acceptsGzip(r.Header.Get("Accept-Encoding")),
	}
}

// listETag closes a list's hash with the two lines both lists' ETags carry and answers the
// validator itself, in referencesETag's form. The trees variant is in the hash rather than left
// to `Vary: Dispatch-Trees` alone, so a caller that gains or drops the header can never match an
// ETag given for the other reading however a cache between it and Dispatch behaves - a list of
// nothing answers the same bytes either way, and would otherwise share one. The parser version
// is in it for the same reason across a deploy: a build that reads markdown differently answers
// different trees, and every validator the build before it gave is then another reading's.
func listETag(hash hash.Hash, trees bool) string {
	_, _ = fmt.Fprintf(hash, "trees=%t\nparser=%d\n", trees, pmdoc.DisplayTreeVersionNow())
	return `"` + hex.EncodeToString(hash.Sum(nil)) + `"`
}

// answerNotModified answers a request whose If-None-Match names etag, and reports whether it did.
// The ETag goes on the answer either way, as RFC 9110 §15.4.5 asks of a 304, and so do the two
// Vary headers every route carries (varyOnDisplayTrees, compressAnswers), which the layers above
// this one have already set on both answers.
func answerNotModified(w http.ResponseWriter, r *http.Request, etag string) bool {
	w.Header().Set("ETag", etag)
	if r.Header.Get("If-None-Match") != etag {
		return false
	}
	w.WriteHeader(http.StatusNotModified)
	return true
}
