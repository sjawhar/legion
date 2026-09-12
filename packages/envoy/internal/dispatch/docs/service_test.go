package docs

import (
	"context"
	"encoding/json"
	"errors"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/reearth/ygo/crdt"
	"github.com/reearth/ygo/persistence"
	ygws "github.com/reearth/ygo/provider/websocket"

	"github.com/sjawhar/envoy/internal/dispatch/events"
	"github.com/sjawhar/envoy/internal/dispatch/identity"
	"github.com/sjawhar/envoy/internal/dispatch/model"
	"github.com/sjawhar/envoy/internal/dispatch/pmdoc"
	"github.com/sjawhar/envoy/internal/dispatch/store"
)

func newTestService(t *testing.T) (*Service, string) {
	t.Helper()
	database := openTestStore(t)
	artifactID := createDocument(t, database, "# First")
	service := New(Deps{
		Store:     database,
		Events:    events.NewBroker(),
		Identity:  identity.HeaderIdentity{Header: "X-Dispatch-User", AllowedLogins: map[string]struct{}{"alice": {}}},
		ServerURL: "https://dispatch.example",
		Settle:    20 * time.Millisecond,
	})
	t.Cleanup(func() {
		if err := service.Shutdown(context.Background()); err != nil {
			t.Errorf("shutdown document service: %v", err)
		}
	})
	return service, artifactID
}

func TestSettlementIndexesRetractsAndRestoresTypedAskBlocks(t *testing.T) {
	service, artifactID := newTestService(t)
	service.settle = time.Hour
	const askMarkdown = ":::ask{#ask-block multiple=\"false\" state=\"open\" urgency=\"high\"}\nWhich transport should we expose?\n\n- REST: Matches the existing platform\n- gRPC: Adds streaming\n:::\n"
	seedServiceText(t, service, artifactID, askMarkdown)

	service.settleRoom(artifactID, 0)
	var askID, question, urgency, state, blockID string
	var multiple bool
	if err := service.store.Pool.QueryRow(context.Background(), `
		select id::text, question, urgency, state, block_id, multiple
		from asks where block_artifact_id = $1
	`, artifactID).Scan(&askID, &question, &urgency, &state, &blockID, &multiple); err != nil {
		t.Fatalf("read indexed ask: %v", err)
	}
	if blockID != "ask-block" || question != "Which transport should we expose?" ||
		urgency != "high" || multiple || state != "open" {
		t.Fatalf("indexed ask = block=%q question=%q urgency=%q multiple=%t state=%q",
			blockID, question, urgency, multiple, state)
	}

	editLiveTree(t, service, artifactID, func(*pmdoc.Node) *pmdoc.Node {
		return &pmdoc.Node{Type: "doc", Children: []*pmdoc.Node{{
			Type: "paragraph", Attrs: pmdoc.Attrs{pmdoc.BlockIDAttr: "replacement"},
			Children: []*pmdoc.Node{{Type: "text", Text: "No decision remains."}},
		}}}
	})
	stateForDelete := service.room(artifactID)
	stateForDelete.mu.Lock()
	deleteGeneration := stateForDelete.gen
	stateForDelete.mu.Unlock()
	service.settleRoom(artifactID, deleteGeneration)
	var resolutionKind string
	if err := service.store.Pool.QueryRow(context.Background(), `
		select state, resolution->>'kind' from asks where id = $1
	`, askID).Scan(&state, &resolutionKind); err != nil {
		t.Fatalf("read retracted ask: %v", err)
	}
	if state != "resolved" || resolutionKind != "retracted" {
		t.Fatalf("retracted ask state=%q resolution=%q", state, resolutionKind)
	}

	editLiveTree(t, service, artifactID, func(*pmdoc.Node) *pmdoc.Node {
		restored, err := pmdoc.Parse(askMarkdown)
		if err != nil {
			t.Fatalf("parse restored ask: %v", err)
		}
		return restored
	})
	stateForRestore := service.room(artifactID)
	stateForRestore.mu.Lock()
	restoreGeneration := stateForRestore.gen
	stateForRestore.mu.Unlock()
	service.settleRoom(artifactID, restoreGeneration)
	if err := service.store.Pool.QueryRow(context.Background(), `
		select state from asks where id = $1
	`, askID).Scan(&state); err != nil {
		t.Fatalf("read restored ask: %v", err)
	}
	if state != "open" {
		t.Fatalf("restored ask state=%q, want open", state)
	}
}

func TestSettlementRepairsServerOwnedAskAttributesOncePerVersion(t *testing.T) {
	service, artifactID := newTestService(t)
	service.settle = time.Hour
	seedServiceText(t, service, artifactID, ":::ask{#ask-block urgency=\"med\" multiple=\"false\" state=\"open\"}\nShip it?\n:::\n")
	service.settleRoom(artifactID, 0)
	var askID string
	if err := service.store.Pool.QueryRow(context.Background(), `
		select id::text from asks where block_artifact_id = $1
	`, artifactID).Scan(&askID); err != nil {
		t.Fatalf("read indexed ask: %v", err)
	}
	answer := model.AskAnswer{User: "alice", Selected: []string{}, At: time.Now().UTC()}
	answerJSON, err := json.Marshal(answer)
	if err != nil {
		t.Fatalf("encode answer: %v", err)
	}
	if _, err := service.store.Pool.Exec(context.Background(), `
		update asks set state = 'answered', answer = $2 where id = $1
	`, askID, answerJSON); err != nil {
		t.Fatalf("answer indexed ask: %v", err)
	}
	editLiveTree(t, service, artifactID, func(tree *pmdoc.Node) *pmdoc.Node {
		tree.Children[0].Attrs["state"] = "open"
		return tree
	})
	state := service.room(artifactID)
	state.mu.Lock()
	generation := state.gen
	state.mu.Unlock()
	service.settleRoom(artifactID, generation)
	waitForDocumentText(t, service, artifactID, ":::ask{#ask-block urgency=\"med\" multiple=\"false\" state=\"answered\" answered_by=\"alice\" answered_at=\""+answer.At.Format(time.RFC3339Nano)+"\" selected=\"[]\"}\nShip it?\n:::\n")
	var repaired int
	if err := service.store.Pool.QueryRow(context.Background(), `
		select count(*) from events where type = 'block.repaired' and payload->>'block_id' = 'ask-block'
	`).Scan(&repaired); err != nil {
		t.Fatalf("count repairs: %v", err)
	}
	if repaired != 1 {
		t.Fatalf("repair events = %d, want one", repaired)
	}
	service.settleRoom(artifactID, generation)
	if err := service.store.Pool.QueryRow(context.Background(), `
		select count(*) from events where type = 'block.repaired' and payload->>'block_id' = 'ask-block'
	`).Scan(&repaired); err != nil {
		t.Fatalf("count idempotent repairs: %v", err)
	}
	if repaired != 1 {
		t.Fatalf("repair events after repeat = %d, want one", repaired)
	}
}

func TestSettleRendersTreeAndWritesVersion(t *testing.T) {
	service, artifactID := newTestService(t)
	seedServiceText(t, service, artifactID, "before")
	editLiveTree(t, service, artifactID, replaceRun("before", "after"))
	version := waitForDocumentVersion(t, service.store, artifactID, 2)
	var markdown string
	if err := service.store.Pool.QueryRow(context.Background(), `select markdown from artifact_versions where artifact_id = $1 and number = 2`, artifactID).Scan(&markdown); err != nil {
		t.Fatal(err)
	}
	if markdown != "after\n" || version.Named {
		t.Fatalf("version 2 = %q named=%v", markdown, version.Named)
	}
}

func TestSettleStampsPersistedLegacyProofDocument(t *testing.T) {
	database := openTestStore(t)
	artifactID := createDocument(t, database, "before")
	seedUnidentifiedProofDocument(t, database, artifactID, "before")
	service := New(Deps{Store: database, Events: events.NewBroker(), Settle: time.Hour})
	t.Cleanup(func() {
		if err := service.Shutdown(context.Background()); err != nil {
			t.Errorf("shutdown document service: %v", err)
		}
	})

	settled := make(chan struct{})
	go func() {
		service.settleRoom(artifactID, 0)
		close(settled)
	}()
	select {
	case <-settled:
	case <-time.After(time.Second):
		t.Fatal("settlement blocked while stamping a legacy document")
	}

	loaded, err := NewPgVersioned(database).Load(context.Background(), artifactID)
	if err != nil {
		t.Fatalf("load stamped document: %v", err)
	}
	doc := crdt.New()
	if err := crdt.ApplyUpdateV1(doc, loaded.Update, nil); err != nil {
		t.Fatalf("decode stamped document: %v", err)
	}
	tree, err := treeOf(doc)
	if err != nil {
		t.Fatalf("read stamped document: %v", err)
	}
	if repairs := pmdoc.BlockIDRepairCount(tree); repairs != 0 {
		t.Fatalf("persisted document has %d unstamped blocks", repairs)
	}
}
func TestSettleCapturesOnlyItsOwnIdentityUpdate(t *testing.T) {
	database := openTestStore(t)
	artifactID := createDocument(t, database, "before")
	seedUnidentifiedProofDocument(t, database, artifactID, "before")
	loaded, err := NewPgVersioned(database).Load(context.Background(), artifactID)
	if err != nil {
		t.Fatalf("load legacy document: %v", err)
	}
	captured := make(chan []byte, 1)
	service := New(Deps{
		Store:       database,
		Persistence: captureAppendUpdateTxStore{VersionedStore: NewPgVersioned(database), captured: captured},
		Events:      events.NewBroker(),
		Settle:      time.Hour,
	})
	t.Cleanup(func() {
		if err := service.Shutdown(context.Background()); err != nil {
			t.Errorf("shutdown document service: %v", err)
		}
	})

	var injected atomic.Bool
	foreignApplied := make(chan struct{})
	var foreignErr error
	var unsubscribe func()
	err = service.srv.Apply(context.Background(), artifactID, func(doc *crdt.Doc, _ func(func(*crdt.Transaction))) {
		unsubscribe = doc.OnUpdate(func(_ []byte, _ any) {
			if !injected.CompareAndSwap(false, true) {
				return
			}
			tree, err := treeOf(doc)
			if err == nil {
				fragment := doc.GetXmlFragment(fragmentName)
				foreignErr = doc.TransactE(func(transaction *crdt.Transaction) error {
					return pmdoc.Update(transaction, fragment, replaceRun("before", "foreign")(tree))
				}, "foreign update")
			}
			if foreignErr == nil {
				foreignErr = err
			}
			close(foreignApplied)
		})
	})
	if err != nil && !errors.Is(err, ygws.ErrNoChanges) {
		t.Fatalf("warm document for settlement: %v", err)
	}
	t.Cleanup(unsubscribe)

	service.settleRoom(artifactID, 0)
	<-foreignApplied
	if foreignErr != nil {
		t.Fatalf("apply foreign update during identity settlement: %v", foreignErr)
	}
	identityUpdate := <-captured
	doc := crdt.New()
	if err := crdt.ApplyUpdateV1(doc, loaded.Update, nil); err != nil {
		t.Fatalf("decode legacy document: %v", err)
	}
	if err := crdt.ApplyUpdateV1(doc, identityUpdate, nil); err != nil {
		t.Fatalf("apply captured identity update: %v", err)
	}
	tree, err := treeOf(doc)
	if err != nil {
		t.Fatalf("read captured identity document: %v", err)
	}
	markdown, err := renderTree(tree)
	if err != nil {
		t.Fatalf("render captured identity document: %v", err)
	}
	if markdown != "before\n" {
		t.Fatalf("identity update changed document = %q, want only identity repairs", markdown)
	}
	state := service.room(artifactID)
	state.mu.Lock()
	generation := state.gen
	state.mu.Unlock()
	if generation != 1 {
		t.Fatalf("generation after foreign update = %d, want 1", generation)
	}
	waitForPersistedProofText(t, database, artifactID, "foreign\n")
	service.settleRoom(artifactID, generation)
	waitForDocumentVersion(t, database, artifactID, 2)
}

func TestSettleStampsLegacyChangeInExactlyOneVersion(t *testing.T) {
	database := openTestStore(t)
	artifactID := createDocument(t, database, "before")
	seedUnidentifiedProofDocument(t, database, artifactID, "before")
	service := New(Deps{Store: database, Events: events.NewBroker(), Settle: time.Hour})
	t.Cleanup(func() {
		if err := service.Shutdown(context.Background()); err != nil {
			t.Errorf("shutdown document service: %v", err)
		}
	})

	editLiveTree(t, service, artifactID, replaceRun("before", "after"))
	waitForPersistedProofText(t, database, artifactID, "after\n")
	state := service.room(artifactID)
	state.mu.Lock()
	generation := state.gen
	state.mu.Unlock()
	service.settleRoom(artifactID, generation)

	waitForDocumentVersion(t, database, artifactID, 2)
	var versions int
	if err := database.Pool.QueryRow(context.Background(), `
		select count(*) from artifact_versions where artifact_id = $1
	`, artifactID).Scan(&versions); err != nil {
		t.Fatalf("count settled versions: %v", err)
	}
	if versions != 2 {
		t.Fatalf("versions after identity settlement = %d, want 2", versions)
	}
	if repairs := pmdoc.BlockIDRepairCount(persistedProofTree(t, database, artifactID)); repairs != 0 {
		t.Fatalf("persisted changed document has %d unstamped blocks", repairs)
	}
}

func TestSettleDiscardsIdentityUpdateWhenVersionTransactionFails(t *testing.T) {
	database := openTestStore(t)
	artifactID := createDocument(t, database, "before")
	seedUnidentifiedProofDocument(t, database, artifactID, "before")
	service := New(Deps{Store: database, Events: events.NewBroker(), Settle: time.Hour})
	t.Cleanup(func() {
		if err := service.Shutdown(context.Background()); err != nil {
			t.Errorf("shutdown document service: %v", err)
		}
	})
	if _, err := database.Pool.Exec(context.Background(), `
		create function dispatch_test_reject_identity_settlement() returns trigger language plpgsql as $$
		begin
			if new.number = 2 then
				raise exception 'reject identity settlement version';
			end if;
			return new;
		end;
		$$
	`); err != nil {
		t.Fatalf("create identity settlement failure function: %v", err)
	}
	if _, err := database.Pool.Exec(context.Background(), `
		create trigger dispatch_test_reject_identity_settlement
		before insert on artifact_versions for each row
		execute function dispatch_test_reject_identity_settlement()
	`); err != nil {
		t.Fatalf("create identity settlement failure trigger: %v", err)
	}
	t.Cleanup(func() {
		_, _ = database.Pool.Exec(context.Background(), `drop trigger if exists dispatch_test_reject_identity_settlement on artifact_versions`)
		_, _ = database.Pool.Exec(context.Background(), `drop function if exists dispatch_test_reject_identity_settlement()`)
	})

	editLiveTree(t, service, artifactID, replaceRun("before", "after"))
	waitForPersistedProofText(t, database, artifactID, "after\n")
	state := service.room(artifactID)
	state.mu.Lock()
	generation := state.gen
	state.mu.Unlock()
	service.settleRoom(artifactID, generation)

	waitForRoomFailure(t, service, artifactID)
	if repairs := pmdoc.BlockIDRepairCount(persistedProofTree(t, database, artifactID)); repairs == 0 {
		t.Fatal("failed settlement persisted identity updates")
	}
	var versions int
	if err := database.Pool.QueryRow(context.Background(), `
		select count(*) from artifact_versions where artifact_id = $1
	`, artifactID).Scan(&versions); err != nil {
		t.Fatalf("count versions after failed settlement: %v", err)
	}
	if versions != 1 {
		t.Fatalf("versions after failed settlement = %d, want 1", versions)
	}
	if _, err := database.Pool.Exec(context.Background(), `drop trigger dispatch_test_reject_identity_settlement on artifact_versions`); err != nil {
		t.Fatalf("drop identity settlement failure trigger: %v", err)
	}
	if _, err := database.Pool.Exec(context.Background(), `drop function dispatch_test_reject_identity_settlement()`); err != nil {
		t.Fatalf("drop identity settlement failure function: %v", err)
	}
	if err := service.awaitRoomRecovery(context.Background(), artifactID); err != nil {
		t.Fatalf("await room recovery: %v", err)
	}
	service.settleRoom(artifactID, 0)

	waitForDocumentVersion(t, database, artifactID, 2)
	if repairs := pmdoc.BlockIDRepairCount(persistedProofTree(t, database, artifactID)); repairs != 0 {
		t.Fatalf("retry persisted document with %d unstamped blocks", repairs)
	}
}

func TestFailedSettlementDoesNotDiscardSuccessorRoomUpdate(t *testing.T) {
	database := openTestStore(t)
	artifactID := createDocument(t, database, "before")
	seedUnidentifiedProofDocument(t, database, artifactID, "before")
	entered := make(chan struct{})
	release := make(chan struct{})
	persistence := &blockingFirstAppendStore{
		VersionedStore: NewPgVersioned(database),
		entered:        entered,
		release:        release,
	}
	service := New(Deps{
		Store:       database,
		Persistence: persistence,
		Events:      events.NewBroker(),
		Settle:      time.Hour,
	})
	t.Cleanup(func() {
		if persistence.released.CompareAndSwap(false, true) {
			close(release)
		}
	})
	t.Cleanup(func() {
		if err := service.Shutdown(context.Background()); err != nil {
			t.Errorf("shutdown document service: %v", err)
		}
	})
	if _, err := database.Pool.Exec(context.Background(), `
		create function dispatch_test_reject_successor_settlement() returns trigger language plpgsql as $$
		begin
			if new.number = 2 then
				raise exception 'reject successor settlement version';
			end if;
			return new;
		end;
		$$
	`); err != nil {
		t.Fatalf("create successor settlement failure function: %v", err)
	}
	if _, err := database.Pool.Exec(context.Background(), `
		create trigger dispatch_test_reject_successor_settlement
		before insert on artifact_versions for each row
		execute function dispatch_test_reject_successor_settlement()
	`); err != nil {
		t.Fatalf("create successor settlement failure trigger: %v", err)
	}
	t.Cleanup(func() {
		_, _ = database.Pool.Exec(context.Background(), `drop trigger if exists dispatch_test_reject_successor_settlement on artifact_versions`)
		_, _ = database.Pool.Exec(context.Background(), `drop function if exists dispatch_test_reject_successor_settlement()`)
	})

	editLiveTree(t, service, artifactID, replaceRun("before", "after"))
	<-entered
	state := service.room(artifactID)
	state.mu.Lock()
	generation := state.gen
	state.mu.Unlock()
	service.settleRoom(artifactID, generation)
	waitForRoomFailure(t, service, artifactID)
	if persistence.released.CompareAndSwap(false, true) {
		close(release)
	}
	if err := service.awaitRoomRecovery(context.Background(), artifactID); err != nil {
		t.Fatalf("evict failed room: %v", err)
	}

	editLiveTree(t, service, artifactID, replaceRun("after", "successor"))
	waitForPersistedProofText(t, database, artifactID, "successor\n")
}

func TestBackfillStampsClosedIssueDocument(t *testing.T) {
	database := openTestStore(t)
	artifactID := createDocument(t, database, "before")
	seedUnidentifiedProofDocument(t, database, artifactID, "before")
	service := New(Deps{Store: database, Events: events.NewBroker(), Settle: time.Hour})
	t.Cleanup(func() {
		if err := service.Shutdown(context.Background()); err != nil {
			t.Errorf("shutdown document service: %v", err)
		}
	})
	if _, err := database.Pool.Exec(context.Background(), `update issues set closed_at = now() where key = 'DOC-1'`); err != nil {
		t.Fatalf("close document issue: %v", err)
	}
	service.SetIssueClosed("DOC-1", true)

	reports, err := service.BackfillBlockIDs(context.Background())
	if err != nil {
		t.Fatalf("backfill closed document: %v", err)
	}
	if len(reports) != 1 {
		t.Fatalf("backfill reports = %#v, want one document", reports)
	}
	if reports[0].Stamped != 1 {
		t.Fatalf("closed document stamped = %d, want 1", reports[0].Stamped)
	}
	if repairs := pmdoc.BlockIDRepairCount(persistedProofTree(t, database, artifactID)); repairs != 0 {
		t.Fatalf("backfill persisted document with %d unstamped blocks", repairs)
	}
}

func TestBackfillDoesNotBypassClosedIssueForConcurrentApplyOps(t *testing.T) {
	database := openTestStore(t)
	artifactID := createDocument(t, database, "before")
	seedUnidentifiedProofDocument(t, database, artifactID, "before")
	service := New(Deps{Store: database, Events: events.NewBroker(), Settle: time.Hour})
	t.Cleanup(func() {
		if err := service.Shutdown(context.Background()); err != nil {
			t.Errorf("shutdown document service: %v", err)
		}
	})
	if _, err := database.Pool.Exec(context.Background(), `update issues set closed_at = now() where key = 'DOC-1'`); err != nil {
		t.Fatalf("close document issue: %v", err)
	}
	service.SetIssueClosed("DOC-1", true)

	entered := make(chan struct{})
	release := make(chan struct{})
	var blocked atomic.Bool
	var released atomic.Bool
	t.Cleanup(func() {
		if released.CompareAndSwap(false, true) {
			close(release)
		}
	})
	service.srv.OnInject = func(ctx context.Context, info ygws.InjectInfo) error {
		if blocked.CompareAndSwap(false, true) {
			close(entered)
			<-release
		}
		return service.allowInject(ctx, info)
	}
	type result struct {
		err error
	}
	backfill := make(chan result, 1)
	go func() {
		_, err := service.BackfillBlockIDs(context.Background())
		backfill <- result{err: err}
	}()
	<-entered
	_, applyErr := service.ApplyOps(context.Background(), artifactID, []model.EditOp{{
		Op:   "replace",
		Find: "before",
		With: "foreign",
	}}, model.Actor{Kind: "session", ID: "session-0123456789abcdef"})
	if released.CompareAndSwap(false, true) {
		close(release)
	}
	backfillResult := <-backfill
	if backfillResult.err != nil {
		t.Fatalf("backfill document: %v", backfillResult.err)
	}
	if !errors.Is(applyErr, ErrIssueClosed) {
		t.Fatalf("concurrent edit during closed-document backfill = %v, want ErrIssueClosed", applyErr)
	}
}

func TestBackfillReportsStoppingDocumentAsSkipped(t *testing.T) {
	database := openTestStore(t)
	artifactID := createDocument(t, database, "before")
	service := New(Deps{Store: database, Events: events.NewBroker(), Settle: time.Hour})
	t.Cleanup(func() {
		if err := service.Shutdown(context.Background()); err != nil {
			t.Errorf("shutdown document service: %v", err)
		}
	})
	service.stopping.Store(true)

	reports, err := service.BackfillBlockIDs(context.Background())
	if err != nil {
		t.Fatalf("backfill stopping service: %v", err)
	}
	if len(reports) != 1 {
		t.Fatalf("backfill reports = %#v, want one document", reports)
	}
	if reports[0].ArtifactID != artifactID {
		t.Fatalf("backfill artifact = %q, want %q", reports[0].ArtifactID, artifactID)
	}
	if reports[0].Skipped != "service stopping" {
		t.Fatalf("backfill skip = %q, want service stopping", reports[0].Skipped)
	}
}

func TestBackfillReportsDocumentPersistenceFailure(t *testing.T) {
	database := openTestStore(t)
	artifactID := createDocument(t, database, "before")
	seedUnidentifiedProofDocument(t, database, artifactID, "before")
	service := New(Deps{
		Store: database,
		Persistence: failingBackfillVersionedStore{
			VersionedStore: NewPgVersioned(database),
			err:            errors.New("persist identity update"),
		},
		Events: events.NewBroker(),
		Settle: time.Hour,
	})
	t.Cleanup(func() {
		if err := service.Shutdown(context.Background()); err != nil {
			t.Errorf("shutdown document service: %v", err)
		}
	})

	reports, err := service.BackfillBlockIDs(context.Background())
	if err != nil {
		t.Fatalf("backfill persistence failure: %v", err)
	}
	if len(reports) != 1 {
		t.Fatalf("backfill reports = %#v, want one document", reports)
	}
	if !errors.Is(reports[0].Err, service.persistence.(failingBackfillVersionedStore).err) {
		t.Fatalf("backfill error = %v, want persistence error", reports[0].Err)
	}
	if repairs := pmdoc.BlockIDRepairCount(persistedProofTree(t, database, artifactID)); repairs == 0 {
		t.Fatal("failed backfill persisted identity updates")
	}
}

func TestSettleWritesArtifactOwnedEventForUnlinkedDocument(t *testing.T) {
	database := openTestStore(t)
	artifactID := createProjectDocument(t, database, "before")
	broker := events.NewBroker()
	service := New(Deps{Store: database, Events: broker, Settle: 20 * time.Millisecond})
	t.Cleanup(func() {
		if err := service.Shutdown(context.Background()); err != nil {
			t.Errorf("shutdown document service: %v", err)
		}
	})
	seedServiceText(t, service, artifactID, "before")
	editLiveTree(t, service, artifactID, replaceRun("before", "after"))
	waitForDocumentVersion(t, database, artifactID, 2)

	var eventArtifactID string
	var eventIssueKey *string
	var lastSeq int
	if err := database.Pool.QueryRow(context.Background(), `
		select e.artifact_id::text, e.issue_key, a.last_seq
		from events e join artifacts a on a.id = e.artifact_id
		where e.artifact_id = $1 and e.type = 'artifact.version'
	`, artifactID).Scan(&eventArtifactID, &eventIssueKey, &lastSeq); err != nil {
		t.Fatalf("read project document event: %v", err)
	}
	if eventArtifactID != artifactID || eventIssueKey != nil || lastSeq != 1 {
		t.Fatalf("project document event artifact=%q issue=%#v last_seq=%d", eventArtifactID, eventIssueKey, lastSeq)
	}
}

func TestUnlinkedDocumentIsAlwaysOpen(t *testing.T) {
	database := openTestStore(t)
	artifactID := createProjectDocument(t, database, "before")
	service := New(Deps{Store: database, Events: events.NewBroker(), Settle: time.Hour})
	t.Cleanup(func() {
		if err := service.Shutdown(context.Background()); err != nil {
			t.Errorf("shutdown document service: %v", err)
		}
	})
	seedServiceText(t, service, artifactID, "before")
	open, err := service.issueOpen(context.Background(), artifactID)
	if err != nil {
		t.Fatalf("check unlinked document open: %v", err)
	}
	if !open {
		t.Fatal("unlinked document is closed")
	}
	version, err := service.NamedVersion(context.Background(), artifactID, "checkpoint", model.Actor{Kind: "user", ID: "alice"})
	if err != nil {
		t.Fatalf("name unlinked document version: %v", err)
	}
	if !version.Named || version.Summary == nil || *version.Summary != "checkpoint" {
		t.Fatalf("unlinked document version = %#v", version)
	}
}

func TestSettleSkipsVersionWhenTreeLeavesTheSchema(t *testing.T) {
	service, artifactID := newTestService(t)
	service.settle = 20 * time.Millisecond
	seedServiceText(t, service, artifactID, "before")
	if err := service.srv.Apply(context.Background(), artifactID, func(doc *crdt.Doc, transact func(func(*crdt.Transaction))) {
		fragment := doc.GetXmlFragment(fragmentName)
		transact(func(txn *crdt.Transaction) {
			fragment.InsertElement(txn, 0, crdt.NewYXmlElement("callout"))
		})
	}); err != nil {
		t.Fatal(err)
	}
	time.Sleep(200 * time.Millisecond)
	var versions int
	if err := service.store.Pool.QueryRow(context.Background(), `select count(*) from artifact_versions where artifact_id = $1`, artifactID).Scan(&versions); err != nil {
		t.Fatal(err)
	}
	if versions != 1 {
		t.Fatalf("settle wrote %d versions for a document outside the schema", versions)
	}
	if _, err := service.Text(context.Background(), artifactID); !errors.Is(err, ErrDocSchema) {
		t.Fatalf("Text err = %v, want ErrDocSchema", err)
	}
}

func TestEvictDropsUnconnectedRoomAndCancelsSettlement(t *testing.T) {
	service, artifactID := newTestService(t)
	const settleInterval = 50 * time.Millisecond
	service.settle = settleInterval
	seedServiceText(t, service, artifactID, "before")
	ctx := context.Background()
	tx, err := service.store.Pool.Begin(ctx)
	if err != nil {
		t.Fatalf("begin transactional edit: %v", err)
	}
	if _, err := service.ReplaceText(WithTx(ctx, tx), artifactID, "after", model.Actor{Kind: "user", ID: "alice"}); err != nil {
		t.Fatalf("replace text before eviction: %v", err)
	}
	if err := tx.Rollback(ctx); err != nil {
		t.Fatalf("roll back document mutation: %v", err)
	}
	if err := service.Evict(ctx, artifactID); err != nil {
		t.Fatalf("evict unconnected document: %v", err)
	}
	waitForNoLiveDocument(t, service, artifactID)
	if got, err := service.Text(ctx, artifactID); err != nil || got != "before\n" {
		t.Fatalf("document after eviction = %q (%v), want persisted text before", got, err)
	}
	time.Sleep(3 * settleInterval)
	var versions int
	if err := service.store.Pool.QueryRow(context.Background(), `
		select count(*) from artifact_versions where artifact_id = $1
	`, artifactID).Scan(&versions); err != nil {
		t.Fatalf("count versions after eviction: %v", err)
	}
	if versions != 1 {
		t.Fatalf("versions after eviction = %d, want 1", versions)
	}
}

func TestSettlePersistsPendingAuthorAfterDisconnect(t *testing.T) {
	service, artifactID := newTestService(t)
	seedServiceText(t, service, artifactID, "before")
	actor := model.Actor{Kind: "user", ID: "alice"}
	connectionID := service.nextConnection.Add(1)
	service.addConnection(artifactID, connectionID, actor)

	editLiveTree(t, service, artifactID, replaceRun("before", "after"))
	service.removeConnection(artifactID, connectionID)

	version := waitForDocumentVersion(t, service.store, artifactID, 2)
	if len(version.Authors) != 1 || version.Authors[0] != actor {
		t.Fatalf("settled version authors = %#v, want %v", version.Authors, actor)
	}
}

func TestSettleWritesDirtyVersionWithoutPendingAuthors(t *testing.T) {
	service, artifactID := newTestService(t)
	seedServiceText(t, service, artifactID, "before")

	editLiveTree(t, service, artifactID, replaceRun("before", "after"))

	version := waitForDocumentVersion(t, service.store, artifactID, 2)
	if len(version.Authors) != 0 {
		t.Fatalf("settled version authors = %#v, want none", version.Authors)
	}
}

func TestConnectedActorIsPendingAfterEachDocumentUpdate(t *testing.T) {
	service, artifactID := newTestService(t)
	seedServiceText(t, service, artifactID, "before")
	actor := model.Actor{Kind: "user", ID: "alice"}
	connectionID := service.nextConnection.Add(1)
	service.addConnection(artifactID, connectionID, actor)

	current := "before"
	for number, markdown := range []string{"after first update", "after second update"} {
		editLiveTree(t, service, artifactID, replaceRun(current, markdown))
		current = markdown
		version := waitForDocumentVersion(t, service.store, artifactID, number+2)
		if len(version.Authors) != 1 || version.Authors[0] != actor {
			t.Fatalf("version %d authors = %#v, want %v", number+2, version.Authors, actor)
		}
	}
}

func TestSupersededSettleGenerationDoesNotWrite(t *testing.T) {
	service, artifactID := newTestService(t)
	service.settle = time.Hour
	seedServiceText(t, service, artifactID, "before")

	editLiveTree(t, service, artifactID, replaceRun("before", "after"))
	state := service.room(artifactID)
	state.mu.Lock()
	stale := state.gen
	state.mu.Unlock()
	service.scheduleSettle(artifactID)
	state.mu.Lock()
	current := state.gen
	state.mu.Unlock()

	service.settleRoom(artifactID, stale)
	var versions int
	if err := service.store.Pool.QueryRow(context.Background(), `select count(*) from artifact_versions where artifact_id = $1`, artifactID).Scan(&versions); err != nil {
		t.Fatalf("count versions after stale settle: %v", err)
	}
	if versions != 1 {
		t.Fatalf("versions after stale settle = %d, want 1", versions)
	}
	service.settleRoom(artifactID, current)
	waitForDocumentVersion(t, service.store, artifactID, 2)
}

func TestSettleRetriesTransientVersionWriteFailure(t *testing.T) {
	service, artifactID := newTestService(t)
	service.settle = 10 * time.Millisecond
	seedServiceText(t, service, artifactID, "before")
	if _, err := service.store.Pool.Exec(context.Background(), `create sequence dispatch_test_settle_failure`); err != nil {
		t.Fatalf("create settlement failure sequence: %v", err)
	}
	if _, err := service.store.Pool.Exec(context.Background(), `
		create function dispatch_test_fail_first_settlement() returns trigger language plpgsql as $$
		begin
			if new.number = 2 and nextval('dispatch_test_settle_failure') = 1 then
				raise exception 'transient settlement write failure';
			end if;
			return new;
		end;
		$$
	`); err != nil {
		t.Fatalf("create settlement failure function: %v", err)
	}
	if _, err := service.store.Pool.Exec(context.Background(), `
		create trigger dispatch_test_fail_first_settlement
		before insert on artifact_versions for each row
		execute function dispatch_test_fail_first_settlement()
	`); err != nil {
		t.Fatalf("create settlement failure trigger: %v", err)
	}
	t.Cleanup(func() {
		_, _ = service.store.Pool.Exec(context.Background(), `drop trigger if exists dispatch_test_fail_first_settlement on artifact_versions`)
		_, _ = service.store.Pool.Exec(context.Background(), `drop function if exists dispatch_test_fail_first_settlement()`)
		_, _ = service.store.Pool.Exec(context.Background(), `drop sequence if exists dispatch_test_settle_failure`)
	})

	if _, err := service.ReplaceText(context.Background(), artifactID, "after", model.Actor{Kind: "user", ID: "alice"}); err != nil {
		t.Fatalf("change document before transient settle failure: %v", err)
	}
	time.Sleep(20 * service.settle)
	var versions int
	if err := service.store.Pool.QueryRow(context.Background(), `select count(*) from artifact_versions where artifact_id = $1`, artifactID).Scan(&versions); err != nil {
		t.Fatalf("count versions after transient settlement failure: %v", err)
	}
	if versions != 2 {
		t.Fatalf("versions after transient settlement failure = %d, want 2 after retry", versions)
	}
}

func TestSettleFailsRoomAfterPersistentVersionWriteFailure(t *testing.T) {
	service, artifactID := newTestService(t)
	service.settle = 10 * time.Millisecond
	seedServiceText(t, service, artifactID, "before")
	if _, err := service.store.Pool.Exec(context.Background(), `
		create function dispatch_test_fail_every_settlement() returns trigger language plpgsql as $$
		begin
			if new.number = 2 then
				raise exception 'persistent settlement write failure';
			end if;
			return new;
		end;
		$$
	`); err != nil {
		t.Fatalf("create persistent settlement failure function: %v", err)
	}
	if _, err := service.store.Pool.Exec(context.Background(), `
		create trigger dispatch_test_fail_every_settlement
		before insert on artifact_versions for each row
		execute function dispatch_test_fail_every_settlement()
	`); err != nil {
		t.Fatalf("create persistent settlement failure trigger: %v", err)
	}
	t.Cleanup(func() {
		_, _ = service.store.Pool.Exec(context.Background(), `drop trigger if exists dispatch_test_fail_every_settlement on artifact_versions`)
		_, _ = service.store.Pool.Exec(context.Background(), `drop function if exists dispatch_test_fail_every_settlement()`)
	})
	if _, err := service.ReplaceText(context.Background(), artifactID, "after", model.Actor{Kind: "user", ID: "alice"}); err != nil {
		t.Fatalf("change document before persistent settlement failure: %v", err)
	}
	waitForRoomFailure(t, service, artifactID)
}

func TestShutdownContextDoesNotWaitForBlockedSettlement(t *testing.T) {
	service, artifactID := newTestService(t)
	service.settle = 5 * time.Millisecond
	seedServiceText(t, service, artifactID, "before")

	blocker, err := service.store.Pool.Begin(context.Background())
	if err != nil {
		t.Fatalf("begin blocker transaction: %v", err)
	}
	defer blocker.Rollback(context.Background())
	if _, err := blocker.Exec(context.Background(), `
		select 1 from issues where key = (select issue_key from artifacts where id = $1) for update
	`, artifactID); err != nil {
		t.Fatalf("lock document issue: %v", err)
	}
	editLiveTree(t, service, artifactID, replaceRun("before", "after"))
	locked := false
	deadline := time.Now().Add(time.Second)
	for time.Now().Before(deadline) {
		var waiting int
		if err := service.store.Pool.QueryRow(context.Background(), `
			select count(*) from pg_stat_activity
			where datname = current_database() and wait_event_type = 'Lock'
		`).Scan(&waiting); err != nil {
			t.Fatalf("inspect database locks: %v", err)
		}
		if waiting > 0 {
			locked = true
			break
		}
		time.Sleep(5 * time.Millisecond)
	}
	if !locked {
		t.Fatal("settlement did not wait on the document lock")
	}
	ctx, cancel := context.WithTimeout(context.Background(), 50*time.Millisecond)
	defer cancel()
	result := make(chan error, 1)
	go func() { result <- service.Shutdown(ctx) }()
	select {
	case err := <-result:
		if !errors.Is(err, context.DeadlineExceeded) {
			t.Fatalf("shutdown error = %v, want deadline exceeded", err)
		}
	case <-time.After(time.Second):
		t.Fatal("shutdown waited on the blocked settlement")
	}
}

func TestIssueReopenRestoresLiveWrites(t *testing.T) {
	service, artifactID := newTestService(t)
	if got := service.events.SubscriberCount(); got != 0 {
		t.Fatalf("document service registered %d event subscriptions, want none", got)
	}
	seedServiceText(t, service, artifactID, "before")
	if _, err := service.store.Pool.Exec(context.Background(), `update issues set closed_at = now() where key = 'DOC-1'`); err != nil {
		t.Fatalf("close document issue: %v", err)
	}
	service.SetIssueClosed("DOC-1", true)
	if _, err := service.ReplaceText(context.Background(), artifactID, "closed", model.Actor{Kind: "user", ID: "alice"}); !errors.Is(err, ErrIssueClosed) {
		t.Fatalf("write to closed issue = %v, want ErrIssueClosed", err)
	}
	if _, err := service.store.Pool.Exec(context.Background(), `update issues set closed_at = null where key = 'DOC-1'`); err != nil {
		t.Fatalf("reopen document issue: %v", err)
	}
	service.SetIssueClosed("DOC-1", false)
	service.events.Publish(model.Event{IssueKey: new("DOC-1"), Type: "issue.closed"})
	if got := service.events.SubscriberCount(); got != 0 {
		t.Fatalf("stale issue.closed event gained %d subscriptions, want none", got)
	}
	if _, err := service.ReplaceText(context.Background(), artifactID, "reopened", model.Actor{Kind: "user", ID: "alice"}); err != nil {
		t.Fatalf("write to reopened issue: %v", err)
	}
	waitForDocumentText(t, service, artifactID, "reopened\n")
}

func TestCancelledTextLoadDoesNotQuarantineRoom(t *testing.T) {
	database := openTestStore(t)
	artifactID := createDocument(t, database, "before")
	service := New(Deps{
		Store:       database,
		Persistence: failingVersionedStore{VersionedStore: NewPgVersioned(database), respectContext: true},
		Events:      events.NewBroker(),
	})
	t.Cleanup(func() { _ = service.Shutdown(context.Background()) })
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	if _, err := service.Text(ctx, artifactID); !errors.Is(err, context.Canceled) {
		t.Fatalf("cancelled text load = %v, want context.Canceled", err)
	}
	if _, err := service.Text(context.Background(), artifactID); err != nil {
		t.Fatalf("text after cancelled load = %v, want room to remain available", err)
	}
}

func TestSettleCapturesAuthorsAtSnapshotTime(t *testing.T) {
	service, artifactID := newTestService(t)
	service.settle = time.Hour
	seedServiceText(t, service, artifactID, "before")
	first := model.Actor{Kind: "user", ID: "alice"}
	second := model.Actor{Kind: "user", ID: "bob"}
	if _, err := service.ReplaceText(context.Background(), artifactID, "after", first); err != nil {
		t.Fatalf("replace document text: %v", err)
	}
	state := service.room(artifactID)
	state.mu.Lock()
	generation := state.gen
	state.mu.Unlock()
	blocker, err := service.store.Pool.Begin(context.Background())
	if err != nil {
		t.Fatalf("begin blocker transaction: %v", err)
	}
	if _, err := blocker.Exec(context.Background(), `
		select 1 from issues where key = (select issue_key from artifacts where id = $1) for update
	`, artifactID); err != nil {
		t.Fatalf("lock document issue: %v", err)
	}
	settled := make(chan struct{})
	go func() {
		service.settleRoom(artifactID, generation)
		close(settled)
	}()
	waitForDatabaseLock(t, service.store)
	service.recordActor(artifactID, second)
	if err := blocker.Commit(context.Background()); err != nil {
		t.Fatalf("release document lock: %v", err)
	}
	select {
	case <-settled:
	case <-time.After(time.Second):
		t.Fatal("settlement did not complete")
	}
	version := waitForDocumentVersion(t, service.store, artifactID, 2)
	if len(version.Authors) != 2 || version.Authors[0] != first || version.Authors[1] != second {
		t.Fatalf("settled version authors = %#v, want %v and %v", version.Authors, first, second)
	}
}

func waitForDatabaseLock(t *testing.T, database *store.Store) {
	t.Helper()
	deadline := time.Now().Add(time.Second)
	for time.Now().Before(deadline) {
		var waiting int
		if err := database.Pool.QueryRow(context.Background(), `
			select count(*) from pg_stat_activity
			where datname = current_database() and wait_event_type = 'Lock'
		`).Scan(&waiting); err != nil {
			t.Fatalf("inspect database locks: %v", err)
		}
		if waiting > 0 {
			return
		}
		time.Sleep(5 * time.Millisecond)
	}
	t.Fatal("settlement did not wait on the document lock")
}

type failingOnceVersionedStore struct {
	VersionedStore
	loads atomic.Int32
}

func (s *failingOnceVersionedStore) Load(ctx context.Context, room string) (persistence.LoadResult, error) {
	if s.loads.Add(1) == 1 {
		return persistence.LoadResult{}, errors.New("transient load failure")
	}
	return s.VersionedStore.Load(ctx, room)
}

type failingVersionedStore struct {
	VersionedStore
	loadErr        error
	loadUpdate     []byte
	appendErr      error
	respectContext bool
}

func (s failingVersionedStore) Load(ctx context.Context, room string) (persistence.LoadResult, error) {
	if s.respectContext && ctx.Err() != nil {
		return persistence.LoadResult{}, ctx.Err()
	}
	if s.loadErr != nil {
		return persistence.LoadResult{}, s.loadErr
	}
	if s.loadUpdate != nil {
		return persistence.LoadResult{Update: s.loadUpdate, Version: 1}, nil
	}
	return s.VersionedStore.Load(ctx, room)
}

func (s failingVersionedStore) AppendUpdate(ctx context.Context, room string, update []byte) (persistence.Version, error) {
	if s.appendErr != nil {
		return 0, s.appendErr
	}
	return s.VersionedStore.AppendUpdate(ctx, room, update)
}

type failingBackfillVersionedStore struct {
	VersionedStore
	err error
}

func (s failingBackfillVersionedStore) AppendUpdateTx(_ context.Context, _ pgx.Tx, _ string, _ []byte) (persistence.Version, error) {
	return 0, s.err
}

type captureAppendUpdateTxStore struct {
	VersionedStore
	captured chan<- []byte
}

func (s captureAppendUpdateTxStore) AppendUpdateTx(ctx context.Context, tx pgx.Tx, room string, update []byte) (persistence.Version, error) {
	s.captured <- append([]byte(nil), update...)
	return s.VersionedStore.AppendUpdateTx(ctx, tx, room, update)
}

type blockingFirstAppendStore struct {
	VersionedStore
	entered  chan<- struct{}
	release  <-chan struct{}
	blocked  atomic.Bool
	released atomic.Bool
}

func (s *blockingFirstAppendStore) AppendUpdate(ctx context.Context, room string, update []byte) (persistence.Version, error) {
	if s.blocked.CompareAndSwap(false, true) {
		close(s.entered)
		<-s.release
	}
	return s.VersionedStore.AppendUpdate(ctx, room, update)
}
func seedServiceText(t *testing.T, service *Service, artifactID, markdown string) {
	t.Helper()
	tx, err := service.store.Pool.Begin(context.Background())
	if err != nil {
		t.Fatalf("begin seed text: %v", err)
	}
	defer tx.Rollback(context.Background())
	if _, err := service.SeedText(context.Background(), tx, artifactID, markdown); err != nil {
		t.Fatalf("seed service text: %v", err)
	}
	if err := tx.Commit(context.Background()); err != nil {
		t.Fatalf("commit seed text: %v", err)
	}
}

func seedUnidentifiedProofDocument(t *testing.T, database *store.Store, artifactID, markdown string) {
	t.Helper()
	tree, err := pmdoc.Parse(markdown)
	if err != nil {
		t.Fatalf("parse unidentified document: %v", err)
	}
	removeBlockIDs(tree)
	doc := crdt.New()
	fragment := doc.GetXmlFragment(fragmentName)
	doc.Transact(func(txn *crdt.Transaction) {
		if err := pmdoc.Update(txn, fragment, tree); err != nil {
			t.Errorf("write unidentified document: %v", err)
		}
	})
	tx, err := database.Pool.Begin(context.Background())
	if err != nil {
		t.Fatalf("begin unidentified document: %v", err)
	}
	defer tx.Rollback(context.Background())
	if _, err := NewPgVersioned(database).AppendUpdateTx(context.Background(), tx, artifactID, crdt.EncodeStateAsUpdateV1(doc, nil)); err != nil {
		t.Fatalf("append unidentified document: %v", err)
	}
	if err := tx.Commit(context.Background()); err != nil {
		t.Fatalf("commit unidentified document: %v", err)
	}
}

func removeBlockIDs(node *pmdoc.Node) {
	delete(node.Attrs, pmdoc.BlockIDAttr)
	for _, child := range node.Children {
		removeBlockIDs(child)
	}
}

func persistedProofTree(t *testing.T, database *store.Store, artifactID string) *pmdoc.Node {
	t.Helper()
	loaded, err := NewPgVersioned(database).Load(context.Background(), artifactID)
	if err != nil {
		t.Fatalf("load persisted document: %v", err)
	}
	doc := crdt.New()
	if err := crdt.ApplyUpdateV1(doc, loaded.Update, nil); err != nil {
		t.Fatalf("decode persisted document: %v", err)
	}
	tree, err := treeOf(doc)
	if err != nil {
		t.Fatalf("read persisted document: %v", err)
	}
	return tree
}

func waitForPersistedProofText(t *testing.T, database *store.Store, artifactID, want string) {
	t.Helper()
	deadline := time.Now().Add(time.Second)
	for time.Now().Before(deadline) {
		markdown, err := renderTree(persistedProofTree(t, database, artifactID))
		if err == nil && markdown == want {
			return
		}
		time.Sleep(5 * time.Millisecond)
	}
	markdown, err := renderTree(persistedProofTree(t, database, artifactID))
	t.Fatalf("persisted document = %q (%v), want %q", markdown, err, want)
}

// editLiveTree writes a browser-style tree change through the live Yjs room.
func editLiveTree(t *testing.T, service *Service, artifactID string, edit func(*pmdoc.Node) *pmdoc.Node) {
	t.Helper()
	err := service.srv.Apply(context.Background(), artifactID, func(doc *crdt.Doc, transact func(func(*crdt.Transaction))) {
		fragment := doc.GetXmlFragment(fragmentName)
		tree, err := pmdoc.Read(fragment)
		if err != nil {
			t.Fatalf("read live tree: %v", err)
		}
		want := edit(tree)
		transact(func(txn *crdt.Transaction) {
			if err := pmdoc.Update(txn, fragment, want); err != nil {
				t.Errorf("update live tree: %v", err)
			}
		})
	})
	if err != nil {
		t.Fatalf("apply browser edit: %v", err)
	}
}

// liveTree reads the resident tree under the room lock.
func liveTree(t *testing.T, service *Service, artifactID string) *pmdoc.Node {
	t.Helper()
	var tree *pmdoc.Node
	err := service.srv.Apply(context.Background(), artifactID, func(doc *crdt.Doc, _ func(func(*crdt.Transaction))) {
		var readErr error
		tree, readErr = pmdoc.Read(doc.GetXmlFragment(fragmentName))
		if readErr != nil {
			t.Errorf("read live tree: %v", readErr)
		}
	})
	if err != nil && !errors.Is(err, ygws.ErrNoChanges) {
		t.Fatalf("read live document: %v", err)
	}
	return tree
}

// replaceRun rewrites the text of the first run containing find, keeping its marks.
func replaceRun(find, with string) func(*pmdoc.Node) *pmdoc.Node {
	return func(tree *pmdoc.Node) *pmdoc.Node {
		var visit func(*pmdoc.Node) bool
		visit = func(node *pmdoc.Node) bool {
			if node.Type == "text" && strings.Contains(node.Text, find) {
				node.Text = strings.Replace(node.Text, find, with, 1)
				return true
			}
			for _, child := range node.Children {
				if visit(child) {
					return true
				}
			}
			return false
		}
		visit(tree)
		return tree
	}
}

// deleteRun removes the first text run with exactly text, preserving ProseMirror's no-empty-text invariant.
func deleteRun(text string) func(*pmdoc.Node) *pmdoc.Node {
	return func(tree *pmdoc.Node) *pmdoc.Node {
		var visit func(*pmdoc.Node) bool
		visit = func(node *pmdoc.Node) bool {
			for index, child := range node.Children {
				if child.Type == "text" && child.Text == text {
					node.Children = append(node.Children[:index], node.Children[index+1:]...)
					return true
				}
				if visit(child) {
					return true
				}
			}
			return false
		}
		visit(tree)
		return tree
	}
}
func waitForRoomClosed(t *testing.T, service *Service, artifactID string) {
	t.Helper()
	deadline := time.Now().Add(time.Second)
	for time.Now().Before(deadline) {
		if service.roomClosed(artifactID) {
			return
		}
		time.Sleep(5 * time.Millisecond)
	}
	t.Fatal("document room did not close after issue event")
}
func waitForRoomFailure(t *testing.T, service *Service, artifactID string) {
	t.Helper()
	deadline := time.Now().Add(3 * time.Second)
	for time.Now().Before(deadline) {
		if service.roomFailure(artifactID) != nil {
			return
		}
		time.Sleep(5 * time.Millisecond)
	}
	t.Fatal("document room did not become unavailable after append failure")
}

func waitForNoLiveDocument(t *testing.T, service *Service, artifactID string) {
	t.Helper()
	deadline := time.Now().Add(time.Second)
	for time.Now().Before(deadline) {
		if service.srv.GetDoc(artifactID) == nil {
			return
		}
		time.Sleep(5 * time.Millisecond)
	}
	t.Fatal("document room remained resident after closure")
}
func waitForDocumentText(t *testing.T, service *Service, artifactID, want string) {
	t.Helper()
	deadline := time.Now().Add(time.Second)
	for time.Now().Before(deadline) {
		got, err := service.Text(context.Background(), artifactID)
		if err == nil && got == want {
			return
		}
		time.Sleep(5 * time.Millisecond)
	}
	got, err := service.Text(context.Background(), artifactID)
	t.Fatalf("document text = %q (%v), want %q", got, err, want)
}

func waitForDocumentVersion(t *testing.T, database *store.Store, artifactID string, number int) model.Version {
	t.Helper()
	deadline := time.Now().Add(time.Second)
	for time.Now().Before(deadline) {
		version, ok, err := loadDocumentVersion(context.Background(), database, artifactID, number)
		if err != nil {
			t.Fatalf("load document version: %v", err)
		}
		if ok {
			return version
		}
		time.Sleep(5 * time.Millisecond)
	}
	t.Fatal("document version did not settle")
	return model.Version{}
}

func loadDocumentVersion(ctx context.Context, database *store.Store, artifactID string, number int) (model.Version, bool, error) {
	var version model.Version
	var authors []byte
	err := database.Pool.QueryRow(ctx, `
		select number, named, summary, authors, created_at
		from artifact_versions where artifact_id = $1 and number = $2
	`, artifactID, number).Scan(&version.Number, &version.Named, &version.Summary, &authors, &version.CreatedAt)
	if errors.Is(err, pgx.ErrNoRows) {
		return model.Version{}, false, nil
	}
	if err != nil {
		return model.Version{}, false, err
	}
	if err := json.Unmarshal(authors, &version.Authors); err != nil {
		return model.Version{}, false, err
	}
	return version, true, nil
}
