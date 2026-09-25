package bus

import (
	"context"
	"testing"
	"time"
)

// deadlinePassedContext is a context whose deadline has passed while its Err is still nil: the
// instant between a context.WithDeadline deadline and the timer that marks it done, which a loaded
// process can stretch.
type deadlinePassedContext struct{ context.Context }

func (deadlinePassedContext) Deadline() (time.Time, bool) { return time.Now().Add(-time.Second), true }

// A dial whose deadline has passed returns an error, never a nil connection with a nil error:
// ensureConnWithContext takes a nil error to mean a connection it can use, and calling JetStream
// on a nil one panics the process. Seen in a loaded package run of
// TestPublishBoundsReconnectWhenNATSUnavailable, as a nil pointer dereference in
// (*nats.Conn).JetStream under PublishCoreTo.
func TestADialPastItsDeadlineReturnsAnError(t *testing.T) {
	conn, err := connectWithContext(deadlinePassedContext{context.Background()}, "deadline-test", []string{"nats://127.0.0.1:1"}, nil, nil)
	if err == nil {
		t.Fatalf("connectWithContext past its deadline returned conn=%v and no error", conn)
	}
	if conn != nil {
		t.Fatalf("connectWithContext past its deadline returned a connection with %v", err)
	}
}
