package docs

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"slices"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/reearth/ygo/crdt"
	ygws "github.com/reearth/ygo/provider/websocket"

	"github.com/sjawhar/envoy/internal/dispatch/events"
	"github.com/sjawhar/envoy/internal/dispatch/model"
	"github.com/sjawhar/envoy/internal/dispatch/pmdoc"
	"github.com/sjawhar/envoy/internal/dispatch/store/storetest"
)

// Settlement repairs an ask block's server-owned attributes into the document as it stands when
// the repair is written, not into the tree settlement read before its database work: a paragraph a
// browser edits in between keeps the edit, and the stored document and the version the settlement
// writes hold both (LEGION-479).
func TestSettlementRepairKeepsAnEditMadeAfterItsRead(t *testing.T) {
	service, artifactID := newTestService(t)
	service.settle = time.Hour
	seedServiceText(t, service, artifactID, ":::ask{#ask-block urgency=\"med\" multiple=\"false\" state=\"open\"}\nShip it?\n:::\n\nContext before.\n")
	service.settleRoom(artifactID, 0)
	answer := answerBlockAsk(t, service, artifactID)

	var edited atomic.Bool
	service.afterSettleReconcile = func(room string) {
		if room == artifactID && edited.CompareAndSwap(false, true) {
			editAsPeer(t, service, artifactID, replaceRun("Context before.", "Context after."))
		}
	}
	settleCurrentGeneration(t, service, artifactID)
	if !edited.Load() {
		t.Fatal("settlement never reached the window between its read and its repair")
	}

	want := ":::ask{#ask-block urgency=\"med\" multiple=\"false\" state=\"answered\" answered_by=\"alice\" answered_at=\"" +
		answer.At.Format(time.RFC3339Nano) + "\" selected=\"[]\"}\nShip it?\n:::\n\nContext after.\n"
	waitForDocumentText(t, service, artifactID, want)
	waitForPersistedProofText(t, service.store, artifactID, want)
	requireLatestVersionMarkdown(t, service, artifactID, want)
}

// The same race on the block-id stamp: a settlement that stamped ids and then sees a peer's edit
// before it versions writes the version of the document as it stands, the edit included, rather
// than of the tree its stamp read (LEGION-479).
func TestSettlementStampVersionKeepsAnEditMadeAfterItsRead(t *testing.T) {
	service, artifactID := newTestService(t)
	service.settle = time.Hour
	seedServiceText(t, service, artifactID, "before")
	settleCurrentGeneration(t, service, artifactID)
	editLiveTree(t, service, artifactID, appendUnidentifiedBlocks(t, "added"))

	var edited atomic.Bool
	service.afterSettleReconcile = func(room string) {
		if room == artifactID && edited.CompareAndSwap(false, true) {
			editAsPeer(t, service, artifactID, replaceRun("before", "before, edited"))
		}
	}
	settleCurrentGeneration(t, service, artifactID)
	if !edited.Load() {
		t.Fatal("settlement never reached the window between its stamp and its version")
	}

	const want = "before, edited\n\nadded\n"
	waitForDocumentText(t, service, artifactID, want)
	requireNoSuppressedSlots(t, service, artifactID, "after the stamp")
	waitForPersistedProofText(t, service.store, artifactID, want)
	requireLatestVersionMarkdown(t, service, artifactID, want)
}

// A settlement whose room is replaced while it does its database work - the room's last browser
// left, and its repair's write loaded the room again from the store - writes nothing into the
// replacement: its version is read from the document it held, which would never get the repair.
// The settlement the replacement's load arms versions the document with the repair (LEGION-479).
func TestSettlementWhoseRoomWasReplacedLeavesTheDocumentToTheReplacement(t *testing.T) {
	service, artifactID := newTestService(t)
	service.settle = time.Hour
	seedServiceText(t, service, artifactID, ":::ask{#ask-block urgency=\"med\" multiple=\"false\" state=\"open\"}\nShip it?\n:::\n\nContext before.\n")
	service.settleRoom(artifactID, 0)
	answer := answerBlockAsk(t, service, artifactID)
	editAsPeer(t, service, artifactID, replaceRun("Context before.", "Context after."))

	held := service.srv.GetDoc(artifactID)
	var replaced atomic.Bool
	service.afterSettleReconcile = func(room string) {
		if room != artifactID || !replaced.CompareAndSwap(false, true) {
			return
		}
		// ygo closes a room the moment its last browser leaves, on that browser's goroutine.
		go func() { _ = service.srv.CloseRoom(room, true) }()
		deadline := time.Now().Add(5 * time.Second)
		for service.srv.GetDoc(room) == held {
			if time.Now().After(deadline) {
				t.Error("the room was never closed")
				return
			}
			time.Sleep(time.Millisecond)
		}
	}
	settleCurrentGeneration(t, service, artifactID)
	if !replaced.Load() {
		t.Fatal("settlement never reached the window between its read and its repair")
	}
	service.afterSettleReconcile = nil
	settleCurrentGeneration(t, service, artifactID)

	want := ":::ask{#ask-block urgency=\"med\" multiple=\"false\" state=\"answered\" answered_by=\"alice\" answered_at=\"" +
		answer.At.Format(time.RFC3339Nano) + "\" selected=\"[]\"}\nShip it?\n:::\n\nContext after.\n"
	waitForDocumentText(t, service, artifactID, want)
	waitForPersistedProofText(t, service.store, artifactID, want)
	requireLatestVersionMarkdown(t, service, artifactID, want)
}

// A settlement that repairs a block versions the document as it stands after its repairs, so the
// version holds an edit a browser made during the settlement's database work. It credits that
// edit's author too: the edit's own settlement finds the document already versioned and writes no
// version that could credit them (LEGION-479).
func TestSettlementRepairCreditsTheAuthorOfAnEditItsVersionHolds(t *testing.T) {
	service, artifactID := newTestService(t)
	service.settle = time.Hour
	seedServiceText(t, service, artifactID, ":::ask{#ask-block urgency=\"med\" multiple=\"false\" state=\"open\"}\nShip it?\n:::\n\nContext before.\n")
	service.settleRoom(artifactID, 0)
	answerBlockAsk(t, service, artifactID)
	bob := model.Actor{Kind: "user", ID: "bob"}
	service.addConnection(artifactID, 1, bob)

	var edited atomic.Bool
	service.afterSettleReconcile = func(room string) {
		if room == artifactID && edited.CompareAndSwap(false, true) {
			editAsPeer(t, service, artifactID, replaceRun("Context before.", "Context after."))
		}
	}
	settleCurrentGeneration(t, service, artifactID)
	if !edited.Load() {
		t.Fatal("settlement never reached the window between its read and its repair")
	}
	versioned := latestVersionNumber(t, service, artifactID)
	if authors := latestVersionAuthors(t, service, artifactID); !slices.Contains(authors, bob) {
		t.Fatalf("version %d authors = %#v, want bob, whose edit it holds", versioned, authors)
	}

	service.afterSettleReconcile = nil
	settleCurrentGeneration(t, service, artifactID)
	if latest := latestVersionNumber(t, service, artifactID); latest != versioned {
		t.Fatalf("latest version = %d, want %d: the edit's own settlement versioned it again", latest, versioned)
	}
}

// An edit a browser makes after a repairing settlement has read the tree its version is rendered
// from - while that version renders - is not in the version, so the version does not credit its
// author. The version the edit's own settlement writes holds it and credits them (LEGION-479).
func TestSettlementRepairCreditsNoAuthorOfAnEditMadeWhileItsVersionRenders(t *testing.T) {
	service, artifactID := newTestService(t)
	service.settle = time.Hour
	seedServiceText(t, service, artifactID, ":::ask{#ask-block urgency=\"med\" multiple=\"false\" state=\"open\"}\nShip it?\n:::\n\nContext before.\n")
	service.settleRoom(artifactID, 0)
	answer := answerBlockAsk(t, service, artifactID)
	bob := model.Actor{Kind: "user", ID: "bob"}
	service.addConnection(artifactID, 1, bob)

	var edited atomic.Bool
	service.afterSettleVersionRead = func(room string) {
		if room == artifactID && edited.CompareAndSwap(false, true) {
			editAsPeer(t, service, artifactID, replaceRun("Context before.", "Context after."))
		}
	}
	settleCurrentGeneration(t, service, artifactID)
	if !edited.Load() {
		t.Fatal("settlement never reached the window between its version's read and its render")
	}
	repaired := ":::ask{#ask-block urgency=\"med\" multiple=\"false\" state=\"answered\" answered_by=\"alice\" answered_at=\"" +
		answer.At.Format(time.RFC3339Nano) + "\" selected=\"[]\"}\nShip it?\n:::\n\n"
	requireLatestVersionMarkdown(t, service, artifactID, repaired+"Context before.\n")
	repairVersion := latestVersionNumber(t, service, artifactID)
	if authors := latestVersionAuthors(t, service, artifactID); slices.Contains(authors, bob) {
		t.Fatalf("version %d authors = %#v, want no bob, whose edit it lacks", repairVersion, authors)
	}

	service.afterSettleVersionRead = nil
	settleCurrentGeneration(t, service, artifactID)
	requireLatestVersionMarkdown(t, service, artifactID, repaired+"Context after.\n")
	editVersion := latestVersionNumber(t, service, artifactID)
	if editVersion != repairVersion+1 {
		t.Fatalf("latest version = %d, want %d, the edit's own", editVersion, repairVersion+1)
	}
	if authors := latestVersionAuthors(t, service, artifactID); !slices.Contains(authors, bob) {
		t.Fatalf("version %d authors = %#v, want bob, whose edit it holds", editVersion, authors)
	}
}

// latestVersionAuthors is the authors the document's latest version credits.
func latestVersionAuthors(t *testing.T, service *Service, artifactID string) []model.Actor {
	t.Helper()
	var raw []byte
	if err := service.store.Pool.QueryRow(context.Background(), `
		select authors from artifact_versions where artifact_id = $1 order by number desc limit 1
	`, artifactID).Scan(&raw); err != nil {
		t.Fatalf("read the latest document version's authors: %v", err)
	}
	var authors []model.Actor
	if err := json.Unmarshal(raw, &authors); err != nil {
		t.Fatalf("decode the latest document version's authors: %v", err)
	}
	return authors
}

// A repairing settlement's version can hold a browser's edit whose own update the room stores
// only after the settlement commits, since the settlement holds the document's lock until then.
// When that append fails, the room fails and reloads from the store without the edit's text, which
// the version already holds; the settlement's own stored update carries the document's delete set,
// the edit's deletions included. The browser resends what the room lacks when it reconnects, and
// the document's next settlement finds the document matching that version and writes no other
// (LEGION-479).
func TestSettlementVersionAheadOfAFailedAppendIsMetByTheBrowsersResend(t *testing.T) {
	database := storetest.Open(t)
	artifactID := createDocument(t, database, "# First")
	store := &failingBrowserAppendStore{
		VersionedStore: NewPgVersioned(database),
		entered:        make(chan struct{}),
		release:        make(chan struct{}),
	}
	service := New(Deps{Store: database, Persistence: store, Events: events.NewBroker(), Settle: time.Hour})
	t.Cleanup(func() { _ = service.Shutdown(context.Background()) })
	seedServiceText(t, service, artifactID, ":::ask{#ask-block urgency=\"med\" multiple=\"false\" state=\"open\"}\nShip it?\n:::\n\nContext before.\n")
	service.settleRoom(artifactID, 0)
	answer := answerBlockAsk(t, service, artifactID)

	var browser *crdt.Doc
	service.afterSettleReconcile = func(room string) {
		if room == artifactID && browser == nil {
			store.failing.Store(true)
			browser = editAsPeer(t, service, artifactID, replaceRun("Context before.", "Context after."))
		}
	}
	settleCurrentGeneration(t, service, artifactID)
	if browser == nil {
		t.Fatal("settlement never reached the window between its read and its repair")
	}
	want := ":::ask{#ask-block urgency=\"med\" multiple=\"false\" state=\"answered\" answered_by=\"alice\" answered_at=\"" +
		answer.At.Format(time.RFC3339Nano) + "\" selected=\"[]\"}\nShip it?\n:::\n\nContext after.\n"
	requireLatestVersionMarkdown(t, service, artifactID, want)
	versioned := latestVersionNumber(t, service, artifactID)

	// The edit's append fails, which fails the room; it reloads without the edit's text.
	<-store.entered
	close(store.release)
	waitForRoomFailure(t, service, artifactID)
	ctx := context.Background()
	if err := service.awaitRoomRecovery(ctx, artifactID); err != nil {
		t.Fatalf("wait for the room to recover: %v", err)
	}
	store.failing.Store(false)
	service.afterSettleReconcile = nil
	if reloaded, err := service.Text(ctx, artifactID); err != nil || strings.Contains(reloaded, "Context after.") {
		t.Fatalf("reloaded document = %q (%v), want it without the edit whose append failed", reloaded, err)
	}

	// The browser reconnects and sends what the room lacks, as its sync does.
	var resendErr error
	if err := service.srv.Apply(ctx, artifactID, func(room *crdt.Doc, _ func(func(*crdt.Transaction))) {
		resendErr = crdt.ApplyUpdateV1(room, crdt.EncodeStateAsUpdateV1(browser, room.StateVector()), "peer")
	}); err != nil && !errors.Is(err, ygws.ErrNoChanges) {
		t.Fatalf("load the room for the browser's resend: %v", err)
	}
	if resendErr != nil {
		t.Fatalf("resend the browser's edit: %v", resendErr)
	}
	settleCurrentGeneration(t, service, artifactID)
	waitForDocumentText(t, service, artifactID, want)
	waitForPersistedProofText(t, service.store, artifactID, want)
	if latest := latestVersionNumber(t, service, artifactID); latest != versioned {
		t.Fatalf("latest version = %d, want %d, which already held the resent edit", latest, versioned)
	}
}

// requireLatestVersionMarkdown requires the document's latest version to hold want.
func requireLatestVersionMarkdown(t *testing.T, service *Service, artifactID, want string) {
	t.Helper()
	var markdown string
	if err := service.store.Pool.QueryRow(context.Background(), `
		select markdown from artifact_versions where artifact_id = $1 order by number desc limit 1
	`, artifactID).Scan(&markdown); err != nil {
		t.Fatalf("read the latest document version: %v", err)
	}
	if markdown != want {
		t.Fatalf("latest version markdown = %q, want %q", markdown, want)
	}
}

// A browser that deletes the ask block settlement is repairing, between the reconciliation and its
// write, keeps the deletion: the repair finds no block to write into, so the settlement wrote nothing
// into the room and leaves the moved document, versions included, to the settlement the deletion
// scheduled. The repair's transaction, which changed nothing, still reports an update; its slot is
// finished with that update and the room's persistence worker consumes it (LEGION-479).
func TestSettlementRepairOfAnAskAPeerDeletedWritesNothing(t *testing.T) {
	service, artifactID := newTestService(t)
	service.settle = time.Hour
	seedServiceText(t, service, artifactID, ":::ask{#ask-block urgency=\"med\" multiple=\"false\" state=\"open\"}\nShip it?\n:::\n\nContext.\n")
	service.settleRoom(artifactID, 0)
	answerBlockAsk(t, service, artifactID)
	versionsBefore := latestVersionNumber(t, service, artifactID)

	var deleted atomic.Bool
	service.afterSettleReconcile = func(room string) {
		if room == artifactID && deleted.CompareAndSwap(false, true) {
			editAsPeer(t, service, artifactID, func(tree *pmdoc.Node) *pmdoc.Node {
				tree.Children = tree.Children[1:]
				return tree
			})
		}
	}
	settleCurrentGeneration(t, service, artifactID)
	if !deleted.Load() {
		t.Fatal("settlement never reached the window between its read and its repair")
	}

	waitForDocumentText(t, service, artifactID, "Context.\n")
	requireNoSuppressedSlots(t, service, artifactID, "after the repair")
	waitForPersistedProofText(t, service.store, artifactID, "Context.\n")
	if versions := latestVersionNumber(t, service, artifactID); versions != versionsBefore {
		t.Fatalf("latest version = %d, want %d: the settlement versioned the ask block the peer deleted", versions, versionsBefore)
	}
}

func latestVersionNumber(t *testing.T, service *Service, artifactID string) int {
	t.Helper()
	var latest int
	if err := service.store.Pool.QueryRow(context.Background(), `
		select coalesce(max(number), 0) from artifact_versions where artifact_id = $1
	`, artifactID).Scan(&latest); err != nil {
		t.Fatalf("read the latest document version: %v", err)
	}
	return latest
}

// A settlement takes a suppression slot before it stamps block ids, and the stamp it then writes
// can find the ids already repaired - here by another writer between settlement's read and its
// write. That slot is released every time: one nothing finishes or discards holds the room's
// persistence worker at the next update it is handed, and every later update of the room with it
// (LEGION-479).
func TestSettlementReleasesTheSlotOfAStampTheDocumentNoLongerNeeds(t *testing.T) {
	service, artifactID := newTestService(t)
	service.settle = time.Hour
	seedServiceText(t, service, artifactID, "before")
	settleCurrentGeneration(t, service, artifactID)

	var stampFirst atomic.Bool
	service.afterSettleRead = func(room string) {
		if room == artifactID && stampFirst.CompareAndSwap(true, false) {
			// Another writer identifies the block first, as an agent's edit stamps every block
			// it addresses.
			editLiveTree(t, service, artifactID, func(tree *pmdoc.Node) *pmdoc.Node {
				pmdoc.EnsureBlockIDs(tree)
				return tree
			})
		}
	}
	want := "before\n"
	for round := range 5 {
		paragraph := fmt.Sprintf("round %d", round)
		editLiveTree(t, service, artifactID, appendUnidentifiedBlocks(t, paragraph))
		want += "\n" + paragraph + "\n"
		stampFirst.Store(true)
		settleCurrentGeneration(t, service, artifactID)
		if stampFirst.Load() {
			t.Fatalf("round %d: settlement never reached the window between its read and its stamp", round)
		}
		requireNoSuppressedSlots(t, service, artifactID, fmt.Sprintf("round %d", round))
	}
	waitForPersistedProofText(t, service.store, artifactID, want)
}

// The block-id backfill decides from a read of a document that it needs stamping, then stamps the
// document as it stands, which another writer can have stamped in between. That stamp's transaction
// writes nothing and still reports an update; the backfill holds it from the room's persistence as
// it holds every stamp it writes, so the room stores no row for it, least of all one recording a
// content change, for which the next settlement would version a document nobody edited
// (LEGION-479).
func TestBackfillStoresNothingForAStampTheDocumentNoLongerNeeds(t *testing.T) {
	database := storetest.Open(t)
	artifactID := createDocument(t, database, "before")
	seedUnidentifiedProofDocument(t, database, artifactID, "before")
	service := New(Deps{Store: database, Events: events.NewBroker(), Settle: time.Hour})
	t.Cleanup(func() {
		if err := service.Shutdown(context.Background()); err != nil {
			t.Errorf("shutdown document service: %v", err)
		}
	})
	alignLatestVersionWithUpdates(t, service, artifactID)

	var stampedFirst atomic.Bool
	service.afterBackfillRead = func(room string) {
		if room == artifactID && stampedFirst.CompareAndSwap(false, true) {
			// Another writer identifies the block first, as an agent's edit stamps every block it
			// addresses.
			editLiveTree(t, service, artifactID, func(tree *pmdoc.Node) *pmdoc.Node {
				pmdoc.EnsureBlockIDs(tree)
				return tree
			})
		}
	}
	reports, err := service.BackfillBlockIDs(context.Background())
	if err != nil {
		t.Fatalf("backfill documents: %v", err)
	}
	if !stampedFirst.Load() {
		t.Fatal("the backfill never reached the window between its read and its stamp")
	}
	if len(reports) != 1 || reports[0].Err != nil || reports[0].Stamped != 0 {
		t.Fatalf("backfill reports = %#v, want the one document, stamping nothing", reports)
	}
	requireNoSuppressedSlots(t, service, artifactID, "after the backfill")

	// The room hands its persistence its updates in order, so once an update written after the
	// backfill is stored, so is anything the backfill left the room to store. The marker gives the
	// paragraph another block id, which no rendering carries, so its update is no content change,
	// and its bytes are its own: an update that wrote nothing has the same bytes as any other that
	// wrote nothing, and would take the marker's classification.
	editLiveTree(t, service, artifactID, func(tree *pmdoc.Node) *pmdoc.Node {
		tree.Children[0].Attrs[pmdoc.BlockIDAttr] = "marker"
		return tree
	})
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	if err := service.waitForPendingUpdates(ctx, artifactID); err != nil {
		t.Fatalf("wait for the room's updates to reach persistence: %v", err)
	}
	if err := service.waitForDurableAppends(ctx, artifactID); err != nil {
		t.Fatalf("wait for the room's updates to become durable: %v", err)
	}
	if rows := contentRowsPastLatestVersion(t, database, artifactID); rows != 0 {
		t.Fatalf("the backfill left %d content-class rows past the latest version's cursor; it wrote nothing", rows)
	}
}

// A settlement that both stamps block ids and repairs an ask block writes two updates into the
// room, and the room's persistence worker is handed each on its own. Both are held from it and
// released once each reaches it (LEGION-479).
func TestSettlementThatStampsAndRepairsReleasesItsSlots(t *testing.T) {
	service, artifactID := newTestService(t)
	service.settle = time.Hour
	seedServiceText(t, service, artifactID, ":::ask{#ask-block urgency=\"med\" multiple=\"false\" state=\"open\"}\nShip it?\n:::\n")
	service.settleRoom(artifactID, 0)
	for round := range 3 {
		answerBlockAsk(t, service, artifactID)
		paragraph := fmt.Sprintf("round %d", round)
		editLiveTree(t, service, artifactID, func(tree *pmdoc.Node) *pmdoc.Node {
			tree.Children[0].Attrs["state"] = "open"
			return appendUnidentifiedBlocks(t, paragraph)(tree)
		})
		settleCurrentGeneration(t, service, artifactID)
		if repairs := pmdoc.BlockIDRepairCount(liveTree(t, service, artifactID)); repairs != 0 {
			t.Fatalf("round %d: settlement left %d unstamped blocks, so it never stamped", round, repairs)
		}
		if state := liveTree(t, service, artifactID).Children[0].Attrs["state"]; state != "answered" {
			t.Fatalf("round %d: ask block state = %v, so settlement never repaired it", round, state)
		}
		requireNoSuppressedSlots(t, service, artifactID, fmt.Sprintf("round %d", round))
		reopenBlockAsk(t, service, artifactID)
	}
}

// A repair whose room retires under its transaction - ygo closes a room the moment its last
// browser leaves, and CloseRoom does for SetIssueClosed and Shutdown, whether or not a
// Server.Apply holds the room - commits into a room whose persistence worker is gone. ygo then
// hands the commit's update to the store on the repair's own goroutine, inside the commit
// (persistStranded), where nothing but that goroutine could release the repair's suppression slot.
// The update is discarded there instead, and the repair, finding its room gone, gives the write
// up and fails the room, so the next settlement or backfill stamps the document (LEGION-479).
func TestASettlementWhoseRoomRetiresUnderItsStampReturns(t *testing.T) {
	service, artifactID := newTestService(t)
	service.settle = time.Hour
	pause := pauseRepairCommits(service)
	seedServiceText(t, service, artifactID, "before")
	settleCurrentGeneration(t, service, artifactID)
	editLiveTree(t, service, artifactID, appendUnidentifiedBlocks(t, "added"))
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	if err := service.waitForPendingUpdates(ctx, artifactID); err != nil {
		t.Fatalf("wait for the edit to reach persistence: %v", err)
	}
	if err := service.waitForDurableAppends(ctx, artifactID); err != nil {
		t.Fatalf("wait for the edit to become durable: %v", err)
	}
	state := service.room(artifactID)
	state.mu.Lock()
	generation := state.gen
	state.mu.Unlock()

	retireRoomUnderRepair(t, service, pause, artifactID, func() { service.settleRoom(artifactID, generation) })

	if err := service.awaitRoomRecovery(ctx, artifactID); err != nil {
		t.Fatalf("wait for the room to recover: %v", err)
	}
	settleCurrentGeneration(t, service, artifactID)
	requireNoSuppressedSlots(t, service, artifactID, "after the next settlement")
	waitForPersistedProofText(t, service.store, artifactID, "before\n\nadded\n")
	if repairs := pmdoc.BlockIDRepairCount(persistedProofTree(t, service.store, artifactID)); repairs != 0 {
		t.Fatalf("the next settlement left %d unstamped blocks", repairs)
	}
}

// The same retirement under the block-id backfill's stamp.
func TestABackfillWhoseRoomRetiresUnderItsStampReturns(t *testing.T) {
	database := storetest.Open(t)
	artifactID := createDocument(t, database, "before")
	seedUnidentifiedProofDocument(t, database, artifactID, "before")
	service := New(Deps{Store: database, Events: events.NewBroker(), Settle: time.Hour})
	t.Cleanup(func() {
		if err := service.Shutdown(context.Background()); err != nil {
			t.Errorf("shutdown document service: %v", err)
		}
	})
	pause := pauseRepairCommits(service)

	var reports []BlockIDBackfill
	var backfillErr error
	retireRoomUnderRepair(t, service, pause, artifactID, func() {
		reports, backfillErr = service.BackfillBlockIDs(context.Background())
	})
	if backfillErr != nil {
		t.Fatalf("backfill documents: %v", backfillErr)
	}
	if len(reports) != 1 || !errors.Is(reports[0].Err, errRoomReplaced) {
		t.Fatalf("backfill reports = %#v, want the one document's stamp given up with its room", reports)
	}

	reports, backfillErr = service.BackfillBlockIDs(context.Background())
	if backfillErr != nil || len(reports) != 1 || reports[0].Err != nil || reports[0].Stamped != 1 {
		t.Fatalf("second backfill = %#v, %v, want the document stamped", reports, backfillErr)
	}
	requireNoSuppressedSlots(t, service, artifactID, "after the second backfill")
	if repairs := pmdoc.BlockIDRepairCount(persistedProofTree(t, database, artifactID)); repairs != 0 {
		t.Fatalf("the second backfill persisted %d unstamped blocks", repairs)
	}
}

// repairCommitPause holds a room's next repair commit in the room's update observers once armed,
// after the commit and before ygo's persistence observer.
type repairCommitPause struct {
	armed   atomic.Bool
	held    chan struct{}
	proceed chan struct{}
	release sync.Once
}

// pauseRepairCommits installs a repairCommitPause on every room service loads from now on, in an
// update observer registered in OnLoadDocument, which ygo fires before the persistence observer it
// registers after OnLoadDocument.
func pauseRepairCommits(service *Service) *repairCommitPause {
	pause := &repairCommitPause{held: make(chan struct{}), proceed: make(chan struct{})}
	load := service.srv.OnLoadDocument
	service.srv.OnLoadDocument = func(ctx context.Context, room string, doc *crdt.Doc) error {
		if err := load(ctx, room, doc); err != nil {
			return err
		}
		doc.OnUpdate(func(_ []byte, origin any) {
			if _, repair := origin.(*identityClosureOrigin); repair && pause.armed.CompareAndSwap(true, false) {
				close(pause.held)
				<-pause.proceed
			}
		})
		return nil
	}
	return pause
}

func (pause *repairCommitPause) let() {
	pause.release.Do(func() { close(pause.proceed) })
}

// retireRoomUnderRepair runs repair, closes the room while repair's first commit into it is held,
// lets the commit go on, and requires repair to return.
func retireRoomUnderRepair(t *testing.T, service *Service, pause *repairCommitPause, artifactID string, repair func()) {
	t.Helper()
	t.Cleanup(pause.let)
	pause.armed.Store(true)
	done := make(chan struct{})
	go func() {
		defer close(done)
		repair()
	}()
	select {
	case <-pause.held:
	case <-done:
		t.Fatal("the repair returned without committing into the room")
	case <-time.After(10 * time.Second):
		t.Fatal("the repair never committed into the room")
	}
	// ygo closes a room this way the moment its last browser leaves. The close returns once the
	// room's persistence worker has exited, which includes its compaction.
	closed := make(chan error, 1)
	go func() { closed <- service.srv.CloseRoom(artifactID, true) }()
	select {
	case err := <-closed:
		if err != nil {
			t.Fatalf("close the room: %v", err)
		}
	case <-time.After(10 * time.Second):
		t.Fatal("deadlock: the room's persistence worker never exited while the repair held the room's lock")
	}
	pause.let()
	select {
	case <-done:
	case <-time.After(10 * time.Second):
		// Release the repair so the test's shutdown can finish.
		service.purgeSuppressedPersistence(artifactID)
		t.Fatal("deadlock: the repair never returned once its room's persistence worker retired under its commit")
	}
}

// answerBlockAsk answers the document's one indexed ask in its row alone, so the block disagrees
// with it until settlement repairs the block.
func answerBlockAsk(t *testing.T, service *Service, artifactID string) model.AskAnswer {
	t.Helper()
	answer := model.AskAnswer{User: "alice", Selected: []string{}, At: time.Now().UTC()}
	answerJSON, err := json.Marshal(answer)
	if err != nil {
		t.Fatalf("encode answer: %v", err)
	}
	tag, err := service.store.Pool.Exec(context.Background(), `
		update asks set state = 'answered', answer = $2 where block_artifact_id = $1
	`, artifactID, answerJSON)
	if err != nil {
		t.Fatalf("answer indexed ask: %v", err)
	}
	if tag.RowsAffected() != 1 {
		t.Fatalf("answered %d asks, want the document's one", tag.RowsAffected())
	}
	return answer
}

// reopenBlockAsk takes the answer back off the document's indexed ask, so the next round can
// answer it again.
func reopenBlockAsk(t *testing.T, service *Service, artifactID string) {
	t.Helper()
	if _, err := service.store.Pool.Exec(context.Background(), `
		update asks set state = 'open', answer = null where block_artifact_id = $1
	`, artifactID); err != nil {
		t.Fatalf("reopen indexed ask: %v", err)
	}
}

// appendUnidentifiedBlocks returns a live edit that appends the blocks parsed from markdown
// without block ids, as a write that predates them leaves a block.
func appendUnidentifiedBlocks(t *testing.T, markdown string) func(*pmdoc.Node) *pmdoc.Node {
	t.Helper()
	parsed, err := pmdoc.Parse(markdown)
	if err != nil {
		t.Fatalf("parse appended blocks: %v", err)
	}
	removeBlockIDs(parsed)
	return func(tree *pmdoc.Node) *pmdoc.Node {
		tree.Children = append(tree.Children, parsed.Children...)
		return tree
	}
}

// requireNoSuppressedSlots waits for the room's persistence worker to consume every suppression
// slot queued for room. A slot still queued at the deadline is released before the test fails,
// so the worker it holds lets the test's shutdown finish.
func requireNoSuppressedSlots(t *testing.T, service *Service, room, when string) {
	t.Helper()
	deadline := time.Now().Add(2 * time.Second)
	for {
		service.suppressMu.Lock()
		queued := len(service.suppressed[room])
		service.suppressMu.Unlock()
		if queued == 0 {
			return
		}
		if time.Now().After(deadline) {
			service.purgeSuppressedPersistence(room)
			t.Fatalf("%s: %d suppression slots still queued for the room", when, queued)
		}
		time.Sleep(5 * time.Millisecond)
	}
}

// editAsPeer writes a tree change as a browser's edit reaches the room: made in a document of its
// own, with a client id of its own, synced from the room and applied to it as a peer's update. It
// returns that browser's document.
func editAsPeer(t *testing.T, service *Service, artifactID string, edit func(*pmdoc.Node) *pmdoc.Node) *crdt.Doc {
	t.Helper()
	room := service.srv.GetDoc(artifactID)
	if room == nil {
		t.Fatal("document room is not resident")
	}
	peer := crdt.New()
	if err := crdt.ApplyUpdateV1(peer, crdt.EncodeStateAsUpdateV1(room, nil), nil); err != nil {
		t.Fatalf("sync peer from the room: %v", err)
	}
	synced := peer.StateVector()
	tree, err := treeOf(peer)
	if err != nil {
		t.Fatalf("read peer document: %v", err)
	}
	fragment := peer.GetXmlFragment(fragmentName)
	peer.Transact(func(txn *crdt.Transaction) {
		if err := pmdoc.Update(txn, fragment, edit(tree)); err != nil {
			t.Errorf("edit peer document: %v", err)
		}
	})
	if err := crdt.ApplyUpdateV1(room, crdt.EncodeStateAsUpdateV1(peer, synced), "peer"); err != nil {
		t.Fatalf("apply the peer's edit to the room: %v", err)
	}
	return peer
}
