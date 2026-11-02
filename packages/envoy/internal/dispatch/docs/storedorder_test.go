package docs

import (
	"context"
	"flag"
	"fmt"
	"math/rand/v2"
	"net/http"
	"net/http/httptest"
	"slices"
	"sort"
	"sync"
	"sync/atomic"
	"testing"
	"time"
	"weak"

	"github.com/jackc/pgx/v5"
	"github.com/reearth/ygo/crdt"
	"github.com/reearth/ygo/persistence"
	ygsync "github.com/reearth/ygo/sync"

	"github.com/sjawhar/envoy/internal/dispatch/docs/docstest"
	"github.com/sjawhar/envoy/internal/dispatch/events"
	"github.com/sjawhar/envoy/internal/dispatch/model"
	"github.com/sjawhar/envoy/internal/dispatch/pmdoc"
	"github.com/sjawhar/envoy/internal/dispatch/store"
	"github.com/sjawhar/envoy/internal/dispatch/store/storetest"
)

// storedOrderRounds is how many rounds TestStoredOrderSoak runs. The default fits the package in
// CI's ten-minute deadline; a proof run raises it (-args -stored-order-rounds=<n>).
var storedOrderRounds = flag.Int("stored-order-rounds", 12, "rounds of agent writes TestStoredOrderSoak runs under typing peers")

var (
	storedOrderAgent = model.Actor{Kind: "session", ID: "session-0123456789abcdef"}
	storedOrderBob   = model.Actor{Kind: "user", ID: "bob"}
)

// craftedHistory writes a document's stored rows directly, for the checker's boundary test: each
// update one client made on the history's own document, stored at the next version with whatever
// class the test gives it.
type craftedHistory struct {
	t          *testing.T
	database   *store.Store
	artifactID string
	doc        *crdt.Doc
	next       int64
}

// newCraftedHistory is a document whose first stored update writes markdown, stored as content.
// Its artifact's first version is createDocument's fixture, recorded at update 0.
func newCraftedHistory(t *testing.T, markdown string) *craftedHistory {
	t.Helper()
	database := storetest.Open(t)
	history := &craftedHistory{t: t, database: database, artifactID: createDocument(t, database, "# First"), doc: crdt.New(crdt.WithClientID(7)), next: 1}
	tree, err := pmdoc.Parse(markdown)
	if err != nil {
		t.Fatalf("parse the history's document: %v", err)
	}
	history.store(history.change(func(txn *crdt.Transaction, fragment *crdt.YXmlFragment) {
		if err := pmdoc.Update(txn, fragment, tree); err != nil {
			t.Errorf("write the history's document: %v", err)
		}
	}), true)
	return history
}

// change runs write in one transaction on the history's document and returns its update.
func (h *craftedHistory) change(write func(txn *crdt.Transaction, fragment *crdt.YXmlFragment)) []byte {
	h.t.Helper()
	var update []byte
	unsubscribe := h.doc.OnUpdate(func(made []byte, _ any) { update = append([]byte(nil), made...) })
	defer unsubscribe()
	fragment := h.doc.GetXmlFragment(fragmentName)
	h.doc.Transact(func(txn *crdt.Transaction) { write(txn, fragment) })
	if update == nil {
		h.t.Fatal("the history's change made no update")
	}
	return update
}

// edit changes the history's document tree and returns the update.
func (h *craftedHistory) edit(edit func(*pmdoc.Node) *pmdoc.Node) []byte {
	h.t.Helper()
	tree, err := treeOf(h.doc)
	if err != nil {
		h.t.Fatalf("read the history's document: %v", err)
	}
	next := edit(tree)
	return h.change(func(txn *crdt.Transaction, fragment *crdt.YXmlFragment) {
		if err := pmdoc.Update(txn, fragment, next); err != nil {
			h.t.Errorf("edit the history's document: %v", err)
		}
	})
}

func (h *craftedHistory) store(update []byte, contentChanged bool) int64 {
	h.t.Helper()
	return h.storeAt(update, contentChanged, time.Now())
}

func (h *craftedHistory) storeAt(update []byte, contentChanged bool, createdAt time.Time) int64 {
	h.t.Helper()
	version := h.next
	h.next++
	if _, err := h.database.Pool.Exec(context.Background(), `
		insert into doc_updates (artifact_id, version, update, content_changed, created_at) values ($1, $2, $3, $4, $5)
	`, h.artifactID, version, update, contentChanged, createdAt); err != nil {
		h.t.Fatalf("store update %d: %v", version, err)
	}
	return version
}

// markdown is what the history's document renders now.
func (h *craftedHistory) markdown() string {
	h.t.Helper()
	markdown, err := renderDocument(h.doc)
	if err != nil {
		h.t.Fatalf("render the history's document: %v", err)
	}
	return markdown
}

// version records the document's next version, holding markdown at docUpdateVersion.
func (h *craftedHistory) version(markdown string, docUpdateVersion int64) {
	h.t.Helper()
	if _, err := h.database.Pool.Exec(context.Background(), `
		insert into artifact_versions (artifact_id, number, markdown, authors, doc_update_version)
		select $1, coalesce(max(number), 0) + 1, $2, '[]', $3 from artifact_versions where artifact_id = $1
	`, h.artifactID, markdown, docUpdateVersion); err != nil {
		h.t.Fatalf("record a version at update %d: %v", docUpdateVersion, err)
	}
}

func (h *craftedHistory) check(since time.Time) StoredOrderReport {
	h.t.Helper()
	report, err := CheckStoredOrder(context.Background(), h.database.Pool, h.artifactID, since)
	if err != nil {
		h.t.Fatalf("check the stored order: %v", err)
	}
	return report
}

// onlyMismatch fails unless report holds exactly one mismatch, of kind, and returns it.
func onlyMismatch(t *testing.T, report StoredOrderReport, kind StoredOrderMismatchKind) StoredOrderMismatch {
	t.Helper()
	if report.Refused != "" || len(report.Mismatches) != 1 || report.Mismatches[0].Kind != kind {
		t.Fatalf("report = %+v, want exactly one %s mismatch", report, kind)
	}
	return report.Mismatches[0]
}

// The checker reports each way a stored history fails to replay - a class its prefix does not
// measure, a version at an update whose fold renders other text, an unrenderable prefix measured as
// no change, an update stored before one it depends on - and refuses a history a prune rewrote; a
// history that replays passes, a compacted base is folded as a state and not checked, and rows
// created before the since time are folded unchecked.
func TestStoredOrderChecker(t *testing.T) {
	const seed = "First.\n\nSecond.\n"

	t.Run("a history that replays passes", func(t *testing.T) {
		history := newCraftedHistory(t, seed)
		edited := history.store(history.edit(replaceRun("Second.", "Second, edited.")), true)
		history.version(history.markdown(), edited)
		marks := history.doc.GetMap(marksMapName)
		marked := history.store(history.change(func(txn *crdt.Transaction, _ *crdt.YXmlFragment) {
			marks.Set(txn, "m1", map[string]any{"kind": "comment"})
		}), false)
		history.version(history.markdown(), marked)
		report := history.check(time.Time{})
		if !report.Clean() || report.Base != 1 || report.UpdatesChecked != 2 || report.UpdatesSkipped != 0 || report.VersionsChecked != 2 || report.VersionsSkipped != 1 {
			t.Fatalf("report = %+v, want a clean check of two updates and two versions, the fixture version skipped", report)
		}
	})

	t.Run("a check that compares nothing is not clean", func(t *testing.T) {
		history := newCraftedHistory(t, seed)
		if report := history.check(time.Time{}); report.Checked() || report.Clean() || len(report.Mismatches) != 0 {
			t.Fatalf("report of a history holding only its base = %+v, want unchecked and not clean", report)
		}
		history.version(history.markdown(), history.store(history.edit(replaceRun("Second.", "Second, edited.")), true))
		if report := history.check(time.Now().Add(time.Hour)); report.Checked() || report.Clean() || report.UpdatesSkipped != 1 {
			t.Fatalf("report with every row before since = %+v, want one update skipped, unchecked and not clean", report)
		}
	})

	t.Run("a wrong class", func(t *testing.T) {
		history := newCraftedHistory(t, seed)
		edited := history.store(history.edit(replaceRun("Second.", "Second, edited.")), false)
		mismatch := onlyMismatch(t, history.check(time.Time{}), StoredOrderClass)
		if mismatch.Update != edited || mismatch.StoredClass || !mismatch.ExpectedClass {
			t.Fatalf("mismatch = %+v, want update %d stored false, expected true", mismatch, edited)
		}
	})

	t.Run("a version at the wrong update", func(t *testing.T) {
		history := newCraftedHistory(t, seed)
		history.store(history.edit(replaceRun("Second.", "Second, edited.")), true)
		history.version(history.markdown(), 1)
		mismatch := onlyMismatch(t, history.check(time.Time{}), StoredOrderVersion)
		if mismatch.DocUpdateVersion != 1 || !mismatch.FoldRenders || mismatch.FoldMD5 == mismatch.StoredMD5 {
			t.Fatalf("mismatch = %+v, want the version at update 1 holding other text than the fold there", mismatch)
		}
	})

	t.Run("an unrenderable prefix", func(t *testing.T) {
		history := newCraftedHistory(t, seed)
		before := history.markdown()
		outside := history.store(history.change(func(txn *crdt.Transaction, fragment *crdt.YXmlFragment) {
			fragment.InsertElement(txn, 0, crdt.NewYXmlElement("callout"))
		}), false)
		history.version(before, outside)
		report := history.check(time.Time{})
		if report.Refused != "" || len(report.Mismatches) != 2 {
			t.Fatalf("report = %+v, want a class and a version mismatch", report)
		}
		class, version := report.Mismatches[0], report.Mismatches[1]
		if class.Kind != StoredOrderClass || class.Update != outside || !class.ExpectedClass {
			t.Fatalf("class mismatch = %+v, want update %d expected true: a side that does not render is a change", class, outside)
		}
		if version.Kind != StoredOrderVersion || version.FoldRenders {
			t.Fatalf("version mismatch = %+v, want a fold that does not render", version)
		}
	})

	t.Run("a fold left pending", func(t *testing.T) {
		history := newCraftedHistory(t, seed)
		history.edit(replaceRun("Second.", "Second, edited."))
		dependent := history.store(history.edit(replaceRun("edited", "edited again")), false)
		mismatch := onlyMismatch(t, history.check(time.Time{}), StoredOrderPending)
		if mismatch.Update != dependent || mismatch.ParkedItems == 0 {
			t.Fatalf("mismatch = %+v, want update %d leaving parked items", mismatch, dependent)
		}
	})

	t.Run("a compacted base", func(t *testing.T) {
		history := newCraftedHistory(t, seed)
		edited := history.store(history.edit(replaceRun("Second.", "Second, edited.")), true)
		history.version(history.markdown(), edited)
		again := history.store(history.edit(replaceRun("First.", "First, edited.")), true)
		history.version(history.markdown(), again)
		if _, err := NewPgVersioned(history.database).Compact(context.Background(), history.artifactID, 2); err != nil {
			t.Fatalf("compact the history: %v", err)
		}
		if report := history.check(time.Time{}); !report.Clean() || report.Base != edited || report.UpdatesChecked != 1 || report.VersionsChecked != 2 || report.VersionsSkipped != 1 {
			t.Fatalf("report = %+v, want a clean check from base %d", report, edited)
		}
		wrong := history.store(history.edit(replaceRun("Second, edited.", "Second.")), false)
		if mismatch := onlyMismatch(t, history.check(time.Time{}), StoredOrderClass); mismatch.Update != wrong {
			t.Fatalf("mismatch = %+v, want update %d", mismatch, wrong)
		}
	})

	t.Run("a prune checkpoint", func(t *testing.T) {
		history := newCraftedHistory(t, seed)
		history.store(history.edit(replaceRun("Second.", "Second, edited.")), true)
		if _, err := history.database.Pool.Exec(context.Background(), `
			insert into doc_checkpoints (artifact_id, ceiling) values ($1, 1)
		`, history.artifactID); err != nil {
			t.Fatalf("checkpoint a prune: %v", err)
		}
		if report := history.check(time.Time{}); report.Refused == "" || report.Clean() {
			t.Fatalf("report = %+v, want the document refused", report)
		}
	})

	t.Run("a finished prune", func(t *testing.T) {
		history := newCraftedHistory(t, seed)
		edited := history.edit(replaceRun("Second.", "Second, edited."))
		history.store(edited, true)
		again := history.edit(replaceRun("First.", "First, edited."))
		history.version(history.markdown(), history.store(again, true))
		if err := NewPgVersioned(history.database).PruneAfter(context.Background(), history.artifactID, 1, nil); err != nil {
			t.Fatalf("prune the history: %v", err)
		}
		if report := history.check(time.Time{}); report.Refused == "" {
			t.Fatalf("report = %+v, want the document refused: a version names an update the prune removed", report)
		}
		// Appended again past the prune, the numbers return, and the version recorded before it
		// names an update other than the one it was rendered from.
		history.next = 2
		history.version(seed, history.store(edited, true))
		history.store(again, true)
		if report := history.check(time.Time{}); report.Refused == "" {
			t.Fatalf("report = %+v, want the document refused: a later version records an earlier update", report)
		}
	})

	t.Run("rows before the since time", func(t *testing.T) {
		history := newCraftedHistory(t, seed)
		old := history.storeAt(history.edit(replaceRun("Second.", "Second, edited.")), false, time.Now().Add(-time.Hour))
		history.store(history.edit(replaceRun("First.", "First, edited.")), true)
		if report := history.check(time.Now().Add(-time.Minute)); !report.Clean() || report.UpdatesSkipped != 1 || report.UpdatesChecked != 1 {
			t.Fatalf("report = %+v, want a clean check that skipped update %d", report, old)
		}
		if mismatch := onlyMismatch(t, history.check(time.Time{}), StoredOrderClass); mismatch.Update != old {
			t.Fatalf("mismatch = %+v, want update %d once nothing is skipped", mismatch, old)
		}
	})
}

// assertStoredOrder waits for the room's updates to be stored, then checks the document's stored
// history and fails the test on each row that does not replay.
func assertStoredOrder(t *testing.T, service *Service, artifactID string) StoredOrderReport {
	t.Helper()
	waitForLandedAppends(t, service, artifactID)
	report, err := CheckStoredOrder(context.Background(), service.store.Pool, artifactID, time.Time{})
	if err != nil {
		t.Fatalf("check the stored order: %v", err)
	}
	if report.Refused != "" {
		t.Errorf("the stored history is refused: %s", report.Refused)
	}
	for _, mismatch := range report.Mismatches {
		t.Errorf("the stored history does not replay: %s", mismatch)
	}
	return report
}

// storedOrderService is a document service holding "First.\n\nSecond.\n", settled, with
// settlement left to the test.
func storedOrderService(t *testing.T) (*Service, string) {
	t.Helper()
	service, artifactID := newTestService(t)
	service.settle = time.Hour
	seedServiceText(t, service, artifactID, "First.\n\nSecond.\n")
	settleCurrentGeneration(t, service, artifactID)
	return service, artifactID
}

// heldAppendService is storedOrderService over a store that holds the room's first browser append
// until release is closed; entered closes once that append has reached the store.
func heldAppendService(t *testing.T) (service *Service, artifactID string, entered <-chan struct{}, release func()) {
	t.Helper()
	database := storetest.Open(t)
	artifactID = createDocument(t, database, "# First")
	enteredCh := make(chan struct{})
	releaseCh := make(chan struct{})
	held := &blockingFirstAppendStore{VersionedStore: NewPgVersioned(database), entered: enteredCh, release: releaseCh}
	service = New(Deps{
		Store:       database,
		Persistence: held,
		Events:      events.NewBroker(),
		Identity:    headerIdentity(database),
		ServerURL:   "https://dispatch.example",
		Settle:      time.Hour,
	})
	release = func() {
		if held.released.CompareAndSwap(false, true) {
			close(releaseCh)
		}
	}
	t.Cleanup(release)
	t.Cleanup(func() {
		if err := service.Shutdown(context.Background()); err != nil {
			t.Errorf("shutdown document service: %v", err)
		}
	})
	seedServiceText(t, service, artifactID, "First.\n\nSecond.\n")
	settleCurrentGeneration(t, service, artifactID)
	return service, artifactID, enteredCh, release
}

// awaitHeldAppend returns once the held append has reached the store.
func awaitHeldAppend(t *testing.T, entered <-chan struct{}) {
	t.Helper()
	select {
	case <-entered:
	case <-time.After(roomWait(t)):
		t.Fatal("bob's append never reached the store")
	}
}

// onFirst returns a hook for room that runs do the first time it is called for it.
func onFirst(room string, do func()) func(string) {
	var once sync.Once
	return func(name string) {
		if name == room {
			once.Do(do)
		}
	}
}

// Window A. Bob's edit is in the room, and its append is held before the store: the agent's
// unconditional edit of another paragraph forks the room with Bob's edit in it, appends first, and
// names a version of its fork in the same transaction. The version holds Bob's edit at the agent's
// update, where the stored updates do not have it.
func TestStoredOrderHeldBrowserEditUnderAnAgentVersion(t *testing.T) {
	service, artifactID, entered, release := heldAppendService(t)
	editAsConnectedPeer(t, service, artifactID, 1, storedOrderBob, replaceRun("Second.", "Second, bob."))
	awaitHeldAppend(t, entered)
	if err := joinedWrite(service, func(ctx context.Context) error {
		if _, err := service.ApplyOps(ctx, artifactID, []model.EditOp{{Op: "replace", Find: "First.", With: "First, agent."}}, storedOrderAgent, nil); err != nil {
			return err
		}
		_, err := service.NamedVersion(ctx, artifactID, "agent's version", storedOrderAgent)
		return err
	}); err != nil {
		t.Fatalf("agent's edit and version: %v", err)
	}
	release()
	assertStoredOrder(t, service, artifactID)
}

// Window A. Bob's append is held as above, and the agent deletes the paragraph Bob typed into. Bob's
// update was classified as a change against the room before the deletion, but is stored after it,
// where it changes nothing.
func TestStoredOrderHeldBrowserEditInAParagraphTheAgentDeletes(t *testing.T) {
	service, artifactID, entered, release := heldAppendService(t)
	editAsConnectedPeer(t, service, artifactID, 1, storedOrderBob, replaceRun("Second.", "Second, bob."))
	awaitHeldAppend(t, entered)
	if _, err := joinedApplyOps(service, artifactID, []model.EditOp{{Op: "delete", Find: "Second, bob."}}, storedOrderAgent, nil); err != nil {
		t.Fatalf("agent's deletion: %v", err)
	}
	release()
	assertStoredOrder(t, service, artifactID)
}

// Window B1. A conditional edit holds the document's lock from before its fork read; Bob edits the
// room after that read (afterForkRead), and the version named in the same transaction re-forks the
// room and renders his edit at the write's own update, before his append.
func TestStoredOrderEditAfterTheForkReadIsNotInTheWritesVersion(t *testing.T) {
	service, artifactID := storedOrderService(t)
	_, token, err := service.TextWithToken(context.Background(), artifactID)
	if err != nil {
		t.Fatalf("read the document token: %v", err)
	}
	service.addConnection(artifactID, 1, storedOrderBob)
	service.afterForkRead = onFirst(artifactID, func() {
		editAsPeer(t, service, artifactID, replaceRun("Second.", "Second, bob."))
	})
	if err := joinedWrite(service, func(ctx context.Context) error {
		if _, err := service.ApplyOps(ctx, artifactID, []model.EditOp{{Op: "replace", Find: "First.", With: "First, agent."}}, storedOrderAgent, &model.EditPrecondition{Document: token}); err != nil {
			return err
		}
		_, err := service.NamedVersion(ctx, artifactID, "agent's version", storedOrderAgent)
		return err
	}); err != nil {
		t.Fatalf("agent's edit and version: %v", err)
	}
	service.afterForkRead = nil
	assertStoredOrder(t, service, artifactID)
}

// removeSecondUnderBob replaces the document with its first paragraph alone, as the agent, while
// Bob types into the second paragraph from inside the window hook names.
func removeSecondUnderBob(t *testing.T, service *Service, artifactID string, hook *func(string)) {
	t.Helper()
	service.addConnection(artifactID, 1, storedOrderBob)
	defer service.removeConnection(artifactID, 1)
	*hook = onFirst(artifactID, func() {
		editAsPeer(t, service, artifactID, replaceRun("Second.", "Second, bob."))
	})
	defer func() { *hook = nil }()
	if _, err := joinedReplaceText(service, artifactID, "First.\n", storedOrderAgent); err != nil {
		t.Fatalf("agent's replacement: %v", err)
	}
}

// Window B2, before the commit. The agent's replacement removes the second paragraph; after its
// fork read Bob types into that paragraph. His update is classified against a room without the
// removal, as a change, and stored after it, where it changes nothing.
func TestStoredOrderEditAfterTheForkReadIntoADeletedParagraph(t *testing.T) {
	service, artifactID := storedOrderService(t)
	removeSecondUnderBob(t, service, artifactID, &service.afterForkRead)
	assertStoredOrder(t, service, artifactID)
}

// Window B2, between the commit and the publish. The same removal; Bob types into the paragraph
// once the write has committed and before the room receives it (afterLiveCommit), so his append
// lands after the write's while his class was measured without it.
func TestStoredOrderEditBetweenCommitAndPublishIntoADeletedParagraph(t *testing.T) {
	service, artifactID := storedOrderService(t)
	removeSecondUnderBob(t, service, artifactID, &service.afterLiveCommit)
	assertStoredOrder(t, service, artifactID)
}

// Window C, settlement. After an agent's edit, a settlement takes the document's lock; Bob's edit
// then reaches the room and its observer is held before it classifies anything (afterSettleLock,
// beforeObserveUpdate), so the settlement finds nothing pending, records the head without Bob's
// update, and renders the room with his edit in it.
func TestStoredOrderSettlementOverAnUnstoredEdit(t *testing.T) {
	service, artifactID := storedOrderService(t)
	if _, err := joinedApplyOps(service, artifactID, []model.EditOp{{Op: "replace", Find: "First.", With: "First, agent."}}, storedOrderAgent, nil); err != nil {
		t.Fatalf("agent's edit: %v", err)
	}
	var release func()
	service.afterSettleLock = onFirst(artifactID, func() {
		release = holdPeerEdit(t, service, artifactID, &service.beforeObserveUpdate, replaceRun("Second.", "Second, bob."))
	})
	before := latestVersionNumber(t, service, artifactID)
	settleCurrentGeneration(t, service, artifactID)
	service.afterSettleLock = nil
	if release == nil {
		t.Fatal("the settlement never reached its lock")
	}
	release()
	if after := latestVersionNumber(t, service, artifactID); after != before+1 {
		t.Fatalf("latest version = %d, want the settlement's version %d", after, before+1)
	}
	assertStoredOrder(t, service, artifactID)
}

// Window C, snapshot. The setup of TestSnapshotCreditsAnEditMadeBetweenItsAuthorsAndCopyOnTheNextVersion:
// Bob's edit reaches the room between the snapshot's authors and its copy, its observer held after
// crediting it and before handing it to persistence (afterCaptureAuthorsTake, afterCreditUpdate).
// The snapshot holds his edit at a doc_update_version without his update.
func TestStoredOrderSnapshotOverAnUnstoredEdit(t *testing.T) {
	service, artifactID := storedOrderService(t)
	alice := model.Actor{Kind: "user", ID: "alice"}
	if _, err := joinedReplaceText(service, artifactID, "First, alice.\n\nSecond.\n", alice); err != nil {
		t.Fatalf("alice's edit: %v", err)
	}
	service.addConnection(artifactID, 1, storedOrderBob)
	ctx := context.Background()
	tx, err := service.store.Pool.Begin(ctx)
	if err != nil {
		t.Fatalf("begin snapshot transaction: %v", err)
	}
	defer tx.Rollback(ctx)
	joined, ledger := service.Join(ctx, tx)
	defer ledger.Discard()
	var release func()
	service.afterCaptureAuthorsTake = onFirst(artifactID, func() {
		release = holdPeerEdit(t, service, artifactID, &service.afterCreditUpdate, replaceRun("Second.", "Second, bob."))
	})
	snapshot, err := service.SnapshotVersion(joined, artifactID, alice)
	if err != nil {
		t.Fatalf("snapshot document: %v", err)
	}
	if !snapshot.Wrote || release == nil {
		t.Fatalf("snapshot = %#v, release=%t, want a version and the author-to-copy window", snapshot, release != nil)
	}
	if err := ledger.Commit(ctx); err != nil {
		t.Fatalf("commit snapshot: %v", err)
	}
	service.afterCaptureAuthorsTake = nil
	release()
	assertStoredOrder(t, service, artifactID)
}

// Window D. The first peer's edit is in the room and its observer is held before it classifies
// anything (beforeObserveUpdate); the second peer builds on a copy of the room, which holds the
// first's edit, and its observer classifies both and hands its own update to persistence first.
// The second update is stored before the one it depends on.
func TestStoredOrderTwoBrowsersOutOfOrder(t *testing.T) {
	service, artifactID := storedOrderService(t)
	head := func() persistence.Version {
		version, err := service.persistence.Head(context.Background(), artifactID)
		if err != nil {
			t.Fatalf("read the stored head: %v", err)
		}
		return version
	}
	before := head()
	releaseFirst := holdPeerEdit(t, service, artifactID, &service.beforeObserveUpdate, replaceRun("Second.", "Second, bob."))
	editAsPeer(t, service, artifactID, replaceRun("bob", "bob and carol"))
	waitFor(t, roomWait(t), "the second peer's update to be stored", func() bool { return head() > before })
	releaseFirst()
	assertStoredOrder(t, service, artifactID)
}

// A transaction with several operations. An accept is the suggestion's own update, then its
// margin record's (ProjectMark), then a named version, as the accept route writes them. Bob types
// between the two operations: the first has appended, so the transaction holds the document's
// lock and his append waits, while the second operation's fork read takes his edit in. The second
// update is made on a basis holding his edit and stored before it, and the version renders it at
// the second update.
func TestStoredOrderAcceptWithABrowserEditBetweenItsOperations(t *testing.T) {
	service, artifactID := storedOrderService(t)
	if _, err := joinedMarkQuote(service, artifactID, MarkSpec{Kind: MarkSuggestion, ID: "s1", By: storedOrderAgent}, "First", nil); err != nil {
		t.Fatalf("mark the suggestion: %v", err)
	}
	alice := model.Actor{Kind: "user", ID: "alice"}
	service.addConnection(artifactID, 1, storedOrderBob)
	if err := joinedWrite(service, func(ctx context.Context) error {
		if err := service.AcceptSuggestion(ctx, artifactID, "s1", "Primary", alice); err != nil {
			return fmt.Errorf("accept: %w", err)
		}
		editAsPeer(t, service, artifactID, replaceRun("Second.", "Second, bob."))
		record := MarkRecord{Kind: "suggestion", By: storedOrderAgent.ID, CreatedAt: time.Now().UTC().Format(time.RFC3339), Text: "Primary", Resolved: true}
		if err := service.ProjectMark(ctx, artifactID, "s1", record, alice); err != nil {
			return fmt.Errorf("project the margin record: %w", err)
		}
		_, err := service.NamedVersion(ctx, artifactID, "Accepted suggestion", alice)
		return err
	}); err != nil {
		t.Fatalf("accept the suggestion: %v", err)
	}
	assertStoredOrder(t, service, artifactID)
}

// Window B2's credit. Bob is connected while he types into the paragraph the agent's replacement
// removes (afterForkRead), so the room credits him with a change; stored after the removal, his
// update changes nothing, and the version the settlement then writes holds nothing of his. That
// version must name the agent alone.
func TestStoredOrderRemeasuredClassMovesCreditAndSettlement(t *testing.T) {
	service, artifactID := storedOrderService(t)
	removeSecondUnderBob(t, service, artifactID, &service.afterForkRead)
	before := latestVersionNumber(t, service, artifactID)
	settleCurrentGeneration(t, service, artifactID)
	if after := latestVersionNumber(t, service, artifactID); after != before+1 {
		t.Fatalf("latest version = %d, want the settlement's version %d", after, before+1)
	}
	if authors := latestVersionAuthors(t, service, artifactID); !slices.Equal(authors, []model.Actor{storedOrderAgent}) {
		t.Errorf("the settlement's version names %+v, want the agent alone: bob's stored update changes nothing", authors)
	}
	assertStoredOrder(t, service, artifactID)
}

// soakTyping is typeIntoDocument's peer - a paragraph inserted at the start, a word typed into it,
// the paragraph deleted, each its own update, a random time up to gap apart, kept within keepUp's
// lead of the room - that sends nothing while paused is set. Each update ygo encodes carries the
// document's whole delete set, so every stored row costs the check more than the one before it;
// a soak pauses its peers while it checks, so the rows a round stores do not grow with the time its
// checks take.
func soakTyping(t *testing.T, service *Service, serverURL, artifactID string, gap time.Duration, paused *atomic.Bool) {
	t.Helper()
	peer := connectPeer(t, serverURL, artifactID, "alice")
	fragment := peer.Doc.GetXmlFragment(fragmentName)
	stopped := make(chan struct{})
	var done sync.WaitGroup
	value, _ := typists.LoadOrStore(artifactID, new(atomic.Int64))
	typing := value.(*atomic.Int64)
	typing.Add(1)
	done.Go(func() {
		for {
			var paragraph *crdt.YXmlElement
			var run *crdt.YXmlText
			for _, change := range []func(*crdt.Transaction){
				func(txn *crdt.Transaction) {
					paragraph = crdt.NewYXmlElement("paragraph")
					fragment.InsertElement(txn, 0, paragraph)
					run = crdt.NewYXmlText()
					paragraph.InsertText(txn, 0, run)
				},
				func(txn *crdt.Transaction) { run.Insert(txn, 0, "typed", nil) },
				func(txn *crdt.Transaction) {
					for index, child := range fragment.Children() {
						if any(child) == any(paragraph) {
							fragment.Delete(txn, index, 1)
							return
						}
					}
				},
			} {
				for paused.Load() {
					select {
					case <-stopped:
						return
					case <-time.After(time.Millisecond):
					}
				}
				select {
				case <-stopped:
					return
				default:
				}
				update := docstest.Transact(peer.Doc, change)
				if update == nil {
					continue
				}
				if err := peer.Write(ygsync.EncodeUpdate(update)); err != nil {
					t.Errorf("send peer update: %v", err)
					return
				}
				keepUp(service, artifactID, peer.Doc, stopped)
				time.Sleep(rand.N(gap))
			}
		}
	})
	// As typeIntoDocument's: the writing ends, the room applies the peer's last update, and the
	// last peer of the room waits for every update to be stored, before the connection closes.
	t.Cleanup(func() {
		close(stopped)
		done.Wait()
		client := peer.Doc.ClientID()
		sent := peer.Doc.StateVector().Clock(client)
		waitFor(t, 30*time.Second, "the room to apply the peer's last update", func() bool {
			room := service.srv.GetDoc(artifactID)
			return room == nil || room.StateVector().Clock(client) >= sent
		})
		if typing.Add(-1) > 0 {
			return
		}
		typists.Delete(artifactID)
		ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
		defer cancel()
		if err := service.waitForPendingUpdates(ctx, artifactID); err != nil {
			t.Errorf("wait for the peer's updates to reach persistence: %v", err)
		}
		if err := service.waitForDurableAppends(ctx, artifactID); err != nil {
			t.Errorf("wait for the peer's updates to become durable: %v", err)
		}
	})
}

// soakWriteKey marks the context of a soak's agent write, which soakStore watches.
type soakWriteKey struct{}

// soakStore is the store under TestStoredOrderSoak: it records whether an agent write appended
// while its room held a browser update it had not stored.
type soakStore struct {
	VersionedStore
	service  atomic.Pointer[Service]
	unstored atomic.Bool
}

func (s *soakStore) AppendUpdateTx(ctx context.Context, tx pgx.Tx, room string, update []byte, contentChanged bool) (persistence.Version, error) {
	if ctx.Value(soakWriteKey{}) != nil && s.service.Load().unstoredRoomUpdates(room) > 0 {
		s.unstored.Store(true)
	}
	return s.VersionedStore.AppendUpdateTx(ctx, tx, room, update, contentChanged)
}

// unstoredRoomUpdates counts the room's updates not yet stored: committed and not yet classified
// (its replica's log), classified and not handed to the store (pendingUpdates), and handed and not
// yet committed (durableAppends).
func (s *Service) unstoredRoomUpdates(room string) int {
	state := s.room(room)
	state.mu.Lock()
	unstored := state.pendingUpdates
	state.mu.Unlock()
	unstored += int(state.durableAppends.Load())
	if live := s.srv.GetDoc(room); live != nil {
		if listed, ok := s.replicas.Load(weak.Make(live)); ok {
			if replica := listed.(weak.Pointer[renderedReplica]).Value(); replica != nil {
				unstored += replica.log.Pending()
			}
		}
	}
	return unstored
}

// soakWrite runs write as joinedWrite does, its context marked for soakStore.
func soakWrite(service *Service, write func(ctx context.Context) error) error {
	ctx := context.WithValue(context.Background(), soakWriteKey{}, true)
	tx, err := service.store.Pool.Begin(ctx)
	if err != nil {
		return fmt.Errorf("begin the write's transaction: %w", err)
	}
	defer tx.Rollback(ctx)
	joined, ledger := service.Join(ctx, tx)
	defer ledger.Discard()
	if err := write(joined); err != nil {
		return err
	}
	return ledger.Commit(ctx)
}

// The soak: two documents, each with two peers typing as browsers do (soakTyping), take an agent
// write per round - a replace and a snapshot, an anchored comment, an ask block, a two-operation
// edit and a snapshot - while settlements run on a short delay, and every document's stored history
// is checked after every round, the peers paused. It counts the rounds in which an agent write
// appended while its room held a browser update it had not stored, logs them with the mismatching
// rounds and the per-write latency, and fails on any mismatch.
func TestStoredOrderSoak(t *testing.T) {
	database := storetest.Open(t)
	watched := &soakStore{VersionedStore: NewPgVersioned(database)}
	service := New(Deps{
		Store:       database,
		Persistence: watched,
		Events:      events.NewBroker(),
		Identity:    headerIdentity(database),
		ServerURL:   "https://dispatch.example",
		Settle:      30 * time.Millisecond,
	})
	watched.service.Store(service)
	t.Cleanup(func() {
		if err := service.Shutdown(context.Background()); err != nil {
			t.Errorf("shutdown document service: %v", err)
		}
	})
	server := httptest.NewServer(http.HandlerFunc(service.ServeHTTP))
	t.Cleanup(server.Close)

	type soakDocument struct {
		id             string
		alpha, charlie string
	}
	var paused atomic.Bool
	paused.Store(true)
	documents := make([]*soakDocument, 2)
	for index := range documents {
		document := &soakDocument{
			id:      createIssueDocument(t, database, index+1, "# Soak"),
			alpha:   "Alpha line.",
			charlie: "Charlie line.",
		}
		seedServiceText(t, service, document.id, "# Soak\n\nAlpha line.\n\nBravo line.\n\nCharlie line.\n")
		documents[index] = document
		for range 2 {
			soakTyping(t, service, server.URL, document.id, 10*time.Millisecond, &paused)
		}
	}

	var latencies []time.Duration
	write := func(write func(ctx context.Context) error) {
		t.Helper()
		started := time.Now()
		if err := soakWrite(service, write); err != nil {
			t.Fatalf("agent write: %v", err)
		}
		latencies = append(latencies, time.Since(started))
	}
	rounds := *storedOrderRounds
	seen := map[string]bool{}
	var mismatches []string
	// check checks every document's rows created at or after since, and reports whether it found
	// a mismatch no earlier check had.
	check := func(when string, since time.Time) bool {
		t.Helper()
		found := false
		for _, document := range documents {
			report, err := CheckStoredOrder(context.Background(), database.Pool, document.id, since)
			if err != nil {
				t.Fatalf("check the stored order: %v", err)
			}
			lines := make([]string, 0, len(report.Mismatches)+1)
			if report.Refused != "" {
				lines = append(lines, "refused: "+report.Refused)
			}
			for _, mismatch := range report.Mismatches {
				lines = append(lines, mismatch.String())
			}
			for _, line := range lines {
				key := document.id + " " + line
				if !seen[key] {
					seen[key] = true
					found = true
					mismatches = append(mismatches, when+": "+key)
				}
			}
		}
		return found
	}
	var counted, mismatching, mismatchingCounted int
	started := time.Now()
	checkedFrom := time.Time{}
	for round := range rounds {
		watched.unstored.Store(false)
		paused.Store(false)
		// The peers' first keystrokes are in flight when the round's first write starts.
		time.Sleep(5 * time.Millisecond)
		for index, document := range documents {
			switch round % 4 {
			case 0:
				next := fmt.Sprintf("Alpha line %d.", round)
				write(func(ctx context.Context) error {
					if _, err := service.ApplyOps(ctx, document.id, []model.EditOp{{Op: "replace", Find: document.alpha, With: next}}, storedOrderAgent, nil); err != nil {
						return err
					}
					_, err := service.SnapshotVersion(ctx, document.id, storedOrderAgent)
					return err
				})
				document.alpha = next
			case 1:
				write(func(ctx context.Context) error {
					_, err := service.MarkQuote(ctx, document.id, MarkSpec{Kind: MarkComment, ID: fmt.Sprintf("soak-%d-%d", index, round), By: storedOrderAgent}, "Bravo line.", nil)
					return err
				})
			case 2:
				ask := fmt.Sprintf(":::ask{#soak-ask-%d urgency=\"med\" multiple=\"false\"}\nWhich option in round %d?\n:::\n", round, round)
				write(func(ctx context.Context) error {
					_, err := service.ApplyOps(ctx, document.id, []model.EditOp{{Op: "insert", After: "end", Markdown: ask}}, storedOrderAgent, nil)
					return err
				})
			case 3:
				charlie, alpha := fmt.Sprintf("Charlie line %d.", round), fmt.Sprintf("Alpha line %d, again.", round)
				write(func(ctx context.Context) error {
					if _, err := service.ApplyOps(ctx, document.id, []model.EditOp{{Op: "replace", Find: document.charlie, With: charlie}}, storedOrderAgent, nil); err != nil {
						return err
					}
					if _, err := service.ApplyOps(ctx, document.id, []model.EditOp{{Op: "replace", Find: document.alpha, With: alpha}}, storedOrderAgent, nil); err != nil {
						return err
					}
					_, err := service.SnapshotVersion(ctx, document.id, storedOrderAgent)
					return err
				})
				document.charlie, document.alpha = charlie, alpha
			}
		}
		paused.Store(true)
		unstored := watched.unstored.Load()
		if unstored {
			counted++
		}
		// Each round checks the rows created since a little before the last round's check, which
		// covers a row whose transaction began before that check and committed after it; the
		// fold still reads the whole history. The full check after the last round covers the rest.
		checking := time.Now()
		if check(fmt.Sprintf("round %d", round), checkedFrom) {
			mismatching++
			if unstored {
				mismatchingCounted++
			}
		}
		checkedFrom = checking.Add(-2 * time.Second)
	}
	check("after the last round", time.Time{})
	elapsed := time.Since(started)
	sorted := slices.Clone(latencies)
	sort.Slice(sorted, func(i, j int) bool { return sorted[i] < sorted[j] })
	percentile := func(p float64) time.Duration {
		if len(sorted) == 0 {
			return 0
		}
		return sorted[min(len(sorted)-1, int(p*float64(len(sorted))))]
	}
	t.Logf("stored-order soak: rounds=%d counted=%d mismatching_rounds=%d mismatching_counted_rounds=%d mismatches=%d writes=%d write_p50=%s write_p99=%s elapsed=%s",
		rounds, counted, mismatching, mismatchingCounted, len(mismatches), len(latencies), percentile(0.50), percentile(0.99), elapsed.Round(time.Millisecond))
	for index, line := range mismatches {
		if index == 20 {
			t.Logf("... and %d more", len(mismatches)-index)
			break
		}
		t.Logf("%s", line)
	}
	if len(mismatches) > 0 {
		t.Errorf("%d stored rows do not replay over %d rounds (%d counted)", len(mismatches), rounds, counted)
	}
}
