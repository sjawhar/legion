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

// A deleted item is not a clock the room holds deleted. A browser's 26-byte frame carrying one
// deleted item of length 1,000 rooted under the fragment is admitted, and ygo integrates it live:
// it decodes the item as one carrying ContentDeleted whose Deleted stays false (sjawhar/ygo
// v1.54.1-sami.3, crdt/update.go:1183-1191, decodeContent :1222-1229), Item.integrate never marks
// it deleted (crdt/item.go:81-420), and buildDeleteSet lists item.Deleted alone
// (crdt/update.go:665-672), so the room's delete set holds no range of that client. Listed as
// deleted from a walk of the room's own encode, as the room once listed every struct born deleted
// - ygo writes the item back under the deleted reference (encodeItem, crdt/update.go:488-491) - a
// varuint length of 1,000 clocks took them off the books, and a delete of every other one of them
// was charged nothing at all. Here that delete is charged against the room's list exactly what it
// is charged against a list seeded from the room's own delete set, and the room covers none of
// those clocks.
func TestADeletedItemTheRoomHoldsLiveLeavesLaterDeletesCharged(t *testing.T) {
	const (
		itemClient = 1<<40 + 3
		itemRun    = 1_000
	)
	service, room, replica, charge := drainedPeerUpdateRoom(t, "# Title\n\nOne paragraph of ordinary prose.\n")
	forged := rootedDeletedItem(itemClient, itemRun)
	if err, called := weighInto(service, room, forged); err != nil || !called {
		t.Fatalf("the %d-byte frame was answered %v (applied %t), want it admitted", len(forged), err, called)
	}
	runtime.KeepAlive(replica)
	if ranges := crdt.DeleteSetFromDoc(room).Ranges(crdt.ClientID(itemClient)); len(ranges) != 0 {
		t.Fatalf("ygo holds %v of the deleted item deleted, want an item it holds live", ranges)
	}

	deletes := everyOtherClockDeleted(itemClient, itemRun)
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
	if charge.deletes().covers(itemClient, 0, itemRun) {
		t.Fatalf("the room lists the %d clocks of a deleted item ygo holds live, want it to list only what ygo holds deleted", itemRun)
	}
}

// Whatever a browser sends, the room lists what ygo holds deleted and nothing else: after each
// frame below is admitted through the hook, the room's list is the room's own delete set run for
// run (crdt.DeleteSetFromDoc), and a delete of every other character of the room's text is charged
// the same against the room's list as against a list seeded from that delete set. The frames run
// in order against one room, as a browser's updates reach one: forgeries - a GC run over clocks
// the room holds live, the reviewer's rooted deleted item of length 1,000, a GC run of a client
// the room holds nothing of - mixed with the honest traffic of an editor, a keystroke, a delete
// and a whole-state handoff, and a delete range past the clocks the room holds, which ygo parks
// and which neither the room's delete set nor the list holds.
func TestEveryAdmittedFrameLeavesTheRoomsListItsOwnDeleteSet(t *testing.T) {
	const (
		deletedItemClient = 1<<40 + 11
		freshGCClient     = 1<<40 + 13
		parkedClient      = 1<<40 + 17
	)
	service, room, replica, charge := drainedPeerUpdateRoom(t, strings.Repeat("word ", 60)+"\n")
	for _, row := range []struct {
		name  string
		frame func(t *testing.T) []byte
	}{
		{"a GC run over every clock the room holds", func(*testing.T) []byte { return gcRunOverHeldClocks(room) }},
		{"a rooted deleted item of length 1,000", func(*testing.T) []byte { return rootedDeletedItem(deletedItemClient, 1_000) }},
		{"a GC run of a client the room holds nothing of", func(*testing.T) []byte {
			return newV1Frame(1).group(1, freshGCClient, 0).gc(7).noDeletes()
		}},
		{"a browser's keystroke", func(t *testing.T) []byte { return peerTyping(t, room, "y") }},
		{"a browser's delete of three characters", func(t *testing.T) []byte { return peerDeletingAt(t, room, 5, 3) }},
		{"the same deleted item again", func(*testing.T) []byte { return rootedDeletedItem(deletedItemClient, 1_000) }},
		{"a browser's whole-state handoff of the room", func(*testing.T) []byte { return crdt.EncodeStateAsUpdateV1(room, nil) }},
		{"a delete range past the clocks the room holds", func(*testing.T) []byte {
			return newV1Frame(0).uint(1).uint(parkedClient).uint(1).uint(0).uint(4).done()
		}},
	} {
		t.Run(row.name, func(t *testing.T) {
			update := row.frame(t)
			if err, called := weighInto(service, room, update); err != nil || !called {
				t.Fatalf("the %d-byte frame was answered %v (applied %t), want it admitted", len(update), err, called)
			}
			runtime.KeepAlive(replica)
			listed, held := listedRuns(charge.deletes()), roomDeletedRuns(room)
			if !maps.EqualFunc(listed, held, slices.Equal) {
				t.Fatalf("the room lists %v deleted, want ygo's own delete set, %v", listed, held)
			}
			probe := peerDeletingEveryOtherCharacter(t, room)
			var truth deletedRuns
			truth.seed(crdt.DeleteSetFromDoc(room))
			against, fresh := weighedAgainst(t, room, charge.deletes(), probe), weighedAgainst(t, room, &truth, probe)
			if !sameCharge(against, fresh) {
				t.Fatalf("a delete of every other character weighs %d structs, %d splits, %d frame bytes and %d structure bytes against the room's list and %d, %d, %d and %d against its own delete set, want the same of each",
					against.added(), against.splits, against.frame, against.structureHeap(),
					fresh.added(), fresh.splits, fresh.frame, fresh.structureHeap())
			}
		})
	}
}

// The same property over a corpus: whatever bytes a browser sends, a room that admitted and
// applied them lists what ygo holds deleted and nothing else. The seeds are the crafted frames of
// the table above; run as an ordinary test it runs them alone, and a frame the hook refuses, or
// one whose apply fails, leaves the room nothing to answer for.
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
		if !maps.EqualFunc(listed, held, slices.Equal) {
			t.Fatalf("after the %d-byte frame the room lists %v deleted, want ygo's own delete set, %v", len(update), listed, held)
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
