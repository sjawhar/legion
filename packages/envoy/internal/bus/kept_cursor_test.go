package bus_test

import (
	"context"
	"encoding/json"
	"testing"
	"time"

	"github.com/sjawhar/envoy/internal/bus"
)

// The drain cursor's body is read by both writers of a role's kept lane - the listener's walk,
// which is its only writer, and the handover Dispatch's outbox makes behind a keep of its own -
// so the two numbers and the tags they are written under are this package's, not either
// caller's. A tag that drifted would decode as zero, which reads as a lane drained to its start
// and leads every handover astray, so the encoding is pinned here: what a walk writes decodes
// back to itself, and a body written exactly as a walk writes it decodes to the numbers in it.
func TestAKeptRoleCursorEncodesUnderTheTagsBothItsReadersExpect(t *testing.T) {
	written := bus.KeptRoleCursor{Kept: 11, Claiming: 12}
	body, err := json.Marshal(written)
	if err != nil {
		t.Fatalf("encode the cursor a walk writes: %v", err)
	}
	if want := `{"kept":11,"claiming":12}`; string(body) != want {
		t.Fatalf("a walk's cursor encodes as %s, want %s: the tags are what the lane's other reader decodes by", body, want)
	}
	var back bus.KeptRoleCursor
	if err := json.Unmarshal(body, &back); err != nil {
		t.Fatalf("decode the cursor a walk writes: %v", err)
	}
	if back != written {
		t.Fatalf("the cursor decoded back as %+v, want the %+v it was written as", back, written)
	}

	// A walk that passed a copy over, or confirmed the one it forwarded, writes the cursor with
	// nothing claimed; a body with no claiming at all is the same cursor.
	for _, stored := range []string{`{"kept":7,"claiming":0}`, `{"kept":7}`} {
		var cursor bus.KeptRoleCursor
		if err := json.Unmarshal([]byte(stored), &cursor); err != nil {
			t.Fatalf("decode %s: %v", stored, err)
		}
		if want := (bus.KeptRoleCursor{Kept: 7}); cursor != want {
			t.Fatalf("%s decoded as %+v, want %+v", stored, cursor, want)
		}
	}
}

// A claim is one walk's hold on a role's lane, and both readers of the cursor judge it by the one
// rule: it is outstanding while it names a copy and is younger than KeptRoleClaimWindow. A claim
// at the window exactly belongs to a walk whose own context has ended, so the copy it named is
// free for the next walk; a cursor claiming nothing is never outstanding, whatever the clock
// says, and its clock is not even read, since the listener's walk counts the reads it makes of
// its own clock (cmd/listener/role_keep.go, walkKeptRole).
func TestAKeptRoleCursorsClaimIsOutstandingOnlyWhileItIsYoungerThanTheWindow(t *testing.T) {
	stored := time.Now()
	claimed := bus.KeptRoleCursor{Kept: 4, Claiming: 5}
	for _, age := range []time.Duration{0, bus.KeptRoleClaimWindow - time.Millisecond} {
		if !claimed.ClaimOutstanding(stored, func() time.Time { return stored.Add(age) }) {
			t.Fatalf("a claim %s old is not outstanding, want it held: the walk that wrote it may be forwarding that copy now", age)
		}
	}
	for _, age := range []time.Duration{bus.KeptRoleClaimWindow, bus.KeptRoleClaimWindow + time.Minute} {
		if claimed.ClaimOutstanding(stored, func() time.Time { return stored.Add(age) }) {
			t.Fatalf("a claim %s old is outstanding, want it free: a walk cannot outlive %s, so the copy it claimed is the next walk's",
				age, bus.KeptRoleClaimWindow)
		}
	}

	unclaimed := bus.KeptRoleCursor{Kept: 4}
	reads := 0
	clock := func() time.Time {
		reads++
		return stored
	}
	if unclaimed.ClaimOutstanding(stored, clock) {
		t.Fatal("a cursor that claims nothing is outstanding, want a lane nothing holds")
	}
	if reads != 0 {
		t.Fatalf("a cursor that claims nothing read its caller's clock %d times, want none: a walk counts the reads of its own clock", reads)
	}
}

// A role's drain cursor is read back as the stream holds it: the body the listener's walk wrote,
// the sequence its next compare-and-swap must name, and the time the stream stored it, which is
// how old the claim it carries is. A lane no walk has written a cursor for is no error but the
// zero cursor, since every copy on such a lane is past it.
func TestReadKeptRoleCursorAnswersTheCursorTheStreamHolds(t *testing.T) {
	client := keptClient(t)
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	const role = "cursor-reader"

	unwritten, err := client.ReadKeptRoleCursor(ctx, role)
	if err != nil {
		t.Fatalf("read the cursor of a lane no walk has drained: %v", err)
	}
	if unwritten != (bus.StoredKeptRoleCursor{}) {
		t.Fatalf("the cursor of a lane no walk has drained is %+v, want the zero cursor", unwritten)
	}

	before := time.Now()
	claimed, err := json.Marshal(bus.KeptRoleCursor{Kept: 17, Claiming: 18})
	if err != nil {
		t.Fatalf("encode the claim a walk writes: %v", err)
	}
	seq, err := client.PublishCursorAt(ctx, bus.KeptRoleCursorSubject(role), claimed, 0)
	if err != nil {
		t.Fatalf("write the cursor a walk claims with: %v", err)
	}

	read, err := client.ReadKeptRoleCursor(ctx, role)
	if err != nil {
		t.Fatalf("read the cursor the walk wrote: %v", err)
	}
	if want := (bus.KeptRoleCursor{Kept: 17, Claiming: 18}); read.Cursor != want {
		t.Fatalf("the cursor read back as %+v, want the %+v the walk wrote", read.Cursor, want)
	}
	if read.Sequence != seq {
		t.Fatalf("the cursor is at sequence %d, want the %d the write answered: the next compare-and-swap names it", read.Sequence, seq)
	}
	if read.Written.Before(before) || read.Written.After(time.Now()) {
		t.Fatalf("the stream stored the cursor at %s, want a time within the write: the claim's age is measured from it", read.Written)
	}
	if !read.Cursor.ClaimOutstanding(read.Written, time.Now) {
		t.Fatal("the claim just written is not outstanding, want the lane held by the walk that wrote it")
	}

	confirmed, err := json.Marshal(bus.KeptRoleCursor{Kept: 18})
	if err != nil {
		t.Fatalf("encode the cursor the walk confirms with: %v", err)
	}
	next, err := client.PublishCursorAt(ctx, bus.KeptRoleCursorSubject(role), confirmed, seq)
	if err != nil {
		t.Fatalf("confirm the copy the walk forwarded: %v", err)
	}
	read, err = client.ReadKeptRoleCursor(ctx, role)
	if err != nil {
		t.Fatalf("read the cursor the walk confirmed with: %v", err)
	}
	if want := (bus.KeptRoleCursor{Kept: 18}); read.Cursor != want || read.Sequence != next {
		t.Fatalf("the cursor read back as %+v at %d, want %+v at %d: the newest write is what counts", read.Cursor, read.Sequence, want, next)
	}
	if read.Cursor.ClaimOutstanding(read.Written, time.Now) {
		t.Fatal("the confirmed cursor claims a copy, want a lane nothing holds")
	}
}
