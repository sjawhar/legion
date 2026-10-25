package docs

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"log/slog"
	"maps"
	"strings"
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

// pushUndecodable carries artifactID's live document past the decode cap in fact: its whole state
// holds more structs than ygo decodes in one update. A browser types maxUpdateItems/2+1 characters
// one at a time at the front of the first text, in each of two edits, and deletes them all in a
// third. Each character typed before the last is a struct of its own, since ygo merges a struct only
// into the one on its left with the clock before it (crdt/transaction.go:362-407), and each edit is
// delivered as its own update, short of the cap, so the room takes every one, as it takes a long
// history arriving edit by edit. The document renders as it did. It returns the room's document.
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
	if clocks := clockSum(room); clocks <= maxUpdateItems || stateDecodes(room) {
		t.Fatalf("the room's document sums to %d clocks and decodes %v, want past the cap and not decoding", clocks, stateDecodes(room))
	}
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
// maxUpdateItems, since the sum counts characters, while its whole state still decodes, since each
// run is one struct. It returns the room's document.
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
	clocks := clockSum(room)
	if clocks <= maxUpdateItems || !stateDecodes(room) {
		t.Fatalf("the room's document sums to %d clocks and decodes %v, want past the cap and decoding", clocks, stateDecodes(room))
	}
	t.Logf("the room's document sums to %d clocks, past the %d cap, and decodes", clocks, maxUpdateItems)
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
		t.Fatalf("the document past the decode cap encodes to %d bytes after the write, %d before: the write changed it", len(after), len(before))
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

// requireConfirmed requires the room's state to keep a confirmation of its document taken past the
// clock sum (Service.pastDecodeCap), answering that it decodes or that it does not, as decodes
// says: so the write asked the decode, not the sum alone.
func requireConfirmed(t *testing.T, service *Service, artifactID string, decodes bool) {
	t.Helper()
	state := service.room(artifactID)
	state.mu.Lock()
	defer state.mu.Unlock()
	if answer := state.decodeCap; answer == nil || answer.clocks <= maxUpdateItems || answer.undecodable == decodes {
		t.Fatalf("the room keeps the confirmation %+v, want one past the cap answering that the document decodes: %v", answer, decodes)
	}
}

// requireLoggedOnce requires write to have recorded on the room's state that it left the document
// alone past the decode cap (logPastDecodeCap), which is what keeps every later pass from logging
// it again, and so also that the write reached its refusal rather than stopping short of it, and
// to have logged it once.
func requireLoggedOnce(t *testing.T, service *Service, artifactID, write string, logs *storetest.LockedLog) {
	t.Helper()
	state := service.room(artifactID)
	state.mu.Lock()
	_, recorded := state.pastDecodeCap[write]
	recordedWrites := maps.Clone(state.pastDecodeCap)
	state.mu.Unlock()
	if !recorded {
		t.Fatalf("the %s never recorded leaving the document past the decode cap; it recorded %v", write, recordedWrites)
	}
	if lines := strings.Count(logs.Lines("leave a document past the decode cap"), fmt.Sprintf("write=%q", write)); lines != 1 {
		t.Fatalf("the %s logged leaving the document past the decode cap %d times, want once:\n%s", write, lines, logs)
	}
}

// requireNeverLogged requires write never to have recorded or logged leaving the document alone
// past the decode cap (logPastDecodeCap).
func requireNeverLogged(t *testing.T, service *Service, artifactID, write string, logs *storetest.LockedLog) {
	t.Helper()
	state := service.room(artifactID)
	state.mu.Lock()
	_, recorded := state.pastDecodeCap[write]
	state.mu.Unlock()
	if lines := logs.Lines("leave a document past the decode cap"); recorded || lines != "" {
		t.Fatalf("the %s recorded leaving the document past the decode cap (%v) and logged:\n%s", write, recorded, lines)
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

// The sweep of unrecorded marks unmarks every expired mark in one transaction on the room, which
// cannot be rolled back once begun, and each mark it takes off writes new format items into the
// document. On a document whose state already could not be decoded it writes nothing: the mark
// stays, the document is left byte for byte as it was, the settlement it ends counts no failure,
// and every later settlement's pass finds the mark expired again without logging it again
// (LEGION-545).
func TestTheMarkSweepLeavesADocumentPastTheDecodeCapAsItWas(t *testing.T) {
	logs := captureLogs(t)
	service, artifactID := sweepableDocument(t, func(seed string) string { return seed })
	room := pushUndecodable(t, service, artifactID)
	before := crdt.EncodeStateAsUpdateV1(room, nil)

	for range 2 {
		settleCurrentGeneration(t, service, artifactID)
	}
	requireLeftAsItWas(t, service, artifactID, room, before)
	if _, _, found := pmdoc.FindMark(liveTree(t, service, artifactID), "proofComment", "dangling"); !found {
		t.Fatal("the sweep took the expired mark off a document past the decode cap")
	}
	requireRoomNotFailed(t, service, artifactID)
	requireConfirmed(t, service, artifactID, false)
	requireLoggedOnce(t, service, artifactID, "unrecorded-mark sweep", logs)
}

// A document whose clocks sum past the decode cap only because people typed and deleted a lot in it
// still decodes, and the sweep still sweeps it: the mark comes off, which changes the document,
// and the sweep neither records nor logs leaving it alone.
func TestTheMarkSweepStillSweepsADocumentOnlyItsClockSumPutsPastTheDecodeCap(t *testing.T) {
	logs := captureLogs(t)
	service, artifactID := sweepableDocument(t, quarterMebibyte)
	room := pushPastClockSum(t, service, artifactID)
	before := crdt.EncodeStateAsUpdateV1(room, nil)

	settleCurrentGeneration(t, service, artifactID)
	requireWrittenTo(t, service, artifactID, room, before)
	if _, _, found := pmdoc.FindMark(liveTree(t, service, artifactID), "proofComment", "dangling"); found {
		t.Fatal("the sweep left the expired mark on a document that decodes")
	}
	requireRoomNotFailed(t, service, artifactID)
	requireConfirmed(t, service, artifactID, true)
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

// The block-id stamp mints an id, a new item, for every block a browser edit left without one or
// with another's. On a document whose state already could not be decoded it writes nothing,
// whether settlement or the backfill runs it, and the document is left byte for byte as it was:
// settlement carries on with the ids unrepaired and counts no failure, and the backfill reports the
// document skipped (LEGION-545).
func TestTheBlockIDStampLeavesADocumentPastTheDecodeCapAsItWas(t *testing.T) {
	for _, test := range stampWrites {
		t.Run(test.name, func(t *testing.T) {
			logs := captureLogs(t)
			service, artifactID := unstampedDocument(t, func(seed string) string { return seed })
			room := pushUndecodable(t, service, artifactID)
			before := crdt.EncodeStateAsUpdateV1(room, nil)

			if report := test.stamp(t, service, artifactID); report != nil &&
				(report.Err != nil || report.Stamped != 0 || report.Skipped != "document past the decode cap") {
				t.Fatalf("the backfill reports %+v, want the document skipped as past the decode cap", *report)
			}
			requireLeftAsItWas(t, service, artifactID, room, before)
			if pmdoc.BlockIDRepairCount(liveTree(t, service, artifactID)) == 0 {
				t.Fatal("a block of a document past the decode cap was stamped")
			}
			requireConfirmed(t, service, artifactID, false)
			requireLoggedOnce(t, service, artifactID, test.write, logs)
		})
	}
}

// A document whose clocks sum past the decode cap only because people typed and deleted a lot in it
// still decodes, and the block-id stamp still stamps it, whether settlement or the backfill runs
// it: every block gets its id, which changes the document, the backfill reports the ids it
// stamped, and neither records nor logs leaving the document alone.
func TestTheBlockIDStampStillStampsADocumentOnlyItsClockSumPutsPastTheDecodeCap(t *testing.T) {
	for _, test := range stampWrites {
		t.Run(test.name, func(t *testing.T) {
			logs := captureLogs(t)
			service, artifactID := unstampedDocument(t, quarterMebibyte)
			room := pushPastClockSum(t, service, artifactID)
			before := crdt.EncodeStateAsUpdateV1(room, nil)

			if report := test.stamp(t, service, artifactID); report != nil &&
				(report.Err != nil || report.Stamped == 0 || report.Skipped != "") {
				t.Fatalf("the backfill reports %+v, want the document's blocks stamped", *report)
			}
			requireWrittenTo(t, service, artifactID, room, before)
			if repairs := pmdoc.BlockIDRepairCount(liveTree(t, service, artifactID)); repairs != 0 {
				t.Fatalf("%d blocks of a document that decodes were left unstamped", repairs)
			}
			requireConfirmed(t, service, artifactID, true)
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
// a full document left out - which adds the answer's text to the document as new items. On a
// document whose state already could not be decoded settlement withholds it, as it withholds an
// answer a full document has no room for: the block still says who answered and when, the ask
// keeps the answer, the document is left byte for byte as it was, settlement logs that it withheld
// the answer and why, and the settlement counts no failure, so the room it runs on does not fail
// (LEGION-545).
func TestSettlementWithholdsAnAnswerFromADocumentPastTheDecodeCapAndCountsNoFailure(t *testing.T) {
	logs := captureLogs(t)
	text := "Ship it on Friday."
	service, artifactID := answeredDocument(t, func(seed string) string { return seed + "\n" }, text)
	room := pushUndecodable(t, service, artifactID)
	before := crdt.EncodeStateAsUpdateV1(room, nil)

	settleAnswered(t, service, artifactID)
	requireLeftAsItWas(t, service, artifactID, room, before)
	if lines := withheldAnswerLines(logs); !strings.Contains(lines, "withheld=1") || !strings.Contains(lines, "holds more than the 1048576 items") {
		t.Fatalf("settlement logged, of withholding answers:\n%s\nwant the one answer withheld past the decode cap", lines)
	}
	requireConfirmed(t, service, artifactID, false)
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
}

// A document whose clocks sum past the decode cap only because people typed and deleted a lot in it
// still decodes, and settlement still writes an answer back into its block there: the document
// renders the answer, which changes it, and settlement logs no answer withheld.
func TestSettlementStillReturnsAnAnswerToADocumentOnlyItsClockSumPutsPastTheDecodeCap(t *testing.T) {
	logs := captureLogs(t)
	text := "Ship it on Friday."
	service, artifactID := answeredDocument(t, quarterMebibyte, text)
	room := pushPastClockSum(t, service, artifactID)
	before := crdt.EncodeStateAsUpdateV1(room, nil)

	settleAnswered(t, service, artifactID)
	requireWrittenTo(t, service, artifactID, room, before)
	markdown, err := renderTree(liveTree(t, service, artifactID))
	if err != nil {
		t.Fatalf("render the live document: %v", err)
	}
	if !strings.Contains(markdown, text) {
		t.Fatal("the document that decodes renders no answer after settlement")
	}
	if lines := withheldAnswerLines(logs); lines != "" {
		t.Fatalf("settlement withheld an answer from a document that decodes:\n%s", lines)
	}
	requireConfirmed(t, service, artifactID, true)
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
// document can hold and still decode, so the heaviest decode a confirmation can make.
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
	if structs := structCount(b, live); structs != maxUpdateItems || !stateDecodes(live) {
		b.Fatalf("the document holds %d structs and decodes %v, want %d and decoding", structs, stateDecodes(live), maxUpdateItems)
	}
	return live
}

// BenchmarkDecodeCapConfirmation times one decode-cap confirmation (elementMeasurer.decodes) as the
// writes outside applyLive pay it once their document's clocks sum past the cap: on a mebibyte of
// the markdown a room can hold, typed over, whose clocks already sum past the cap, and on a
// document at the cap itself.
func BenchmarkDecodeCapConfirmation(b *testing.B) {
	for _, document := range []struct {
		name  string
		build func(b *testing.B) *crdt.Doc
	}{
		{"1MiB", func(b *testing.B) *crdt.Doc { return residentRoomDocument(b, 1<<20) }},
		{"at-the-cap", documentAtTheCap},
	} {
		b.Run(document.name, func(b *testing.B) {
			live := document.build(b)
			measurer := &elementMeasurer{}
			for b.Loop() {
				if !measurer.decodes(live) {
					b.Fatal("the document does not decode")
				}
			}
			b.ReportMetric(float64(clockSum(live)), "clocks")
			b.ReportMetric(float64(structCount(b, live)), "structs")
			b.ReportMetric(float64(len(crdt.EncodeStateAsUpdateV1(live, nil))), "state-bytes")
		})
	}
}
