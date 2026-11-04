package docs

import (
	"maps"
	"runtime"
	"slices"
	"strings"
	"testing"

	"github.com/reearth/ygo/crdt"
)

// listedRuns is what the list holds, each client's runs in order, read under its lock.
func listedRuns(d *deletedRuns) map[uint64][]clockRun {
	d.mu.RLock()
	defer d.mu.RUnlock()
	runs := make(map[uint64][]clockRun, len(d.clients))
	for client, held := range d.clients {
		runs[client] = slices.Clone(held)
	}
	return runs
}

// roomDeletedRuns is the room's own delete set as the list would hold it: every range
// crdt.DeleteSetFromDoc answers for, seeded into a list of its own (deletedRuns.seed), so the two
// are merged by the same rule and compare run for run.
func roomDeletedRuns(room *crdt.Doc) map[uint64][]clockRun {
	var seeded deletedRuns
	seeded.seed(crdt.DeleteSetFromDoc(room))
	return listedRuns(&seeded)
}

// weighedAgainst is what the peer update hook's weighing makes of update against known, the list
// of what the room is taken to hold deleted.
func weighedAgainst(t *testing.T, room *crdt.Doc, known *deletedRuns, update []byte) peerWeighing {
	t.Helper()
	weighed := peerWeighing{held: room.StateVector(), known: known}
	if _, err := walkUpdate(update, fromPeer, &weighed); err != nil {
		t.Fatal(err)
	}
	return weighed
}

// sameCharge is whether two weighings charge their room the same: the structs they add to its
// total, their splits, the structure part they move and the bytes they add to its frame total.
func sameCharge(a, b peerWeighing) bool {
	return a.added() == b.added() && a.splits == b.splits && a.deletes == b.deletes &&
		a.frame == b.frame && a.structureHeap() == b.structureHeap()
}

// chargesAtLeast is whether a charges its room at least what b charges it, part for part: what a
// delete weighed against the room's list comes to beside what it weighs against the room's own
// delete set, which the list is a subset of, so a range weighed against it is charged no less
// (deletedRuns).
func chargesAtLeast(a, b peerWeighing) bool {
	return a.added() >= b.added() && a.splits >= b.splits && a.deletes >= b.deletes &&
		a.frame >= b.frame && a.structureHeap() >= b.structureHeap()
}

// listedWithin is whether every run listed lies inside a run held: the room's list holds no clock
// ygo holds live.
func listedWithin(listed, held map[uint64][]clockRun) bool {
	for client, runs := range listed {
		for _, run := range runs {
			inside := false
			for _, holds := range held[client] {
				inside = inside || (holds.start <= run.start && run.end <= holds.end)
			}
			if !inside {
				return false
			}
		}
	}
	return true
}

// everyOtherClockDeleted is the frame a browser sends that deletes every other clock in
// [0, clocks) of client: a delete range of one clock each, no two of them touching, so each range
// the room does not hold deleted is charged its own splits.
func everyOtherClockDeleted(client, clocks uint64) []byte {
	f := newV1Frame(0).uint(1).uint(client).uint((clocks + 1) / 2)
	for clock := uint64(0); clock < clocks; clock += 2 {
		f = f.uint(clock).uint(1)
	}
	return f.done()
}

// A struct the room holds live is not a clock the room holds deleted, whatever the struct's own
// bytes say. The frame here is the reviewer's rooted deleted item of length 1,000 - 26 bytes
// whose varuint length can claim up to 2^53 clocks - sent for a client the room holds no earlier
// clock of, so ygo parks it rather than integrating it and the room holds none of its clocks,
// live or deleted. Listed as deleted from the bytes a browser sent, or from a walk of the room's
// own encode of its commit, those 1,000 clocks come off the books and a later delete of them is
// charged nothing: at d88e2988, where the room listed every struct its own commit's encode read
// as born deleted, this frame's integrated form left a 500-range delete weighing 0 structs, 0
// splits and 0 frame bytes against the room's list where ygo's own delete set charged 999, 999
// and 81,360. The pin closes that form of the divergence - Item.integrate marks an arriving
// ContentDeleted item deleted as Yjs does (sjawhar/ygo v1.55.1-sami.1, crdt/item.go:298-306 and
// :453-455), so an integrated deleted item is one ygo holds deleted and the commit's delete set
// carries it - and a parked one is the struct it still leaves live. Here the delete of every
// other clock is charged against the room's list exactly what it is charged against a list
// seeded from the room's own delete set, and the room covers none of those clocks.
func TestADeletedItemTheRoomHoldsLiveLeavesLaterDeletesCharged(t *testing.T) {
	const (
		itemClient = 1<<40 + 3
		itemClock  = 1
		itemRun    = 1_000
	)
	service, room, replica, charge := drainedPeerUpdateRoom(t, "# Title\n\nOne paragraph of ordinary prose.\n")
	forged := newV1Frame(1).group(1, itemClient, itemClock).
		rooted(byte(structDeleted), fragmentName, "").uint(itemRun).noDeletes()
	if err, called := weighInto(service, room, forged); err != nil || !called {
		t.Fatalf("the %d-byte frame was answered %v (applied %t), want it admitted", len(forged), err, called)
	}
	runtime.KeepAlive(replica)
	if room.PendingStats().Items == 0 {
		t.Fatal("the fixture did not park the deleted item, so the room holds no struct it reads as born deleted")
	}
	if ranges := crdt.DeleteSetFromDoc(room).Ranges(crdt.ClientID(itemClient)); len(ranges) != 0 {
		t.Fatalf("ygo holds %v of the deleted item deleted, want an item it holds live", ranges)
	}

	deletes := everyOtherClockDeleted(itemClient, itemClock+itemRun)
	var truth deletedRuns
	truth.seed(crdt.DeleteSetFromDoc(room))
	listed := weighedAgainst(t, room, charge.deletes(), deletes)
	fresh := weighedAgainst(t, room, &truth, deletes)
	if !sameCharge(listed, fresh) {
		t.Fatalf("the delete weighs %d structs, %d splits and %d frame bytes against the room's list and %d, %d and %d against its own delete set, want the same of each",
			listed.added(), listed.splits, listed.frame, fresh.added(), fresh.splits, fresh.frame)
	}
	if listed.added() == 0 || listed.splits == 0 || listed.frame == 0 || listed.structureHeap() == 0 {
		t.Fatalf("the delete of every other clock of the item weighs %d structs, %d splits, %d frame bytes and %d structure bytes, want each of them charged",
			listed.added(), listed.splits, listed.frame, listed.structureHeap())
	}
	if charge.deletes().covers(itemClient, itemClock, itemRun) {
		t.Fatalf("the room lists the %d clocks of a deleted item ygo holds live, want it to list only what ygo holds deleted", itemRun)
	}
}

// Whatever a browser sends, the room lists only clocks ygo holds deleted: after each frame below
// is admitted through the hook, the room's list is the room's own delete set run for run
// (crdt.DeleteSetFromDoc), and a delete of every other character of the room's text is charged
// the same against the room's list as against a list seeded from that delete set. The frames run
// in order against one room, as a browser's updates reach one: forgeries - a GC run over clocks
// the room holds live, the reviewer's rooted deleted item of length 1,000, a GC run of a client
// the room holds nothing of - mixed with the honest traffic of an editor, a keystroke, a delete
// and a whole-state handoff, and a delete range past the clocks the room holds, which ygo parks
// and which neither the room's delete set nor the list holds.
//
// A frame carrying a GC struct the room does not hold yet is the one exception, and the rows
// after it inherit it. ygo decodes such a struct as an item already marked deleted and appends it
// to the store without integrating it (sjawhar/ygo v1.55.1-sami.1, crdt/update.go:1161-1174 and
// :938-949), so it never joins the transaction's delete set, which is what the commit's update
// carries (reearth/ygo#350) and all the room lists from. The room's own delete set holds those
// clocks; its list does not until a load seeds them (deletedRuns.seed). From there the property
// is the one that matters for the budget: the list is a subset of ygo's delete set, so a later
// delete weighs at least as much against the list as against the truth and is never under-charged.
func TestEveryAdmittedFrameLeavesTheRoomsListItsOwnDeleteSet(t *testing.T) {
	const (
		deletedItemClient = 1<<40 + 11
		freshGCClient     = 1<<40 + 13
		parkedClient      = 1<<40 + 17
	)
	service, room, replica, charge := drainedPeerUpdateRoom(t, strings.Repeat("word ", 60)+"\n")
	gcSent := false
	for _, row := range []struct {
		name  string
		gc    bool
		frame func(t *testing.T) []byte
	}{
		{"a GC run over every clock the room holds", true, func(*testing.T) []byte { return gcRunOverHeldClocks(room) }},
		{"a rooted deleted item of length 1,000", false, func(*testing.T) []byte { return rootedDeletedItem(deletedItemClient, 1_000) }},
		{"a GC run of a client the room holds nothing of", true, func(*testing.T) []byte {
			return newV1Frame(1).group(1, freshGCClient, 0).gc(7).noDeletes()
		}},
		{"a browser's keystroke", false, func(t *testing.T) []byte { return peerTyping(t, room, "y") }},
		{"a browser's delete of three characters", false, func(t *testing.T) []byte { return peerDeletingAt(t, room, 5, 3) }},
		{"the same deleted item again", false, func(*testing.T) []byte { return rootedDeletedItem(deletedItemClient, 1_000) }},
		{"a browser's whole-state handoff of the room", false, func(*testing.T) []byte { return crdt.EncodeStateAsUpdateV1(room, nil) }},
		{"a delete range past the clocks the room holds", false, func(*testing.T) []byte {
			return newV1Frame(0).uint(1).uint(parkedClient).uint(1).uint(0).uint(4).done()
		}},
	} {
		gcSent = gcSent || row.gc
		exact := !gcSent
		t.Run(row.name, func(t *testing.T) {
			update := row.frame(t)
			if err, called := weighInto(service, room, update); err != nil || !called {
				t.Fatalf("the %d-byte frame was answered %v (applied %t), want it admitted", len(update), err, called)
			}
			runtime.KeepAlive(replica)
			listed, held := listedRuns(charge.deletes()), roomDeletedRuns(room)
			if !listedWithin(listed, held) {
				t.Fatalf("the room lists %v deleted, want a subset of ygo's own delete set, %v", listed, held)
			}
			if exact && !maps.EqualFunc(listed, held, slices.Equal) {
				t.Fatalf("the room lists %v deleted, want ygo's own delete set, %v", listed, held)
			}
			probe := peerDeletingEveryOtherCharacter(t, room)
			var truth deletedRuns
			truth.seed(crdt.DeleteSetFromDoc(room))
			against, fresh := weighedAgainst(t, room, charge.deletes(), probe), weighedAgainst(t, room, &truth, probe)
			if !chargesAtLeast(against, fresh) || (exact && !sameCharge(against, fresh)) {
				t.Fatalf("a delete of every other character weighs %d structs, %d splits, %d frame bytes and %d structure bytes against the room's list and %d, %d, %d and %d against its own delete set, want at least each of them (exactly, with no GC run sent: %t)",
					against.added(), against.splits, against.frame, against.structureHeap(),
					fresh.added(), fresh.splits, fresh.frame, fresh.structureHeap(), exact)
			}
		})
	}
}

// The same property over a corpus: whatever bytes a browser sends, a room that admitted and
// applied them lists only clocks ygo holds deleted - the list is a subset of the room's own
// delete set, equal to it but for the clocks of any GC struct the frame carried. The seeds are
// the crafted frames of the table above; run as an ordinary test it runs them alone, and a frame
// the hook refuses, or one whose apply fails, leaves the room nothing to answer for.
func FuzzAnAdmittedFrameLeavesTheRoomsListItsOwnDeleteSet(f *testing.F) {
	f.Add(rootedDeletedItem(1<<40+19, 1_000))
	f.Add(newV1Frame(1).group(1, 1<<40+23, 0).gc(7).noDeletes())
	f.Add(newV1Frame(0).uint(1).uint(1<<40 + 29).uint(1).uint(0).uint(4).done())
	f.Add(newV1Frame(1).group(1, 1<<40+31, 0).rooted(byte(structString), fragmentName, "").str("a").noDeletes())
	f.Fuzz(func(t *testing.T, update []byte) {
		service, room, replica, charge := drainedPeerUpdateRoom(t, "# Title\n\nOne paragraph of ordinary prose.\n")
		err, called := weighInto(service, room, update)
		runtime.KeepAlive(replica)
		if err != nil || !called {
			return
		}
		listed, held := listedRuns(charge.deletes()), roomDeletedRuns(room)
		if !listedWithin(listed, held) {
			t.Fatalf("after the %d-byte frame the room lists %v deleted, want a subset of ygo's own delete set, %v", len(update), listed, held)
		}
	})
}

// A commit into a room holding thousands of separate deleted runs costs the list no more than one
// into a room holding a few dozen. ygo writes the room's whole delete set into the update of every
// transaction (deletedRuns), so a keystroke's update carries every run the room holds; the walk the
// room's charge already makes of that update lists each range of it (roomCharge.applied), and a
// range the list covers already costs a read of the list and no write, so nothing is allocated for
// it however many runs there are.
func TestACommitIntoARoomOfThousandsOfDeletedRunsAllocatesNoMore(t *testing.T) {
	perCommit := func(runs int) float64 {
		t.Helper()
		_, room, replica, charge := drainedPeerUpdateRoom(t, strings.Repeat("word ", runs)+"\n")
		text := firstText(room.GetXmlFragment(fragmentName))
		if text.Len() < 2*runs {
			t.Fatalf("the room's paragraph holds %d characters, want at least %d", text.Len(), 2*runs)
		}
		room.Transact(func(txn *crdt.Transaction) {
			for at := 2*runs - 1; at > 0; at -= 2 {
				text.Delete(txn, at, 1)
			}
		})
		runtime.KeepAlive(replica)
		listed := listedRuns(charge.deletes())
		if held := roomDeletedRuns(room); !maps.EqualFunc(listed, held, slices.Equal) {
			t.Fatalf("the room lists %d clients' runs, want ygo's own delete set, %d clients'", len(listed), len(held))
		}
		total := 0
		for _, client := range listed {
			total += len(client)
		}
		if total != runs {
			t.Fatalf("the room holds %d deleted runs, want the %d its transaction deleted", total, runs)
		}
		var logged []byte
		stop := room.OnUpdate(func(update []byte, _ any) { logged = update })
		room.Transact(func(txn *crdt.Transaction) { text.Insert(txn, 0, "y", nil) })
		stop()
		if len(logged) == 0 {
			t.Fatal("the keystroke's transaction logged no update")
		}
		return testing.AllocsPerRun(10, func() { charge.applied(logged) })
	}
	few, thousands := perCommit(50), perCommit(4_000)
	t.Logf("a commit allocates %.0f objects against 50 deleted runs and %.0f against 4,000", few, thousands)
	if thousands > few {
		t.Fatalf("a commit into a room of 4,000 deleted runs allocates %.0f objects, past the %.0f one into a room of 50 allocates", thousands, few)
	}
}
