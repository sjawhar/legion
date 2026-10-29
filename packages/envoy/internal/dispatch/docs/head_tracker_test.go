package docs

import (
	"testing"
	"time"
)

// The head tracker trusts itself only while its LISTEN connection goes on delivering: a connection
// that stands and carries nothing - half-open - says nothing of the writes committed since. Trust
// lapses headTrustWindow after the last delivery, with no event to end it, and returns with the
// next delivery, which arrives after every notification committed before it. No database: the
// tracker's clock is the test's.
func TestTheHeadTrackerStopsTrustingAListenConnectionThatHasDeliveredNothingForTheTrustWindow(t *testing.T) {
	const room, head = "room", int64(7)
	clock := time.Unix(1_000_000, 0)
	tracker := newHeadTracker()
	tracker.now = func() time.Time { return clock }
	tracker.connected()
	tracker.delivered()
	tracker.hold(room)
	tracker.observe(room, head)
	trusts := func(when string, want bool) {
		t.Helper()
		if got, known := tracker.head(room); known != want || (want && got != head) {
			t.Errorf("%s: head = %d, %v, want %d, %v", when, got, known, head, want)
		}
		if token := tracker.token(room); token.trusted != want {
			t.Errorf("%s: token trusted = %v, want %v", when, token.trusted, want)
		}
		if figures := tracker.figures(); figures.Trusted != want {
			t.Errorf("%s: figures trusted = %v, want %v", when, figures.Trusted, want)
		}
	}

	trusts("at the delivery", true)
	clock = clock.Add(headTrustWindow - time.Millisecond)
	trusts("just inside the trust window", true)
	if quiet, listening := tracker.quiet(); quiet != headTrustWindow-time.Millisecond || !listening {
		t.Errorf("quiet = %s, %v, want %s, true", quiet, listening, headTrustWindow-time.Millisecond)
	}
	token := tracker.token(room)

	clock = clock.Add(time.Millisecond)
	trusts("the trust window after the last delivery", false)
	if tracker.unchanged(room, token) {
		t.Error("a token taken while trusted is unchanged once the trust window has passed with nothing delivered")
	}

	tracker.delivered()
	trusts("at the next delivery", true)
	if !tracker.unchanged(room, token) {
		t.Error("a token taken while trusted is not unchanged at the next delivery, though nothing wrote the document")
	}
	clock = clock.Add(headTrustWindow)
	trusts("the trust window after that delivery", false)
}

// The head tracker keeps an entry only for a document this process holds or writes: a notice of
// any other document makes none, an entry goes with its last holder, and a token taken under an
// entry that has gone never matches again, not even once a new holder makes the entry anew. A
// write of this process keeps its document's entry, and keeps it untrusted, until the tracker sees
// the write's end, or, for a write whose end it cannot see, until the document's next notice. No
// database: the tracker's clock is the test's.
func TestATrackerEntryGoesWithItsLastHolderAndItsTokensNeverMatchAgain(t *testing.T) {
	const room, written, unseen = "room", "written", "unseen"
	clock := time.Unix(1_000_000, 0)
	tracker := newHeadTracker()
	tracker.now = func() time.Time { return clock }
	tracker.connected()
	entries := func(when string, want int) {
		t.Helper()
		if got := tracker.figures().Entries; got != want {
			t.Errorf("%s: %d tracker entries, want %d", when, got, want)
		}
	}

	tracker.observe("unheld", 3)
	tracker.invalidate("unheld")
	entries("after a notice and a bare id of a document nothing here holds", 0)
	if token := tracker.token("unheld"); token.trusted {
		t.Error("a token of a document with no entry is trusted")
	}
	if _, trusted := tracker.head("unheld"); trusted {
		t.Error("the head of a document with no entry is trusted, want the store read")
	}

	tracker.hold(room) // a cached read, say
	tracker.hold(room) // and a cold read in flight
	tracker.observe(room, 4)
	token := tracker.token(room)
	if !token.trusted || !tracker.unchanged(room, token) {
		t.Fatalf("a token of a held document is not unchanged with nothing written (%+v)", token)
	}
	if head, trusted := tracker.head(room); !trusted || head != 4 {
		t.Errorf("head of a held document = %d, %v, want 4, true", head, trusted)
	}
	tracker.release(room)
	entries("with one of two holders gone", 1)
	if !tracker.unchanged(room, token) {
		t.Error("a token is not unchanged while its document is still held and nothing wrote it")
	}
	tracker.release(room)
	entries("with the last holder gone", 0)
	if tracker.unchanged(room, token) {
		t.Error("a token taken under an entry that has gone is unchanged")
	}
	if _, trusted := tracker.head(room); trusted {
		t.Error("the head of a document whose entry has gone is trusted")
	}
	tracker.release(room)
	entries("after a release with no hold to end", 0)

	tracker.observe(room, 5)
	tracker.hold(room)
	if tracker.unchanged(room, token) {
		t.Error("a token taken under an entry that has gone is unchanged once a new holder makes the entry anew")
	}
	if _, trusted := tracker.head(room); trusted {
		t.Error("an entry made anew trusts a head it heard before it was made")
	}
	if fresh := tracker.token(room); !tracker.unchanged(room, fresh) {
		t.Error("a token taken under the entry made anew is not unchanged with nothing written since")
	}
	tracker.release(room)
	entries("with the new holder gone", 0)

	write := tracker.beginOwnWrite(written)
	entries("with a write of this process in flight to a document nothing holds", 1)
	tracker.hold(written)
	if overlapping := tracker.token(written); overlapping.trusted {
		t.Error("a token taken while a write of this process is in flight is trusted")
	}
	tracker.release(written)
	entries("with the write still in flight and its reader gone", 1)
	if _, trusted := tracker.head(written); trusted {
		t.Error("the head of a document with a write of this process in flight is trusted")
	}
	write.end(true, 9)
	entries("once the write has ended with nothing holding the document", 0)

	untracked := tracker.beginOwnWrite(unseen)
	untracked.untrack()
	tracker.hold(unseen)
	if token := tracker.token(unseen); token.trusted {
		t.Error("a token taken after a write whose end the tracker cannot see, before its notice, is trusted")
	}
	tracker.release(unseen)
	entries("with a write whose end the tracker cannot see, before its notice", 1)
	tracker.observe(unseen, 2)
	entries("once that write's notice has been heard", 0)
}
