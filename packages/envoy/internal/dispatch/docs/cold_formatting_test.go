package docs

import (
	"fmt"
	"math/rand/v2"
	"testing"
	"time"

	"github.com/reearth/ygo/crdt"
	"github.com/sjawhar/envoy/internal/dispatch/model"
)

// The replica is not the only copy that replays a room's commits: so does the fold of the room's
// stored updates, which a cold read renders once the room is gone (loadTree, coldRead), and
// newDocumentCopy turns ygo's format cleanup off for that fold too. The two cases here hold the
// fold to what the room itself held - the markdown a reader gets with the room evicted must be the
// markdown the room held, byte for byte - on the shape sjawhar/legion#2496's review names and on a
// randomised churn. With crdt.WithFormatCleanup(false) taken out of newDocumentCopy the first case
// fails; the second one passes either way, and is kept for the shapes the first does not reach.

// C1. The review's shape, then the room goes: four of the service's own writes leave the room
// holding a marked run, a plain one and a marked one, and the stored fold must render what the
// room rendered rather than the text a fold that cleaned up would hold.
func TestColdReadReplaysTheRoomsFormattingOnTheReviewsShape(t *testing.T) {
	service, artifactID, sh, live, _ := newFormattingRoom(t)

	writeInRoom(t, service, artifactID, "unitalicise the tail", func(fragment *crdt.YXmlFragment) func(*crdt.Transaction) {
		text := xmlTextWithPrefix(t, fragment, formattingRun)
		return func(txn *crdt.Transaction) { text.Format(txn, 2, 3, crdt.Attributes{"emphasis": nil}) }
	})
	writeInRoom(t, service, artifactID, "insert a bold character", func(fragment *crdt.YXmlFragment) func(*crdt.Transaction) {
		text := xmlTextWithPrefix(t, fragment, formattingRun)
		return func(txn *crdt.Transaction) {
			text.Insert(txn, 0, "x", crdt.Attributes{"strong": markOn})
		}
	})
	writeInRoom(t, service, artifactID, "delete the character after it", func(fragment *crdt.YXmlFragment) func(*crdt.Transaction) {
		text := xmlTextWithPrefix(t, fragment, "x")
		return func(txn *crdt.Transaction) { text.Delete(txn, 1, 1) }
	})
	writeInRoom(t, service, artifactID, "unmark the second character", func(fragment *crdt.YXmlFragment) func(*crdt.Transaction) {
		text := xmlTextWithPrefix(t, fragment, "x")
		return func(txn *crdt.Transaction) {
			text.Format(txn, 1, 1, crdt.Attributes{"strong": nil, "emphasis": nil})
		}
	})
	const want = `"x"+strong=map[] "x" "xxy"+strong=map[marker:*] `
	if got := textRuns(roomText(t, live, "x")); got != want {
		t.Fatalf("the room's run is %s, and the review's shape is %s", got, want)
	}
	waitForLandedAppends(t, service, artifactID)
	sh.check(t)
	assertColdReadIsTheRoomsRendering(t, service, artifactID)
}

// C2. A browser marking and unmarking overlapping runs, interleaved with the service's own writes
// and with typing, then the room goes.
func TestColdReadReplaysTheRoomsFormattingUnderChurn(t *testing.T) {
	for _, seed := range []uint64{1, 2, 3} {
		t.Run(fmt.Sprintf("seed %d", seed), func(t *testing.T) {
			service, artifactID := newTestService(t)
			service.settle = time.Hour
			seedServiceText(t, service, artifactID, formattingChurnDocument())
			peer := newRoomPeer(t, service, artifactID, 1, reuseBob, nil)
			rng := rand.New(rand.NewPCG(644, seed))

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
				if round%7 == 6 {
					if err := joinedWrite(service, applyOps(service, artifactID, reuseAgent, model.EditOp{
						Op:   "replace",
						Find: fmt.Sprintf("Churn paragraph %d holds", round/7),
						With: fmt.Sprintf("Churn paragraph %d now holds", round/7),
					})); err != nil {
						t.Fatalf("the agent's write: %v", err)
					}
					peer.sync()
				}
				if round%11 == 10 {
					texts = peer.markableTexts()
					target := texts[rng.IntN(len(texts))]
					peer.send(func(txn *crdt.Transaction) { target.Insert(txn, rng.IntN(target.Len()+1), "typed", nil) })
				}
			}

			waitForLandedAppends(t, service, artifactID)
			assertColdReadIsTheRoomsRendering(t, service, artifactID)
		})
	}
}

// assertColdReadIsTheRoomsRendering renders the resident room, evicts it, and holds the cold read
// of its stored updates to that rendering.
func assertColdReadIsTheRoomsRendering(t *testing.T, service *Service, artifactID string) {
	t.Helper()
	live := service.srv.GetDoc(artifactID)
	if live == nil {
		t.Fatal("the room is not resident")
	}
	snapshot, err := snapshotDocument(live)
	if err != nil {
		t.Fatalf("copy the room: %v", err)
	}
	held, _, err := shadowRender(snapshot)
	if err != nil || held == nil {
		t.Fatalf("render the room: %v", err)
	}
	evicted := service.invalidateForEviction(artifactID)
	if evicted == nil {
		t.Fatal("the service holds no state for the room")
	}
	if err := service.evictRoom(artifactID, evicted); err != nil {
		t.Fatalf("evict the room: %v", err)
	}
	cold := readCold(t, service, artifactID)
	if cold.markdown == *held {
		return
	}
	at := 0
	for at < len(cold.markdown) && at < len(*held) && cold.markdown[at] == (*held)[at] {
		at++
	}
	t.Errorf("the cold read differs from what the room held at byte %d of %d:\n  cold: %q\n  room: %q",
		at, len(*held), excerptAround(cold.markdown, at), excerptAround(*held, at))
}

func excerptAround(text string, at int) string {
	return text[max(0, at-80):min(len(text), at+80)]
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
