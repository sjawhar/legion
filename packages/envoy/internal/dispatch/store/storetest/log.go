package storetest

import (
	"strings"
	"sync"
)

// LockedLog is a log sink that the goroutines of the code under test write while the test reads
// it, such as the handler a test installs with slog.SetDefault.
type LockedLog struct {
	mu      sync.Mutex
	written strings.Builder
}

func (l *LockedLog) Write(p []byte) (int, error) {
	l.mu.Lock()
	defer l.mu.Unlock()
	return l.written.Write(p)
}

// String is everything logged so far.
func (l *LockedLog) String() string {
	l.mu.Lock()
	defer l.mu.Unlock()
	return l.written.String()
}

// Lines is the logged lines that hold substring.
func (l *LockedLog) Lines(substring string) string {
	var matched strings.Builder
	for line := range strings.Lines(l.String()) {
		if strings.Contains(line, substring) {
			matched.WriteString(line)
		}
	}
	return matched.String()
}
