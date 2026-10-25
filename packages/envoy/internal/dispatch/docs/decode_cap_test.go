package docs

import (
	"bytes"
	"context"
	"encoding/json"
	"strings"
	"testing"
	"time"

	"github.com/reearth/ygo/crdt"

	"github.com/sjawhar/envoy/internal/dispatch/model"
	"github.com/sjawhar/envoy/internal/dispatch/pmdoc"
)

// pushPastDecodeCap carries artifactID's live document past the decode cap the way a long edit
// history does, as a browser's edit reaches the room: a run of maxUpdateItems characters typed into
// its first text and deleted again in one edit, so its clocks sum past the cap (pastDecodeCap)
// while it renders as it did. ygo holds the run as one struct, so the room still decodes it, as a
// room whose document has grown this far does. It returns the room's document.
func pushPastDecodeCap(t *testing.T, service *Service, artifactID string) *crdt.Doc {
	t.Helper()
	room := service.srv.GetDoc(artifactID)
	if room == nil {
		t.Fatal("document room is not resident")
	}
	browser, text := peerDocument(t, room)
	synced := browser.StateVector()
	run := strings.Repeat("x", maxUpdateItems)
	browser.Transact(func(txn *crdt.Transaction) {
		text.Insert(txn, 0, run, nil)
		text.Delete(txn, 0, len(run))
	})
	if err := crdt.ApplyUpdateV1(room, crdt.EncodeStateAsUpdateV1(browser, synced), "peer"); err != nil {
		t.Fatalf("apply the browser's long history to the room: %v", err)
	}
	if !pastDecodeCap(room) {
		t.Fatal("the room's document is not past the decode cap")
	}
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

// requireLoggedOnce requires write to have recorded on the room's state that it left the document
// alone past the decode cap (logPastDecodeCap), which is what keeps every later pass from logging
// it again, and so also that the write reached its refusal rather than stopping short of it.
func requireLoggedOnce(t *testing.T, service *Service, artifactID, write string) {
	t.Helper()
	state := service.room(artifactID)
	state.mu.Lock()
	defer state.mu.Unlock()
	if _, logged := state.pastDecodeCap[write]; !logged {
		t.Fatalf("the %s never recorded leaving the document past the decode cap; it recorded %v", write, state.pastDecodeCap)
	}
}

// The sweep of unrecorded marks unmarks every expired mark in one transaction on the room, which
// cannot be rolled back once begun, and each mark it takes off writes new format items into the
// document. On a document already past the decode cap it writes nothing: the mark stays, the
// document is left byte for byte as it was, the settlement it ends counts no failure, and every
// later settlement's pass finds the mark expired again without logging it again (LEGION-545).
func TestTheMarkSweepLeavesADocumentPastTheDecodeCapAsItWas(t *testing.T) {
	service, artifactID := newTestService(t)
	service.settle = time.Hour
	service.unrecordedMarkTTL = 0
	seedServiceText(t, service, artifactID, "The quick brown fox")
	browserMark(t, service, artifactID, "proofComment", "dangling", "brown")
	room := pushPastDecodeCap(t, service, artifactID)
	before := crdt.EncodeStateAsUpdateV1(room, nil)

	for range 2 {
		settleCurrentGeneration(t, service, artifactID)
	}
	requireLeftAsItWas(t, service, artifactID, room, before)
	if _, _, found := pmdoc.FindMark(liveTree(t, service, artifactID), "proofComment", "dangling"); !found {
		t.Fatal("the sweep took the expired mark off a document past the decode cap")
	}
	requireRoomNotFailed(t, service, artifactID)
	requireLoggedOnce(t, service, artifactID, "unrecorded-mark sweep")
}

// The block-id stamp mints an id, a new item, for every block a browser edit left without one or
// with another's. On a document already past the decode cap it writes nothing, whether settlement
// or the backfill runs it, and the document is left byte for byte as it was: settlement carries on
// with the ids unrepaired and counts no failure, and the backfill reports the document skipped
// (LEGION-545).
func TestTheBlockIDStampLeavesADocumentPastTheDecodeCapAsItWas(t *testing.T) {
	for _, test := range []struct {
		name  string
		write string
		stamp func(t *testing.T, service *Service, artifactID string)
	}{
		{
			name:  "settlement",
			write: "settlement's block-id stamp",
			stamp: func(t *testing.T, service *Service, artifactID string) {
				reconciled := false
				service.afterSettleReconcile = func(room string) { reconciled = reconciled || room == artifactID }
				settleCurrentGeneration(t, service, artifactID)
				if !reconciled {
					t.Fatal("the settlement ended before it reconciled the document it could not stamp")
				}
				requireRoomNotFailed(t, service, artifactID)
			},
		},
		{
			name:  "the backfill",
			write: "block-id backfill",
			stamp: func(t *testing.T, service *Service, artifactID string) {
				reports, err := service.BackfillBlockIDs(context.Background())
				if err != nil {
					t.Fatalf("backfill block ids: %v", err)
				}
				for _, report := range reports {
					if report.ArtifactID != artifactID {
						continue
					}
					if report.Err != nil || report.Stamped != 0 || report.Skipped != "document past the decode cap" {
						t.Fatalf("the backfill reports %+v, want the document skipped as past the decode cap", report)
					}
					return
				}
				t.Fatalf("the backfill reports %+v, none of them for the document", reports)
			},
		},
	} {
		t.Run(test.name, func(t *testing.T) {
			service, artifactID := newTestService(t)
			service.settle = time.Hour
			seedServiceText(t, service, artifactID, "before")
			settleCurrentGeneration(t, service, artifactID)
			editLiveTree(t, service, artifactID, appendUnidentifiedBlocks(t, "added"))
			room := pushPastDecodeCap(t, service, artifactID)
			before := crdt.EncodeStateAsUpdateV1(room, nil)

			test.stamp(t, service, artifactID)
			requireLeftAsItWas(t, service, artifactID, room, before)
			if pmdoc.BlockIDRepairCount(liveTree(t, service, artifactID)) == 0 {
				t.Fatal("a block of a document past the decode cap was stamped")
			}
			requireLoggedOnce(t, service, artifactID, test.write)
		})
	}
}

// An answer is stored on its ask as well as in its block, and settlement writes it back into a
// block that holds the rest of its ask's state without it - a returning block, or one whose answer
// a full document left out - which adds the answer's text to the document as new items. On a
// document already past the decode cap settlement withholds it, as it withholds an answer a full
// document has no room for: the block still says who answered and when, the ask keeps the answer,
// the document is left byte for byte as it was, and the settlement counts no failure, so the room
// it runs on does not fail (LEGION-545).
func TestSettlementWithholdsAnAnswerFromADocumentPastTheDecodeCapAndCountsNoFailure(t *testing.T) {
	service, artifactID := newTestService(t)
	service.settle = time.Hour
	seedServiceText(t, service, artifactID, ":::ask{#ask-block urgency=\"med\" multiple=\"false\" state=\"open\"}\nShip it?\n:::\n\nContext.\n")
	service.settleRoom(artifactID, 0)

	text := "Ship it on Friday."
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
	// The block says who answered and when, and holds no answer: what a settlement that withheld
	// the answer leaves (setAskServerAttributes), and what settlement writes the answer back into.
	editAsPeer(t, service, artifactID, func(tree *pmdoc.Node) *pmdoc.Node {
		pmdoc.Walk(tree, func(node *pmdoc.Node) bool {
			if node.Type == "ask" {
				setAskServerAttributes(node, model.Ask{State: "answered", Answer: &answer}, "", true)
			}
			return true
		})
		return tree
	})
	room := pushPastDecodeCap(t, service, artifactID)
	before := crdt.EncodeStateAsUpdateV1(room, nil)

	reconciled := false
	service.afterSettleReconcile = func(room string) { reconciled = reconciled || room == artifactID }
	settleCurrentGeneration(t, service, artifactID)
	if !reconciled {
		t.Fatal("the settlement ended before it reconciled the answered block")
	}
	requireLeftAsItWas(t, service, artifactID, room, before)
	requireRoomNotFailed(t, service, artifactID)
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
