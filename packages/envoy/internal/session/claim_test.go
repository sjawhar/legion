package session

import (
	"errors"
	"testing"
	"time"

	natsgo "github.com/nats-io/nats.go"
)

// Session routes are keyed by session ID, but opencode session state lives on
// shared disk, so several live processes can hold the same session at once: the
// process explicitly driving it (launched with -s <session>), plus any process
// that merely has it loaded. Every such process heartbeats a claim. Blind
// last-writer-wins therefore routes deliveries to an arbitrary holder, and a
// holder that is not driving the session starts its own model loop on delivery —
// two loops interleaving one transcript (observed 2026-07-29 on
// ses_05fce2520ffeoVkqBxn2DfJyft, which flip-flopped between four ports).
//
// mergeForClaim decides which claim survives.

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

func TestMergeForClaim_NonDriverCannotStealFromFreshDriver(t *testing.T) {
	cur := SessionEntry{Port: 100, Driving: true, UpdatedAt: claimNow - 1_000}
	next := SessionEntry{Port: 200, Driving: false}

	got, err := mergeForClaim(cur, nil, next, claimNow)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if got.Port != 100 {
		t.Fatalf("expected the driving incumbent to keep the route, got port %d", got.Port)
	}
	if got.UpdatedAt != cur.UpdatedAt {
		t.Fatalf("expected the incumbent's freshness to be preserved, got %d", got.UpdatedAt)
	}
}

func TestMergeForClaim_NonDriverTakesOverStaleDriver(t *testing.T) {
	// A driving claim that has not been refreshed within the staleness window is
	// assumed gone (process exited), so a remaining holder may take the route
	// rather than leaving the session unreachable.
	cur := SessionEntry{Port: 100, Driving: true, UpdatedAt: claimNow - int64(claimStaleAfter/time.Millisecond) - 1}
	next := SessionEntry{Port: 200, Driving: false}

	got, err := mergeForClaim(cur, nil, next, claimNow)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if got.Port != 200 {
		t.Fatalf("expected takeover of a stale driving claim, got port %d", got.Port)
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

func TestMergeForClaim_KeepsIncumbentMetadataWhenChallengerLoses(t *testing.T) {
	cur := SessionEntry{Port: 100, Driving: true, Dir: "/work/real", Title: "real", UpdatedAt: claimNow - 1_000}
	next := SessionEntry{Port: 200, Driving: false, Dir: "/work/other", Title: "other"}

	got, err := mergeForClaim(cur, nil, next, claimNow)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if got.Dir != "/work/real" || got.Title != "real" {
		t.Fatalf("expected the incumbent's metadata to survive a rejected claim, got %+v", got)
	}
}

// --- Registry integration: Put must enforce the precedence rule ---

func TestSessionRegistry_PutRejectsNonDrivingClaimOverLiveDriver(t *testing.T) {
	client := setupNATS(t)
	reg, err := OpenSessionRegistry(client.Conn, WithSessionReplicas(1), WithSessionTTL(10*time.Minute))
	if err != nil {
		t.Fatalf("failed to open session registry: %v", err)
	}

	driver := SessionEntry{Port: 42145, MachineID: "m", Dir: "/work/driver", Driving: true}
	if err := reg.Put("ses_contended", driver); err != nil {
		t.Fatalf("driver put failed: %v", err)
	}

	// A different live process that merely has the session loaded heartbeats a claim.
	bystander := SessionEntry{Port: 34751, MachineID: "m", Dir: "/work/bystander"}
	if err := reg.Put("ses_contended", bystander); err != nil {
		t.Fatalf("bystander put failed: %v", err)
	}

	got, err := reg.Get("ses_contended")
	if err != nil {
		t.Fatalf("get failed: %v", err)
	}
	if got.Port != 42145 {
		t.Fatalf("expected the route to stay with the driving process, got port %d", got.Port)
	}
}

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
