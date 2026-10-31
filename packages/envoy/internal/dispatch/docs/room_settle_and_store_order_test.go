package docs

import (
	"context"
	"encoding/json"
	"fmt"
	"slices"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/reearth/ygo/crdt"
	"github.com/reearth/ygo/persistence"

	"github.com/sjawhar/envoy/internal/dispatch/events"
	"github.com/sjawhar/envoy/internal/dispatch/model"
	"github.com/sjawhar/envoy/internal/dispatch/store"
	"github.com/sjawhar/envoy/internal/dispatch/store/storetest"
)

// Two processes editing one document settle it as one process does (plan D3.7): a content change
// the other process made re-arms a settlement this process has armed and arms none where none is,
// and the version names the last typist whichever process settles it, because the actor comes
// from the store, not from the settling process's memory. And the store never holds a state the
// document never held (plan D3.8): a live write stores the room's unstored updates ahead of its own
// row.

// A content change task B made, which task A's catch-up applies, arms no settlement on task A
// where none is armed: task B armed its own for it. Where task A has one armed - its own peer typed
// within the settle delay - the change re-arms it, a settle delay from now, so the burst the two
// processes' peers type in turn settles once, after its last keystroke on either process.
func TestARemoteChangeRearmsOnlyAnArmedSettlement(t *testing.T) {
	pair := newProcessPair(t)
	bob := model.Actor{Kind: "user", ID: "bob-on-task-b"}
	if err := pair.taskA.warmLiveDocument(context.Background(), pair.artifactID); err != nil {
		t.Fatalf("load task A's room: %v", err)
	}
	room := pair.taskA.srv.GetDoc(pair.artifactID)
	state := pair.taskA.room(pair.artifactID)
	settleOf := func() (timer *time.Timer, armed bool, generation uint64) {
		state.mu.Lock()
		defer state.mu.Unlock()
		return state.settle, pair.taskA.isSettleTimerArmed(state.settle), state.roomGeneration
	}
	// The seed's publish armed a settlement on task A; nothing of task A's own is owed past the
	// settlement newProcessPair ran, so it goes.
	state.mu.Lock()
	pair.taskA.stopSettleTimer(state.settle)
	state.mu.Unlock()
	catchUpB := func(markdown string) {
		t.Helper()
		editAsBrowserPeer(t, pair.taskB, pair.artifactID, 1, bob, appendBlocks(t, markdown))
		waitForLandedAppends(t, pair.taskB, pair.artifactID)
		outcome, err := pair.taskA.catchUpRoom(context.Background(), pair.database.Pool, pair.artifactID, room)
		if err != nil || outcome != catchUpApplied {
			t.Fatalf("catch task A's room up = %v, %v, want catchUpApplied", outcome, err)
		}
	}

	catchUpB("Bob's first paragraph.\n")
	if _, armed, _ := settleOf(); armed {
		t.Fatal("task B's change armed a settlement on task A, which had none armed: two processes would each version the burst")
	}

	// Task A's own peer types: its settlement is armed a settle delay out (an hour here).
	editAsConnectedPeer(t, pair.taskA, pair.artifactID, 2, model.Actor{Kind: "user", ID: "alice-on-task-a"}, appendBlocks(t, "Alice's paragraph.\n"))
	waitForLandedAppends(t, pair.taskA, pair.artifactID)
	before, armed, generation := settleOf()
	if !armed {
		t.Fatal("task A's own peer's change armed no settlement, so nothing here is tested")
	}

	catchUpB("Bob's second paragraph.\n")
	after, armed, rearmed := settleOf()
	if !armed {
		t.Fatal("task B's change left task A's armed settlement disarmed")
	}
	if after == before || rearmed == generation {
		t.Fatal("task B's change left task A's settlement where it was armed: task A would settle half the burst while bob still types")
	}
	if pair.taskA.isSettleTimerArmed(before) {
		t.Fatal("the settlement task A had armed is still armed beside the one the re-arm made")
	}
}

// A settlement names the last typist the store records, on whichever process it runs: alice types
// on task A after bob typed on task B, so task B's settlement - whose room took alice's edit in
// through a catch-up, which credits no one here - names alice, as the pending-settlement row her
// append wrote says. Named from task B's own memory, it was bob, whose edit task B saw last.
func TestASettlementOnTheOtherProcessNamesTheTypistWhoseAppendLandedLast(t *testing.T) {
	pair := newProcessPair(t)
	alice := model.Actor{Kind: "user", ID: "alice-on-task-a"}
	bob := model.Actor{Kind: "user", ID: "bob-on-task-b"}
	if err := pair.taskA.warmLiveDocument(context.Background(), pair.artifactID); err != nil {
		t.Fatalf("load task A's room: %v", err)
	}
	editAsBrowserPeer(t, pair.taskB, pair.artifactID, 1, bob, appendBlocks(t, "Bob's paragraph.\n"))
	waitForLandedAppends(t, pair.taskB, pair.artifactID)
	if _, err := pair.taskA.catchUpRoom(context.Background(), pair.database.Pool, pair.artifactID, pair.taskA.srv.GetDoc(pair.artifactID)); err != nil {
		t.Fatalf("catch task A's room up: %v", err)
	}
	editAsBrowserPeer(t, pair.taskA, pair.artifactID, 2, alice, appendBlocks(t, "Alice's paragraph.\n"))
	waitForLandedAppends(t, pair.taskA, pair.artifactID)
	roomB := pair.taskB.srv.GetDoc(pair.artifactID)
	if outcome, err := pair.taskB.catchUpRoom(context.Background(), pair.database.Pool, pair.artifactID, roomB); err != nil || outcome != catchUpApplied {
		t.Fatalf("catch task B's room up = %v, %v, want catchUpApplied", outcome, err)
	}
	stateB := pair.taskB.room(pair.artifactID)
	stateB.mu.Lock()
	remembered := stateB.lastActor
	stateB.mu.Unlock()
	if remembered == nil || *remembered != bob {
		t.Fatalf("task B remembers %+v as the latest edit source, want bob, so nothing here is tested", remembered)
	}

	versions := latestVersionNumber(t, pair.taskB, pair.artifactID)
	settleCurrentGeneration(t, pair.taskB, pair.artifactID)
	if now := latestVersionNumber(t, pair.taskB, pair.artifactID); now != versions+1 {
		t.Fatalf("task B's settlement left the latest version at %d, want %d", now, versions+1)
	}
	if authors := latestVersionAuthors(t, pair.taskB, pair.artifactID); !slices.Contains(authors, alice) || !slices.Contains(authors, bob) {
		t.Fatalf("task B's version lists %+v, want %s and %s", authors, alice.ID, bob.ID)
	}
	if actor := latestVersionEventActor(t, pair.database, pair.artifactID); actor != alice {
		t.Fatalf("task B's version names %+v, want %+v, whose append landed last", actor, alice)
	}
}

// latestVersionEventActor is the actor of artifactID's latest artifact.version event.
func latestVersionEventActor(t *testing.T, database *store.Store, artifactID string) model.Actor {
	t.Helper()
	var raw []byte
	if err := database.Pool.QueryRow(context.Background(), `
		select actor from events where type = 'artifact.version' and payload->>'artifact_id' = $1
		order by seq desc limit 1
	`, artifactID).Scan(&raw); err != nil {
		t.Fatalf("read the latest version event: %v", err)
	}
	var actor model.Actor
	if err := json.Unmarshal(raw, &actor); err != nil {
		t.Fatalf("decode the latest version event's actor: %v", err)
	}
	return actor
}

// gatedAppends holds a room's own appends (AppendUpdateWithCredit) at a gate the test closes and
// opens, so a browser's update the room applied and broadcast stays unstored for as long as the
// test needs: taken off the room's recorded updates and waiting to append, with the room's later
// updates queued behind it.
type gatedAppends struct {
	VersionedStore
	mu      sync.Mutex
	gate    chan struct{}
	waiting atomic.Int32
}

func newGatedAppends(database *store.Store) *gatedAppends {
	gate := make(chan struct{})
	close(gate)
	return &gatedAppends{VersionedStore: NewPgVersioned(database), gate: gate}
}

func (g *gatedAppends) AppendUpdateWithCredit(ctx context.Context, room string, update []byte, contentChanged bool, credit *UpdateCredit, browser *BrowserAppend) (persistence.Version, error) {
	g.mu.Lock()
	gate := g.gate
	g.mu.Unlock()
	g.waiting.Add(1)
	<-gate
	g.waiting.Add(-1)
	return g.VersionedStore.AppendUpdateWithCredit(ctx, room, update, contentChanged, credit, browser)
}

// hold closes the gate to every append that reaches it from now on.
func (g *gatedAppends) hold() {
	g.mu.Lock()
	defer g.mu.Unlock()
	g.gate = make(chan struct{})
}

// release opens the gate. Opening an open gate does nothing.
func (g *gatedAppends) release() {
	g.mu.Lock()
	defer g.mu.Unlock()
	select {
	case <-g.gate:
	default:
		close(g.gate)
	}
}

// newGatedServiceInstance is newTestServiceInstance whose room appends pass a gate the test holds.
func newGatedServiceInstance(t *testing.T, database *store.Store) (*Service, *gatedAppends) {
	t.Helper()
	gated := newGatedAppends(database)
	service := New(Deps{
		Store:       database,
		Persistence: gated,
		Events:      events.NewBroker(),
		Identity:    headerIdentity(database),
		ServerURL:   "https://dispatch.example",
		Settle:      time.Hour,
	})
	t.Cleanup(func() {
		if err := service.Shutdown(context.Background()); err != nil {
			t.Errorf("shutdown document service: %v", err)
		}
	})
	// Registered after the shutdown, so it runs first: a test that fails with the gate held does
	// not leave the shutdown waiting on an append that never comes.
	t.Cleanup(gated.release)
	return service, gated
}

// requireStoreNeverTorn fails unless the stored history, loaded fresh, decodes with nothing
// pending: every stored update's dependencies are stored before it. It returns the stored
// document's rendering.
func requireStoreNeverTorn(t *testing.T, database *store.Store, artifactID, after string) string {
	t.Helper()
	loaded, err := NewPgVersioned(database).Load(context.Background(), artifactID)
	if err != nil {
		t.Fatalf("%s: load the stored history: %v", after, err)
	}
	doc := newDocumentCopy()
	if err := crdt.ApplyUpdateV1(doc, loaded.Update, nil); err != nil {
		t.Fatalf("%s: decode the stored history: %v", after, err)
	}
	if pending := doc.PendingStats(); pending.Items > 0 || pending.DeleteRanges > 0 {
		t.Fatalf("%s: the stored history through version %d holds %d item(s) and %d delete range(s) waiting for updates it does not hold: a state the document never held",
			after, loaded.Version, pending.Items, pending.DeleteRanges)
	}
	tree, err := treeOf(doc)
	if err != nil {
		t.Fatalf("%s: read the stored document: %v", after, err)
	}
	markdown, err := renderTree(tree)
	if err != nil {
		t.Fatalf("%s: render the stored document: %v", after, err)
	}
	return markdown
}

// A live write's warm fork holds its room's updates the store does not hold yet - a browser's
// edit whose append is waiting, and the browser's next keystroke queued behind it - and the write's
// own row, made on them, commits while they still wait. The write stores them first, in its own
// transaction (storeRoomUpdatesAhead), so the history stored after every commit, the write's and
// each held append's, loads with nothing pending; the held appends then find their updates stored
// and store nothing twice, and the browser, not the agent, keeps the credit for them. Without that,
// the store held the agent's row ahead of the updates it edits, which every reader of the store - a
// cold fork, a read, the other process's catch-up - took for the document.
//
// Peers on both processes and agent writes on both interleave, 200 rounds: each round a peer on one
// process rewrites a paragraph, the other process catches up, and there a peer rewrites another
// paragraph, whose appends are held while an agent edits it. The paragraphs are rewritten rather
// than added, and the history is compacted every few rounds as rooms' persistence compacts it, so a
// round costs the same at the end as at the start.
func TestStoredHistoryIsNeverTorn(t *testing.T) {
	database := storetest.Open(t)
	artifactID := createDocument(t, database, "# First")
	taskA, gateA := newGatedServiceInstance(t, database)
	taskB, gateB := newGatedServiceInstance(t, storetest.SecondProcess(t, database))
	seedServiceText(t, taskA, artifactID, "First.\n\nOther-start.\n\nTyped-start.\n")
	settleCurrentGeneration(t, taskA, artifactID)
	for _, service := range []*Service{taskA, taskB} {
		if err := service.warmLiveDocument(context.Background(), artifactID); err != nil {
			t.Fatalf("load a room: %v", err)
		}
	}
	type process struct {
		name    string
		service *Service
		gate    *gatedAppends
	}
	processes := [2]process{{"task A", taskA, gateA}, {"task B", taskB, gateB}}
	agent := model.Actor{Kind: "session", ID: "agent-session"}
	catchUp := func(on process) {
		t.Helper()
		room := on.service.srv.GetDoc(artifactID)
		if room == nil {
			t.Fatalf("%s holds no room", on.name)
		}
		if _, err := on.service.catchUpRoom(context.Background(), database.Pool, artifactID, room); err != nil {
			t.Fatalf("catch %s's room up: %v", on.name, err)
		}
		if roomBehind(t, on.service, artifactID) {
			t.Fatalf("%s's room is behind the head after its catch-up", on.name)
		}
	}

	const rounds, compactEvery = 200, 10
	otherText, typedText := "Other-start.", "Typed-start."
	for round := range rounds {
		writer, other := processes[round%2], processes[(round+1)%2]
		typist := model.Actor{Kind: "user", ID: fmt.Sprintf("typist-%d", round)}

		// A peer on the other process types; its append lands, and the writer's room catches up.
		next := fmt.Sprintf("Other-%d.", round)
		editAsBrowserPeer(t, other.service, artifactID, 1, model.Actor{Kind: "user", ID: "peer-on-" + other.name},
			replaceRun(otherText, next))
		otherText = next
		waitForLandedAppends(t, other.service, artifactID)
		requireStoreNeverTorn(t, database, artifactID, fmt.Sprintf("round %d: after %s's peer's append", round, other.name))
		catchUp(writer)

		// A peer on the writer's process rewrites a paragraph and then types a keystroke into it,
		// while the room's appends are held: the rewrite's append waits at the gate, and the
		// keystroke's is queued behind it.
		writer.gate.hold()
		writer.service.addConnection(artifactID, 2, typist)
		editAsPeer(t, writer.service, artifactID, replaceRun(typedText, fmt.Sprintf("Typed-%d-x.", round)))
		waitFor(t, roomWait(t), "the typist's rewrite to wait at the gate", func() bool { return writer.gate.waiting.Load() == 1 })
		keystroke := editAsPeer(t, writer.service, artifactID, replaceRun(fmt.Sprintf("Typed-%d-x.", round), fmt.Sprintf("Typed-%d-xy.", round)))
		writer.service.removeConnection(artifactID, 2)
		if !writer.service.hasPendingUpdates(artifactID) {
			t.Fatalf("round %d: the typist's keystroke is not queued behind the held rewrite, so nothing here is tested", round)
		}

		// An agent edits the typist's words, on the room that holds them and the store that does not.
		typedText = fmt.Sprintf("Typed-%d-agent.", round)
		if _, err := joinedApplyOps(writer.service, artifactID, []model.EditOp{{
			Op: "replace", Find: fmt.Sprintf("Typed-%d-xy", round), With: fmt.Sprintf("Typed-%d-agent", round),
		}}, agent, nil); err != nil {
			t.Fatalf("round %d: the agent's edit on %s: %v", round, writer.name, err)
		}
		requireStoreNeverTorn(t, database, artifactID, fmt.Sprintf("round %d: after the agent's commit on %s", round, writer.name))

		writer.gate.release()
		waitForLandedAppends(t, writer.service, artifactID)
		stored := requireStoreNeverTorn(t, database, artifactID, fmt.Sprintf("round %d: after the held appends landed on %s", round, writer.name))
		if round == 0 {
			requireStoredOnce(t, database, artifactID, keystroke)
			if owed := pendingAuthorRows(t, database, artifactID); !slices.Contains(owed, typist) {
				t.Fatalf("pending authors = %+v, want the typist %s, whose updates the agent's transaction stored", owed, typist.ID)
			}
		}
		if live := liveMarkdownOf(t, writer.service, artifactID); live != stored {
			t.Fatalf("round %d: %s's room renders %q, the store %q", round, writer.name, live, stored)
		}
		if !strings.Contains(stored, typedText) || !strings.Contains(stored, otherText) {
			t.Fatalf("round %d: the stored document %q lacks %q or %q", round, stored, typedText, otherText)
		}

		if (round+1)%compactEvery == 0 {
			// Both rooms at the head, as a compaction with compactKeep leaves them standing.
			catchUp(other)
			if _, err := NewPgVersioned(database).Compact(context.Background(), artifactID, compactKeep); err != nil {
				t.Fatalf("round %d: compact the stored history: %v", round, err)
			}
			requireStoreNeverTorn(t, database, artifactID, fmt.Sprintf("round %d: after a compaction", round))
		}
	}
	stored := requireStoreNeverTorn(t, database, artifactID, "the last round")
	for _, on := range processes {
		catchUp(on)
		if err := on.service.roomFailure(artifactID); err != nil {
			t.Fatalf("%s's room failed: %v", on.name, err)
		}
		if live := liveMarkdownOf(t, on.service, artifactID); live != stored {
			t.Fatalf("%s's room renders %q, the store %q", on.name, live, stored)
		}
	}
}

// requireStoredOnce fails unless what peer, a browser's own document, has written is in exactly one
// stored row: an update a live write stored ahead of its own row is not stored again by its room's
// append.
func requireStoredOnce(t *testing.T, database *store.Store, artifactID string, peer *crdt.Doc) {
	t.Helper()
	rows, err := database.Pool.Query(context.Background(), `
		select update from doc_updates where artifact_id = $1 order by version
	`, artifactID)
	if err != nil {
		t.Fatalf("read the stored updates: %v", err)
	}
	defer rows.Close()
	own := peer.ClientID()
	stored := 0
	for rows.Next() {
		var update []byte
		if err := rows.Scan(&update); err != nil {
			t.Fatalf("read a stored update: %v", err)
		}
		encoded, err := crdt.EncodeStateVectorFromUpdate(update)
		if err != nil {
			t.Fatalf("read a stored update's state vector: %v", err)
		}
		clocks, err := crdt.DecodeStateVectorV1(encoded)
		if err != nil {
			t.Fatalf("decode a stored update's state vector: %v", err)
		}
		if clocks[own] > 0 {
			stored++
		}
	}
	if err := rows.Err(); err != nil {
		t.Fatalf("read the stored updates: %v", err)
	}
	if stored != 1 {
		t.Fatalf("the browser's keystroke is in %d stored rows, want exactly one", stored)
	}
}
