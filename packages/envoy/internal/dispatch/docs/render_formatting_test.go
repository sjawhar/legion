package docs

import (
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
)

// The cases here hold a room's replica to the room's own formatting, down to the markers behind
// it. From ygo v1.54.1 a document that applies a remote update touching formatted text follows it
// with a local commit of its own, the format cleanup Yjs runs (cleanupYTextAfterTransaction,
// crdt.WithFormatCleanup): the room runs it for the updates its browsers send, and never for the
// transactions it commits itself - Dispatch's own in-room writes (Server.Apply, serviceTransact)
// and a live write's fork. Every copy that replays the room's commits - the replica
// (renderedReplica), the shadow here, the fold of the stored updates - applies each of them as a
// remote update, the room's local ones included, so a copy that cleaned up would delete markers
// the room keeps and hold formatting the room never held. newDocumentCopy turns the cleanup off
// for every such copy; with crdt.WithFormatCleanup(false) taken out of it, both cases below fail.

// formattingRun is the run the fixed case marks and unmarks, seeded bold and italic
// (formattingDocument) and long enough to hold the review's shape: a marked 'x', a plain 'x' and a
// marked 'xxy'.
const formattingRun = "xxxxy"

// markOn is a mark's value as the browser editor and the service write it: a mark with no
// attributes of its own.
var markOn = map[string]any{}

// formattingMarks are the marks the storm sets and clears. Each one renders, so a mark the
// replica holds and the room does not shows in the markdown too (renderShadow.check); none of them
// may cover the text of a code block, which is why the storm picks from markableTexts.
var formattingMarks = []string{"strong", "emphasis", "strike_through"}

// formattingWords are the words the storm types.
var formattingWords = strings.Fields("one two three alpha beta gamma")

// TestRenderReuseAReplicaReplaysTheRoomsFormattingExactly holds that the room's replica carries
// exactly the formatting the room committed, under writes of both kinds: the room's own local
// transactions, which it never cleans up after, and its browsers' updates, which it does.
func TestRenderReuseAReplicaReplaysTheRoomsFormattingExactly(t *testing.T) {
	t.Run("a plain run between two marked ones", testAReplicaKeepsARunTheRoomLeftPlain)
	t.Run("local writes and browser edits interleaved", testAReplicaReplaysInterleavedFormatting)
}

// F1. The shape sjawhar/legion#2496's review names: four of the service's own writes leave the
// room holding 'x', 'x', 'xxy' - marked, plain, marked - where a replica that ran the cleanup
// keeps, of each key, only the last marker of the gap the third write's delete makes, and so
// holds a text reading 'xxxxy', all marked, with the plain run swallowed. Every write is a local
// transaction of the room's (writeInRoom), which the room runs no cleanup for, so the markers
// they leave are the room's own and the replica must keep every one - which is why such a replica
// is caught at that third write, by the markers it tombstones and the room does not.
func testAReplicaKeepsARunTheRoomLeftPlain(t *testing.T) {
	service, artifactID, sh, live, replica := newFormattingRoom(t)

	// The run's tail loses its italics: the marker that ends them stands in the gap the later
	// writes add their own markers to, which is what gives the cleanup two markers of one key
	// there to call the first redundant.
	writeInRoom(t, service, artifactID, "unitalicise the tail", serviceWrite, func(fragment *crdt.YXmlFragment) func(*crdt.Transaction) {
		text := xmlTextWithPrefix(t, fragment, formattingRun)
		return func(txn *crdt.Transaction) {
			text.Format(txn, 2, 3, crdt.Attributes{"emphasis": nil})
		}
	})
	replicaHoldsTheRoom(t, live, replica, "the tail lost its italics")

	// A bold character typed at the head of the run, and taken out again: the room keeps the
	// markers it left, since nothing cleans up after a local transaction.
	writeInRoom(t, service, artifactID, "insert a bold character", serviceWrite, func(fragment *crdt.YXmlFragment) func(*crdt.Transaction) {
		text := xmlTextWithPrefix(t, fragment, formattingRun)
		return func(txn *crdt.Transaction) {
			text.Insert(txn, 0, "x", crdt.Attributes{"strong": markOn})
		}
	})
	replicaHoldsTheRoom(t, live, replica, "a bold character was inserted")

	writeInRoom(t, service, artifactID, "delete the character after it", serviceWrite, func(fragment *crdt.YXmlFragment) func(*crdt.Transaction) {
		text := xmlTextWithPrefix(t, fragment, "x")
		return func(txn *crdt.Transaction) { text.Delete(txn, 1, 1) }
	})
	replicaHoldsTheRoom(t, live, replica, "the character after it was deleted")

	// The second character loses both marks, which leaves the plain run between two marked ones.
	writeInRoom(t, service, artifactID, "unmark the second character", serviceWrite, func(fragment *crdt.YXmlFragment) func(*crdt.Transaction) {
		text := xmlTextWithPrefix(t, fragment, "x")
		return func(txn *crdt.Transaction) {
			text.Format(txn, 1, 1, crdt.Attributes{"strong": nil, "emphasis": nil})
		}
	})
	replicaHoldsTheRoom(t, live, replica, "the second character was unmarked")

	// The room's own runs, so a change in what the writes above leave is told apart from a
	// replica that drifted from them. The tail keeps the bold the seeded markdown wrote, whose
	// value carries the marker it was written with; the head carries the write's own bold.
	const want = `"x"+strong=map[] "x" "xxy"+strong=map[marker:*] `
	if got := textRuns(roomText(t, live, "x")); got != want {
		t.Fatalf("the room's run is %s, and the review's shape is %s", got, want)
	}
	waitForLandedAppends(t, service, artifactID)
	sh.check(t)
}

// F2. A seeded storm of formatted writes of both kinds on one live room: the service's own local
// transactions in the room (writeInRoom: typing, marking and unmarking runs, deleting), the
// committed live writes of the document API (joinedApplyOps, a markdown replace that bolds or
// italicises a word, forked and published into the room), and a connected browser's own updates,
// which the room applies as remote and cleans up after. After every commit the replica, caught up,
// must stand exactly where the room stands: the same state vector and delete set, and the same
// delta for every text node of the fragment.
func testAReplicaReplaysInterleavedFormatting(t *testing.T) {
	seeds, steps := []uint64{1, 2, 3, 4}, 160
	if raceEnabled || testing.Short() {
		seeds, steps = []uint64{1, 2}, 60
	}
	for _, seed := range seeds {
		t.Run(fmt.Sprintf("seed %d", seed), func(t *testing.T) { formattingRound(t, seed, steps) })
	}
}

func formattingRound(t *testing.T, seed uint64, steps int) {
	t.Helper()
	service, artifactID, sh, live, replica := newFormattingRoom(t)
	bob := newRoomPeer(t, service, artifactID, 2, reuseBob, nil)
	rng := rand.New(rand.NewPCG(644, seed))
	word := func() string { return formattingWords[rng.IntN(len(formattingWords))] }
	// flipped is a mark the run at index does not carry as it stands, cleared where it does: a
	// format that restates what the text already holds writes no marker and commits a transaction
	// with no content, whose update is the room's delete set and nothing else - and two such
	// commits encode the same bytes, which the shadow reads as two classes of one update.
	flipped := func(text *crdt.YXmlText, index int) crdt.Attributes {
		mark := formattingMarks[rng.IntN(len(formattingMarks))]
		if _, held := marksAt(text, index)[mark]; held {
			return crdt.Attributes{mark: nil}
		}
		return crdt.Attributes{mark: markOn}
	}
	// textOp is one write on text: a word typed with or without marks, a run marked or unmarked,
	// or characters deleted. It answers no change where the text is too short for the op drawn,
	// which leaves the step to write nothing.
	textOp := func(text *crdt.YXmlText) (string, func(txn *crdt.Transaction)) {
		length := text.Len()
		switch rng.IntN(5) {
		case 0, 1:
			at := rng.IntN(length + 1)
			var typed crdt.Attributes
			if rng.IntN(2) == 0 {
				typed = flipped(text, at)
			}
			return "type", func(txn *crdt.Transaction) { text.Insert(txn, at, word(), typed) }
		case 2:
			if length < 4 {
				return "", nil
			}
			at := rng.IntN(length - 3)
			run := 1 + rng.IntN(min(4, length-at))
			marks := flipped(text, at)
			return "format", func(txn *crdt.Transaction) { text.Format(txn, at, run, marks) }
		default:
			if length < 4 {
				return "", nil
			}
			at, run := rng.IntN(length-3), 1+rng.IntN(2)
			if marked, markedRun, ok := markedRunOf(rng, text); ok && markedRun < length && rng.IntN(2) == 0 {
				at, run = marked, markedRun
			}
			return "delete", func(txn *crdt.Transaction) { text.Delete(txn, at, run) }
		}
	}

	// focused is the text node the storm is working in. It stays on one node for several steps,
	// so that its marks, unmarks and deletes meet each other's markers: a gap holding two markers
	// of one key is where the cleanup finds one to call redundant, and a storm that spread every
	// write over the whole document would make such a gap only rarely.
	focus := 0
	focused := func(texts []*crdt.YXmlText) *crdt.YXmlText {
		if len(texts) == 0 {
			return nil
		}
		if rng.IntN(4) == 0 {
			focus = rng.IntN(len(texts))
		}
		return texts[focus%len(texts)]
	}

	kinds := map[string]int{}
	bolded := false
	for step := range steps {
		made := ""
		switch kind := rng.IntN(10); {
		case kind < 5: // the service's own write, a local transaction of the room's
			writeInRoom(t, service, artifactID, "the storm's local write", serviceWrite, func(fragment *crdt.YXmlFragment) func(*crdt.Transaction) {
				text := focused(textsUnder(fragment, true))
				if text == nil {
					return nil
				}
				name, change := textOp(text)
				if change == nil {
					return nil
				}
				made = "local " + name
				return change
			})
		case kind < 9: // a connected browser's update, which the room applies as remote
			bob.sync()
			text := focused(bob.markableTexts())
			if text == nil {
				break
			}
			name, change := textOp(text)
			if change == nil {
				break
			}
			made = "browser " + name
			bob.send(change)
		default: // a committed live write of the document API, forked and published into the room
			find, with := "iota", "**iota**"
			if bolded {
				find, with = "**iota**", "iota"
			}
			if strings.Count(renderOfRoom(t, live), find) != 1 {
				break
			}
			if _, err := joinedApplyOps(service, artifactID,
				[]model.EditOp{{Op: "replace", Find: find, With: with}}, reuseAgent, nil); err != nil {
				t.Fatalf("step %d: the document API's replace of %q: %v", step, find, err)
			}
			bolded, made = !bolded, "api replace"
		}
		if made == "" {
			kinds["skipped"]++
			continue
		}
		kinds[made]++
		replicaHoldsTheRoom(t, live, replica, fmt.Sprintf("step %d, %s", step, made))
	}

	waitForLandedAppends(t, service, artifactID)
	sh.check(t)
	t.Logf("seed %d: %v", seed, kinds)
	if kinds["local format"] == 0 || kinds["browser format"] == 0 {
		t.Errorf("seed %d wrote no formatting of one of the two kinds (%v): the round proved nothing", seed, kinds)
	}
}

// formattingDocument is a small document for the formatting cases: the run the fixed case works
// on, seeded bold and italic, paragraphs and a list the storm types and marks into, the word the
// document API's replace bolds, and a code block, which carries no mark.
func formattingDocument() string {
	return "# Formatting\n\n**_" + formattingRun + "_**\n\nAlpha beta gamma.\n\n- Delta epsilon\n- Zeta eta\n\n```\ncode stays plain\n```\n\nTheta iota kappa.\n"
}

// newFormattingRoom is a service whose settlement waits an hour, with renders watched from before
// the formatting document is seeded, its room loaded, and its replica started and caught up by one
// browser edit.
func newFormattingRoom(t *testing.T) (*Service, string, *renderShadow, *crdt.Doc, *renderedReplica) {
	t.Helper()
	service, artifactID := newTestService(t)
	service.settle = time.Hour
	sh := watchRenders(t, service, artifactID)
	seedServiceText(t, service, artifactID, formattingDocument())
	editAsConnectedPeer(t, service, artifactID, 1, reuseAlice, replaceRun("Alpha beta", "Alpha beta beta"))
	waitForLandedAppends(t, service, artifactID)
	live := service.srv.GetDoc(artifactID)
	replica := service.replicaOf(live)
	if replica == nil || replica.doc == nil || replica.foreign.Load() != 1 {
		t.Fatal("the warm-up edit left no replica standing at one foreign update")
	}
	sh.check(t)
	return service, artifactID, sh, live, replica
}

// roomCommit names which of the two kinds of local transaction writeInRoom commits on the room.
type roomCommit int

const (
	// serviceWrite is one of the service's own in-room writes (Server.Apply, serviceTransact),
	// the way Dispatch's in-room writes commit: a local transaction of the room's, which ygo
	// runs no format cleanup after and whose origin the room's observer reads as the service's.
	serviceWrite roomCommit = iota
	// noOrigin is the way ygo's own formatting cleanup commits: doc.Transact with no origin
	// argument at all (crdt.Doc.Transact takes the origin variadically). Server.Apply captures
	// only what its own transact helper commits, so such a transaction is not captured and Apply
	// answers ErrNoChanges - which is the point of it: the commit reaches the room's update
	// observers, and ygo's persistence, naming no one.
	noOrigin
)

// writeInRoom commits change on the room's live document, as the commit kind names. prepare runs
// before the transaction opens and answers the change to commit, or nil for nothing to write:
// reading a shared type takes the document's lock, which the open transaction holds, so the nodes
// a change writes are found first. made names the write in a failure.
func writeInRoom(t *testing.T, service *Service, artifactID, made string, kind roomCommit, prepare func(fragment *crdt.YXmlFragment) func(*crdt.Transaction)) {
	t.Helper()
	err := service.srv.Apply(context.Background(), artifactID, func(doc *crdt.Doc, transact func(func(*crdt.Transaction))) {
		change := prepare(doc.GetXmlFragment(fragmentName))
		if change == nil {
			return
		}
		if kind == noOrigin {
			doc.Transact(change)
			return
		}
		wrapped, release := service.serviceTransact(transact)
		defer release()
		wrapped(change)
	})
	if err != nil && !errors.Is(err, ygws.ErrNoChanges) {
		t.Fatalf("write %s into the room: %v", made, err)
	}
}

// allTextRuns is the runs and marks of every text node under doc's fragment, in document order.
func allTextRuns(doc *crdt.Doc) []string {
	texts := textsUnder(doc.GetXmlFragment(fragmentName), false)
	runs := make([]string, len(texts))
	for index, text := range texts {
		runs[index] = textRuns(text)
	}
	return runs
}

// textRuns is a text node's delta - its runs and the marks each carries, with every mark's value
// (crdt.YXmlText.ToDelta) - written so a failure reads as the formatting itself.
func textRuns(text *crdt.YXmlText) string {
	var out strings.Builder
	for _, run := range text.ToDelta() {
		fmt.Fprintf(&out, "%q", fmt.Sprint(run.Insert))
		for _, mark := range slices.Sorted(maps.Keys(run.Attributes)) {
			fmt.Fprintf(&out, "+%s=%v", mark, run.Attributes[mark])
		}
		out.WriteString(" ")
	}
	return out.String()
}

// marksAt is the marks the text carries at index, as of now: those of the run that holds it, or
// of the last run for an index at the text's end. It reads the text outside any transaction
// (crdt.YText.ToDelta), as its callers do. A run's length is counted in bytes, which is ygo's
// index for the ASCII the document here holds (formattingDocument, formattingWords).
func marksAt(text *crdt.YXmlText, index int) crdt.Attributes {
	at := 0
	marks := crdt.Attributes{}
	for _, run := range text.ToDelta() {
		marks = run.Attributes
		inserted, ok := run.Insert.(string)
		if !ok {
			at++
		} else {
			at += len(inserted)
		}
		if at > index {
			break
		}
	}
	return marks
}

// markedRunOf is a run of the text that carries marks, drawn at random as the text stands.
// Deleting one strands the markers that open and close it in one gap, where the cleanup calls the
// first of each key redundant - it is the editing a user does, bold a word and then change their
// mind - and a storm that deleted only at random would reach that gap by chance. The offsets are
// read afresh for every delete, since every other write moves them.
func markedRunOf(rng *rand.Rand, text *crdt.YXmlText) (at, run int, ok bool) {
	var runs [][2]int
	for _, delta := range text.ToDelta() {
		length := 1
		if inserted, isText := delta.Insert.(string); isText {
			length = len(inserted)
		}
		if len(delta.Attributes) > 0 {
			runs = append(runs, [2]int{at, length})
		}
		at += length
	}
	if len(runs) == 0 {
		return 0, 0, false
	}
	found := runs[rng.IntN(len(runs))]
	return found[0], found[1], true
}

// roomText is the room's text node whose text starts with prefix.
func roomText(t *testing.T, live *crdt.Doc, prefix string) *crdt.YXmlText {
	t.Helper()
	return xmlTextWithPrefix(t, live.GetXmlFragment(fragmentName), prefix)
}

// deleteDifference names the clients whose tombstoned runs the replica and the room disagree on,
// with both sides' runs: a cleanup the replica ran alone tombstones the markers of one write, and
// naming them reads better than two whole delete sets of a storm's document.
func deleteDifference(replica, room *crdt.IDSet) string {
	clients := map[crdt.ClientID]bool{}
	for _, client := range append(replica.Clients(), room.Clients()...) {
		clients[client] = true
	}
	var out strings.Builder
	for _, client := range slices.Sorted(maps.Keys(clients)) {
		mine, theirs := fmt.Sprint(replica.Ranges(client)), fmt.Sprint(room.Ranges(client))
		if mine == theirs {
			continue
		}
		fmt.Fprintf(&out, "client %d: the replica %s, the room %s; ", client, mine, theirs)
	}
	return out.String()
}
