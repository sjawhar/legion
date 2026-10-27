package docs

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"testing"

	"github.com/jackc/pgx/v5"
	"github.com/reearth/ygo/crdt"
	"github.com/reearth/ygo/persistence"

	"github.com/sjawhar/envoy/internal/dispatch/model"
	"github.com/sjawhar/envoy/internal/dispatch/pmdoc"
	"github.com/sjawhar/envoy/internal/dispatch/store"
	"github.com/sjawhar/envoy/internal/dispatch/store/storetest"
)

// rebuildUpdateItems is the number of paragraphs each update of oversizedHistory adds; eleven of
// them exceed ygo's cap of 1<<20 items in one update.
const rebuildUpdateItems = 100_000

// oversizedHistory is eleven updates one client made in turn, each prepending rebuildUpdateItems
// empty paragraphs. Each is accepted on its own: AppendUpdate applies it to an empty document,
// where the first integrates and each later one parks whole behind the clock gap the earlier ones
// leave, within ygo's pending cap of 100,000 items. Merged, they are one client's 1.1 million
// items, which ygo refuses from that client's header before it integrates any (sjawhar/ygo
// v1.51.3-sami.1, crdt/update.go:545-558 decodeAndPark), so the history cannot load and no load
// of it walks a million items first. Each paragraph is prepended because ygo finds an insert
// position by walking the fragment's children from its start (crdt/yxml.go leftChildAt):
// appending takes quadratic time, about 30 s for 100,000.
func oversizedHistory(t *testing.T) [][]byte {
	t.Helper()
	doc := crdt.New()
	fragment := doc.GetXmlFragment(fragmentName)
	updates := make([][]byte, 11)
	for index := range updates {
		before := doc.StateVector()
		doc.Transact(func(transaction *crdt.Transaction) {
			for range rebuildUpdateItems {
				fragment.InsertElement(transaction, 0, crdt.NewYXmlElement("paragraph"))
			}
		})
		updates[index] = crdt.EncodeStateAsUpdateV1(doc, before)
	}
	return updates
}

// rebuildDocument rebuilds the way the route does: inside a transaction joined with Service.Join,
// committed only when the rebuild succeeds.
func rebuildDocument(t *testing.T, service *Service, artifactID string, markdown *string) (RebuildReport, VersionResult, error) {
	t.Helper()
	ctx := context.Background()
	tx, err := service.store.Pool.Begin(ctx)
	if err != nil {
		t.Fatalf("begin rebuild: %v", err)
	}
	defer tx.Rollback(ctx)
	joined, ledger := service.Join(ctx, tx)
	defer ledger.Discard()
	report, written, err := service.RebuildDocument(joined, artifactID, markdown, model.Actor{Kind: "user", ID: "alice"})
	if err != nil {
		return RebuildReport{}, VersionResult{}, err
	}
	if err := ledger.Commit(ctx); err != nil {
		t.Fatalf("commit rebuild: %v", err)
	}
	return report, written, nil
}

// A history that exceeds ygo's merged-update item cap cannot load. Rebuilding it preserves the
// latest saved version, advances the durable cursor, and leaves the artifact's version history.
// Until the rebuild's transaction ends its room refuses loads and a second rebuild, which would
// read the old history the transaction has not yet replaced; once it commits, the room opens.
func TestRebuildDocumentRestoresDocumentThatCannotLoad(t *testing.T) {
	database := storetest.Open(t)
	artifactID := createDocument(t, database, "before\n")
	persistence := NewPgVersioned(database)
	for _, update := range oversizedHistory(t) {
		if _, err := persistence.AppendUpdate(context.Background(), artifactID, update); err != nil {
			t.Fatalf("append accepted history update: %v", err)
		}
	}

	service := newSchemaRepairService(database)
	t.Cleanup(func() {
		if err := service.Shutdown(context.Background()); err != nil {
			t.Errorf("shutdown rebuilt service: %v", err)
		}
	})
	// The read meets the history that does not decode itself, the state a rebuild repairs.
	if _, err := service.Text(context.Background(), artifactID); !errors.Is(err, ErrDocumentUnloadable) {
		t.Fatalf("read over-cap history: %v, want ErrDocumentUnloadable", err)
	}
	ctx := context.Background()
	tx, err := database.Pool.Begin(ctx)
	if err != nil {
		t.Fatalf("begin rebuild: %v", err)
	}
	defer tx.Rollback(ctx)
	joined, ledger := service.Join(ctx, tx)
	defer ledger.Discard()
	report, written, err := service.RebuildDocument(joined, artifactID, nil, model.Actor{Kind: "user", ID: "alice"})
	if err != nil {
		t.Fatalf("rebuild document: %v", err)
	}
	if report.SourceVersion != 1 || report.RemovedUpdates != 11 || report.Head != 12 || written.Wrote {
		t.Fatalf("rebuild report = %#v (%#v), want source version 1, 11 updates removed, head 12, no version", report, written)
	}
	if _, _, err := rebuildDocument(t, service, artifactID, nil); !errors.Is(err, ErrDocumentLive) {
		t.Fatalf("second rebuild before the first commits: %v, want ErrDocumentLive", err)
	}
	if err := service.warmLiveDocument(ctx, artifactID); !errors.Is(err, ErrServiceUnavailable) {
		t.Fatalf("load before the rebuild commits: %v, want ErrServiceUnavailable", err)
	}
	if err := ledger.Commit(ctx); err != nil {
		t.Fatalf("commit rebuild: %v", err)
	}
	if text, err := service.Text(context.Background(), artifactID); err != nil || text != "before\n" {
		t.Fatalf("read rebuilt document: %q (%v), want latest version markdown", text, err)
	}
	var updates, versions int
	if err := database.Pool.QueryRow(context.Background(), `select count(*) from doc_updates where artifact_id = $1`, artifactID).Scan(&updates); err != nil {
		t.Fatalf("count rebuilt updates: %v", err)
	}
	if err := database.Pool.QueryRow(context.Background(), `select count(*) from artifact_versions where artifact_id = $1`, artifactID).Scan(&versions); err != nil {
		t.Fatalf("count artifact versions after rebuild: %v", err)
	}
	if updates != 1 || versions != 1 {
		t.Fatalf("rebuild left %d updates and %d artifact versions, want 1 and 1", updates, versions)
	}
	if err := service.warmLiveDocument(ctx, artifactID); err != nil {
		t.Fatalf("load after the rebuild commits: %v", err)
	}
}

// A resident room remains live even when no writer holds its advisory lock, so the server registry
// refuses rebuilding it without changing durable history.
func TestRebuildDocumentRefusesALiveRoom(t *testing.T) {
	service, artifactID := newTestService(t)
	seedServiceText(t, service, artifactID, "before\n")
	if err := service.warmLiveDocument(context.Background(), artifactID); err != nil {
		t.Fatalf("load live room: %v", err)
	}
	var before int
	if err := service.store.Pool.QueryRow(context.Background(), `select count(*) from doc_updates where artifact_id = $1`, artifactID).Scan(&before); err != nil {
		t.Fatalf("count updates before rebuild: %v", err)
	}
	if _, _, err := rebuildDocument(t, service, artifactID, nil); !errors.Is(err, ErrDocumentLive) {
		t.Fatalf("rebuild resident room: %v, want ErrDocumentLive", err)
	}
	var after int
	if err := service.store.Pool.QueryRow(context.Background(), `select count(*) from doc_updates where artifact_id = $1`, artifactID).Scan(&after); err != nil {
		t.Fatalf("count updates after refused rebuild: %v", err)
	}
	if after != before {
		t.Fatalf("resident-room rebuild changed doc_updates from %d to %d", before, after)
	}
}

// rebuildCaptureStore counts rebuilds, and while invalid loads the stored document once, of either
// kind (a room's or a cold read's), as a history that does not decode at the head it stands at.
type rebuildCaptureStore struct {
	VersionedStore
	invalid  bool
	rebuilds int
}

func (s *rebuildCaptureStore) Load(ctx context.Context, room string) (persistence.LoadResult, error) {
	if s.invalid {
		s.invalid = false
		head, err := s.VersionedStore.Head(ctx, room)
		if err != nil {
			return persistence.LoadResult{}, err
		}
		return persistence.LoadResult{Update: []byte{0xff}, Version: head}, nil
	}
	return s.VersionedStore.Load(ctx, room)
}

func (s *rebuildCaptureStore) LoadDocument(ctx context.Context, room string) (LoadedDocument, error) {
	if s.invalid {
		s.invalid = false
		if err := crdt.ApplyUpdateV1(newDocumentCopy(), []byte{0xff}, nil); err != nil {
			return LoadedDocument{}, fmt.Errorf("%w: decode live document: %w", ErrDocumentUnloadable, err)
		}
	}
	return s.VersionedStore.LoadDocument(ctx, room)
}

func (s *rebuildCaptureStore) RebuildTx(ctx context.Context, tx pgx.Tx, room string, loaded persistence.Version, seed []byte) (RebuildReport, error) {
	s.rebuilds++
	return s.VersionedStore.RebuildTx(ctx, tx, room, loaded, seed)
}

func TestRebuildDocumentWritesThroughItsInjectedPersistence(t *testing.T) {
	database := storetest.Open(t)
	artifactID := createDocument(t, database, "before\n")
	persist := &rebuildCaptureStore{
		VersionedStore: NewPgVersioned(database),
		invalid:        true,
	}
	service := New(Deps{Store: database, Persistence: persist})
	replacement := "replacement\n"
	if _, _, err := rebuildDocument(t, service, artifactID, &replacement); err != nil {
		t.Fatalf("rebuild document: %v", err)
	}
	if persist.rebuilds != 1 {
		t.Fatalf("injected persistence rebuild calls = %d, want 1", persist.rebuilds)
	}
}

// A rebuild from the latest version restores the server's own rendering of the document, which it
// reads back as every stored rendering is read, counting no elements: a version of 16,385
// paragraphs, past the 65,536 elements one write may make - stored before that bound, or grown past
// it by browser edits - rebuilds whole.
func TestARebuildFromTheLatestVersionRestoresItWhateverItWeighs(t *testing.T) {
	database := storetest.Open(t)
	artifactID := createDocument(t, database, "before\n")
	heavy := strings.Repeat("a\n\n", 16_384) + "a\n"
	if size := pmdoc.MeasureDocument(heavy); !size.TooHeavy() {
		t.Fatalf("the version measures %+v, want past the element limit", size)
	}
	if _, err := database.Pool.Exec(context.Background(), `update artifact_versions set markdown = $2 where artifact_id = $1`, artifactID, heavy); err != nil {
		t.Fatalf("store a version past the element limit: %v", err)
	}
	service := New(Deps{Store: database, Persistence: &rebuildCaptureStore{VersionedStore: NewPgVersioned(database), invalid: true}})
	if _, _, err := rebuildDocument(t, service, artifactID, nil); err != nil {
		t.Fatalf("rebuild from a version past the element limit: %v", err)
	}
	if text, err := service.Text(context.Background(), artifactID); err != nil || text != heavy {
		t.Fatalf("the rebuilt document reads %d bytes (%v), want the version's %d", len(text), err, len(heavy))
	}
}

// A rebuild whose seed holds more items than ygo decodes in one update (maxUpdateItems) is refused
// as too large to store, as an append of such an update is (AppendUpdateTx), not failed as a fault:
// the route answers it 413 CAP_EXCEEDED rather than 500. 160,000 empty list items make about 1.12
// million items, from 320 KB of markdown, and replace no history.
func TestARebuildWhoseSeedPassesTheItemCapIsRefusedAsTooLarge(t *testing.T) {
	database := storetest.Open(t)
	artifactID := createDocument(t, database, "before\n")
	items := strings.Repeat("-\n", 160_000)
	if _, err := database.Pool.Exec(context.Background(), `update artifact_versions set markdown = $2 where artifact_id = $1`, artifactID, items); err != nil {
		t.Fatalf("store a version past the item cap: %v", err)
	}
	history := func(t *testing.T) string {
		t.Helper()
		var rows string
		if err := database.Pool.QueryRow(context.Background(), `
			select coalesce(string_agg(version::text || ':' || md5(update), ',' order by version), '')
			from doc_updates where artifact_id = $1
		`, artifactID).Scan(&rows); err != nil {
			t.Fatalf("read document history: %v", err)
		}
		return rows
	}
	before := history(t)
	persist := &rebuildCaptureStore{VersionedStore: NewPgVersioned(database), invalid: true}
	service := New(Deps{Store: database, Persistence: persist})
	_, _, err := rebuildDocument(t, service, artifactID, nil)
	if !errors.Is(err, ErrDocumentTooLarge) || !strings.HasPrefix(err.Error(), "document too large to store: ") || !strings.Contains(err.Error(), fmt.Sprintf("exceed the %d", maxUpdateItems)) {
		t.Fatalf("rebuild from a version past ygo's item cap: %v, want ErrDocumentTooLarge naming the cap, unwrapped", err)
	}
	if after := history(t); after != before {
		t.Fatalf("the refused rebuild changed the document history from %q to %q", before, after)
	}
}

// Markdown a caller supplies to a rebuild is weighed as a new document's is (SeedText): its
// rendering, which can run longer than what was sent, may hold no more than one upload. 16,384
// headings of exactly 1 MiB, which render as 1,064,959 bytes, are refused and replace no history;
// the same headings two bytes shorter rebuild the document.
func TestARebuildFromSuppliedMarkdownIsWeighedAsANewDocument(t *testing.T) {
	database := storetest.Open(t)
	artifactID := createDocument(t, database, "before\n")
	persist := &rebuildCaptureStore{VersionedStore: NewPgVersioned(database), invalid: true}
	service := New(Deps{Store: database, Persistence: persist})
	headings := func(width int) string {
		return strings.Repeat("# "+strings.Repeat("a", width-3)+"\n", 16_384)
	}
	long := headings(64)
	if _, _, err := rebuildDocument(t, service, artifactID, &long); !errors.Is(err, ErrDocumentTooLarge) || !strings.Contains(err.Error(), "1064959 bytes") {
		t.Fatalf("rebuild from 1 MiB of headings that render as 1,064,959 bytes: %v, want ErrDocumentTooLarge naming the rendering", err)
	}
	if persist.rebuilds != 0 {
		t.Fatalf("the refused rebuild replaced the history %d times, want none", persist.rebuilds)
	}
	persist.invalid = true
	short := headings(62)
	if _, _, err := rebuildDocument(t, service, artifactID, &short); err != nil {
		t.Fatalf("rebuild from headings whose rendering fits: %v", err)
	}
	if persist.rebuilds != 1 {
		t.Fatalf("the rebuild that fits replaced the history %d times, want once", persist.rebuilds)
	}
}

// corruptStoredHistory makes artifactID's stored history one update ygo cannot decode, at the head
// it stood at, so a rebuild has something to rebuild and no room's cursor moves. It announces
// nothing, so no room of the document hears of it.
func corruptStoredHistory(t *testing.T, database *store.Store, artifactID string) {
	t.Helper()
	ctx := context.Background()
	tx, err := database.Pool.Begin(ctx)
	if err != nil {
		t.Fatalf("begin the corruption: %v", err)
	}
	defer tx.Rollback(ctx)
	for _, statement := range []string{
		`delete from doc_checkpoints where artifact_id = $1`,
		`delete from doc_updates where artifact_id = $1 and version < (select max(version) from doc_updates where artifact_id = $1)`,
		`update doc_updates set update = decode('deadbeef', 'hex') where artifact_id = $1`,
	} {
		if _, err := tx.Exec(ctx, statement, artifactID); err != nil {
			t.Fatalf("corrupt the document's history: %s: %v", statement, err)
		}
	}
	if err := tx.Commit(ctx); err != nil {
		t.Fatalf("commit the corruption: %v", err)
	}
}

// A rebuild asks every other process whether it holds a room of the document (plan D9): task B's
// room is resident, so a rebuild through task A, whose own server holds none, is refused as live
// and replaces nothing, though the stored history cannot load. Before the holds query task A
// asked only its own rooms and replaced the history under task B's.
func TestARebuildIsRefusedWhileAnotherProcessHoldsTheRoom(t *testing.T) {
	database := storetest.Open(t)
	artifactID := createDocument(t, database, "# First")
	taskA, _ := newTestServiceInstance(t, database)
	taskB, _ := newTestServiceInstance(t, storetest.SecondProcess(t, database))
	seedServiceText(t, taskA, artifactID, "before\n")
	runRoomSync(t, newRoomSync(taskA))
	runRoomSync(t, newRoomSync(taskB))
	if err := taskB.warmLiveDocument(context.Background(), artifactID); err != nil {
		t.Fatalf("load task B's room: %v", err)
	}
	corruptStoredHistory(t, database, artifactID)
	if taskA.srv.GetDoc(artifactID) != nil {
		t.Fatal("task A holds a room of the document, so its own check would refuse the rebuild")
	}
	head := storedHead(t, database, artifactID)

	_, _, err := rebuildDocument(t, taskA, artifactID, nil)
	if !errors.Is(err, ErrDocumentLive) || !strings.Contains(err.Error(), "another server process") {
		t.Fatalf("a rebuild through task A while task B holds the room = %v, want ErrDocumentLive naming another server process", err)
	}
	if after := storedHead(t, database, artifactID); after != head {
		t.Fatalf("the refused rebuild moved the head from %d to %d", head, after)
	}
}

// Two rebuilds of one document on two processes that each loaded the same history that cannot
// load: both wait on the issue's row (lockArtifactOwner) until the test lets them go, so each
// validated the old history before either replaces it. Exactly one replaces it; the other finds
// the head moved under the document's lock (RebuildTx) and is refused as live, rather than
// replacing the history the first had just rebuilt.
func TestTwoRebuildsOnTwoProcessesAtOnceReplaceTheHistoryOnce(t *testing.T) {
	database := storetest.Open(t)
	artifactID := createDocument(t, database, "# First")
	taskA, _ := newTestServiceInstance(t, database)
	taskB, _ := newTestServiceInstance(t, storetest.SecondProcess(t, database))
	seedServiceText(t, taskA, artifactID, "before\n")
	corruptStoredHistory(t, database, artifactID)
	head := storedHead(t, database, artifactID)

	ctx := context.Background()
	gate, err := database.Pool.Begin(ctx)
	if err != nil {
		t.Fatalf("begin the issue's lock: %v", err)
	}
	defer gate.Rollback(ctx)
	if _, err := gate.Exec(ctx, `
		select 1 from issues where key = (select issue_key from artifacts where id = $1) for update
	`, artifactID); err != nil {
		t.Fatalf("lock the document's issue: %v", err)
	}
	results := make(chan error, 2)
	for _, service := range []*Service{taskA, taskB} {
		go func() {
			results <- joinedWrite(service, func(ctx context.Context) error {
				_, _, err := service.RebuildDocument(ctx, artifactID, nil, model.Actor{Kind: "user", ID: "alice"})
				return err
			})
		}()
	}
	if waiting := storetest.WaitForLockWaiters(t, gate, 2, roomWait(t)); waiting < 2 {
		t.Fatalf("%d of the two rebuilds reached the issue's row", waiting)
	}
	if err := gate.Rollback(ctx); err != nil {
		t.Fatalf("release the issue's row: %v", err)
	}
	var succeeded int
	var refusals []error
	for range 2 {
		if err := <-results; err == nil {
			succeeded++
		} else {
			refusals = append(refusals, err)
		}
	}
	if succeeded != 1 || len(refusals) != 1 || !errors.Is(refusals[0], ErrDocumentLive) || !strings.Contains(refusals[0].Error(), "changed while it was being rebuilt") {
		t.Fatalf("two rebuilds at once: %d succeeded, refusals %v; want one success and one ErrDocumentLive for the moved head", succeeded, refusals)
	}
	var rows int
	if err := database.Pool.QueryRow(ctx, `select count(*) from doc_updates where artifact_id = $1`, artifactID).Scan(&rows); err != nil {
		t.Fatalf("count the rebuilt rows: %v", err)
	}
	if after := storedHead(t, database, artifactID); rows != 1 || after != head+1 {
		t.Fatalf("after the rebuilds the history holds %d rows at head %d, want the one row a single rebuild seeds at %d", rows, after, head+1)
	}
}
