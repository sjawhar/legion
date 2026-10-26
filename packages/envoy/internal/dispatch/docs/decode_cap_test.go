package docs

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"maps"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/reearth/ygo/crdt"

	"github.com/sjawhar/envoy/internal/dispatch/model"
	"github.com/sjawhar/envoy/internal/dispatch/pmdoc"
	"github.com/sjawhar/envoy/internal/dispatch/store/storetest"
)

// captureLogs sends the service's log to the returned sink until the test ends, so a test can read
// what a write logged as well as what it left in the document.
func captureLogs(t *testing.T) *storetest.LockedLog {
	t.Helper()
	logs := &storetest.LockedLog{}
	previous := slog.Default()
	slog.SetDefault(slog.New(slog.NewTextHandler(logs, &slog.HandlerOptions{Level: slog.LevelInfo})))
	t.Cleanup(func() { slog.SetDefault(previous) })
	return logs
}

// roomDocument is artifactID's room document, which the test must already have loaded.
func roomDocument(t *testing.T, service *Service, artifactID string) *crdt.Doc {
	t.Helper()
	room := service.srv.GetDoc(artifactID)
	if room == nil {
		t.Fatal("document room is not resident")
	}
	return room
}

// deliver applies what browser has written since synced to room, as a browser's edit reaches it.
func deliver(t *testing.T, room, browser *crdt.Doc, synced crdt.StateVector) {
	t.Helper()
	if err := crdt.ApplyUpdateV1(room, crdt.EncodeStateAsUpdateV1(browser, synced), "peer"); err != nil {
		t.Fatalf("apply the browser's edit to the room: %v", err)
	}
}

// typeAndDelete has browser type n characters one at a time at the front of text, in one edit,
// and delete them all in another, and delivers both to room. Each character is a struct of its
// own, in the browser and in the room, since ygo merges a struct only into the one on its left
// with the clock before it (crdt/transaction.go:362-407), and the deletion leaves each where it
// is, so the room's struct total grows by n and its rendering is as it was. Each insert at the
// front walks past the deleted characters already there, so a second call on one text walks the
// first call's n characters for each of its own.
func typeAndDelete(t *testing.T, room, browser *crdt.Doc, text *crdt.YXmlText, n int) {
	t.Helper()
	synced := browser.StateVector()
	browser.Transact(func(txn *crdt.Transaction) {
		for range n {
			text.Insert(txn, 0, "x", nil)
		}
	})
	browser.Transact(func(txn *crdt.Transaction) { text.Delete(txn, 0, n) })
	deliver(t, room, browser, synced)
}

// pushUndecodable carries artifactID's live document past the decode cap in fact: its whole state
// holds more structs than ygo decodes in one update. A browser types maxUpdateItems/2+1 characters
// one at a time at the front of the first text, in each of two edits, and deletes them all in a
// third, each delivered as its own update, short of the cap, so the room takes every one, as it
// takes a long history arriving edit by edit (typeAndDelete gives why each character stays a
// struct). The document renders as it did. It returns the room's document.
func pushUndecodable(t *testing.T, service *Service, artifactID string) *crdt.Doc {
	t.Helper()
	room := roomDocument(t, service, artifactID)
	browser, text := peerDocument(t, room)
	typed := 0
	for range 2 {
		synced := browser.StateVector()
		browser.Transact(func(txn *crdt.Transaction) {
			for range maxUpdateItems/2 + 1 {
				text.Insert(txn, 0, "x", nil)
				typed++
			}
		})
		deliver(t, room, browser, synced)
	}
	synced := browser.StateVector()
	browser.Transact(func(txn *crdt.Transaction) { text.Delete(txn, 0, typed) })
	deliver(t, room, browser, synced)
	if clocks, structs := clockSum(room), structCount(t, room); clocks <= maxUpdateItems || structs <= maxUpdateItems {
		t.Fatalf("the room's document sums to %d clocks and holds %d structs, want both past the %d cap", clocks, structs, maxUpdateItems)
	}
	return room
}

// pushToTheCap carries artifactID's live document to the decode cap exactly: a browser types and
// deletes characters at the front of the first text (typeAndDelete) until the room's struct total
// (countRoomStructs) is maxUpdateItems, the most a document can hold and still decode. Every write
// that adds a struct to it would carry it past. It returns the room's document.
func pushToTheCap(t *testing.T, service *Service, artifactID string) *crdt.Doc {
	t.Helper()
	room := roomDocument(t, service, artifactID)
	browser, text := peerDocument(t, room)
	for structs := countRoomStructs(room); structs < maxUpdateItems; structs = countRoomStructs(room) {
		typeAndDelete(t, room, browser, text, int(maxUpdateItems-structs))
	}
	if structs, decoded := countRoomStructs(room), structCount(t, room); structs != maxUpdateItems || decoded != maxUpdateItems {
		t.Fatalf("the room's struct total is %d and its state %d structs, want both at the %d cap", structs, decoded, maxUpdateItems)
	}
	return room
}

// parkToTheCap carries artifactID's live document to the decode cap exactly with structs it parks.
// A browser's first edit never reaches the room, and every keystroke after it then waits in the
// room for that one (crdt.Doc.PendingStats): each is encoded into the room's state and can split
// two structs once what it waits for arrives (countRoomStructs), but none of them is in the
// room's state vector. So the room's struct total reaches the cap while its clocks sum to a few
// hundred: the clock sum alone, the filter the three writes answered on before, would have let
// each of them write. A few keystrokes the room does take, typed and deleted (typeAndDelete), top
// the total up to the cap exactly; they come from a browser that copied the room before anything
// parked, since a copy taken after parks the same keystrokes and sends them again with its own.
// It returns the room's document.
func parkToTheCap(t *testing.T, service *Service, artifactID string) *crdt.Doc {
	t.Helper()
	room := roomDocument(t, service, artifactID)
	browser, browserText := peerDocument(t, room)
	parker, text := peerDocument(t, room)
	parker.Transact(func(txn *crdt.Transaction) { text.Insert(txn, 0, "lost", nil) })
	synced := parker.StateVector()
	parked := int(maxUpdateItems-countRoomStructs(room))/(1+splitsAStructCanCause) - 8
	parker.Transact(func(txn *crdt.Transaction) {
		for range parked {
			text.Insert(txn, 0, "x", nil)
		}
	})
	deliver(t, room, parker, synced)
	if stats := room.PendingStats(); stats.Items != parked {
		t.Fatalf("the room parks %d structs, want the %d keystrokes whose first edit never arrived", stats.Items, parked)
	}
	for structs := countRoomStructs(room); structs < maxUpdateItems; structs = countRoomStructs(room) {
		typeAndDelete(t, room, browser, browserText, int(maxUpdateItems-structs))
	}
	if structs, clocks := countRoomStructs(room), clockSum(room); structs != maxUpdateItems || clocks > maxUpdateItems/64 {
		t.Fatalf("the room's struct total is %d and its clocks sum to %d, want the %d cap and a sum far short of it", structs, clocks, maxUpdateItems)
	}
	t.Logf("the room's struct total is the %d cap, with %d structs parked, and its clocks sum to %d", maxUpdateItems, room.PendingStats().Items, clockSum(room))
	return room
}

// quarterMebibyte is seed followed by paragraphs of ordinary words until it is 256 KiB of
// markdown, about two thousand elements, well within what one upload may hold.
func quarterMebibyte(seed string) string {
	padding := strings.Repeat("word ", 199) + "word"
	var markdown strings.Builder
	markdown.WriteString(seed)
	for paragraph := 0; markdown.Len() < 256<<10; paragraph++ {
		fmt.Fprintf(&markdown, "\n\nParagraph %d runs on in ordinary words: %s.", paragraph, padding)
	}
	markdown.WriteString("\n")
	return markdown.String()
}

// pushPastClockSum carries artifactID's live document, a quarter of a mebibyte
// (quarterMebibyte), past the decode cap by its clock sum alone, the way a busy document gets
// there: a browser types a mebibyte across its paragraphs, a kibibyte at a time, deleting each run
// once it has typed it, and the room takes the session in one update. Its clocks then sum past
// maxUpdateItems, since the sum counts characters, while its struct total stays far short of it,
// since each run is one struct. It returns the room's document.
func pushPastClockSum(t *testing.T, service *Service, artifactID string) *crdt.Doc {
	t.Helper()
	room := roomDocument(t, service, artifactID)
	browser, _ := peerDocument(t, room)
	texts := paragraphTexts(browser)
	if len(texts) == 0 {
		t.Fatal("the document holds no paragraph to type into")
	}
	run := strings.Repeat("typed ", 1<<10)[:1<<10]
	synced := browser.StateVector()
	for kibibyte := range 1 << 10 {
		text := texts[kibibyte*7%len(texts)]
		browser.Transact(func(txn *crdt.Transaction) { text.Insert(txn, 0, run, nil) })
		browser.Transact(func(txn *crdt.Transaction) { text.Delete(txn, 0, len(run)) })
	}
	deliver(t, room, browser, synced)
	clocks, structs := clockSum(room), countRoomStructs(room)
	if clocks <= maxUpdateItems || structs > maxUpdateItems/8 {
		t.Fatalf("the room's document sums to %d clocks and holds %d structs, want past the cap and far short of it", clocks, structs)
	}
	t.Logf("the room's document sums to %d clocks, past the %d cap, and holds %d structs", clocks, maxUpdateItems, structs)
	return room
}

// requireLeftAsItWas requires the room to hold before, the document as the test held it before the
// write it drove, byte for byte, in the same room.
func requireLeftAsItWas(t *testing.T, service *Service, artifactID string, room *crdt.Doc, before []byte) {
	t.Helper()
	if now := service.srv.GetDoc(artifactID); now != room {
		t.Fatal("the room's document was replaced, so the write's effect on it cannot be read")
	}
	if after := crdt.EncodeStateAsUpdateV1(room, nil); !bytes.Equal(after, before) {
		t.Fatalf("the document encodes to %d bytes after the write, %d before: the write changed it", len(after), len(before))
	}
}

// requireWrittenTo requires the room to hold something other than before, the document as the test
// held it before the write it drove, in the same room.
func requireWrittenTo(t *testing.T, service *Service, artifactID string, room *crdt.Doc, before []byte) {
	t.Helper()
	if now := service.srv.GetDoc(artifactID); now != room {
		t.Fatal("the room's document was replaced, so the write's effect on it cannot be read")
	}
	if after := crdt.EncodeStateAsUpdateV1(room, nil); bytes.Equal(after, before) {
		t.Fatalf("the document encodes to the same %d bytes after the write as before: the write left it alone", len(after))
	}
}

// requireRoomNotFailed requires the room to have counted no settle failure and not to have failed:
// three failures in a row fail the room (maxSettleFailures).
func requireRoomNotFailed(t *testing.T, service *Service, artifactID string) {
	t.Helper()
	state := service.room(artifactID)
	state.mu.Lock()
	defer state.mu.Unlock()
	if state.settleFailures != 0 || state.failed != nil {
		t.Fatalf("the room counted %d settle failures and failed with %v, want none", state.settleFailures, state.failed)
	}
}

// writeCounts records the counts the service's measurer makes of server writes' documents against
// the decode cap (elementMeasurer.measuredWrite): how many it made, and how many of them went on
// to copy the document and make the write on the copy.
type writeCounts struct{ counted, copied atomic.Int64 }

// recordWriteCounts records service's counts of its writes from now on.
func recordWriteCounts(service *Service) *writeCounts {
	counts := &writeCounts{}
	service.measurer.measuredWrite = func(copied bool) {
		counts.counted.Add(1)
		if copied {
			counts.copied.Add(1)
		}
	}
	return counts
}

// requireCounted requires the writes to have been counted at least once, and to have copied the
// document exactly when copied says: so the answer came from the step the test means it to.
func requireCounted(t *testing.T, counts *writeCounts, copied bool) {
	t.Helper()
	if counted, copies := counts.counted.Load(), counts.copied.Load(); counted == 0 || (copies > 0) != copied {
		t.Fatalf("the writes were counted %d times and copied the document %d times, want counted, and copied: %t", counted, copies, copied)
	}
}

// requireNeverCounted requires no write to have been counted: the ceiling of the document's struct
// total, which costs no encode, answered each one (structCeiling).
func requireNeverCounted(t *testing.T, counts *writeCounts) {
	t.Helper()
	if counted := counts.counted.Load(); counted != 0 {
		t.Fatalf("the writes were counted %d times (%d of them on a copy), want answered without a count", counted, counts.copied.Load())
	}
}

// requireLoggedOnce requires write to have recorded on the room's state that it left the document
// alone at the decode cap (logPastDecodeCap), which is what keeps every later pass from logging it
// again, and so also that the write reached its refusal rather than stopping short of it, and to
// have logged it once.
func requireLoggedOnce(t *testing.T, service *Service, artifactID, write string, logs *storetest.LockedLog) {
	t.Helper()
	state := service.room(artifactID)
	state.mu.Lock()
	_, recorded := state.pastDecodeCap[write]
	recordedWrites := maps.Clone(state.pastDecodeCap)
	state.mu.Unlock()
	if !recorded {
		t.Fatalf("the %s never recorded leaving the document alone at the decode cap; it recorded %v", write, recordedWrites)
	}
	if lines := strings.Count(logs.Lines("leave a document the write would carry past the decode cap"), fmt.Sprintf("write=%q", write)); lines != 1 {
		t.Fatalf("the %s logged leaving the document alone at the decode cap %d times, want once:\n%s", write, lines, logs)
	}
}

// requireNeverLogged requires write never to have recorded or logged leaving the document alone
// at the decode cap (logPastDecodeCap).
func requireNeverLogged(t *testing.T, service *Service, artifactID, write string, logs *storetest.LockedLog) {
	t.Helper()
	state := service.room(artifactID)
	state.mu.Lock()
	_, recorded := state.pastDecodeCap[write]
	state.mu.Unlock()
	if lines := logs.Lines("leave a document the write would carry past the decode cap"); recorded || lines != "" {
		t.Fatalf("the %s recorded leaving the document alone at the decode cap (%v) and logged:\n%s", write, recorded, lines)
	}
}

// sweepableDocument is a document whose text "brown" carries a comment mark no comment row records,
// one the next settlement's sweep finds expired, with markdown seed as its text.
func sweepableDocument(t *testing.T, seed func(string) string) (*Service, string) {
	t.Helper()
	service, artifactID := newTestService(t)
	service.settle = time.Hour
	service.unrecordedMarkTTL = 0
	seedServiceText(t, service, artifactID, seed("The quick brown fox"))
	browserMark(t, service, artifactID, "proofComment", "dangling", "brown")
	return service, artifactID
}

// requireMarked requires the live document to carry the expired mark still, or no longer, as
// marked says.
func requireMarked(t *testing.T, service *Service, artifactID string, marked bool) {
	t.Helper()
	if _, _, found := pmdoc.FindMark(liveTree(t, service, artifactID), "proofComment", "dangling"); found != marked {
		t.Fatalf("the document carries the expired mark: %t, want %t", found, marked)
	}
}

// splitRuns is seed with the mark's text "brown" split into two runs, "br" and a bold "own", so
// the mark covers two runs and taking it off writes new format items: one to restore it after the
// first run and one to end it again where the second begins. A mark that is all its runs'
// formatting comes off by deleting the format items around it, which adds no struct.
func splitRuns(seed string) string {
	return strings.Replace(seed, "brown", "br**own**", 1)
}

// The sweep of unrecorded marks unmarks every expired mark in one transaction on the room, which
// cannot be rolled back once begun, and a mark covering more than one run writes new format items
// into the document as it comes off. On a document at the decode cap, or past it, or at it with
// structs it parks, which the clock sum does not see, it writes nothing: the mark stays, the
// document is left byte for byte as it was, the settlement it ends counts no failure, and every
// later settlement's pass finds the mark expired again without logging it again (LEGION-545). A
// document past the cap is refused on its own count, which no copy of it could be made from; one
// at it, on what the sweep added to a copy.
func TestTheMarkSweepLeavesADocumentAtOrPastTheDecodeCapAsItWas(t *testing.T) {
	for _, test := range []struct {
		name   string
		push   func(t *testing.T, service *Service, artifactID string) *crdt.Doc
		copied bool
	}{
		{"at the cap", pushToTheCap, true},
		{"past the cap", pushUndecodable, false},
		{"at the cap with parked structs", parkToTheCap, true},
	} {
		t.Run(test.name, func(t *testing.T) {
			logs := captureLogs(t)
			service, artifactID := sweepableDocument(t, splitRuns)
			room := test.push(t, service, artifactID)
			before := crdt.EncodeStateAsUpdateV1(room, nil)
			counts := recordWriteCounts(service)

			for range 2 {
				settleCurrentGeneration(t, service, artifactID)
			}
			requireLeftAsItWas(t, service, artifactID, room, before)
			requireMarked(t, service, artifactID, true)
			requireRoomNotFailed(t, service, artifactID)
			requireCounted(t, counts, test.copied)
			requireLoggedOnce(t, service, artifactID, "unrecorded-mark sweep", logs)
		})
	}
}

// A mark that is all its run's formatting comes off by deleting the format items around it, which
// adds no struct, so the sweep takes it off a document at the decode cap: the copy it counted shows
// the sweep adding nothing, and the document it leaves is still within the cap.
func TestTheMarkSweepStillSweepsADocumentAtTheDecodeCapWhereItAddsNoStruct(t *testing.T) {
	logs := captureLogs(t)
	service, artifactID := sweepableDocument(t, func(seed string) string { return seed })
	room := pushToTheCap(t, service, artifactID)
	before := crdt.EncodeStateAsUpdateV1(room, nil)
	counts := recordWriteCounts(service)

	settleCurrentGeneration(t, service, artifactID)
	requireWrittenTo(t, service, artifactID, room, before)
	requireMarked(t, service, artifactID, false)
	if structs := countRoomStructs(room); structs > maxUpdateItems {
		t.Fatalf("the sweep left the document's struct total at %d, past the %d cap", structs, maxUpdateItems)
	}
	requireRoomNotFailed(t, service, artifactID)
	requireCounted(t, counts, true)
	requireNeverLogged(t, service, artifactID, "unrecorded-mark sweep", logs)
}

// A document whose clocks sum past the decode cap only because people typed and deleted a lot in it
// holds far fewer structs than the cap, and the sweep still sweeps it: the mark comes off, which
// changes the document, and the sweep neither records nor logs leaving it alone. Its clocks are past
// the ceiling the sweep asks first, so the document is counted, but the count with what the sweep
// adds at most answers without a copy.
func TestTheMarkSweepStillSweepsADocumentOnlyItsClockSumPutsPastTheDecodeCap(t *testing.T) {
	logs := captureLogs(t)
	service, artifactID := sweepableDocument(t, quarterMebibyte)
	room := pushPastClockSum(t, service, artifactID)
	before := crdt.EncodeStateAsUpdateV1(room, nil)
	counts := recordWriteCounts(service)

	settleCurrentGeneration(t, service, artifactID)
	requireWrittenTo(t, service, artifactID, room, before)
	requireMarked(t, service, artifactID, false)
	requireRoomNotFailed(t, service, artifactID)
	requireCounted(t, counts, false)
	requireNeverLogged(t, service, artifactID, "unrecorded-mark sweep", logs)
}

// unstampedDocument is a settled document with markdown seed as its text, to which a browser edit
// has then added blocks without block ids, which the next stamp mints ids for.
func unstampedDocument(t *testing.T, seed func(string) string) (*Service, string) {
	t.Helper()
	service, artifactID := newTestService(t)
	service.settle = time.Hour
	seedServiceText(t, service, artifactID, seed("before"))
	settleCurrentGeneration(t, service, artifactID)
	editLiveTree(t, service, artifactID, appendUnidentifiedBlocks(t, "added"))
	return service, artifactID
}

// stampWrite is one of the two writes that run the block-id stamp, and what it reports of it: a
// settlement, which must reach its reconciliation whether it stamped or not, and the backfill,
// whose report on the document is returned for the test to judge.
type stampWrite struct {
	name  string
	write string
	stamp func(t *testing.T, service *Service, artifactID string) *BlockIDBackfill
}

var stampWrites = []stampWrite{
	{
		name:  "settlement",
		write: "settlement's block-id stamp",
		stamp: func(t *testing.T, service *Service, artifactID string) *BlockIDBackfill {
			reconciled := false
			service.afterSettleReconcile = func(room string) { reconciled = reconciled || room == artifactID }
			settleCurrentGeneration(t, service, artifactID)
			if !reconciled {
				t.Fatal("the settlement ended before it reconciled the document")
			}
			requireRoomNotFailed(t, service, artifactID)
			return nil
		},
	},
	{
		name:  "the backfill",
		write: "block-id backfill",
		stamp: func(t *testing.T, service *Service, artifactID string) *BlockIDBackfill {
			reports, err := service.BackfillBlockIDs(context.Background())
			if err != nil {
				t.Fatalf("backfill block ids: %v", err)
			}
			for _, report := range reports {
				if report.ArtifactID == artifactID {
					return &report
				}
			}
			t.Fatalf("the backfill reports %+v, none of them for the document", reports)
			return nil
		},
	},
}

// The block-id stamp mints an id, a new attribute item, for every block a browser edit left without
// one or with another's. On a document at the decode cap, or past it, it writes nothing, whether
// settlement or the backfill runs it, and the document is left byte for byte as it was: settlement
// carries on with the ids unrepaired and counts no failure, and the backfill reports the document
// skipped (LEGION-545).
func TestTheBlockIDStampLeavesADocumentAtOrPastTheDecodeCapAsItWas(t *testing.T) {
	for _, document := range []struct {
		name   string
		push   func(t *testing.T, service *Service, artifactID string) *crdt.Doc
		copied bool
	}{
		{"at the cap", pushToTheCap, true},
		{"past the cap", pushUndecodable, false},
	} {
		for _, test := range stampWrites {
			t.Run(document.name+", "+test.name, func(t *testing.T) {
				logs := captureLogs(t)
				service, artifactID := unstampedDocument(t, func(seed string) string { return seed })
				room := document.push(t, service, artifactID)
				before := crdt.EncodeStateAsUpdateV1(room, nil)
				counts := recordWriteCounts(service)

				if report := test.stamp(t, service, artifactID); report != nil &&
					(report.Err != nil || report.Stamped != 0 || report.Skipped != "the stamp would carry the document past the decode cap") {
					t.Fatalf("the backfill reports %+v, want the document skipped at the decode cap", *report)
				}
				requireLeftAsItWas(t, service, artifactID, room, before)
				if pmdoc.BlockIDRepairCount(liveTree(t, service, artifactID)) == 0 {
					t.Fatal("a block of a document at the decode cap was stamped")
				}
				requireCounted(t, counts, document.copied)
				requireLoggedOnce(t, service, artifactID, test.write, logs)
			})
		}
	}
}

// A document whose clocks sum past the decode cap only because people typed and deleted a lot in it
// holds far fewer structs than the cap, and the block-id stamp still stamps it, whether settlement
// or the backfill runs it: every block gets its id, which changes the document, the backfill
// reports the ids it stamped, and neither records nor logs leaving the document alone. The count
// with what the stamp adds at most answers without a copy.
func TestTheBlockIDStampStillStampsADocumentOnlyItsClockSumPutsPastTheDecodeCap(t *testing.T) {
	for _, test := range stampWrites {
		t.Run(test.name, func(t *testing.T) {
			logs := captureLogs(t)
			service, artifactID := unstampedDocument(t, quarterMebibyte)
			room := pushPastClockSum(t, service, artifactID)
			before := crdt.EncodeStateAsUpdateV1(room, nil)
			counts := recordWriteCounts(service)

			if report := test.stamp(t, service, artifactID); report != nil &&
				(report.Err != nil || report.Stamped == 0 || report.Skipped != "") {
				t.Fatalf("the backfill reports %+v, want the document's blocks stamped", *report)
			}
			requireWrittenTo(t, service, artifactID, room, before)
			if repairs := pmdoc.BlockIDRepairCount(liveTree(t, service, artifactID)); repairs != 0 {
				t.Fatalf("%d blocks of a document that decodes were left unstamped", repairs)
			}
			requireCounted(t, counts, false)
			requireNeverLogged(t, service, artifactID, test.write, logs)
		})
	}
}

// answeredDocument is a document with markdown seed around an ask block whose ask is answered
// with text, and whose block says who answered and when but holds no answer: what a settlement
// that withheld the answer leaves (setAskServerAttributes), and what the next settlement writes
// the answer back into.
func answeredDocument(t *testing.T, seed func(string) string, text string) (*Service, string) {
	t.Helper()
	service, artifactID := newTestService(t)
	service.settle = time.Hour
	seedServiceText(t, service, artifactID, seed(":::ask{#ask-block urgency=\"med\" multiple=\"false\" state=\"open\"}\nShip it?\n:::\n\nContext."))
	service.settleRoom(artifactID, 0)

	answer := model.AskAnswer{User: "alice", Selected: []string{}, Text: &text, At: time.Now().UTC()}
	answerJSON, err := json.Marshal(answer)
	if err != nil {
		t.Fatalf("encode answer: %v", err)
	}
	if tag, err := service.store.Pool.Exec(context.Background(), `
		update asks set state = 'answered', answer = $2 where block_artifact_id = $1
	`, artifactID, answerJSON); err != nil || tag.RowsAffected() != 1 {
		t.Fatalf("answer the document's ask: %v, %d rows", err, tag.RowsAffected())
	}
	editAsPeer(t, service, artifactID, func(tree *pmdoc.Node) *pmdoc.Node {
		pmdoc.Walk(tree, func(node *pmdoc.Node) bool {
			if node.Type == "ask" {
				setAskServerAttributes(node, model.Ask{State: "answered", Answer: &answer}, "", true)
			}
			return true
		})
		return tree
	})
	return service, artifactID
}

// settleAnswered runs a settlement of the answered document and requires it to reach its
// reconciliation of the answered block and to count no failure.
func settleAnswered(t *testing.T, service *Service, artifactID string) {
	t.Helper()
	reconciled := false
	service.afterSettleReconcile = func(room string) { reconciled = reconciled || room == artifactID }
	settleCurrentGeneration(t, service, artifactID)
	if !reconciled {
		t.Fatal("the settlement ended before it reconciled the answered block")
	}
	requireRoomNotFailed(t, service, artifactID)
}

// withheldAnswerLines is what settlement logged of withholding answers from their blocks
// (withholdAnswers).
func withheldAnswerLines(logs *storetest.LockedLog) string {
	return logs.Lines("settlement withholds restored answers from their blocks")
}

// An answer is stored on its ask as well as in its block, and settlement writes it back into a
// block that holds the rest of its ask's state without it - a returning block, or one whose answer
// a full document left out - which adds the answer to the document as a new item. On a document at
// the decode cap, or past it, settlement withholds it, as it withholds an answer a full document
// has no room for: the block still says who answered and when, the ask keeps the answer, the
// document is left byte for byte as it was, settlement logs that it withheld the answer and why,
// and the settlement counts no failure, so the room it runs on does not fail (LEGION-545).
func TestSettlementWithholdsAnAnswerFromADocumentAtOrPastTheDecodeCapAndCountsNoFailure(t *testing.T) {
	for _, document := range []struct {
		name   string
		push   func(t *testing.T, service *Service, artifactID string) *crdt.Doc
		copied bool
	}{
		{"at the cap", pushToTheCap, true},
		{"past the cap", pushUndecodable, false},
	} {
		t.Run(document.name, func(t *testing.T) {
			logs := captureLogs(t)
			text := "Ship it on Friday."
			service, artifactID := answeredDocument(t, func(seed string) string { return seed + "\n" }, text)
			room := document.push(t, service, artifactID)
			before := crdt.EncodeStateAsUpdateV1(room, nil)
			counts := recordWriteCounts(service)

			settleAnswered(t, service, artifactID)
			requireLeftAsItWas(t, service, artifactID, room, before)
			if lines := withheldAnswerLines(logs); !strings.Contains(lines, "withheld=1") || !strings.Contains(lines, "holding more than the 1048576 items") {
				t.Fatalf("settlement logged, of withholding answers:\n%s\nwant the one answer withheld at the decode cap", lines)
			}
			requireCounted(t, counts, document.copied)
			var kept model.AskAnswer
			var stored []byte
			if err := service.store.Pool.QueryRow(context.Background(), `
				select answer from asks where block_artifact_id = $1
			`, artifactID).Scan(&stored); err != nil {
				t.Fatalf("read the ask's answer: %v", err)
			}
			if err := json.Unmarshal(stored, &kept); err != nil || kept.Text == nil || *kept.Text != text {
				t.Fatalf("the ask whose answer settlement withheld holds %s (%v), want its answer", stored, err)
			}
		})
	}
}

// A document whose clocks sum past the decode cap only because people typed and deleted a lot in it
// holds far fewer structs than the cap, and settlement still writes an answer back into its block
// there: the document renders the answer, which changes it, and settlement logs no answer withheld.
// The count with what the repairs add at most answers without a copy.
func TestSettlementStillReturnsAnAnswerToADocumentOnlyItsClockSumPutsPastTheDecodeCap(t *testing.T) {
	logs := captureLogs(t)
	text := "Ship it on Friday."
	service, artifactID := answeredDocument(t, quarterMebibyte, text)
	room := pushPastClockSum(t, service, artifactID)
	before := crdt.EncodeStateAsUpdateV1(room, nil)
	counts := recordWriteCounts(service)

	settleAnswered(t, service, artifactID)
	requireWrittenTo(t, service, artifactID, room, before)
	requireRendersAnswer(t, service, artifactID, text)
	if lines := withheldAnswerLines(logs); lines != "" {
		t.Fatalf("settlement withheld an answer from a document that decodes:\n%s", lines)
	}
	requireCounted(t, counts, false)
}

// requireRendersAnswer requires the live document to render text, the answer settlement returned.
func requireRendersAnswer(t *testing.T, service *Service, artifactID, text string) {
	t.Helper()
	markdown, err := renderTree(liveTree(t, service, artifactID))
	if err != nil {
		t.Fatalf("render the live document: %v", err)
	}
	if !strings.Contains(markdown, text) {
		t.Fatal("the document renders no answer after settlement")
	}
}

// Each of the three writes on a document well short of the decode cap is answered by the ceiling of
// its struct total, the clock sum with what it parks, and what the write adds at most
// (structCeiling): the document is never counted and never copied, and the write writes.
func TestServerWritesWellShortOfTheDecodeCapAreNeverCounted(t *testing.T) {
	t.Run("the sweep", func(t *testing.T) {
		service, artifactID := sweepableDocument(t, func(seed string) string { return seed })
		counts := recordWriteCounts(service)
		settleCurrentGeneration(t, service, artifactID)
		requireMarked(t, service, artifactID, false)
		requireNeverCounted(t, counts)
	})
	for _, test := range stampWrites {
		t.Run("the stamp, "+test.name, func(t *testing.T) {
			service, artifactID := unstampedDocument(t, func(seed string) string { return seed })
			counts := recordWriteCounts(service)
			if report := test.stamp(t, service, artifactID); report != nil && (report.Err != nil || report.Stamped == 0) {
				t.Fatalf("the backfill reports %+v, want the document's blocks stamped", *report)
			}
			if repairs := pmdoc.BlockIDRepairCount(liveTree(t, service, artifactID)); repairs != 0 {
				t.Fatalf("%d blocks were left unstamped", repairs)
			}
			requireNeverCounted(t, counts)
		})
	}
	t.Run("settlement's repair of a returning answer", func(t *testing.T) {
		text := "Ship it on Friday."
		service, artifactID := answeredDocument(t, func(seed string) string { return seed + "\n" }, text)
		counts := recordWriteCounts(service)
		settleAnswered(t, service, artifactID)
		requireRendersAnswer(t, service, artifactID, text)
		requireNeverCounted(t, counts)
	})
}

// parkedFork is a document as a write's fork can hold it: parked structs of a browser's
// keystrokes, every one waiting for a first edit that never arrived, and a delete range of another
// browser's, waiting for the insert it deletes, none of which is in its state vector.
func parkedFork(t *testing.T, parked int) *crdt.Doc {
	t.Helper()
	writer := crdt.New(crdt.WithClientID(7))
	text := writer.GetText("t")
	writer.Transact(func(txn *crdt.Transaction) { text.Insert(txn, 0, "base", nil) })
	fork := newDocumentCopy()
	if err := crdt.ApplyUpdateV1(fork, crdt.EncodeStateAsUpdateV1(writer, nil), nil); err != nil {
		t.Fatalf("open the fork: %v", err)
	}
	other := crdt.New(crdt.WithClientID(8))
	if err := crdt.ApplyUpdateV1(other, crdt.EncodeStateAsUpdateV1(writer, nil), nil); err != nil {
		t.Fatalf("open the second writer: %v", err)
	}
	writer.Transact(func(txn *crdt.Transaction) { text.Insert(txn, 0, "lost", nil) })
	afterLost := writer.StateVector()
	writer.Transact(func(txn *crdt.Transaction) {
		for range parked {
			text.Insert(txn, 0, "x", nil)
		}
	})
	otherText := other.GetText("t")
	other.Transact(func(txn *crdt.Transaction) { otherText.Insert(txn, 0, "gone ", nil) })
	afterGone := other.StateVector()
	other.Transact(func(txn *crdt.Transaction) { otherText.Delete(txn, 0, 5) })
	for _, update := range [][]byte{crdt.EncodeStateAsUpdateV1(writer, afterLost), crdt.EncodeStateAsUpdateV1(other, afterGone)} {
		if err := crdt.ApplyUpdateV1(fork, update, nil); err != nil {
			t.Fatalf("park an update in the fork: %v", err)
		}
	}
	if stats := fork.PendingStats(); stats.Items != parked || stats.DeleteRanges == 0 {
		t.Fatalf("the fork parks %d structs and %d delete ranges, want %d and some", stats.Items, stats.DeleteRanges, parked)
	}
	return fork
}

// A write's fork whose parked structs take its struct total past the decode cap is refused, though
// its clocks sum to a handful: the parked structs and the skips before them are in the state a
// load decodes, and each can still split two structs once what it waits for arrives, but the state
// vector counts none of them. The clock sum alone, the filter refuseUndecodable answered on before,
// let such a fork through. The ceiling it answers on now (structCeiling) is never below the total,
// and one fork short of the cap is still let through.
func TestAWriteLeavingParkedStructsPastTheDecodeCapIsRefusedAsUndecodable(t *testing.T) {
	past := parkedFork(t, maxUpdateItems/(1+splitsAStructCanCause)+1)
	if clocks := clockSum(past); clocks > 16 {
		t.Fatalf("the fork's clocks sum to %d, want the handful its integrated text takes", clocks)
	}
	structs := countRoomStructs(past)
	if structs <= maxUpdateItems || structCeiling(past) < structs {
		t.Fatalf("the fork's struct total is %d and its ceiling %d, want the total past the %d cap and the ceiling at or above it", structs, structCeiling(past), maxUpdateItems)
	}
	err := refuseUndecodable(past)
	if !errors.Is(err, ErrDocumentTooLarge) || !strings.Contains(err.Error(), "this change would leave the document's live state past the 1048576 items ygo decodes in one update") {
		t.Fatalf("refuseUndecodable of a fork past the cap with parked structs: %v, want ErrDocumentTooLarge naming the cap", err)
	}

	short := parkedFork(t, 1000)
	if structs := countRoomStructs(short); structCeiling(short) < structs {
		t.Fatalf("the short fork's struct total is %d and its ceiling %d, want the ceiling at or above it", structs, structCeiling(short))
	}
	if err := refuseUndecodable(short); err != nil {
		t.Fatalf("refuseUndecodable of a fork far short of the cap: %v, want nil", err)
	}
}

// structCount is how many structs doc's whole state holds, as ygo's decoder counts them against
// maxUpdateItems: each client's structs encoded alone, whose update declares their count in its
// header (updateItems).
func structCount(tb testing.TB, doc *crdt.Doc) uint64 {
	tb.Helper()
	full := doc.StateVector()
	var structs uint64
	for client := range full {
		others := maps.Clone(full)
		delete(others, client)
		items, ok := updateItems(crdt.EncodeStateAsUpdateV1(doc, others))
		if !ok {
			tb.Fatalf("client %d's structs encode to an update of more than one client", client)
		}
		structs += items
	}
	return structs
}

// documentAtTheCap is a mebibyte of the markdown a room can hold, typed over
// (residentRoomDocument), with characters then typed one at a time at the front of its first
// paragraph and deleted again until its state holds exactly maxUpdateItems structs: the most a
// document can hold and still decode, so the heaviest document a server write's count can copy.
func documentAtTheCap(b *testing.B) *crdt.Doc {
	b.Helper()
	live := residentRoomDocument(b, 1<<20)
	texts := paragraphTexts(live)
	short := maxUpdateItems - int(structCount(b, live))
	live.Transact(func(txn *crdt.Transaction) {
		for range short {
			texts[0].Insert(txn, 0, "x", nil)
		}
	})
	live.Transact(func(txn *crdt.Transaction) { texts[0].Delete(txn, 0, short) })
	if structs, total := structCount(b, live), countRoomStructs(live); structs != maxUpdateItems || total != maxUpdateItems {
		b.Fatalf("the document holds %d structs and a struct total of %d, want both %d", structs, total, maxUpdateItems)
	}
	return live
}

// BenchmarkDecodeCapConfirmation times what a server write outside applyLive pays near the decode
// cap, on a mebibyte of the markdown a room can hold, typed over, whose clocks already sum past the
// cap, and on a document at the cap itself: the count it makes now (elementMeasurer.structsAfter)
// of a write a bound answers on the document's struct total alone ("count"), and of one it does
// not, which copies the document, types one character into the copy and counts it before and
// after ("copy-and-count"); and the encode-and-decode into a fresh document those replaced.
func BenchmarkDecodeCapConfirmation(b *testing.B) {
	typeOne := func(copied *crdt.Doc) {
		text := paragraphTexts(copied)[0]
		copied.Transact(func(txn *crdt.Transaction) { text.Insert(txn, 0, "x", nil) })
	}
	for _, document := range []struct {
		name  string
		build func(b *testing.B) *crdt.Doc
	}{
		{"1MiB", func(b *testing.B) *crdt.Doc { return residentRoomDocument(b, 1<<20) }},
		{"at-the-cap", documentAtTheCap},
	} {
		b.Run(document.name, func(b *testing.B) {
			live := document.build(b)
			clocks, structs, state := clockSum(live), countRoomStructs(live), len(crdt.EncodeStateAsUpdateV1(live, nil))
			for _, step := range []struct {
				name    string
				measure func(measurer *elementMeasurer) error
			}{
				{"count", func(measurer *elementMeasurer) error {
					if got := measurer.structsAfter(live, serverWrite{bounded: true, apply: typeOne}); got != structs {
						return fmt.Errorf("the write was counted to %d structs, want the document's %d", got, structs)
					}
					return nil
				}},
				{"copy-and-count", func(measurer *elementMeasurer) error {
					if got := measurer.structsAfter(live, serverWrite{apply: typeOne}); got <= structs {
						return fmt.Errorf("the write was counted to %d structs, want more than the document's %d", got, structs)
					}
					return nil
				}},
				{"encode-and-decode", func(*elementMeasurer) error {
					return crdt.ApplyUpdateV1(newDocumentCopy(), crdt.EncodeStateAsUpdateV1(live, nil), nil)
				}},
			} {
				b.Run(step.name, func(b *testing.B) {
					measurer := &elementMeasurer{}
					for b.Loop() {
						if err := step.measure(measurer); err != nil {
							b.Fatal(err)
						}
					}
					b.ReportMetric(float64(clocks), "clocks")
					b.ReportMetric(float64(structs), "structs")
					b.ReportMetric(float64(state), "state-bytes")
				})
			}
		})
	}
}
