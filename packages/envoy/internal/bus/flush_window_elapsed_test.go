package bus

import (
	"errors"
	"testing"
	"time"

	"github.com/sjawhar/envoy/internal/testnats"
)

// A core publish whose flush window was spent before the flush began is answered with the text a
// spent window has always had, which deployed log filters match, and is known to a publish window
// as a spent flush (errFlushWindowElapsed), which the window answers ErrUnavailable: the Dispatch
// outbox then releases the event for its next scan rather than backing it off. The negative
// control is the same publish and flush with time left, which the server confirms.
func TestASpentFlushWindowKeepsItsTextAndIsUnavailableToAWindow(t *testing.T) {
	client, err := ConnectOwningStream([]string{testnats.URL(t)})
	if err != nil {
		t.Fatalf("connect: %v", err)
	}
	t.Cleanup(client.Close)
	const subject = "notifications.team.spent-flush-window"
	w := client.Window().(*window)
	if err := w.pin(); err != nil {
		t.Fatalf("pin the window's connection: %v", err)
	}
	flushWithin := func(deadline time.Time) error {
		t.Helper()
		before := observe(w.conn)
		if err := w.conn.Publish(subject, []byte("{}")); err != nil {
			t.Fatalf("publish: %v", err)
		}
		return confirmFlushed(w.conn, subject, before, deadline)
	}
	if err := flushWithin(time.Now().Add(5 * time.Second)); err != nil {
		t.Fatalf("control: a flush with its window to spend answered %v", err)
	}

	spent := flushWithin(time.Now().Add(-time.Second))
	if spent == nil {
		t.Fatal("a flush whose window was spent answered nil")
	}
	if got, want := spent.Error(), errWindowElapsed(subject).Error(); got != want {
		t.Errorf("a spent flush window reads %q, want %q", got, want)
	}
	if !errors.Is(spent, errFlushWindowElapsed) {
		t.Errorf("a spent flush window %v is not errFlushWindowElapsed", spent)
	}
	if answered := w.answered(spent); !errors.Is(answered, ErrUnavailable) {
		t.Errorf("a window answers a spent flush window %v, want ErrUnavailable", answered)
	}
}
