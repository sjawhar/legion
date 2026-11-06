package docs

import (
	"context"
	"errors"
	"testing"
	"time"

	ygws "github.com/reearth/ygo/provider/websocket"

	"github.com/sjawhar/envoy/internal/dispatch/store/storetest"
)

// The room-counters hook counts each room of a document the service closes, and no close that
// closed nothing (a refusal for the room's peers, a document no room holds), and records the time
// of each settlement-resumption pass that armed a document's owed settlement.
func TestRoomCountersCountServiceClosesAndSettlementResumptions(t *testing.T) {
	ctx := context.Background()
	database := storetest.Open(t)
	artifactID := createDocument(t, database, "before")
	service, server := newTestServiceInstance(t, database)
	service.hooks = newTestHooks()
	seedServiceText(t, service, artifactID, "before")
	waitForLandedAppends(t, service, artifactID)
	alice := connectWithScope(t, server.URL, artifactID, "alice", "read-write", "before\n")

	if err := service.srv.CloseRoom(artifactID, false); !errors.Is(err, ygws.ErrRoomHasPeers) {
		t.Fatalf("an unforced close of the room alice is in = %v, want ErrRoomHasPeers", err)
	}
	if closes := requireRoomCounters(t, service, artifactID).RoomCloses; closes != 0 {
		t.Fatalf("room closes after a close refused for the room's peers = %d, want 0", closes)
	}
	if err := service.srv.CloseRoom(artifactID, true); err != nil {
		t.Fatalf("close the room: %v", err)
	}
	if closes := requireRoomCounters(t, service, artifactID).RoomCloses; closes != 1 {
		t.Fatalf("room closes after one close of the room = %d, want 1", closes)
	}
	requireEnds(t, alice, "closing the room left alice's browser connected")
	const untouched = "00000000-0000-0000-0000-000000000000"
	if err := service.srv.CloseRoom(untouched, true); !errors.Is(err, ygws.ErrRoomNotFound) {
		t.Fatalf("a close of a document no room holds = %v, want ErrRoomNotFound", err)
	}

	// A document of its own, which no room holds and nothing settles, so its pending row stands
	// until the pass reads it.
	owed := createIssueDocument(t, database, 2, "# Owed")
	if resumed := requireRoomCounters(t, service, owed).SettlementResumptions; len(resumed) != 0 {
		t.Fatalf("settlement resumptions before any pass = %v, want none", resumed)
	}
	if _, err := database.Pool.Exec(ctx, `
		insert into doc_settlements_pending (artifact_id, marked_at) values ($1, now() - interval '2 minutes')
	`, owed); err != nil {
		t.Fatalf("owe the document's settlement: %v", err)
	}
	before := time.Now()
	if err := service.resumeOwedSettlements(ctx, settlementResumeAge); err != nil {
		t.Fatalf("resume the owed settlements: %v", err)
	}
	after := time.Now()
	resumed := requireRoomCounters(t, service, owed).SettlementResumptions
	if len(resumed) != 1 || resumed[0].Before(before) || resumed[0].After(after) {
		t.Fatalf("settlement resumptions after one pass that armed the document = %v, want one time in [%s, %s]",
			resumed, before.Format(time.RFC3339Nano), after.Format(time.RFC3339Nano))
	}
	if other := requireRoomCounters(t, service, untouched); other.RoomCloses != 0 || len(other.SettlementResumptions) != 0 {
		t.Fatalf("the counters of a document whose only close found no room = %+v, want zero", other)
	}
}

func requireRoomCounters(t *testing.T, service *Service, artifactID string) RoomCounters {
	t.Helper()
	counters, err := service.RoomCounters(artifactID)
	if err != nil {
		t.Fatalf("read the room counters: %v", err)
	}
	return counters
}
