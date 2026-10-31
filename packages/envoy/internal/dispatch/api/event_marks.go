package api

import (
	"context"
	"sync"

	"github.com/sjawhar/envoy/internal/dispatch/events"
)

// eventMarksKept bounds the recent marks a server keeps (eventMarks). A mark is 32 bytes.
const eventMarksKept = 32

// eventMarks is the event log marks a server keeps (events.Mark), which bound the ids a writer can
// still hold open (events.Horizon.WriterFloor): the boot mark register takes (takeBootMark), and the
// recent marks the relay's first start and GET /auth/whoami take, oldest first.
//
// One mark taken now counts only once every transaction that holds a write lock on events took its
// transaction id after the mark (events.Horizon.WriterFloor). Under event writes that overlap
// without a break a mark taken now never does, but an older one does as soon as the writes open
// when it was taken have ended, and those are this service's own writes, which take milliseconds. Keeping the recent marks is what
// lets a reader answer at once from the newest one that counts rather than wait for the writes open
// at its own mark. Past eventMarksKept the whole list is thinned to every other mark, starting
// from the oldest, so the oldest mark, the first to count during a long write, stays, and the kept
// ones stay spread over the time they cover.
type eventMarks struct {
	mu     sync.Mutex
	boot   events.Mark
	recent []events.Mark
}

// setBoot records the boot mark.
func (m *eventMarks) setBoot(mark events.Mark) {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.boot = mark
}

// add keeps mark as the newest recent mark.
func (m *eventMarks) add(mark events.Mark) {
	m.mu.Lock()
	defer m.mu.Unlock()
	if len(m.recent) >= eventMarksKept {
		kept := m.recent[:0]
		for i, mark0 := range m.recent {
			if i%2 == 0 {
				kept = append(kept, mark0)
			}
		}
		m.recent = kept
	}
	m.recent = append(m.recent, mark)
}

// forget drops every recent mark, which a reset of the event log leaves naming positions in a log
// that no longer exists (reseedEventCursor). WriterFloor passes over such a mark anyway; dropping
// them keeps it from reading their rows.
func (m *eventMarks) forget() {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.recent = nil
}

// candidates is every mark kept, newest first, the boot mark last: the order WriterFloor takes.
func (m *eventMarks) candidates() []events.Mark {
	m.mu.Lock()
	defer m.mu.Unlock()
	out := make([]events.Mark, 0, len(m.recent)+1)
	for i := len(m.recent) - 1; i >= 0; i-- {
		out = append(out, m.recent[i])
	}
	if m.boot.Xid != 0 {
		out = append(out, m.boot)
	}
	return out
}

// writerFloor takes a mark now, keeps it, and answers WriterFloor over every mark kept: the
// highest id at or below which no event can still be open, whether a kept mark counted (ok), and
// the newest id the new mark saw. With no mark counting, the floor is the higher of the horizon
// this process proved and the one persisted (events.Horizon.Persisted). It waits on no
// transaction.
func (s *server) writerFloor(ctx context.Context) (floor int64, ok bool, newest int64, err error) {
	horizon := s.horizon()
	mark, err := horizon.Mark(ctx)
	if err != nil {
		return 0, false, 0, err
	}
	s.eventMarks.add(mark)
	floor, ok, err = horizon.WriterFloor(ctx, s.eventMarks.candidates()...)
	if err != nil || ok {
		return floor, ok, mark.Newest, err
	}
	// No mark counts: the floor is the horizon proven here or persisted by any process, whichever
	// is higher, every id at or below which is final.
	persisted, err := horizon.Persisted(ctx)
	return max(floor, persisted), false, mark.Newest, err
}
