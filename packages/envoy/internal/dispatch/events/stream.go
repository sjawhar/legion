package events

import (
	"sync"

	"github.com/sjawhar/envoy/internal/dispatch/model"
)

// streamBuffer is how far behind a subscriber may fall before the stream drops it: 64 events,
// which is more than any one request commits and far less than a replay page (api's maxSSEReplay
// is 1000). A subscriber that cannot drain 64 events is not reading, and a stream that blocked
// for it would hold up every other subscriber of this process.
const streamBuffer = 64

// Stream fans committed events out to the subscribers of one process, each on a bounded channel.
// A subscriber whose channel is full is dropped and its channel closed rather than waited for:
// that ends the SSE response body and sends the browser to its reconnect-from-last-id, which
// replays what it missed from the event log, instead of letting it miss an event in silence.
//
// The reader's live feed is a Stream of its own, held by the event stream relay
// (api/event_stream_relay.go) and published into only by that relay, from the rows it reads back
// out of Postgres once they have committed. Nothing at a write site publishes into it, so an open
// SSE connection sees exactly the rows the database holds, in the id order the commit-order lock
// gives them (Broker.Append) — including the events another envoy-dispatch process committed,
// which no in-process fan-out can carry. Broker holds a Stream of its own for the subscribers
// that do want this process's own writes: the outbox's local wake and the delivery hub's intake.
type Stream struct {
	mu          sync.Mutex
	nextID      uint64
	subscribers map[uint64]chan model.Event
}

// NewStream creates an empty fan-out.
func NewStream() *Stream {
	return &Stream{subscribers: make(map[uint64]chan model.Event)}
}

// Subscribe returns a committed-event stream and a cancellation function.
func (s *Stream) Subscribe() (<-chan model.Event, func()) {
	s.mu.Lock()
	defer s.mu.Unlock()
	id := s.nextID
	s.nextID++
	ch := make(chan model.Event, streamBuffer)
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
func (s *Stream) SubscriberCount() int {
	s.mu.Lock()
	defer s.mu.Unlock()
	return len(s.subscribers)
}

// Publish fans a committed event out to local subscribers. An overflowed SSE
// client is disconnected before it can advance its replay cursor past an event.
func (s *Stream) Publish(e model.Event) {
	s.mu.Lock()
	defer s.mu.Unlock()
	for id, ch := range s.subscribers {
		select {
		case ch <- e:
		default:
			delete(s.subscribers, id)
			close(ch)
		}
	}
}

// CloseAll disconnects every active local subscriber, ending their SSE response
// bodies as if the server had restarted. Its production caller is the event stream
// relay's reset path (api/event_stream_relay.go, resetCursor): when the event log's
// ids have restarted under the relay's cursor, the rows those ids now name are not
// the ones a subscriber was already sent, so a reader resuming on the old ids cannot
// be served that log in order and every subscription is ended rather than served
// something that would mean the wrong thing. Each browser then reconnects from its
// last id and resolves its position against the log as it now stands. The test hook
// behind POST /api/v1/events/_test/disconnect (api/events.go) calls it for the same
// effect: it exercises a client's reconnect-from-lastId path without seeding enough
// events to trip the replay cap.
func (s *Stream) CloseAll() {
	s.mu.Lock()
	defer s.mu.Unlock()
	for id, ch := range s.subscribers {
		delete(s.subscribers, id)
		close(ch)
	}
}
