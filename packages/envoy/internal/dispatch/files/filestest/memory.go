// Package filestest holds what Dispatch's file tests run against: Memory, the in-memory
// files.Store of the API, backfill and health-probe tests, and S3, a fake bucket on loopback that
// files.NewS3 reaches through the AWS SDK's own environment (ServeS3). Nothing a deployment builds
// imports it.
package filestest

import (
	"context"
	"fmt"
	"sync"

	"github.com/sjawhar/envoy/internal/dispatch/files"
)

// Memory is a files.Store in process memory.
type Memory struct {
	mu      sync.Mutex
	objects map[string]memoryObject
	failure error
}

var _ files.Store = (*Memory)(nil)

type memoryObject struct {
	mime string
	body []byte
}

// NewMemory returns an empty Memory.
func NewMemory() *Memory {
	return &Memory{objects: map[string]memoryObject{}}
}

// SetFailure makes every call answer err, a test's unreachable bucket; nil makes it reachable again.
func (m *Memory) SetFailure(err error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.failure = err
}

// Put stores a copy of body under sha unless the store already holds sha. Like the bucket, it
// refuses a body past files.MaxObjectSize and one whose SHA-256 is not sha (S3 checks the
// checksum files.S3 sends), so a caller that hands the wrong hash fails here as it would there.
func (m *Memory) Put(_ context.Context, sha, mime string, body []byte) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	if m.failure != nil {
		return m.failure
	}
	if len(body) > files.MaxObjectSize {
		return fmt.Errorf("put file %s: %d bytes, limit %d", sha, len(body), files.MaxObjectSize)
	}
	if got := files.SHA256(body); got != sha {
		return fmt.Errorf("put file %s: the body's SHA-256 is %s", sha, got)
	}
	if _, held := m.objects[sha]; held {
		return nil
	}
	m.objects[sha] = memoryObject{mime: mime, body: append([]byte(nil), body...)}
	return nil
}

// Get returns a copy of the object under sha, or an error files.ErrNotFound matches.
func (m *Memory) Get(_ context.Context, sha string) ([]byte, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	if m.failure != nil {
		return nil, m.failure
	}
	object, held := m.objects[sha]
	if !held {
		return nil, fmt.Errorf("get file %s: %w", sha, files.ErrNotFound)
	}
	return append([]byte(nil), object.body...), nil
}

// Healthy answers the failure SetFailure set, or nil.
func (m *Memory) Healthy(context.Context) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	return m.failure
}

// Len is how many distinct objects the store holds.
func (m *Memory) Len() int {
	m.mu.Lock()
	defer m.mu.Unlock()
	return len(m.objects)
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
