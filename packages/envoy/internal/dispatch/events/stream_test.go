package events

import (
	"testing"

	"github.com/sjawhar/envoy/internal/dispatch/model"
)

// TestStreamDropsASubscriberThatCannotKeepUp is the overflow contract every reader of a Stream
// rests on: a subscriber whose channel is full is dropped and its channel closed, rather than
// waited for. The SSE handler ends its response body when its channel closes, and the browser
// reconnects from the last id it saw. Blocking instead would hold the event stream relay -- one
// goroutine for the whole process -- behind the slowest reader of any open page, and silently
// skipping the event would leave that client wrong with no reconnect to repair it.
func TestStreamDropsASubscriberThatCannotKeepUp(t *testing.T) {
	const buffer = 8
	stream := NewStream[model.Event]()
	subscription, cancel := stream.Subscribe(buffer)
	defer cancel()

	for sequence := 1; sequence <= buffer+1; sequence++ {
		stream.Publish(model.Event{Seq: sequence})
	}
	if count := stream.SubscriberCount(); count != 0 {
		t.Fatalf("subscriber count after overflow = %d, want 0", count)
	}
	for sequence := 1; sequence <= buffer; sequence++ {
		event, open := <-subscription
		if !open || event.Seq != sequence {
			t.Fatalf("event %d = %#v (open=%t), want the buffered events in order", sequence, event, open)
		}
	}
	if _, open := <-subscription; open {
		t.Fatal("overflowed subscriber remained open")
	}
}

// TestStreamBuffersAsManyValuesAsItsSubscriberAsked: each subscriber chooses how far behind it may
// fall, so a slow consumer with a large buffer survives a burst that drops one with a small buffer.
func TestStreamBuffersAsManyValuesAsItsSubscriberAsked(t *testing.T) {
	stream := NewStream[int]()
	small, cancelSmall := stream.Subscribe(1)
	defer cancelSmall()
	large, cancelLarge := stream.Subscribe(4)
	defer cancelLarge()

	for value := 1; value <= 4; value++ {
		stream.Publish(value)
	}
	if count := stream.SubscriberCount(); count != 1 {
		t.Fatalf("subscriber count after a burst of 4 = %d, want the 4-slot subscriber alone", count)
	}
	if value, open := <-small; !open || value != 1 {
		t.Fatalf("1-slot subscriber received %d (open=%t), want 1 before it was dropped", value, open)
	}
	if _, open := <-small; open {
		t.Fatal("1-slot subscriber remained open after the burst overflowed it")
	}
	for want := 1; want <= 4; want++ {
		if value := <-large; value != want {
			t.Fatalf("4-slot subscriber received %d, want %d", value, want)
		}
	}
}

// TestStreamCloseAllEndsEverySubscription: the event stream relay ends a generation's
// subscriptions this way, at a reset event log and from the test hook behind POST
// /api/v1/events/_test/disconnect, which drives a client's reconnect on demand.
func TestStreamCloseAllEndsEverySubscription(t *testing.T) {
	stream := NewStream[model.Event]()
	first, cancelFirst := stream.Subscribe(1)
	defer cancelFirst()
	second, cancelSecond := stream.Subscribe(1)
	defer cancelSecond()
	if count := stream.SubscriberCount(); count != 2 {
		t.Fatalf("subscriber count = %d, want 2", count)
	}

	stream.CloseAll()

	for name, subscription := range map[string]<-chan model.Event{"first": first, "second": second} {
		if _, open := <-subscription; open {
			t.Fatalf("%s subscription stayed open after CloseAll", name)
		}
	}
	if count := stream.SubscriberCount(); count != 0 {
		t.Fatalf("subscriber count after CloseAll = %d, want 0", count)
	}
}
