package session

import (
	"errors"
	"testing"
	"time"

	natsgo "github.com/nats-io/nats.go"
)

// Session routes are keyed by session ID, but a harness can keep session state
// on shared disk, so several live processes can hold the same session at once:
// the process explicitly driving it, plus any process that merely has it loaded.
// Every such process heartbeats a claim. mergeForClaim arbitrated between them until
// dispatch://INFRA-889, by rules written on the serving ports the two claims named; a
// registration may no longer carry a port, so the entry follows the latest claim and only a
// transient KV error stops a write.

const claimNow = int64(1_700_000_000_000)

func TestMergeForClaim_NewSessionAccepted(t *testing.T) {
	next := SessionEntry{Port: 100, Driving: false}

	got, err := mergeForClaim(SessionEntry{}, natsgo.ErrKeyNotFound, next, claimNow)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if got.Port != 100 {
		t.Fatalf("expected the first claim to be accepted, got port %d", got.Port)
	}
	if got.UpdatedAt != claimNow {
		t.Fatalf("expected UpdatedAt stamped to now, got %d", got.UpdatedAt)
	}
}

func TestMergeForClaim_TransientGetErrorRefusesWrite(t *testing.T) {
	boom := errors.New("kv unavailable")

	_, err := mergeForClaim(SessionEntry{}, boom, SessionEntry{Port: 100}, claimNow)
	if !errors.Is(err, boom) {
		t.Fatalf("expected the transient KV error to be returned, got %v", err)
	}
}

func TestMergeForClaim_SameProcessRefreshes(t *testing.T) {
	cur := SessionEntry{Port: 100, Driving: true, UpdatedAt: claimNow - 120_000}
	next := SessionEntry{Port: 100, Driving: true}

	got, err := mergeForClaim(cur, nil, next, claimNow)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if got.Port != 100 || got.UpdatedAt != claimNow {
		t.Fatalf("expected same-port heartbeat to refresh, got %+v", got)
	}
}

func TestMergeForClaim_DriverBeatsNonDrivingIncumbent(t *testing.T) {
	cur := SessionEntry{Port: 100, Driving: false, UpdatedAt: claimNow - 1_000}
	next := SessionEntry{Port: 200, Driving: true}

	got, err := mergeForClaim(cur, nil, next, claimNow)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if got.Port != 200 {
		t.Fatalf("expected the driving process to take the route, got port %d", got.Port)
	}
}

func TestMergeForClaim_NewDriverTakesOverFromOldDriver(t *testing.T) {
	// Genuine re-home: the session was restarted in a new process with -s, so
	// the newer driving claim wins even though the incumbent is also driving.
	cur := SessionEntry{Port: 100, Driving: true, UpdatedAt: claimNow - 1_000}
	next := SessionEntry{Port: 200, Driving: true}

	got, err := mergeForClaim(cur, nil, next, claimNow)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if got.Port != 200 {
		t.Fatalf("expected the newer driving claim to win a re-home, got port %d", got.Port)
	}
}

// --- Registry integration: Put writes through mergeForClaim ---

func TestSessionRegistry_PutAcceptsDrivingClaimOverBystander(t *testing.T) {
	client := setupNATS(t)
	reg, err := OpenSessionRegistry(client.Conn, WithSessionReplicas(1), WithSessionTTL(10*time.Minute))
	if err != nil {
		t.Fatalf("failed to open session registry: %v", err)
	}

	if err := reg.Put("ses_rehomed", SessionEntry{Port: 34751, MachineID: "m", Dir: "/work/bystander"}); err != nil {
		t.Fatalf("bystander put failed: %v", err)
	}
	if err := reg.Put("ses_rehomed", SessionEntry{Port: 42145, MachineID: "m", Dir: "/work/driver", Driving: true}); err != nil {
		t.Fatalf("driver put failed: %v", err)
	}

	got, err := reg.Get("ses_rehomed")
	if err != nil {
		t.Fatalf("get failed: %v", err)
	}
	if got.Port != 42145 {
		t.Fatalf("expected the driving process to take the route, got port %d", got.Port)
	}
}
