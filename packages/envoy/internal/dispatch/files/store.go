// Package files holds the bytes of Dispatch's uploaded files (an image, an attachment; never a
// document's markdown) outside Postgres, keyed by their content.
//
// A version's row keeps the file's name, type, size and SHA-256; the bytes live in a Store under
// the hash, so one file uploaded twice is stored once. A deployment with no Store configured keeps
// the bytes in the row, as every deployment did before the store existed, and a row that still
// holds bytes is served from the row whichever store is configured, so the backfill
// (BackfillRows) can move rows one at a time while the server runs.
package files

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
)

// MaxObjectSize is the largest file Dispatch stores, the upload route's own cap.
const MaxObjectSize = 25 << 20

// ErrNotFound is Get's answer for a hash the store holds no object under.
var ErrNotFound = errors.New("file not found")

// Store keeps uploaded files by the hex SHA-256 of their bytes.
type Store interface {
	// Put stores body under sha, the hex SHA-256 of body, with mime as its content type. A
	// store that already holds sha writes nothing and answers nil.
	Put(ctx context.Context, sha, mime string, body []byte) error
	// Get returns the whole object stored under sha, or an error errors.Is matches to
	// ErrNotFound. Any other error is a store failure: the object exists as far as the caller
	// knows, and could not be read.
	Get(ctx context.Context, sha string) ([]byte, error)
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

// Verify reads sha back from store and checks the bytes against the hash: the read-back the
// backfill does before it clears a row, and the whole of its verify-only pass.
func Verify(ctx context.Context, store Store, sha string) error {
	body, err := store.Get(ctx, sha)
	if err != nil {
		return err
	}
	if got := SHA256(body); got != sha {
		return fmt.Errorf("object %s reads back as %s (%d bytes)", sha, got, len(body))
	}
	return nil
}
