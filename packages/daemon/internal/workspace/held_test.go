package workspace

import (
	"context"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"
)

// Provisioning records, in the shared clone it creates, every commit the clone held then (Held),
// on the host's path and on a pod's feed: another tree's branch the remote had is among them. It
// writes the record once: a later provisioning on the same clone, after the remote gained a
// branch, leaves it as it was.
func TestProvisionRecordsWhatTheSharedCloneHeldWhenItWasMade(t *testing.T) {
	for _, path := range []string{"host", "feed"} {
		t.Run(path, func(t *testing.T) {
			run := newLocalRunner(t)
			foreign := pushRemoteBranch(t, run.remote, "legion/WIDGETS-7", "other.txt")
			req := provisionRequest(t)
			if path == "feed" {
				req = feedRequest(t, run, req)
			}
			working, err := Provision(context.Background(), run, req)
			if err != nil {
				t.Fatalf("Provision: %v", err)
			}
			held, ok, err := Held(working.Clone)
			if err != nil || !ok {
				t.Fatalf("Held = %v, %v, %v; want the clone's record", held, ok, err)
			}
			main := commitOf(t, working.Clone, "main@origin")
			for _, want := range []string{foreign, main} {
				if got := commitOf(t, working.Clone, want+" & ::("+strings.Join(held, " | ")+")"); got != want {
					t.Errorf("no commit of the record %v contains %s", held, want)
				}
			}

			later := pushRemoteBranch(t, run.remote, "legion/WIDGETS-9", "later.txt")
			again := req
			again.Issue = "WIDGETS-43"
			if path == "feed" {
				again = feedRequest(t, run, again)
			}
			if _, err := Provision(context.Background(), run, again); err != nil {
				t.Fatalf("second Provision: %v", err)
			}
			after, _, err := Held(working.Clone)
			if err != nil || !slices.Equal(after, held) || slices.Contains(after, later) {
				t.Errorf("after a second provisioning the record is %v (%v), want %v unchanged", after, err, held)
			}
		})
	}
}

// Held reads no record from a clone that has none, and refuses one holding anything but commit
// ids, naming the file.
func TestHeldRefusesARecordThatIsNotCommitIDs(t *testing.T) {
	clone := t.TempDir()
	if err := os.Mkdir(filepath.Join(clone, ".jj"), 0o755); err != nil {
		t.Fatal(err)
	}
	if held, ok, err := Held(clone); held != nil || ok || err != nil {
		t.Fatalf("Held of a clone with no record = %v, %v, %v; want none", held, ok, err)
	}
	record := filepath.Join(clone, ".jj", heldFile)
	if err := os.WriteFile(record, []byte(strings.Repeat("a", 40)+"\nnot a commit\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	if _, _, err := Held(clone); err == nil || !strings.Contains(err.Error(), record) {
		t.Fatalf("Held = %v, want a refusal naming %s", err, record)
	}
}
