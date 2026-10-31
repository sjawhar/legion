package docs

import (
	"context"
	"errors"
	"testing"
	"time"

	"github.com/reearth/ygo/crdt"
	ygws "github.com/reearth/ygo/provider/websocket"

	"github.com/sjawhar/envoy/internal/dispatch/pmdoc"
)

// browserOrigin tags the updates this file applies as a browser peer's, as the room's sync applies
// them. It must not be zero sized, since ygo compares origins by interface equality.
type browserOrigin struct{ _ byte }

// Each copy of a room replays the room's own commits to the room's markdown: the replica its reads
// walk, the fold of its stored history, and the room that loads that history again. ygo follows a
// remote apply that changes formatted text with a cleanup commit of its own; the room runs none
// for its own commits (a repair or a mark sweep in Server.Apply), so a copy that applied those
// commits with the cleanup on deleted formatting the room kept, and *x*y read back as *xy*. The
// room itself still cleans up after a browser's update, as Yjs does, and that cleanup is stored
// with the rest of its history.
func TestEveryCopyOfARoomReplaysItsFormattedCommitsToItsMarkdown(t *testing.T) {
	service, artifactID := newTestService(t)
	service.settle = time.Hour
	seedServiceText(t, service, artifactID, "seed")
	ctx := context.Background()

	// The room's own commits, each a transaction of the room's: emphasis written, then partly
	// overwritten and deleted, leaving markers a remote apply's cleanup removes.
	var text *crdt.YXmlText
	roomCommit := func(change func(*crdt.Transaction)) {
		t.Helper()
		if err := service.srv.Apply(ctx, artifactID, func(doc *crdt.Doc, transact func(func(*crdt.Transaction))) {
			if text == nil {
				fragment := doc.GetXmlFragment(fragmentName)
				transact(func(txn *crdt.Transaction) {
					paragraph := crdt.NewYXmlElement("paragraph")
					fragment.InsertElement(txn, fragment.Len(), paragraph)
					text = crdt.NewYXmlText()
					paragraph.InsertText(txn, 0, text)
				})
			}
			transact(change)
		}); err != nil {
			t.Fatalf("commit in the room: %v", err)
		}
	}
	writeItalicLeavingMarkers(func(change func(*crdt.YXmlText, *crdt.Transaction)) {
		roomCommit(func(txn *crdt.Transaction) { change(text, txn) })
	})

	// A browser's commits, the same edits in a paragraph of its own, each applied to the room as
	// its sync applies a peer's update.
	browser := crdt.New()
	if err := crdt.ApplyUpdateV1(browser, encodedRoom(t, service, artifactID), nil); err != nil {
		t.Fatalf("open the document in the browser: %v", err)
	}
	browserFragment := browser.GetXmlFragment(fragmentName)
	var browserText *crdt.YXmlText
	cleanups := 0
	browserCommit := func(change func(*crdt.Transaction)) {
		t.Helper()
		before := browser.StateVector()
		browser.Transact(change)
		update := crdt.EncodeStateAsUpdateV1(browser, before)
		origin := &browserOrigin{}
		// The update reaches the room outside Apply's own transact, as a peer's sync applies it,
		// so Apply reports no changes of its own.
		err := service.srv.Apply(ctx, artifactID, func(doc *crdt.Doc, _ func(func(*crdt.Transaction))) {
			unsubscribe := doc.OnUpdate(func(_ []byte, committed any) {
				if committed != origin {
					cleanups++
				}
			})
			defer unsubscribe()
			if err := crdt.ApplyUpdateV1(doc, update, origin); err != nil {
				t.Errorf("apply the browser's update: %v", err)
			}
		})
		if err != nil && !errors.Is(err, ygws.ErrNoChanges) {
			t.Fatalf("send the browser's update: %v", err)
		}
	}
	browserCommit(func(txn *crdt.Transaction) {
		paragraph := crdt.NewYXmlElement("paragraph")
		browserFragment.InsertElement(txn, browserFragment.Len(), paragraph)
		browserText = crdt.NewYXmlText()
		paragraph.InsertText(txn, 0, browserText)
	})
	writeItalicLeavingMarkers(func(change func(*crdt.YXmlText, *crdt.Transaction)) {
		browserCommit(func(txn *crdt.Transaction) { change(browserText, txn) })
	})
	if cleanups == 0 {
		t.Fatal("the room made no cleanup commit after the browser's updates; it must clean up as Yjs does")
	}

	want := roomMarkdown(t, service, artifactID)
	if got, err := service.Text(ctx, artifactID); err != nil || got != want {
		t.Fatalf("the replica reads %q (%v), want the room's %q", got, err, want)
	}
	if err := service.Evict(ctx, artifactID); err != nil {
		t.Fatalf("evict the room: %v", err)
	}
	if got, err := service.Text(ctx, artifactID); err != nil || got != want {
		t.Fatalf("the stored history folds to %q (%v), want the room's %q", got, err, want)
	}
	if got := roomMarkdown(t, service, artifactID); got != want {
		t.Fatalf("the reloaded room renders %q, want the room's %q", got, want)
	}

	// The reloaded room loaded its history with the cleanup off and turned it back on once that
	// apply returned (applyLoadedState), so it cleans up after a browser's update again.
	cleanups = 0
	browserCommit(func(txn *crdt.Transaction) {
		paragraph := crdt.NewYXmlElement("paragraph")
		browserFragment.InsertElement(txn, browserFragment.Len(), paragraph)
		browserText = crdt.NewYXmlText()
		paragraph.InsertText(txn, 0, browserText)
	})
	writeItalicLeavingMarkers(func(change func(*crdt.YXmlText, *crdt.Transaction)) {
		browserCommit(func(txn *crdt.Transaction) { change(browserText, txn) })
	})
	if cleanups == 0 {
		t.Fatal("the reloaded room made no cleanup commit after the browser's updates; its load left the cleanup off")
	}
}

// writeItalicLeavingMarkers makes, as four commits, emphasised text whose last two commits leave
// format markers that a remote apply's cleanup removes: the room renders *x*y, and a copy that
// cleaned up renders *xy*.
func writeItalicLeavingMarkers(commit func(func(*crdt.YXmlText, *crdt.Transaction))) {
	emphasis := crdt.Attributes{"emphasis": crdt.Attributes{"marker": "*"}}
	plain := crdt.Attributes{"emphasis": nil}
	commit(func(text *crdt.YXmlText, txn *crdt.Transaction) { text.Insert(txn, 0, "xy", emphasis) })
	commit(func(text *crdt.YXmlText, txn *crdt.Transaction) { text.Insert(txn, 1, "x", plain) })
	commit(func(text *crdt.YXmlText, txn *crdt.Transaction) { text.Delete(txn, 1, 1) })
	commit(func(text *crdt.YXmlText, txn *crdt.Transaction) { text.Format(txn, 1, 1, plain) })
}

// encodedRoom is the room's whole state, encoded under its lock.
func encodedRoom(t *testing.T, service *Service, artifactID string) []byte {
	t.Helper()
	var state []byte
	err := service.srv.Apply(context.Background(), artifactID, func(doc *crdt.Doc, _ func(func(*crdt.Transaction))) {
		state = crdt.EncodeStateAsUpdateV1(doc, nil)
	})
	if err != nil && !errors.Is(err, ygws.ErrNoChanges) {
		t.Fatalf("encode the room: %v", err)
	}
	return state
}

// roomMarkdown renders the room's own tree, not the replica its reads walk. Nothing else writes
// the room while this test reads it.
func roomMarkdown(t *testing.T, service *Service, artifactID string) string {
	t.Helper()
	var markdown string
	err := service.srv.Apply(context.Background(), artifactID, func(doc *crdt.Doc, _ func(func(*crdt.Transaction))) {
		tree, err := pmdoc.Read(doc.GetXmlFragment(fragmentName))
		if err != nil {
			t.Errorf("read the room's tree: %v", err)
			return
		}
		if markdown, err = renderTree(tree); err != nil {
			t.Errorf("render the room's tree: %v", err)
		}
	})
	if err != nil && !errors.Is(err, ygws.ErrNoChanges) {
		t.Fatalf("read the room: %v", err)
	}
	return markdown
}
