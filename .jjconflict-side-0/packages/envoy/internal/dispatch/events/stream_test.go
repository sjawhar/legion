package events

import (
	"testing"

	"github.com/sjawhar/envoy/internal/dispatch/model"
)

// TestStreamDropsASubscriberThatCannotKeepUp is the overflow contract every reader of a Stream
// rests on: a subscriber whose channel is full is dropped and its channel closed, rather than
// waited for. An events subscription ends with an ended frame when its channel closes, and the
// page subscribes again from the last id it applied, which replays what it missed. Blocking
// instead would hold the event stream relay -- one goroutine for the whole process -- behind the
// slowest reader of any open page, and silently skipping the event would leave that page wrong
// with no resubscribe to repair it.
func TestStreamDropsASubscriberThatCannotKeepUp(t *testing.T) {
	stream := NewStream()
	subscription, cancel := stream.Subscribe()
	defer cancel()

	for sequence := 1; sequence <= streamBuffer+1; sequence++ {
		stream.Publish(model.Event{Seq: sequence})
	}
	if count := stream.SubscriberCount(); count != 0 {
		t.Fatalf("subscriber count after overflow = %d, want 0", count)
	}
	for sequence := 1; sequence <= streamBuffer; sequence++ {
		event, open := <-subscription
		if !open || event.Seq != sequence {
			t.Fatalf("event %d = %#v (open=%t), want the buffered events in order", sequence, event, open)
		}
	}
	if _, open := <-subscription; open {
		t.Fatal("overflowed subscriber remained open")
	}
}

// TestStreamCloseAllEndsEverySubscription: the event stream relay ends every events subscription
// this way when the event log is reset under its cursor, so each page subscribes again against
// the log as it now stands.
func TestStreamCloseAllEndsEverySubscription(t *testing.T) {
	stream := NewStream()
	first, cancelFirst := stream.Subscribe()
	defer cancelFirst()
	second, cancelSecond := stream.Subscribe()
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
