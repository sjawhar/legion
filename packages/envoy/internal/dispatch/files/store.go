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
	"sync"
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
	// Get returns the whole object stored under sha, or ErrNotFound. Any other error is a
	// store failure: the object exists as far as the caller knows, and could not be read.
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

// Memory is a Store in process memory: the stand-in the API tests run against, and nothing a
// deployment uses.
type Memory struct {
	mu      sync.Mutex
	objects map[string]memoryObject
	// Fail, when set, is the error every call answers: a test's unreachable bucket.
	Fail error
	puts int
}

type memoryObject struct {
	mime string
	body []byte
}

// NewMemory returns an empty Memory.
func NewMemory() *Memory {
	return &Memory{objects: map[string]memoryObject{}}
}

func (m *Memory) Put(_ context.Context, sha, mime string, body []byte) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	if m.Fail != nil {
		return m.Fail
	}
	if _, held := m.objects[sha]; held {
		return nil
	}
	m.objects[sha] = memoryObject{mime: mime, body: append([]byte(nil), body...)}
	m.puts++
	return nil
}

func (m *Memory) Get(_ context.Context, sha string) ([]byte, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	if m.Fail != nil {
		return nil, m.Fail
	}
	object, held := m.objects[sha]
	if !held {
		return nil, ErrNotFound
	}
	return append([]byte(nil), object.body...), nil
}

func (m *Memory) Healthy(context.Context) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	return m.Fail
}

// Len is how many distinct objects the store holds.
func (m *Memory) Len() int {
	m.mu.Lock()
	defer m.mu.Unlock()
	return len(m.objects)
}

// Puts is how many Put calls wrote an object: a second upload of the same bytes is not one.
func (m *Memory) Puts() int {
	m.mu.Lock()
	defer m.mu.Unlock()
	return m.puts
}

// MIME is the content type sha was stored with, and "" when the store does not hold it.
func (m *Memory) MIME(sha string) string {
	m.mu.Lock()
	defer m.mu.Unlock()
	return m.objects[sha].mime
}

// Delete removes sha, so a test can stand in for an object that went missing from the bucket.
func (m *Memory) Delete(sha string) {
	m.mu.Lock()
	defer m.mu.Unlock()
	delete(m.objects, sha)
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
