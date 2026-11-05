package docs

import (
	"bytes"
	"fmt"
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

// Whatever a browser sends, the room lists the clocks ygo holds deleted and all of them: after
// each frame below is admitted through the hook, the room's list is the room's own delete set
// run for run (crdt.DeleteSetFromDoc), and a delete of every other character of the room's text
// is charged the same against the room's list as against a list seeded from that delete set. The
// frames run in order against one room, as a browser's updates reach one: forgeries - a GC run
// over clocks the room holds live, the reviewer's rooted deleted item of length 1,000, a GC run
// of a client the room holds nothing of - mixed with the honest traffic of an editor, a
// keystroke, a delete and a whole-state handoff, and a delete range past the clocks the room
// holds, which ygo parks and which neither the room's delete set nor the list holds.
//
// A frame carrying a GC struct the room does not hold yet is in that rule, not an exception to
// it. ygo decodes such a struct as an item already marked deleted and appends it to the store
// without integrating it (sjawhar/ygo v1.55.1-sami.1, crdt/update.go:1161-1174 and :938-949), so
// it joins no transaction's delete set, which is what the commit's update carries beside its
// structs (reearth/ygo#350) - but that update carries the struct itself, written back as a GC
// struct (encodeItem, crdt/update.go:441-466), and the walk the room's charge already makes of
// the commit lists it (roomCharge.applied, deletedRuns). So the room's delete set holds those
// clocks and so does its list, from the drain of the commit that appended them.
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

// fuzzHeldClient and fuzzHeldClocks are what a corpus entry writes where it means the fuzz
// room's own client and the number of clocks the room holds of it. The room's structs are a
// fresh document's (drainedPeerUpdateRoom, encodeDocumentTree, crdt.New), so its client is drawn
// anew at every run and no byte a corpus entry holds could name it: without these every input
// was a frame of clocks the room holds nothing of, and the forgeries the table above turns on -
// a GC run over exactly the clocks the room holds, which ygo skips whole, a delete range over
// them, which the room lists already - were out of the fuzzer's reach. Each input has them
// rewritten to that run's own client and clocks (namingTheFuzzRoomsClocks), so an entry naming
// them reaches the same structs at every run and a crash the fuzzer finds reproduces. Both are
// six-byte varuints, past the 32 bits a browser's client id is drawn from (crdt.New), so no
// honest frame carries their bytes.
const (
	fuzzHeldClient = 1<<40 + 101
	fuzzHeldClocks = 1<<40 + 103
)

// fuzzRoomsOwnClocks is the one client the fuzz room holds and the clocks it holds of it. A room
// of several clients answers for the lowest, and one of none - which the fixture never builds -
// answers false.
func fuzzRoomsOwnClocks(room *crdt.Doc) (uint64, uint64, bool) {
	held := room.StateVector()
	if len(held) == 0 {
		return 0, 0, false
	}
	client := slices.Sorted(maps.Keys(held))[0]
	return uint64(client), held[client], true
}

// namingTheFuzzRoomsClocks is update with every mention of the two sentinels rewritten to the
// room's own client and the clocks it holds of it (fuzzHeldClient, fuzzHeldClocks). The varuints
// it writes in are shorter than the ones it reads out, which moves every byte after them, as a
// corpus entry's own bytes move under the fuzzer's mutations. Bytes that mention neither
// sentinel are left exactly as the fuzzer wrote them.
func namingTheFuzzRoomsClocks(update []byte, room *crdt.Doc) []byte {
	client, clocks, held := fuzzRoomsOwnClocks(room)
	if !held {
		return update
	}
	update = bytes.ReplaceAll(update, varuint(fuzzHeldClient), varuint(client))
	return bytes.ReplaceAll(update, varuint(fuzzHeldClocks), varuint(clocks))
}

// The same property over a corpus: whatever bytes a browser sends, a room that admitted and
// applied them lists ygo's own delete set, run for run, the clocks of the frame's GC structs
// included, which the room lists from the commit that appended them - and the delete a browser
// sends next is charged against that list exactly as it would be against ygo's own delete set,
// which is what the list is kept for (weighPeerUpdate). The seeds are the crafted frames of the
// table above and the ones that name the room's own clocks through the sentinels: a GC run over
// every clock the room holds beside one of a client it holds nothing of (the gcRunOverHeldClocks
// shape), a delete range over those same clocks, and a frame that mixes the two with a fresh
// client's keystroke. Run as an ordinary test it runs the seeds alone, and a frame the hook
// refuses, or one whose apply fails, leaves the room nothing to answer for.
func FuzzAnAdmittedFrameLeavesTheRoomsListItsOwnDeleteSet(f *testing.F) {
	f.Add(rootedDeletedItem(1<<40+19, 1_000))
	f.Add(newV1Frame(1).group(1, 1<<40+23, 0).gc(7).noDeletes())
	f.Add(newV1Frame(0).uint(1).uint(1<<40 + 29).uint(1).uint(0).uint(4).done())
	f.Add(newV1Frame(1).group(1, 1<<40+31, 0).rooted(byte(structString), fragmentName, "").str("a").noDeletes())
	f.Add(newV1Frame(2).group(1, fuzzHeldClient, 0).gc(fuzzHeldClocks).group(1, gcForgeryClient, 0).gc(1).noDeletes())
	f.Add(newV1Frame(0).uint(1).uint(fuzzHeldClient).uint(1).uint(0).uint(fuzzHeldClocks).done())
	f.Add(newV1Frame(2).
		group(1, fuzzHeldClient, 0).gc(fuzzHeldClocks).
		group(1, 1<<40+37, 0).rooted(byte(structString), fragmentName, "").str("z").
		uint(1).uint(fuzzHeldClient).uint(1).uint(0).uint(fuzzHeldClocks).done())
	f.Fuzz(func(t *testing.T, update []byte) {
		service, room, replica, charge := drainedPeerUpdateRoom(t, "# Title\n\nOne paragraph of ordinary prose.\n")
		update = namingTheFuzzRoomsClocks(update, room)
		err, called := weighInto(service, room, update)
		runtime.KeepAlive(replica)
		if err != nil || !called {
			return
		}
		listed, held := listedRuns(charge.deletes()), roomDeletedRuns(room)
		if !maps.EqualFunc(listed, held, slices.Equal) {
			t.Fatalf("after the %d-byte frame the room lists %v deleted, want ygo's own delete set, %v", len(update), listed, held)
		}
		// The delete that follows, over the room's own clocks and past them, weighs the same
		// against the list as against ygo's delete set: a list equal run for run charges alike.
		client, clocks, _ := fuzzRoomsOwnClocks(room)
		probe := everyOtherClockDeleted(client, clocks+8)
		var truth deletedRuns
		truth.seed(crdt.DeleteSetFromDoc(room))
		against, fresh := weighedAgainst(t, room, charge.deletes(), probe), weighedAgainst(t, room, &truth, probe)
		if !sameCharge(against, fresh) {
			t.Fatalf("after the %d-byte frame a delete of every other clock weighs %d structs, %d splits, %d frame bytes and %d structure bytes against the room's list and %d, %d, %d and %d against its own delete set, want the same of each",
				len(update), against.added(), against.splits, against.frame, against.structureHeap(),
				fresh.added(), fresh.splits, fresh.frame, fresh.structureHeap())
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

// The two rules the room leans on when it lists a GC run out of its own commit, pinned against
// the ygo the module names. A GC struct over clocks the document already holds is skipped by the
// apply before anything is appended (sjawhar/ygo v1.55.1-sami.1, integrateGroup steps over a
// struct ending at or before the client's clock, crdt/update.go:899-917), so the store is as it
// was and the transaction logs no update at all (collectAndEncodeLocked logs one only for a
// transaction that changed the document or parked something, crdt/doc.go:844-873). A GC struct
// past those clocks is appended to the store already deleted, without integrating it
// (crdt/update.go:1161-1174 and :938-949), so ygo holds it deleted (buildDeleteSet reads
// item.Deleted, crdt/update.go:659-678) while no transaction's delete set names it - Item.delete
// steps past an item already deleted before it reaches txn.deleteSet.add (crdt/item.go:500-507,
// the range at :536; reearth/ygo#350) - and the commit's own logged update carries it, written
// back as a GC struct because it has no parent, no origins and no parent id (encodeItem,
// crdt/update.go:441-466).
//
// Together they are why the walk of the room's own commit may list a GC run as the room's
// (deletedRuns, roomCharge.applied): every GC struct it reads is one ygo holds deleted, and
// never a clock the document held before the commit. Should a later ygo stop skipping the first,
// or stop logging the second, this fails here rather than the room's list quietly listing clocks
// ygo holds live.
func TestAGCRunOverClocksADocumentHoldsIsSkippedAndOneBeyondThemIsLoggedDeleted(t *testing.T) {
	const (
		heldClient = 1<<40 + 41
		gcClient   = 1<<40 + 43
		gcRun      = 7
	)
	doc := newDocumentCopy()
	seed := newV1Frame(1).group(1, heldClient, 0).rooted(byte(structString), fragmentName, "").str("hello world").noDeletes()
	if err := crdt.ApplyUpdateV1(doc, seed, nil); err != nil {
		t.Fatal(err)
	}
	held := doc.StateVector()[crdt.ClientID(heldClient)]
	structs := countRoomStructs(doc)
	if held == 0 || structs == 0 {
		t.Fatalf("the document holds %d clocks of the seeded client in %d structs, want a document holding its text", held, structs)
	}
	log := doc.OpenUpdateLog()
	t.Cleanup(log.Close)
	log.Drain()

	over := newV1Frame(1).group(1, heldClient, 0).gc(held).noDeletes()
	if err := crdt.ApplyUpdateV1(doc, over, nil); err != nil {
		t.Fatal(err)
	}
	if _, logged := log.Drain(); len(logged) != 0 {
		t.Fatalf("a GC run over the %d clocks the document holds logged %d updates, want none: ygo skips such a struct", held, len(logged))
	}
	if after := countRoomStructs(doc); after != structs {
		t.Fatalf("a GC run over the clocks the document holds left it %d structs, want the %d it held", after, structs)
	}
	if ranges := crdt.DeleteSetFromDoc(doc).Ranges(crdt.ClientID(heldClient)); len(ranges) != 0 {
		t.Fatalf("a GC run over the clocks the document holds left ygo holding %v of them deleted, want none", ranges)
	}

	past := newV1Frame(1).group(1, gcClient, 0).gc(gcRun).noDeletes()
	if err := crdt.ApplyUpdateV1(doc, past, nil); err != nil {
		t.Fatal(err)
	}
	_, logged := log.Drain()
	if len(logged) != 1 {
		t.Fatalf("a GC run past the clocks the document holds logged %d updates, want the one its commit made", len(logged))
	}
	walked, _, err := walkRecorded(logged[0].Update, fromRoom)
	if err != nil {
		t.Fatal(err)
	}
	runs := 0
	for _, s := range walked.structs {
		if s.kind == structGC && s.client == gcClient && s.clock == 0 && s.length == gcRun {
			runs++
		}
	}
	if runs != 1 || len(walked.structs) != 1 {
		t.Fatalf("the commit's logged update carries %d structs, %d of them the %d-clock GC run that arrived, want that run alone", len(walked.structs), runs, gcRun)
	}
	for _, ranged := range walked.deletes {
		if ranged.client == gcClient {
			t.Fatalf("the commit's own delete set names the GC run's clocks %v, want a run no commit's delete set holds (reearth/ygo#350)", ranged)
		}
	}
	if ranges := crdt.DeleteSetFromDoc(doc).Ranges(crdt.ClientID(gcClient)); len(ranges) != 1 || ranges[0].Clock != 0 || ranges[0].Len != gcRun {
		t.Fatalf("ygo holds %v of the GC run deleted, want its whole %d clocks", ranges, gcRun)
	}
}

// An honest browser keeps ygo's GC on, as every Yjs browser does: the paragraphs it writes and
// then deletes while it is offline are collected into GC structs, and its whole state carries
// them to the room on every connect. The room appends each of them to its store already deleted
// (deletedRuns), so from the second connect on the room holds every one of those clocks deleted
// and the browser's handoff adds nothing to its heap. Listed only from the delete sets of the
// room's commits, which name none of them (reearth/ygo#350), the room charged that handoff
// afresh on every reconnect - 400 bytes of structure part, one struct of the struct total and
// about 86 frame bytes for each unlisted GC run, for a room whose heap did not move, and the
// structure part no count clears - so a room held open across a day's reconnects grew without
// bound. Listed from the GC structs of the room's own commits too, the over-charge is nothing:
// here, over six connects of one browser holding 10, 500 and 10,000 GC runs, every reconnect
// weighs against the room's list exactly what it weighs against a list seeded fresh from
// crdt.DeleteSetFromDoc, and moves the structure part by nothing.
func TestAnHonestBrowsersReconnectsAreNotOverChargedForItsOwnGCRuns(t *testing.T) {
	for _, runs := range []int{10, 500, 10_000} {
		t.Run(fmt.Sprintf("%d GC runs", runs), func(t *testing.T) {
			service, room, replica, charge := drainedPeerUpdateRoom(t, "# Title\n\nOne paragraph of ordinary prose.\n")
			handoff := browserHoldingGCRuns(t, room, runs)
			if held := gcRunsIn(t, handoff); held < runs {
				t.Fatalf("the browser's %d-byte handoff carries %d GC runs, want the %d it collected", len(handoff), held, runs)
			}
			for connect := 1; connect <= 6; connect++ {
				var truth deletedRuns
				truth.seed(crdt.DeleteSetFromDoc(room))
				against := weighedAgainst(t, room, charge.deletes(), handoff)
				fresh := weighedAgainst(t, room, &truth, handoff)
				structure, _, _, _ := chargeParts(charge)
				if err, called := weighInto(service, room, handoff); err != nil || !called {
					t.Fatalf("connect %d: the %d-byte handoff was answered %v (applied %t), want it admitted", connect, len(handoff), err, called)
				}
				runtime.KeepAlive(replica)
				grew, _, _, _ := chargeParts(charge)
				t.Logf("connect %d: the handoff weighs %d structs, %d splits and %d frame bytes against the room's list and %d, %d and %d against its own delete set, and moves the structure part by %d bytes",
					connect, against.added(), against.splits, against.frame,
					fresh.added(), fresh.splits, fresh.frame, grew-structure)
				if !sameCharge(against, fresh) {
					t.Fatalf("connect %d: the handoff weighs %d structs, %d splits, %d frame bytes and %d structure bytes against the room's list and %d, %d, %d and %d against its own delete set, want no over-charge",
						connect, against.added(), against.splits, against.frame, against.structureHeap(),
						fresh.added(), fresh.splits, fresh.frame, fresh.structureHeap())
				}
				if connect > 1 && grew != structure {
					t.Fatalf("connect %d: the handoff moved the structure part from %d to %d bytes, want a reconnect of a browser the room already holds to move it by nothing", connect, structure, grew)
				}
			}
		})
	}
}

// browserHoldingGCRuns is the whole state of a browser that worked offline on a copy of the room
// with ygo's GC on: it cut the start of the room's own paragraph, wrote 2*runs paragraphs of its
// own and cut every other one of them, so that no two of its deletions touch. ygo collects a
// deleted paragraph as the transaction commits, turning every item under it into a GC struct
// (gcTxnDeleteSet's ContentType case, sjawhar/ygo v1.55.1-sami.1, crdt/snapshot.go:370-404,
// through gcTypeItems, :431-470), so the copy's encode carries runs separated deleted runs of
// the browser's own client, each of them a deleted paragraph the room integrates beside the GC
// structs of what was under it, which no commit's delete set names. That is what a browser hands
// back on every connect.
func browserHoldingGCRuns(t *testing.T, room *crdt.Doc, runs int) []byte {
	t.Helper()
	browser, text := peerDocument(t, room)
	fragment := browser.GetXmlFragment(fragmentName)
	browser.Transact(func(txn *crdt.Transaction) { text.Delete(txn, 0, min(4, text.Len())) })
	browser.Transact(func(txn *crdt.Transaction) {
		// Prepended, as document_copy_test's browser does: ygo walks a fragment's children from
		// its start to find an insert position, so prepending each is the cheap end.
		for range 2 * runs {
			paragraph := crdt.NewYXmlElement("paragraph")
			fragment.InsertElement(txn, 0, paragraph)
			paragraph.InsertText(txn, 0, crdt.NewYXmlText())
		}
	})
	browser.Transact(func(txn *crdt.Transaction) {
		for at := 2*runs - 1; at > 0; at -= 2 {
			fragment.Delete(txn, at, 1)
		}
	})
	return crdt.EncodeStateAsUpdateV1(browser, nil)
}

// gcRunsIn is how many GC structs update carries.
func gcRunsIn(t *testing.T, update []byte) int {
	t.Helper()
	walked, _, err := walkRecorded(update, fromPeer)
	if err != nil {
		t.Fatal(err)
	}
	runs := 0
	for _, s := range walked.structs {
		if s.kind == structGC {
			runs++
		}
	}
	return runs
}

// A browser that stays connected types and deletes through the hook, as a person writing does,
// and the room is never over-charged for it: after each of its updates the room's list is ygo's
// own delete set run for run, and the next update weighs the same against the list as against a
// list seeded fresh from it. The room's own GC runs on every commit that deletes (gcTxnDeleteSet,
// sjawhar/ygo v1.55.1-sami.1, crdt/snapshot.go:370-407), so the commits here carry the GC structs
// the walk now lists beside the delete sets it always read.
func TestAConnectedTypingSessionWithDeletesIsNeverOverCharged(t *testing.T) {
	service, room, replica, charge := drainedPeerUpdateRoom(t, "# Title\n\nOne paragraph of ordinary prose.\n")
	for round := range 12 {
		update := peerTyping(t, room, "typed ")
		if round%3 == 2 {
			update = peerDeletingAt(t, room, 2, 4)
		}
		if err, called := weighInto(service, room, update); err != nil || !called {
			t.Fatalf("round %d: the %d-byte update was answered %v (applied %t), want it admitted", round, len(update), err, called)
		}
		runtime.KeepAlive(replica)
		listed, held := listedRuns(charge.deletes()), roomDeletedRuns(room)
		if !maps.EqualFunc(listed, held, slices.Equal) {
			t.Fatalf("round %d: the room lists %v deleted, want ygo's own delete set, %v", round, listed, held)
		}
		probe := peerDeletingEveryOtherCharacter(t, room)
		var truth deletedRuns
		truth.seed(crdt.DeleteSetFromDoc(room))
		against, fresh := weighedAgainst(t, room, charge.deletes(), probe), weighedAgainst(t, room, &truth, probe)
		if !sameCharge(against, fresh) {
			t.Fatalf("round %d: a delete of every other character weighs %d structs, %d splits, %d frame bytes and %d structure bytes against the room's list and %d, %d, %d and %d against its own delete set, want no over-charge",
				round, against.added(), against.splits, against.frame, against.structureHeap(),
				fresh.added(), fresh.splits, fresh.frame, fresh.structureHeap())
		}
	}
}
