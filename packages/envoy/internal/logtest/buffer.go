package logtest

import (
	"strings"
	"sync"
)

// Buffer is the sink the goroutines of the code under test write while the test reads it, such as
// the handler a test installs with SetDefault: the two happen at once, and the buffer's own mutex
// is what makes the read safe, so the race detector sees a log the test may read at any moment
// rather than a data race. A type that needs a reader of its own embeds this one rather than
// copying the body.
type Buffer struct {
	mu      sync.Mutex
	written strings.Builder
}

// Write appends p to the buffer.
func (b *Buffer) Write(p []byte) (int, error) {
	b.mu.Lock()
	defer b.mu.Unlock()
	return b.written.Write(p)
}

// String is everything logged so far.
func (b *Buffer) String() string {
	b.mu.Lock()
	defer b.mu.Unlock()
	return b.written.String()
}

// Len is how many bytes have been written so far, for a reader that measures the log or marks a
// place in it rather than reading it whole.
func (b *Buffer) Len() int {
	b.mu.Lock()
	defer b.mu.Unlock()
	return b.written.Len()
}

// Lines is the logged lines that hold substring.
func (b *Buffer) Lines(substring string) string {
	var matched strings.Builder
	for line := range strings.Lines(b.String()) {
		if strings.Contains(line, substring) {
			matched.WriteString(line)
		}
	}
	return matched.String()
}
