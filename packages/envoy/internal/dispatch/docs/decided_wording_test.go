package docs

import (
	"context"
	"encoding/json"
	"errors"
	"testing"
	"time"

	"github.com/sjawhar/envoy/internal/dispatch/model"
)

// askPartState is the ask's part index as its state row records it: the revision its parts have
// reached and the hash of the parts indexed at that revision, "" for an ask no index has reached.
func askPartState(t *testing.T, service *Service, askID string) (int, string) {
	t.Helper()
	var revision int
	var sum *string
	if err := service.store.Pool.QueryRow(context.Background(),
		`select revision, body_sha256 from ask_block_state where ask_id = $1::uuid`, askID).Scan(&revision, &sum); err != nil {
		t.Fatalf("read the part index of ask %s: %v", askID, err)
	}
	if sum == nil {
		return revision, ""
	}
	return revision, *sum
}

// A restoration holds no lock on the ask it restores, so a second pod on the release before
// LEGION-651 can answer the ask again between the plan and the write phase. The plan read the
// answer the decision was given on, and the parts queued behind the restore would write that
// answer back over the new one and clear owed_at over it. The restore's update names the answer
// the plan read, so it leaves the moved row alone and returns no id, and the restoration is
// refused (errDecidedWordingsMoved): the transaction rolls back and writes nothing, neither the
// decided wording nor the parts it planned.
func TestARestorationRefusesAnAskAnsweredSinceItsPlan(t *testing.T) {
	ctx := context.Background()
	service, original := newTestService(t)
	service.settle = time.Hour
	seedServiceText(t, service, original, "Context\n\n"+shipBlock)
	settleCurrentGeneration(t, service, original)
	answer, _ := decidedShip("answered")
	if err := closeSource(ctx, service, original, "ship", blockAskOn(t, service, original, "ship"), answer, nil); err != nil {
		t.Fatal(err)
	}
	source := blockAskOn(t, service, original, "ship")
	rewordAsAnEarlierRelease(t, service, original, source)
	revision, sum := askPartState(t, service, source.ID)

	tx, err := service.store.Pool.Begin(ctx)
	if err != nil {
		t.Fatalf("begin the restoration: %v", err)
	}
	defer tx.Rollback(ctx)
	queued, ledger := queueWrites(ctx, tx)
	restored, err := service.restoreWordings(queued, tx, nil)
	if err != nil {
		t.Fatalf("plan the restoration: %v", err)
	}
	if len(restored) != 1 || restored[0].ask != source.ID {
		t.Fatalf("the restoration planned %d asks (%v), want the reworded one", len(restored), restored)
	}

	// The second pod's answer, written on a connection of its own while the restoration's update
	// is still queued: the restoration holds no row of the ask, so this commits at once.
	moved := model.AskAnswer{User: "bob", Selected: []string{"Tuesday"}, At: time.Date(2026, 10, 10, 9, 0, 0, 0, time.UTC)}
	encoded, err := json.Marshal(moved)
	if err != nil {
		t.Fatalf("encode the second pod's answer: %v", err)
	}
	if _, err := service.store.Pool.Exec(ctx, `update asks set answer = $2 where id = $1`, source.ID, encoded); err != nil {
		t.Fatalf("answer the decided ask again: %v", err)
	}

	if err := ledger.runQueue(ctx, nil); !errors.Is(err, errDecidedWordingsMoved) {
		t.Fatalf("the restoration's write phase = %v, want %v", err, errDecidedWordingsMoved)
	}
	if err := tx.Rollback(ctx); err != nil {
		t.Fatalf("roll the refused restoration back: %v", err)
	}

	after := blockAskOn(t, service, original, "ship")
	if after.Question != "Ship on Tuesday?" {
		t.Fatalf("the refused restoration left the ask asking %q, want the wording it was not allowed to restore", after.Question)
	}
	if after.Answer == nil || after.Answer.User != "bob" {
		t.Fatalf("the ask's answer = %v, want the second pod's", after.Answer)
	}
	if nextRevision, nextSum := askPartState(t, service, source.ID); nextRevision != revision || nextSum != sum {
		t.Fatalf("the refused restoration left the ask's parts at revision %d (%s), want the %d (%s) they stood at",
			nextRevision, nextSum, revision, sum)
	}
}
