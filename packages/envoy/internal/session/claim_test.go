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
	next := SessionEntry{Dir: "/work/first", Driving: false}

	got, err := mergeForClaim(natsgo.ErrKeyNotFound, next, claimNow)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if got.Dir != "/work/first" {
		t.Fatalf("expected the first claim to be accepted, got %+v", got)
	}
	if got.UpdatedAt != claimNow {
		t.Fatalf("expected UpdatedAt stamped to now, got %d", got.UpdatedAt)
	}
}

func TestMergeForClaim_TransientGetErrorRefusesWrite(t *testing.T) {
	boom := errors.New("kv unavailable")

	_, err := mergeForClaim(boom, SessionEntry{Dir: "/work/first"}, claimNow)
	if !errors.Is(err, boom) {
		t.Fatalf("expected the transient KV error to be returned, got %v", err)
	}
}

func TestMergeForClaim_SameProcessRefreshes(t *testing.T) {
	next := SessionEntry{Dir: "/work/first", Driving: true}

	got, err := mergeForClaim(nil, next, claimNow)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if got.Dir != "/work/first" || got.UpdatedAt != claimNow {
		t.Fatalf("expected the same process's heartbeat to refresh, got %+v", got)
	}
}

// The latest claim takes the route whatever the incumbent said. Two rows stood here until
// dispatch://INFRA-889, both decided by the two claims' serving ports: a driving claim displaced
// a non-driving incumbent, and a newer driving claim displaced a live driving one (a genuine
// re-home, the session restarted in a new process with -s). No claim carries a port now, so
// neither rule can fire, the incumbent no longer reaches mergeForClaim at all, and the two rows
// were one call with one argument apiece. What the incumbent held is covered where it is still
// read, through the registry: TestSessionRegistry_PutWritesTheLatestClaimThrough writes a live
// non-driving row and then the driving claim that displaces it.
func TestMergeForClaim_TheLatestClaimTakesTheRoute(t *testing.T) {
	next := SessionEntry{Dir: "/work/newcomer", Driving: true}

	got, err := mergeForClaim(nil, next, claimNow)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if got.Dir != "/work/newcomer" {
		t.Fatalf("expected the latest claim to take the route, got %+v", got)
	}
}

// --- Registry integration: Put writes through mergeForClaim ---

func TestSessionRegistry_PutWritesTheLatestClaimThrough(t *testing.T) {
	client := setupNATS(t)
	reg, err := OpenSessionRegistry(client.Conn, WithSessionReplicas(1), WithSessionTTL(10*time.Minute))
	if err != nil {
		t.Fatalf("failed to open session registry: %v", err)
	}

	if err := reg.Put("ses_rehomed", SessionEntry{MachineID: "m", Dir: "/work/bystander"}); err != nil {
		t.Fatalf("bystander put failed: %v", err)
	}
	if err := reg.Put("ses_rehomed", SessionEntry{MachineID: "m", Dir: "/work/driver", Driving: true}); err != nil {
		t.Fatalf("driver put failed: %v", err)
	}

	got, err := reg.Get("ses_rehomed")
	if err != nil {
		t.Fatalf("get failed: %v", err)
	}
	if got.Dir != "/work/driver" {
		t.Fatalf("expected the latest claim to take the route, got %+v", got)
	}
}
