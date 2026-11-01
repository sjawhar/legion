package docs

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"maps"
	"math/rand/v2"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/reearth/ygo/crdt"
	ygws "github.com/reearth/ygo/provider/websocket"

	"github.com/sjawhar/envoy/internal/dispatch/model"
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

// A room loads its stored history as it was stored. A whole-state load runs ygo's full cleanup,
// which removes only repeated markers at a text's start, and such markers reach a history when two
// peers that do not clean up format the same text at once (as rooms did before ygo cleaned up
// after a remote apply). With the cleanup on, the load deleted one of them in a commit the store
// never sees, so the room held a state its history and every copy of it lack; the markdown is the
// same either way, so only the state shows it.
func TestARoomLoadsItsStoredHistoryAsItWasStored(t *testing.T) {
	service, artifactID := newTestService(t)
	service.settle = time.Hour
	seedServiceText(t, service, artifactID, "seed")
	ctx := context.Background()
	base := encodedRoom(t, service, artifactID)
	if err := service.Evict(ctx, artifactID); err != nil {
		t.Fatalf("evict the room: %v", err)
	}

	// Two peers that do not clean up: one writes a paragraph, both then bold it at once.
	first := newDocumentCopy(crdt.WithClientID(1001))
	second := newDocumentCopy(crdt.WithClientID(1002))
	for _, peer := range []*crdt.Doc{first, second} {
		if err := crdt.ApplyUpdateV1(peer, base, nil); err != nil {
			t.Fatalf("open the document in a peer: %v", err)
		}
	}
	before := first.StateVector()
	first.Transact(func(txn *crdt.Transaction) {
		fragment := txn.GetXmlFragment(fragmentName)
		paragraph := crdt.NewYXmlElement("paragraph")
		fragment.InsertElement(txn, fragment.Len(), paragraph)
		text := crdt.NewYXmlText()
		paragraph.InsertText(txn, 0, text)
		text.Insert(txn, 0, "abc", nil)
	})
	written := crdt.EncodeStateAsUpdateV1(first, before)
	if err := crdt.ApplyUpdateV1(second, written, nil); err != nil {
		t.Fatalf("send the paragraph to the second peer: %v", err)
	}
	strong := crdt.Attributes{"strong": crdt.Attributes{"marker": "**"}}
	bold := func(peer *crdt.Doc) []byte {
		children := peer.GetXmlFragment(fragmentName).Children()
		paragraph := children[len(children)-1].(*crdt.YXmlElement)
		target := paragraph.Children()[0].(*crdt.YXmlText)
		before := peer.StateVector()
		peer.Transact(func(txn *crdt.Transaction) { target.Format(txn, 0, 3, strong) })
		return crdt.EncodeStateAsUpdateV1(peer, before)
	}
	stored := [][]byte{written, bold(first), bold(second)}
	fold := newDocumentCopy()
	if err := crdt.ApplyUpdateV1(fold, base, nil); err != nil {
		t.Fatalf("fold the base: %v", err)
	}
	for index, update := range stored {
		if _, err := service.persistence.AppendUpdate(ctx, artifactID, update); err != nil {
			t.Fatalf("store update %d: %v", index, err)
		}
		if err := crdt.ApplyUpdateV1(fold, update, nil); err != nil {
			t.Fatalf("fold update %d: %v", index, err)
		}
	}

	want := crdt.EncodeStateAsUpdateV1(fold, nil)
	if got := encodedRoom(t, service, artifactID); !bytes.Equal(got, want) {
		t.Fatalf("the loaded room holds a state of %d bytes, want its stored history's %d bytes", len(got), len(want))
	}
}

// The shapes a room's own commits leave are not only the crafted ones. A browser marks and
// unmarks runs that overlap each other; the service writes in the room between its updates
// (writeInRoom), marking, unmarking and deleting whole marked runs; an agent rewrites a paragraph
// through the document API; and the browser types. The room's own writes have the last word, as
// a repair or a sweep does: ygo runs no cleanup after a local transaction of the room's, so the
// markers they leave are the room's, and the fold of the stored updates a cold read renders must
// hold them. With crdt.WithFormatCleanup(false) taken out of newDocumentCopy the fold deletes
// markers of those gaps, and the cold read renders a marked run where the room rendered a plain
// one.
func TestAColdReadReplaysAChurnOfFormattingToTheRoomsMarkdown(t *testing.T) {
	for _, seed := range []uint64{1, 2, 3} {
		t.Run(fmt.Sprintf("seed %d", seed), func(t *testing.T) { formattingChurnRound(t, seed) })
	}
}

func formattingChurnRound(t *testing.T, seed uint64) {
	t.Helper()
	service, artifactID := newTestService(t)
	service.settle = time.Hour
	seedServiceText(t, service, artifactID, formattingChurnDocument())
	peer := newRoomPeer(t, service, artifactID, 1, reuseBob, nil)
	rng := rand.New(rand.NewPCG(644, seed))
	kinds := map[string]int{}
	// roomWrite is one of the service's own writes in the room, on the text node at index among
	// the markable ones: a whole marked run deleted, which strands the markers that opened and
	// closed it in one gap, or a short run marked or unmarked.
	roomWrite := func(index int) {
		writeInRoom(t, service, artifactID, "the churn's local write", serviceWrite, func(fragment *crdt.YXmlFragment) func(*crdt.Transaction) {
			written := textsUnder(fragment, true)
			if len(written) == 0 {
				return nil
			}
			target := written[index%len(written)]
			if marked, run, ok := markedRunOf(rng, target); ok && run < target.Len() && rng.IntN(2) == 0 {
				kinds["local delete"]++
				return func(txn *crdt.Transaction) { target.Delete(txn, marked, run) }
			}
			if target.Len() < 6 {
				return nil
			}
			at, run := rng.IntN(target.Len()-5), 1+rng.IntN(5)
			mark := formattingMarks[rng.IntN(len(formattingMarks))]
			var value any = markOn
			if _, held := marksAt(target, at)[mark]; held {
				value = nil
			}
			kinds["local format"]++
			return func(txn *crdt.Transaction) { target.Format(txn, at, run, crdt.Attributes{mark: value}) }
		})
	}

	for round := range 40 {
		texts := peer.markableTexts()
		if len(texts) == 0 {
			t.Fatal("the document holds no markable text")
		}
		text := texts[rng.IntN(len(texts))]
		if text.Len() < 6 {
			continue
		}
		at, length := rng.IntN(text.Len()-5), 1+rng.IntN(5)
		mark := formattingMarks[rng.IntN(len(formattingMarks))]
		var value any = markOn
		if rng.IntN(2) == 0 {
			value = nil
		}
		peer.send(func(txn *crdt.Transaction) { text.Format(txn, at, length, crdt.Attributes{mark: value}) })
		kinds["browser format"]++
		if round%3 == 2 {
			roomWrite(rng.IntN(len(texts)))
			peer.sync()
		}
		if round%7 == 6 {
			find := fmt.Sprintf("Churn paragraph %d holds", round/7)
			// The churn's own deletes can take the words the replace looks for, which is no
			// failure of the room's: the round writes nothing then.
			if strings.Count(roomMarkdown(t, service, artifactID), find) == 1 {
				if err := joinedWrite(service, applyOps(service, artifactID, reuseAgent, model.EditOp{
					Op:   "replace",
					Find: find,
					With: fmt.Sprintf("Churn paragraph %d now holds", round/7),
				})); err != nil {
					t.Fatalf("the agent's write: %v", err)
				}
				kinds["api replace"]++
			}
			peer.sync()
		}
		if round%11 == 10 {
			texts = peer.markableTexts()
			target := texts[rng.IntN(len(texts))]
			peer.send(func(txn *crdt.Transaction) { target.Insert(txn, rng.IntN(target.Len()+1), "typed", nil) })
			kinds["browser type"]++
		}
	}

	// The room's own writes close the churn, at the marked runs the browser left: a character
	// typed at a run's head carrying that run's own mark with a marker of its own, the run's
	// first character deleted after it, and that position unmarked - the editing that leaves two
	// markers of one key in one gap and a plain run behind them, which is where a cleanup calls
	// the first marker redundant. A browser update to the same text afterwards would have the
	// room clean the gap up and store the cleanup, which leaves the fold nothing to disagree
	// about, so the room's own writes come last.
	peer.sync()
	for range 6 {
		index, closingAt, typed := 0, 0, false
		writeInRoom(t, service, artifactID, "a marked character the room types", serviceWrite, func(fragment *crdt.YXmlFragment) func(*crdt.Transaction) {
			written := textsUnder(fragment, true)
			for _, candidate := range rng.Perm(len(written)) {
				target := written[candidate]
				at, _, ok := markedRunOf(rng, target)
				if !ok || target.Len() < at+2 {
					continue
				}
				// A mark the run carries with a marker of its own, which the seeded markdown
				// wrote: the write marks the character it types with that key and no marker, so
				// the two markers differ and ygo writes the write's own rather than letting the
				// run's stand for it.
				mark := markWithAMarker(marksAt(target, at))
				if mark == "" {
					continue
				}
				index, closingAt, typed = candidate, at, true
				return func(txn *crdt.Transaction) {
					target.Insert(txn, closingAt, "z", crdt.Attributes{mark: markOn})
				}
			}
			return nil
		})
		if !typed {
			continue
		}
		writeInRoom(t, service, artifactID, "the character after it deleted", serviceWrite, func(fragment *crdt.YXmlFragment) func(*crdt.Transaction) {
			written := textsUnder(fragment, true)
			target := written[index%len(written)]
			if target.Len() < closingAt+2 {
				return nil
			}
			return func(txn *crdt.Transaction) { target.Delete(txn, closingAt+1, 1) }
		})
		writeInRoom(t, service, artifactID, "the character after it unmarked", serviceWrite, func(fragment *crdt.YXmlFragment) func(*crdt.Transaction) {
			written := textsUnder(fragment, true)
			target := written[index%len(written)]
			if target.Len() < closingAt+2 {
				return nil
			}
			cleared := crdt.Attributes{}
			for _, mark := range formattingMarks {
				cleared[mark] = nil
			}
			return func(txn *crdt.Transaction) { target.Format(txn, closingAt+1, 1, cleared) }
		})
		kinds["local closing shape"]++
	}

	waitForLandedAppends(t, service, artifactID)
	t.Logf("seed %d: %v", seed, kinds)
	if kinds["local delete"] == 0 || kinds["api replace"] == 0 || kinds["local closing shape"] == 0 {
		t.Errorf("seed %d left the room no local delete, no agent write or no closing shape (%v): the round proved nothing", seed, kinds)
	}
	want, state := roomMarkdown(t, service, artifactID), encodedRoom(t, service, artifactID)
	if err := service.Evict(context.Background(), artifactID); err != nil {
		t.Fatalf("evict the room: %v", err)
	}
	cold := readCold(t, service, artifactID).markdown
	if cold != want {
		at := 0
		for at < len(cold) && at < len(want) && cold[at] == want[at] {
			at++
		}
		t.Errorf("the cold read differs from what the room held at byte %d of %d:\n  cold: %q\n  room: %q",
			at, len(want), excerptAround(cold, at), excerptAround(want, at))
	}
	if reloaded := encodedRoom(t, service, artifactID); !bytes.Equal(reloaded, state) {
		t.Errorf("the room reloaded from its stored history holds a state of %d bytes, want the %d bytes it held: the fold of its updates is not what it stored",
			len(reloaded), len(state))
	}
}

func excerptAround(text string, at int) string {
	return text[max(0, at-80):min(len(text), at+80)]
}

// markWithAMarker is the first of marks, in name order, whose value carries attributes of its
// own - the marker the seeded markdown wrote the mark with - or "" where no mark does. A mark
// written with another value is a marker of its own, where a mark written with the value held
// restates what the text carries and writes none.
func markWithAMarker(marks crdt.Attributes) string {
	for _, mark := range slices.Sorted(maps.Keys(marks)) {
		switch value := marks[mark].(type) {
		case crdt.Attributes:
			if len(value) > 0 {
				return mark
			}
		case map[string]any:
			if len(value) > 0 {
				return mark
			}
		}
	}
	return ""
}

// formattingChurnDocument is a document of marked and plain runs a browser can mark over, with
// paragraphs an agent rewrites.
func formattingChurnDocument() string {
	source := "# Formatting churn\n\n"
	for section := range 6 {
		source += fmt.Sprintf("Churn paragraph %d holds **bold words** and _italic words_ and plain words a browser marks over and over again.\n\n", section)
		source += fmt.Sprintf("- Churn bullet %d with **marked** and plain text\n  - Churn nested %d with _marked_ and plain text\n\n", section, section)
	}
	return source
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
