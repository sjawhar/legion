package session

import (
	"testing"
	"time"
)

// Session routes are keyed by session ID, but a harness can keep session state on shared disk, so
// several live processes can hold the same session at once: the process explicitly driving it,
// plus any process that merely has it loaded. Every such process heartbeats a claim, and the
// route follows whichever claim was written last.

func TestSessionRegistry_PutWritesTheLatestClaimThrough(t *testing.T) {
	client := setupNATS(t)
	reg, err := OpenSessionRegistry(client.Conn, WithSessionReplicas(1), WithSessionTTL(10*time.Minute))
	if err != nil {
		t.Fatalf("failed to open session registry: %v", err)
	}

	if err := reg.Put("ses_rehomed", SessionEntry{MachineID: "m", Dir: "/work/bystander"}); err != nil {
		t.Fatalf("bystander put failed: %v", err)
	}
	if err := reg.Put("ses_rehomed", SessionEntry{MachineID: "m", Dir: "/work/driver"}); err != nil {
		t.Fatalf("driver put failed: %v", err)
	}

	got, err := reg.Get("ses_rehomed")
	if err != nil {
		t.Fatalf("get failed: %v", err)
	}
	if got.Dir != "/work/driver" {
		t.Fatalf("expected the latest claim to take the route, got %+v", got)
	}
	if got.UpdatedAt == 0 {
		t.Fatalf("expected the write to stamp the entry with the moment of the claim, got %+v", got)
	}
}
