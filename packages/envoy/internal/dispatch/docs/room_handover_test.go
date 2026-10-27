package docs

import (
	"context"
	"errors"
	"net/http"
	"net/http/httptest"
	"slices"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/reearth/ygo/crdt"

	"github.com/sjawhar/envoy/internal/dispatch/model"
)

// A forced close of a document's room used to throw the room's record away, so the next access
// loaded the document afresh. ygo takes the closed room out of its map before the close returns,
// so a browser that opens the document in between registers on that record (authorize registers a
// browser before its room is loaded or joined), the room that browser's connect loads registers on
// it too, and an edit typed in that room is credited and recorded on it. Throwing the record away
// lost all three: the person's keystrokes were credited to nobody, and the version that carried
// them listed no author for them.
//
// Every path that closes a room and settles its record itself is covered. The first two are
// closeReservedRoom's (a settlement whose room's stored history was replaced, and the catch-up's
// close of such a room); the last three are evictRoom's (Evict, Quiesce and a failed room's
// eviction).
func TestAForcedCloseKeepsTheBrowserThatJoinedWhileItRan(t *testing.T) {
	bob := model.Actor{Kind: "user", ID: "bob"}
	const connection = 7
	for _, closer := range []struct {
		name string
		// loads is whether a room can load in the window as well, and so whether the browser can
		// type in it there. A failed room refuses every load until its eviction has run
		// (onLoadDocument), so only the connect attaches on that path, and the browser types once
		// the document has a room again.
		loads bool
		force func(t *testing.T, service *Service, artifactID string, live *crdt.Doc)
	}{
		{"a settlement on a replaced history", true, func(t *testing.T, service *Service, artifactID string, live *crdt.Doc) {
			state := service.room(artifactID)
			state.mu.Lock()
			generation := state.roomGeneration
			state.mu.Unlock()
			service.reloadReplacedRoom(context.Background(), artifactID, state, generation, live,
				errors.New("the document's stored history was replaced under its room"))
		}},
		{"the catch-up's close of a replaced history", true, func(t *testing.T, service *Service, artifactID string, live *crdt.Doc) {
			closed, err := service.closeRoomBehindHead(context.Background(), artifactID, live)
			if err != nil {
				t.Fatalf("close the room the catch-up found on a replaced history: %v", err)
			}
			if !closed {
				t.Fatal("the catch-up's close closed no room")
			}
		}},
		{"Evict", true, func(t *testing.T, service *Service, artifactID string, _ *crdt.Doc) {
			if err := service.Evict(context.Background(), artifactID); err != nil {
				t.Fatalf("evict the document's room: %v", err)
			}
		}},
		{"Quiesce", true, func(t *testing.T, service *Service, _ string, _ *crdt.Doc) {
			if err := service.Quiesce(context.Background()); err != nil {
				t.Fatalf("quiesce the service: %v", err)
			}
		}},
		{"a failed room's eviction", false, func(t *testing.T, service *Service, artifactID string, _ *crdt.Doc) {
			service.failRoom(artifactID, errors.New("the room failed"))
			// The eviction runs on its own goroutine; the record it hands over carries the
			// failure, so the service holding none is the hand-over.
			waitFor(t, 10*time.Second, "the failed room's eviction to settle its record", func() bool {
				return service.roomFailure(artifactID) == nil
			})
		}},
	} {
		t.Run(closer.name, func(t *testing.T) {
			service, artifactID := newTestService(t)
			service.settle = time.Hour
			seedServiceText(t, service, artifactID, "before")
			if err := service.warmLiveDocument(context.Background(), artifactID); err != nil {
				t.Fatalf("load the document's room: %v", err)
			}
			waitForLandedAppends(t, service, artifactID)
			live := service.srv.GetDoc(artifactID)
			if live == nil {
				t.Fatal("the document's room is not resident")
			}

			var joined atomic.Bool
			service.afterRoomClosed = func(room string) {
				if room != artifactID || !joined.CompareAndSwap(false, true) {
					return
				}
				service.addConnection(artifactID, connection, bob)
				if !closer.loads {
					return
				}
				if err := service.warmLiveDocument(context.Background(), artifactID); err != nil {
					t.Errorf("load the room that replaces the closed one: %v", err)
					return
				}
				editAsPeer(t, service, artifactID, replaceRun("before", "before, typed by bob"))
			}
			closer.force(t, service, artifactID, live)
			if !joined.Load() {
				t.Fatal("the close never reached the window a browser joins the record in")
			}

			state := service.room(artifactID)
			state.mu.Lock()
			actor, connected := state.connected[connection]
			state.mu.Unlock()
			if !connected {
				t.Fatal("the record the service holds lists no browser; the close dropped the one that joined while it ran")
			}
			if actor.Kind != bob.Kind || actor.ID != bob.ID {
				t.Fatalf("the record lists %+v for the browser that joined while the room closed, want %+v", actor, bob)
			}

			if !closer.loads {
				if err := service.warmLiveDocument(context.Background(), artifactID); err != nil {
					t.Fatalf("reload the document's room: %v", err)
				}
				editAsPeer(t, service, artifactID, replaceRun("before", "before, typed by bob"))
			}
			settleCurrentGeneration(t, service, artifactID)
			number := latestVersionNumber(t, service, artifactID)
			authors := versionAuthors(t, service.store, artifactID, number)
			if len(authors) != 1 || authors[0].ID != bob.ID {
				t.Fatalf("version %d lists %+v, want the browser that was connected for the edit: %+v",
					number, authors, []model.Actor{bob})
			}
			if markdown, _ := latestVersionOf(t, service.store, artifactID); markdown != "before, typed by bob\n" {
				t.Fatalf("version %d holds %q, want the browser's edit", number, markdown)
			}
		})
	}
}

// The room that replaces a closed one loads while the close has yet to settle the closed room's
// record, so its load registers what the document is on this process - the closed issue it read,
// the ask blocks it holds, the latest edit source of a settlement it owes, and the settlement that
// load arms - on the record the close used to throw away. A record that started empty treated a
// closed issue's document as open, credited the next browser edit with every ask block the
// document already held, and left the settlement to the timer that found its record gone and armed
// another a settle delay later.
func TestAForcedCloseKeepsWhatTheRoomThatReplacedItLoaded(t *testing.T) {
	const ask = ":::ask{#transport urgency=\"high\" multiple=\"false\" state=\"open\"}\nWhich transport?\n:::\n"
	bob := model.Actor{Kind: "user", ID: "bob"}
	// closeWithABrowserJoining closes the document's room and connects a browser in the window
	// between that close and the service's settling of the room's record, so the room the browser
	// opens loads onto the record the close is still to settle. It answers the record the service
	// holds once the close has returned.
	closeWithABrowserJoining := func(t *testing.T, service *Service, artifactID string) *roomState {
		t.Helper()
		server := httptest.NewServer(http.HandlerFunc(service.ServeHTTP))
		t.Cleanup(server.Close)
		var joined atomic.Bool
		service.afterRoomClosed = func(room string) {
			if room != artifactID || !joined.CompareAndSwap(false, true) {
				return
			}
			connectPeer(t, server.URL, artifactID, "bob")
			waitFor(t, 10*time.Second, "the browser's room to load onto the record the close is settling", func() bool {
				return service.srv.GetDoc(artifactID) != nil
			})
		}
		if err := service.Evict(context.Background(), artifactID); err != nil {
			t.Fatalf("evict the document's room: %v", err)
		}
		if !joined.Load() {
			t.Fatal("the close never reached the window a room loads in")
		}
		return service.room(artifactID)
	}

	t.Run("the closed issue its load read", func(t *testing.T) {
		service, artifactID := newTestService(t)
		service.settle = time.Hour
		seedServiceText(t, service, artifactID, "before")
		if err := service.warmLiveDocument(context.Background(), artifactID); err != nil {
			t.Fatalf("load the document's room: %v", err)
		}
		waitForLandedAppends(t, service, artifactID)
		// The issue's row alone: SetIssueClosed would close this room itself, and the close under
		// test is the one the browser joins.
		if _, err := service.store.Pool.Exec(context.Background(),
			`update issues set closed_at = now() where key = 'DOC-1'`); err != nil {
			t.Fatalf("close the issue: %v", err)
		}

		state := closeWithABrowserJoining(t, service, artifactID)
		state.mu.Lock()
		closed := state.closed
		browsers := len(state.connected)
		state.mu.Unlock()
		if browsers != 1 {
			t.Fatalf("the record the service holds lists %d browsers, want the one that joined while the room closed", browsers)
		}
		if !closed {
			t.Fatal("the record the service holds forgot that the document's issue is closed")
		}
	})

	t.Run("the ask blocks, edit source and settlement its load read", func(t *testing.T) {
		service, artifactID := newTestService(t)
		// Nothing settles during the test, so the settlement the replacement room's load arms is
		// the one the record carries at the end: armed on the live record, it runs at the time it
		// was armed for rather than a settle delay after a timer finds its record gone.
		service.settle = time.Hour
		seedServiceText(t, service, artifactID, ask)
		if err := service.warmLiveDocument(context.Background(), artifactID); err != nil {
			t.Fatalf("load the document's room: %v", err)
		}
		service.addConnection(artifactID, 1, bob)
		editAsPeer(t, service, artifactID, replaceRun("Which transport?", "Which transport, bob?"))
		service.removeConnection(artifactID, 1)
		waitForLandedAppends(t, service, artifactID)
		waitForSettlementOwed(t, service, artifactID)

		state := closeWithABrowserJoining(t, service, artifactID)
		state.mu.Lock()
		_, held := state.askBlocks["transport"]
		blocks := state.askBlocks
		lastActor := state.lastActor
		unsettled := state.unsettled
		armed := service.isSettleTimerArmed(state.settle)
		state.mu.Unlock()
		if !held {
			t.Fatalf("the record the service holds knows the ask blocks %v, want the one the document holds", blocks)
		}
		if lastActor == nil || lastActor.ID != bob.ID {
			t.Fatalf("the record holds %+v as the document's latest edit source, want %+v", lastActor, bob)
		}
		if !unsettled {
			t.Fatal("the record holds no edit source a settlement still owes")
		}
		if !armed {
			t.Fatal("the record carries no armed settlement; the one the replacement room's load armed went with the record")
		}
	})
}

// A forced close keeps the closed room's record while something holds it, and the latest edit
// source it records is one such holder. The room that loads next reads the document's
// pending-settlement row, and that row names the source as the store has it: another process's
// edit that two browsers could have made names none. The load takes the row's answer in place of
// the one the record kept from the room that went, or the settlement after it names the earlier
// editor on its version and events rather than SettlementActor.
func TestARoomLoadedOntoAKeptRecordTakesTheStoredEditSource(t *testing.T) {
	alice := model.Actor{Kind: "user", ID: "alice"}
	service, artifactID := newTestService(t)
	service.settle = time.Hour
	seedServiceText(t, service, artifactID, "before")
	if err := service.warmLiveDocument(context.Background(), artifactID); err != nil {
		t.Fatalf("load the document's room: %v", err)
	}
	waitForLandedAppends(t, service, artifactID)
	service.recordActor(t, artifactID, alice)
	// The other process's edit, which several browsers there could have made, names no source.
	if _, err := service.store.Pool.Exec(context.Background(), `
		insert into doc_settlements_pending (artifact_id, last_actor) values ($1, null)
		on conflict (artifact_id) do update set last_actor = null
	`, artifactID); err != nil {
		t.Fatalf("record the other process's edit as naming no source: %v", err)
	}
	kept, held := service.rooms.Load(artifactID)
	if !held {
		t.Fatal("the document holds no record before the close, so nothing here is tested")
	}
	if err := service.Evict(context.Background(), artifactID); err != nil {
		t.Fatalf("evict the document's room: %v", err)
	}
	if err := service.warmLiveDocument(context.Background(), artifactID); err != nil {
		t.Fatalf("reload the document's room: %v", err)
	}

	state := service.room(artifactID)
	if state != kept.(*roomState) {
		t.Fatal("the close dropped the record its edit source held, so the reload made a fresh one and nothing here is tested")
	}
	state.mu.Lock()
	lastActor := state.lastActor
	credit := settlementAuthors(state, map[string]model.Actor{actorKey(alice): alice})
	state.mu.Unlock()
	if lastActor != nil {
		t.Fatalf("the reloaded room keeps %+v as the latest edit source, want none, as the stored row names", lastActor)
	}
	if credit.actor != SettlementActor {
		t.Fatalf("the settlement would name %+v on its version, want %+v", credit.actor, SettlementActor)
	}
}

// A browser that reconnects, an edit that lands or a write that commits while Quiesce closes the
// rooms records an edit source again, and quiescing refuses the settlement that would read it.
// Quiesce drops that source once every close and settlement has ended, so the record of a document
// the browser-test harness truncates next is not held for good.
func TestQuiesceDropsAnEditSourceRecordedWhileItClosesTheRooms(t *testing.T) {
	alice := model.Actor{Kind: "user", ID: "alice"}
	service, artifactID := newTestService(t)
	service.settle = time.Hour
	seedServiceText(t, service, artifactID, "before")
	if err := service.warmLiveDocument(context.Background(), artifactID); err != nil {
		t.Fatalf("load the document's room: %v", err)
	}
	waitForLandedAppends(t, service, artifactID)
	var recorded atomic.Bool
	service.afterRoomClosed = func(room string) {
		if room == artifactID && recorded.CompareAndSwap(false, true) {
			service.recordActor(t, artifactID, alice)
		}
	}
	if err := service.Quiesce(context.Background()); err != nil {
		t.Fatalf("quiesce the service: %v", err)
	}
	if !recorded.Load() {
		t.Fatal("the quiesce closed no room, so nothing here is tested")
	}
	if _, kept := service.rooms.Load(artifactID); kept {
		t.Fatal("the quiesce left the record of a closed document held by an edit source nothing will settle")
	}
}

// rowIsCurrent is how a load tells whether the pending-settlement row it read can stand for the
// record's latest edit source: not once the record recorded a source after the mark, nor while it
// held a browser edit whose append has yet to write the row. A settlement or an open write records
// no source, so it never keeps the record's: the warm-up of a reopened document's settlement is
// the load that must read the row.
func TestALoadTakesTheStoredEditSourceOnlyWhenNothingNewerIsRecorded(t *testing.T) {
	quietState := func() *roomState { return newRoomState() }
	for _, row := range []struct {
		name    string
		mark    func(service *Service, room string) editSourceMark
		after   func(state *roomState) *roomState
		current bool
	}{
		{"nothing recorded since the mark", func(s *Service, room string) editSourceMark { return s.markEditSource(room) },
			func(state *roomState) *roomState { return state }, true},
		{"a source recorded after the mark", func(s *Service, room string) editSourceMark { return s.markEditSource(room) },
			func(state *roomState) *roomState { state.creditSeq.Add(1); return state }, false},
		{"a record registered after the mark with no source", func(*Service, string) editSourceMark { return editSourceMark{quiet: true} },
			func(*roomState) *roomState { return quietState() }, true},
		{"a record registered after the mark with a source", func(*Service, string) editSourceMark { return editSourceMark{quiet: true} },
			func(*roomState) *roomState { fresh := quietState(); fresh.creditSeq.Add(1); return fresh }, false},
	} {
		t.Run(row.name, func(t *testing.T) {
			service, artifactID := newTestService(t)
			state := quietState()
			service.rooms.Store(artifactID, state)
			mark := row.mark(service, artifactID)
			current := mark.rowIsCurrent(row.after(state))
			service.rooms.Delete(artifactID)
			if current != row.current {
				t.Fatalf("rowIsCurrent = %t, want %t", current, row.current)
			}
		})
	}
	for _, held := range []struct {
		name    string
		hold    func(state *roomState)
		current bool
	}{
		{"an in-flight credit", func(state *roomState) { state.inflight[1] = &inflightCredit{seq: 1} }, false},
		{"an update persistence has not taken", func(state *roomState) { state.pendingUpdates = 1 }, false},
		{"a durable append", func(state *roomState) { state.durableAppends.Add(1) }, false},
		{"a running settlement", func(state *roomState) { state.settling = 1 }, true},
		{"an open write", func(state *roomState) { state.liveWriter = &liveWrite{} }, true},
	} {
		t.Run("the record held "+held.name+" at the mark", func(t *testing.T) {
			service, artifactID := newTestService(t)
			state := quietState()
			held.hold(state)
			service.rooms.Store(artifactID, state)
			current := service.markEditSource(artifactID).rowIsCurrent(state)
			// The planted hold is no real work, so the record leaves before the service shuts down.
			service.rooms.Delete(artifactID)
			if current != held.current {
				t.Fatalf("rowIsCurrent = %t with %s held at the mark, want %t", current, held.name, held.current)
			}
		})
	}
}

// The record a forced close keeps can hold the failures of the room that replaced the closed one
// by the time the close returns: that room loaded onto it and its settlement ran. The close counts
// none of them as the closed room's, so a third failure of the replacement still fails it.
func TestAForcedCloseKeepsTheFailuresOfTheRoomThatReplacedIt(t *testing.T) {
	for _, closer := range []struct {
		name  string
		force func(t *testing.T, service *Service, artifactID string, live *crdt.Doc)
	}{
		{"the catch-up's close of a replaced history", func(t *testing.T, service *Service, artifactID string, live *crdt.Doc) {
			if _, err := service.closeRoomBehindHead(context.Background(), artifactID, live); err != nil {
				t.Fatalf("close the room the catch-up found on a replaced history: %v", err)
			}
		}},
		{"Evict", func(t *testing.T, service *Service, artifactID string, _ *crdt.Doc) {
			if err := service.Evict(context.Background(), artifactID); err != nil {
				t.Fatalf("evict the document's room: %v", err)
			}
		}},
	} {
		t.Run(closer.name, func(t *testing.T) {
			service, artifactID := newTestService(t)
			service.settle = time.Hour
			seedServiceText(t, service, artifactID, "before")
			if err := service.warmLiveDocument(context.Background(), artifactID); err != nil {
				t.Fatalf("load the document's room: %v", err)
			}
			waitForLandedAppends(t, service, artifactID)
			live := service.srv.GetDoc(artifactID)
			state := service.room(artifactID)
			state.mu.Lock()
			state.settleFailures = 2
			state.mu.Unlock()
			service.afterRoomClosed = func(room string) {
				if room != artifactID {
					return
				}
				// The replacement room's first settlement failed while the close ran.
				state.mu.Lock()
				state.settleFailures++
				state.mu.Unlock()
			}
			closer.force(t, service, artifactID, live)
			state.mu.Lock()
			failures := state.settleFailures
			state.mu.Unlock()
			if failures != 1 {
				t.Fatalf("the kept record counts %d settlement failures, want the replacement's one: the closed room's two end with it, the replacement's stand", failures)
			}
		})
	}
}

// An issue's close moves its rooms' generation, as a forced close does, and the record of a room
// it closes can outlive the close. The failures the closed room's settlements counted end there,
// so the room of a reopened issue starts its own count, as the room that replaces a force-closed
// one does.
func TestAnIssueCloseEndsItsRoomsSettlementFailures(t *testing.T) {
	service, artifactID := newTestService(t)
	service.settle = time.Hour
	seedServiceText(t, service, artifactID, "before")
	if err := service.warmLiveDocument(context.Background(), artifactID); err != nil {
		t.Fatalf("load the document's room: %v", err)
	}
	waitForLandedAppends(t, service, artifactID)
	state := service.room(artifactID)
	state.mu.Lock()
	state.settleFailures = 2
	state.mu.Unlock()

	if _, err := service.store.Pool.Exec(context.Background(), `update issues set closed_at = now() where key = 'DOC-1'`); err != nil {
		t.Fatalf("close the document's issue: %v", err)
	}
	service.SetIssueClosed(context.Background(), "DOC-1", true)

	state.mu.Lock()
	failures := state.settleFailures
	closed := state.closed
	state.mu.Unlock()
	if !closed {
		t.Fatal("the issue's close did not reach the document's record, so nothing here is tested")
	}
	if failures != 0 {
		t.Fatalf("the record counts %d settlement failures after its issue closed, want none: the closed room's end with it", failures)
	}
}

// A failed room's record is replaced, and the updates its persistence has not taken yet go to the
// record registered in its place, with the appends they count. ygo hands such an update on through
// its stranded persistence, which can run after the eviction has cleared the failure, and the
// service drops it only while the room is failed (StoreUpdateContext). An update whose class the
// record no longer holds is appended with no credit at all, so the person who typed it is on no
// version and in no pending row - the loss this issue is about, on the one path that outlives the
// record.
func TestAFailedRoomsUnstoredUpdatesGoToTheRecordThatReplacesIt(t *testing.T) {
	service, artifactID := newTestService(t)
	service.settle = time.Hour
	seedServiceText(t, service, artifactID, "before")
	if err := service.warmLiveDocument(context.Background(), artifactID); err != nil {
		t.Fatalf("load the document's room: %v", err)
	}
	waitForLandedAppends(t, service, artifactID)

	update := []byte("bob's update")
	credit := &UpdateCredit{}
	service.recordUpdateClass(artifactID, update, documentUpdateClass{contentChanged: true, durable: true, credit: credit})
	state := service.room(artifactID)
	state.mu.Lock()
	service.handOverFailedRoomLocked(artifactID, state)
	state.mu.Unlock()

	successor := service.room(artifactID)
	if successor == state {
		t.Fatal("the failed room's record was kept, so nothing here is tested")
	}
	class, counted, found := service.consumeUpdateClass(artifactID, update)
	if !found {
		t.Fatal("the update the failed room had not stored is unknown to the record that replaced it: its append is credited to nobody")
	}
	if class.credit != credit {
		t.Fatal("the carried update lost the credit its observer recorded")
	}
	if counted != successor {
		t.Fatal("the carried update was counted on another record than the one that holds it")
	}
	service.finishDurableAppend(artifactID, counted)
	successor.mu.Lock()
	appends := successor.durableAppends.Load()
	pending := successor.pendingUpdates
	successor.mu.Unlock()
	if appends != 0 || pending != 0 {
		t.Fatalf("the record that replaced the failed room counts %d appends and %d pending updates once the carried append finished, want none", appends, pending)
	}
}

// The two shapes of a failed room's update, through the persistence adapter itself.
//
// One is handed on while the failure stands: the service drops it, the browser resends it to the
// room that replaces this one, and the work the record counts for it ends there. Left counted, it
// is an append no callback can ever finish, and the record - or the one registered in its place,
// which carries what the room's persistence never took - waits on it for ever: its settlement
// retries without end, and the record is never released (unusedLocked).
//
// The other is handed on after the hand-over, by ygo's stranded persistence once the eviction has
// cleared the failure. That one is stored, and it must still carry the class and the credit its
// observer recorded, or the person who typed it is on no version and in no pending row.
func TestAFailedRoomsUpdatesEndOrMoveWithTheRecordThatReplacesIt(t *testing.T) {
	service, artifactID := newTestService(t)
	service.settle = time.Hour
	seedServiceText(t, service, artifactID, "before")
	if err := service.warmLiveDocument(context.Background(), artifactID); err != nil {
		t.Fatalf("load the document's room: %v", err)
	}
	waitForLandedAppends(t, service, artifactID)
	adapter := &servicePersistenceAdapter{store: service.persistence, service: service}
	state := service.room(artifactID)

	// The update the failure drops, handed on while the room is failed, and the one the room's
	// persistence never took, which ygo hands on after the eviction clears that failure. Each
	// carries a browser's credit, as the room's update observer gives it, and the second is a
	// browser's own update, since the adapter appends it for real.
	bob := model.Actor{Kind: "user", ID: "bob"}
	service.addConnection(artifactID, 11, bob)
	hold := &roomHold{}
	dropped := []byte("the update the failure dropped")
	droppedCredit := service.creditContentChange(artifactID, nil, hold)
	service.recordUpdateClass(artifactID, dropped,
		documentUpdateClass{contentChanged: true, durable: true, credit: droppedCredit})
	_, stranded := peerEdit(t, service.srv.GetDoc(artifactID), appendBlocks(t, "typed while the room was failing.\n"))
	service.recordUpdateClass(artifactID, stranded,
		documentUpdateClass{contentChanged: true, durable: true, credit: service.creditContentChange(artifactID, nil, hold)})
	state.mu.Lock()
	state.failed = errors.New("the room failed on an append")
	state.mu.Unlock()
	if err := adapter.StoreUpdate(artifactID, dropped); err != nil {
		t.Fatalf("hand the failed room's update to its persistence: %v", err)
	}
	state.mu.Lock()
	pending, appends := state.pendingUpdates, state.durableAppends.Load()
	inflight := len(state.inflight)
	state.failed = nil
	service.handOverFailedRoomLocked(artifactID, state)
	state.mu.Unlock()
	if pending != 1 || appends != 1 {
		t.Fatalf("the failed room's record counts %d pending updates and %d appends after an update it dropped, want 1 and 1: the dropped update's own count must end with it",
			pending, appends)
	}
	if inflight != 1 {
		t.Fatalf("the failed room's record holds %d in-flight credits after an update it dropped, want 1: the dropped update's own credit must land with it", inflight)
	}

	// The update the room's persistence never took moves to the record registered in its place,
	// with the one append it counts, where ygo's stranded persistence finds it.
	successor := service.room(artifactID)
	if successor == state {
		t.Fatal("the failed room's record was kept, so nothing here is tested")
	}
	successor.mu.Lock()
	pending, appends = successor.pendingUpdates, successor.durableAppends.Load()
	successor.mu.Unlock()
	if pending != 1 || appends != 1 {
		t.Fatalf("the record that replaced the failed room counts %d pending updates and %d appends, want 1 and 1: the update the room's persistence never took, and no other", pending, appends)
	}
	if err := adapter.StoreUpdate(artifactID, stranded); err != nil {
		t.Fatalf("hand the stranded update on after the hand-over: %v", err)
	}
	if _, _, droppedFound := service.consumeUpdateClass(artifactID, dropped); droppedFound {
		t.Fatal("the update the failure dropped was carried to the record that replaced the failed room")
	}
	successor.mu.Lock()
	pending, appends = successor.pendingUpdates, successor.durableAppends.Load()
	successor.mu.Unlock()
	if pending != 0 || appends != 0 {
		t.Fatalf("the record counts %d pending updates and %d appends once its carried append was stored, want none", pending, appends)
	}
	if stored := storedMarkdown(t, service.store, artifactID); !strings.Contains(stored, "typed while the room was failing.") {
		t.Fatalf("the stored document after the stranded append = %q, want the paragraph it carried", stored)
	}
	if authors := pendingAuthorRows(t, service.store, artifactID); !slices.Contains(authors, bob) {
		t.Fatalf("the document's pending authors after the stranded append = %v, want %v among them: that keystroke's author is credited to nobody otherwise", authors, bob)
	}
}
