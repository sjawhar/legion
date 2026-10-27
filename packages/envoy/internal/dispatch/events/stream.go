package events

import "sync"

// Stream fans values out to the subscribers of one process, each on a channel whose size the
// subscriber chooses. A subscriber whose channel is full is dropped and its channel closed rather
// than waited for, so it learns that it missed a value instead of missing it in silence: an SSE
// connection subscribes again and catches up what it missed from the event log, or ends its
// response when that backlog is past the replay's bounds (api/events.go streamEvents). A stream
// that blocked for a subscriber that is not reading would hold up every other subscriber of this
// process.
//
// The reader's live feed is a Stream of its own, held by the event stream relay
// (api/event_stream_relay.go) and published into only by that relay, from the rows it reads back
// out of Postgres once they have committed, each with its encoded frame. Nothing at a write site
// publishes into it, so an open SSE connection sees exactly the rows the database holds, in the id
// order the commit-order lock gives them (Broker.Append) — including the events another
// envoy-dispatch process committed, which no in-process fan-out can carry. Broker holds a Stream
// of its own for the subscribers that do want this process's own writes: the outbox's local wake
// and the delivery hub's intake.
//
// Every subscriber receives the same value, so a value that holds a slice or a map shares it:
// a subscriber reads what it receives and modifies none of it.
type Stream[T any] struct {
	mu          sync.Mutex
	nextID      uint64
	subscribers map[uint64]chan T
}

// NewStream creates an empty fan-out.
func NewStream[T any]() *Stream[T] {
	return &Stream[T]{subscribers: make(map[uint64]chan T)}
}

// Subscribe returns a channel of buffer slots and a cancellation function, which is idempotent.
// buffer is how far behind the subscriber may fall before the stream drops it.
func (s *Stream[T]) Subscribe(buffer int) (<-chan T, func()) {
	s.mu.Lock()
	defer s.mu.Unlock()
	id := s.nextID
	s.nextID++
	ch := make(chan T, buffer)
	s.subscribers[id] = ch
	var once sync.Once
	return ch, func() {
		once.Do(func() {
			s.mu.Lock()
			defer s.mu.Unlock()
			if existing, ok := s.subscribers[id]; ok {
				delete(s.subscribers, id)
				close(existing)
			}
		})
	}
}

// SubscriberCount reports the active local fan-out subscriptions.
func (s *Stream[T]) SubscriberCount() int {
	s.mu.Lock()
	defer s.mu.Unlock()
	return len(s.subscribers)
}

// Publish fans a value out to the subscribers. A subscriber whose channel is full is dropped and
// its channel closed, after the values it already holds, so it can tell where it stopped receiving
// and recover from there rather than skip a value it was never sent.
func (s *Stream[T]) Publish(value T) {
	s.mu.Lock()
	defer s.mu.Unlock()
	for id, ch := range s.subscribers {
		select {
		case ch <- value:
		default:
			delete(s.subscribers, id)
			close(ch)
		}
	}
}

// CloseAll ends every subscription, closing each channel as an overflow does. The event stream
// relay calls it when it ends a generation whose subscribers it can no longer serve in order (a
// reset event log, api/event_stream_relay.go resetCursor) and from the test hook behind POST
// /api/v1/events/_test/disconnect (api/events.go), which drives a client's
// reconnect-from-last-id on demand.
func (s *Stream[T]) CloseAll() {
	s.mu.Lock()
	defer s.mu.Unlock()
	for id, ch := range s.subscribers {
		delete(s.subscribers, id)
		close(ch)
	}
}
