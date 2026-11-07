// Package testlog holds the one locked log buffer the daemon's tests share. A test that drives
// real goroutines — a consumer, a supervisor, a process the test started — gives the code under
// test a sink it writes while the test reads what has arrived so far, and the two happen at once:
// the buffer's own mutex is what makes the read safe, so the race detector sees a log a test may
// read at any moment rather than a data race. A type that needs a reader of its own embeds this
// one rather than copying the body.
package testlog

import (
	"strings"
	"sync"
)

// Buffer is a strings.Builder an io.Writer's user and a reader may hold at the same time.
type Buffer struct {
	mu  sync.Mutex
	buf strings.Builder
}

// Write appends p to the buffer.
func (b *Buffer) Write(p []byte) (int, error) {
	b.mu.Lock()
	defer b.mu.Unlock()
	return b.buf.Write(p)
}

// String returns everything written so far.
func (b *Buffer) String() string {
	b.mu.Lock()
	defer b.mu.Unlock()
	return b.buf.String()
}

// Len is how many bytes have been written so far, for a reader that measures the log rather
// than copying it out to measure what it copied.
func (b *Buffer) Len() int {
	b.mu.Lock()
	defer b.mu.Unlock()
	return b.buf.Len()
}

// Reset drops everything written so far, for a test that reads one phase of its subject at a time.
func (b *Buffer) Reset() {
	b.mu.Lock()
	defer b.mu.Unlock()
	b.buf.Reset()
}
