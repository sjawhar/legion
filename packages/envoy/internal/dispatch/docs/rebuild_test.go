package docs

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"testing"

	"github.com/jackc/pgx/v5"
	"github.com/reearth/ygo/crdt"
	"github.com/reearth/ygo/encoding"
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

// refusedDeleteRangeUpdate is a V1 update with no structs and one delete range, which ygo refuses
// whenever it decodes it: clock + length passes 2^53-1, the highest clock a Yjs peer can make and
// the highest ygo's own state encodings carry. An earlier release took such an update, so a
// document stored before this one's ygo can hold it; AppendUpdate refuses it now.
func refusedDeleteRangeUpdate(client, clock, length uint64) []byte {
	return encoding.EncodeBytes(func(enc *encoding.Encoder) {
		enc.WriteVarUint(0) // no struct groups
		enc.WriteVarUint(1) // one client's deletions
		enc.WriteVarUint(client)
		enc.WriteVarUint(1) // one range
		enc.WriteVarUint(clock)
		enc.WriteVarUint(length)
	})
}

// A stored history ygo refuses from its clocks reads as a document that cannot load
// (ErrDocumentUnloadable), the answer every history that does not decode gets. Such a history
// does not even merge: the fold cannot apply the refused update, so the stored updates are served
// merged (stateThrough), and the merge refuses them as the apply did. A read that meets a merge
// ygo will not make must still name the document unloadable, which tells the caller the document
// is the one a person decides to rebuild, rather than a server fault it should retry past.
func TestAStoredHistoryYgoRefusesFromItsClocksCannotLoad(t *testing.T) {
	ctx := context.Background()
	database := storetest.Open(t)
	artifactID := createDocument(t, database, "before\n")
	tree, err := pmdoc.Parse("before\n")
	if err != nil {
		t.Fatalf("parse the document: %v", err)
	}
	seed := crdt.New()
	fragment := seed.GetXmlFragment(fragmentName)
	seed.Transact(func(txn *crdt.Transaction) {
		if err := pmdoc.Update(txn, fragment, tree); err != nil {
			t.Errorf("write the seed document: %v", err)
		}
	})
	persist := NewPgVersioned(database)
	if _, err := persist.AppendUpdate(ctx, artifactID, crdt.EncodeStateAsUpdateV1(seed, nil)); err != nil {
		t.Fatalf("store the document's first update: %v", err)
	}
	refused := refusedDeleteRangeUpdate(7, 1<<53, 2)
	if _, err := persist.AppendUpdate(ctx, artifactID, refused); err == nil {
		t.Fatal("the store appended an update ygo refuses")
	}
	if _, err := database.Pool.Exec(ctx, `
		insert into doc_updates (artifact_id, version, update, content_changed) values ($1, 2, $2, true)
	`, artifactID, refused); err != nil {
		t.Fatalf("store the update an earlier release took: %v", err)
	}

	service := newSchemaRepairService(database)
	t.Cleanup(func() {
		if err := service.Shutdown(context.Background()); err != nil {
			t.Errorf("shutdown service: %v", err)
		}
	})
	if _, err := service.Text(ctx, artifactID); !errors.Is(err, ErrDocumentUnloadable) {
		t.Fatalf("read the history ygo refuses: %v, want ErrDocumentUnloadable", err)
	}
}

// A resident room remains live even when no writer holds its advisory lock, so the server registry
// refuses rebuilding it without changing durable history. The refusal names what a rebuild would
// discard, names no other repair, and tells the caller to stop and ask a person.
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
	_, _, err := rebuildDocument(t, service, artifactID, nil)
	if !errors.Is(err, ErrDocumentLive) || !strings.Contains(err.Error(), "A rebuild discards every edit made after saved version ") || !strings.Contains(err.Error(), "Stop. Ask a person whether to rebuild the document") || strings.Contains(err.Error(), "either") || strings.Contains(err.Error(), "replace") || strings.Contains(err.Error(), "retry") {
		t.Fatalf("rebuild resident room: %v, want ErrDocumentLive naming what a rebuild discards and telling the caller to stop and ask a person", err)
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
// kind (a room's or a cold read's), as a history that does not decode.
type rebuildCaptureStore struct {
	VersionedStore
	invalid  bool
	rebuilds int
}

func (s *rebuildCaptureStore) Load(ctx context.Context, room string) (persistence.LoadResult, error) {
	if s.invalid {
		s.invalid = false
		return persistence.LoadResult{Update: []byte{0xff}}, nil
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

func (s *rebuildCaptureStore) RebuildTx(ctx context.Context, tx pgx.Tx, room string, seed []byte) (RebuildReport, error) {
	s.rebuilds++
	return s.VersionedStore.RebuildTx(ctx, tx, room, seed)
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

// storeLatestVersion makes markdown the latest saved version of artifactID, as a build that wrote
// it stored it, and returns a service whose next load of the document's history does not decode,
// the one state a rebuild repairs.
func storeLatestVersion(t *testing.T, database *store.Store, artifactID, markdown string) *Service {
	t.Helper()
	if _, err := database.Pool.Exec(context.Background(), `update artifact_versions set markdown = $2 where artifact_id = $1`, artifactID, markdown); err != nil {
		t.Fatalf("store the latest version: %v", err)
	}
	return New(Deps{Store: database, Persistence: &rebuildCaptureStore{VersionedStore: NewPgVersioned(database), invalid: true}})
}

// A rebuild from the latest saved version reads markdown the request did not send. A version that
// holds an element this build does not know, which a newer build wrote during a release, is newer
// content (ErrDocNewerContent), as every read of the document is, and not invalid markdown of the
// caller's. Nothing is replaced.
func TestARebuildFromAVersionHoldingNewerContentIsRefusedAsNewerContent(t *testing.T) {
	database := storetest.Open(t)
	artifactID := createDocument(t, database, "before\n")
	newer := "before\n\n:::" + UnknownElementForTest + "{#newer-1}\nWritten by a newer build.\n:::\n"
	service := storeLatestVersion(t, database, artifactID, newer)
	_, _, err := rebuildDocument(t, service, artifactID, nil)
	if !errors.Is(err, ErrDocNewerContent) || errors.Is(err, ErrInvalidMarkdown) {
		t.Fatalf("rebuild from a version holding newer content: %v, want ErrDocNewerContent and not ErrInvalidMarkdown", err)
	}
	if !strings.Contains(err.Error(), UnknownElementForTest) {
		t.Errorf("the refusal %q does not name the element %q", err, UnknownElementForTest)
	}
}

// A version that holds an element this build reads but may not create yet holds content the
// document already stored, so a rebuild from it restores that content: the seed puts back what the
// version holds and creates nothing.
func TestARebuildFromAVersionHoldingAnElementThisBuildCannotCreateRestoresIt(t *testing.T) {
	restore, err := pmdoc.UseStagedBlockForTest("future", 3)
	if err != nil {
		t.Fatalf("stage the future block as not creatable: %v", err)
	}
	t.Cleanup(restore)
	database := storetest.Open(t)
	artifactID := createDocument(t, database, "before\n")
	staged := "before\n\n:::future{#future-1}\nBody.\n:::\n"
	service := storeLatestVersion(t, database, artifactID, staged)
	if _, _, err := rebuildDocument(t, service, artifactID, nil); err != nil {
		t.Fatalf("rebuild from a version holding an element this build cannot create: %v", err)
	}
	if text, err := service.Text(context.Background(), artifactID); err != nil || text != staged {
		t.Fatalf("the rebuilt document reads %q (%v), want the version's %q", text, err, staged)
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
