// Package files holds the bytes of Dispatch's uploaded files (an image, an attachment; never a
// document's markdown) outside Postgres, keyed by their content.
//
// A version's row keeps the file's name, type, size and SHA-256; the bytes live in a Store under
// the hash, so one file uploaded twice is stored once. A deployment with no Store configured keeps
// the bytes in the row, as every deployment did before the store existed, and a row that still
// holds bytes is served from the row whichever store is configured, so the backfill
// (BackfillRows) can move rows one at a time while the server runs, and RestoreRows can put them
// back.
package files

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"hash"
	"io"
	"time"
)

// MaxObjectSize is the largest file Dispatch stores, the upload route's own cap.
const MaxObjectSize = 25 << 20

const (
	// WriteTimeout bounds one Put: the upload route calls it with no connection or lock held,
	// and a bucket that never answers must fail the upload rather than hold its goroutine.
	WriteTimeout = 30 * time.Second
	// OpenTimeout bounds the time to a Get's first byte. The body that follows is read at the
	// client's pace and bounded only by the request's own context.
	OpenTimeout = 30 * time.Second
	// healthTimeout bounds the bucket probe /healthz runs, as the database probe is bounded: a
	// probe that answers late is as bad as one that never answers.
	healthTimeout = 2 * time.Second
)

// ErrNotFound is Get's answer for a hash the store holds no object under.
var ErrNotFound = errors.New("file not found")

// Object is one stored file open for reading. The caller reads Body to its end and closes it;
// Size is the length the store declares, which a caller sends as Content-Length before the first
// byte.
type Object struct {
	Body io.ReadCloser
	Size int64
}

// Store keeps uploaded files by the hex SHA-256 of their bytes.
type Store interface {
	// Put stores body under sha, the hex SHA-256 of body, with mime as its content type. A
	// store that already holds sha writes nothing and answers nil.
	Put(ctx context.Context, sha, mime string, body []byte) error
	// Get opens the object stored under sha, or answers an error errors.Is matches to
	// ErrNotFound. Any other error is a store failure: the object exists as far as the caller
	// knows, and could not be opened. The body's reads can fail too, and fail before the body
	// is done when the bytes read do not hash to sha.
	Get(ctx context.Context, sha string) (*Object, error)
	// Healthy answers nil when the store can be reached with the credentials it holds.
	Healthy(ctx context.Context) error
}

// Key is the object key a file is stored under: its hash below one prefix, so the bucket's
// policy can name every file with one pattern.
func Key(sha string) string {
	return "files/sha256/" + sha
}

// SHA256 is the hex SHA-256 of body, the hash a version row records and a Store keys by.
func SHA256(body []byte) string {
	sum := sha256.Sum256(body)
	return hex.EncodeToString(sum[:])
}

// VerifyingReader hashes what it passes through and refuses to deliver a body that does not hash
// to the hash it was opened under, or whose length is not the declared size: the reader every
// Store's Get wraps its body in. It always holds back part of what it has read until it has seen
// the body's end and checked it, so a caller copying it to a response can never have written the
// last byte of a bad object before the check fails: the client is left short, which every HTTP
// client reports, rather than handed a complete body of the wrong bytes under the right hash.
type VerifyingReader struct {
	body   io.ReadCloser
	sha    string
	size   int64
	read   int64
	hash   hash.Hash
	buffer []byte
	// held is what has been read and hashed but not yet handed out; eof says the body ended and
	// held is its verified tail.
	held []byte
	eof  bool
	err  error
}

// NewVerifyingReader wraps body, size bytes long, which must hash to sha.
func NewVerifyingReader(body io.ReadCloser, sha string, size int64) *VerifyingReader {
	return &VerifyingReader{body: body, sha: sha, size: size, hash: sha256.New(), buffer: make([]byte, 32<<10)}
}

func (r *VerifyingReader) Read(p []byte) (int, error) {
	if r.err != nil {
		return 0, r.err
	}
	// Read until more is held than the caller takes, so at least a byte stays back, or the
	// body's end is known and the whole of it checked.
	for !r.eof && len(r.held) <= len(p) {
		n, err := r.body.Read(r.buffer)
		if n > 0 {
			r.read += int64(n)
			if r.read > r.size {
				r.err = fmt.Errorf("object %s: body is longer than the %d bytes declared", r.sha, r.size)
				return 0, r.err
			}
			_, _ = r.hash.Write(r.buffer[:n])
			r.held = append(r.held, r.buffer[:n]...)
		}
		if errors.Is(err, io.EOF) {
			r.eof = true
			if r.read != r.size {
				r.err = fmt.Errorf("object %s: body ended after %d of %d bytes", r.sha, r.read, r.size)
				return 0, r.err
			}
			if got := hex.EncodeToString(r.hash.Sum(nil)); got != r.sha {
				r.err = fmt.Errorf("object %s reads back as %s (%d bytes)", r.sha, got, r.read)
				return 0, r.err
			}
		} else if err != nil {
			r.err = err
			return 0, err
		}
	}
	n := copy(p, r.held)
	r.held = r.held[n:]
	if r.eof && len(r.held) == 0 {
		return n, io.EOF
	}
	return n, nil
}

func (r *VerifyingReader) Close() error {
	return r.body.Close()
}

// Verify reads sha back from store to its end and checks the bytes against the hash: the
// read-back the backfill does before it clears a row, and the whole of its verify-only pass.
func Verify(ctx context.Context, store Store, sha string) error {
	object, err := store.Get(ctx, sha)
	if err != nil {
		return err
	}
	defer object.Body.Close()
	_, err = io.Copy(io.Discard, object.Body)
	return err
}

// ReadAll reads the object under sha whole: the backfill's restore, which writes the bytes back
// into a row. Verified by the body's reader as every Get is.
func ReadAll(ctx context.Context, store Store, sha string) ([]byte, error) {
	object, err := store.Get(ctx, sha)
	if err != nil {
		return nil, err
	}
	defer object.Body.Close()
	body := make([]byte, 0, object.Size)
	buffer := make([]byte, 32<<10)
	for {
		n, err := object.Body.Read(buffer)
		body = append(body, buffer[:n]...)
		if errors.Is(err, io.EOF) {
			return body, nil
		}
		if err != nil {
			return nil, err
		}
	}
}
