package docs

import (
	"context"
	"errors"
	"fmt"
	"maps"
	"reflect"
	"slices"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/reearth/ygo/crdt"

	"github.com/sjawhar/envoy/internal/dispatch/events"
	"github.com/sjawhar/envoy/internal/dispatch/model"
	"github.com/sjawhar/envoy/internal/dispatch/pmdoc"
	"github.com/sjawhar/envoy/internal/dispatch/store"
	"github.com/sjawhar/envoy/internal/dispatch/store/storetest"
)

// proofHistory writes a document's stored history as a room stores one: each write is the update
// that takes its document from what it held to markdown's tree.
type proofHistory struct {
	t   *testing.T
	doc *crdt.Doc
}

// newProofHistory is a writer of client's updates, starting from the updates in from.
func newProofHistory(t *testing.T, client crdt.ClientID, from ...[]byte) *proofHistory {
	t.Helper()
	doc := crdt.New(crdt.WithClientID(client))
	for _, update := range from {
		if err := crdt.ApplyUpdateV1(doc, update, nil); err != nil {
			t.Fatalf("open the writer: %v", err)
		}
	}
	return &proofHistory{t: t, doc: doc}
}

func (h *proofHistory) write(markdown string) []byte {
	h.t.Helper()
	tree, err := pmdoc.Parse(markdown)
	if err != nil {
		h.t.Fatalf("parse %q: %v", markdown, err)
	}
	before := h.doc.StateVector()
	fragment := h.doc.GetXmlFragment(fragmentName)
	if err := h.doc.TransactE(func(txn *crdt.Transaction) error {
		return pmdoc.Update(txn, fragment, tree)
	}); err != nil {
		h.t.Fatalf("write %q: %v", markdown, err)
	}
	return crdt.EncodeStateAsUpdateV1(h.doc, before)
}

func appendUpdates(t *testing.T, versioned *PgVersioned, artifactID string, updates ...[]byte) {
	t.Helper()
	for index, update := range updates {
		if _, err := versioned.AppendUpdate(context.Background(), artifactID, update); err != nil {
			t.Fatalf("append update %d: %v", index, err)
		}
	}
}

func readStamp(t *testing.T, versioned *PgVersioned, artifactID string) DocumentStamp {
	t.Helper()
	stamp, err := versioned.DocumentStamp(context.Background(), artifactID)
	if err != nil {
		t.Fatalf("read the document's stamp: %v", err)
	}
	return stamp
}

func loadDocument(t *testing.T, versioned *PgVersioned, artifactID string) LoadedDocument {
	t.Helper()
	loaded, err := versioned.LoadDocument(context.Background(), artifactID)
	if err != nil {
		t.Fatalf("load the document: %v", err)
	}
	return loaded
}

// docMarkdown is what doc renders, or why it does not.
func docMarkdown(doc *crdt.Doc) string {
	markdown, err := renderDocument(doc)
	if err != nil {
		return "unrenderable: " + err.Error()
	}
	return markdown
}

// A prune deletes the updates past its target, and the next append takes the number the first of
// them had, with other content. The stamp names the row at the head, not only its number, so the
// stamp after the prune and the append differs from the one before them, and the load shows the
// new content under it.
func TestAStampChangesWhenAPruneAndAnAppendRepeatAVersionNumber(t *testing.T) {
	database := storetest.Open(t)
	versioned := NewPgVersioned(database)
	ctx := context.Background()
	artifactID := createDocument(t, database, "one")
	if empty := readStamp(t, versioned, artifactID); empty != (DocumentStamp{}) {
		t.Fatalf("a document with no stored update has the stamp %+v, want none", empty)
	}
	writer := newProofHistory(t, 101)
	one, two, three := writer.write("one\n"), writer.write("two\n"), writer.write("three\n")
	appendUpdates(t, versioned, artifactID, one, two, three)
	before := readStamp(t, versioned, artifactID)
	if before.Version != 3 || before.Row == "" {
		t.Fatalf("stamp after three updates = %+v, want version 3 and its row", before)
	}
	if err := versioned.PruneAfter(ctx, artifactID, 2, nil); err != nil {
		t.Fatalf("prune to version 2: %v", err)
	}
	if pruned := readStamp(t, versioned, artifactID); pruned.Version != 2 || pruned.Row == "" {
		t.Fatalf("stamp after the prune = %+v, want version 2 and its row", pruned)
	}
	appendUpdates(t, versioned, artifactID, newProofHistory(t, 202, one, two).write("four\n"))
	after := readStamp(t, versioned, artifactID)
	if after.Version != 3 || after == before {
		t.Fatalf("stamp after the prune and an append = %+v, want version 3 under another row than %+v", after, before)
	}
	loaded := loadDocument(t, versioned, artifactID)
	if got := docMarkdown(loaded.Doc); got != "four\n" || loaded.Stamp != after {
		t.Fatalf("load = %q under %+v, want %q under %+v", got, loaded.Stamp, "four\n", after)
	}
}

// Compaction folds the log into the head's row, which it rewrites: the stamp moves while the
// version and the document stay as they were.
func TestACompactionChangesTheStampAndKeepsTheDocument(t *testing.T) {
	database := storetest.Open(t)
	versioned := NewPgVersioned(database)
	artifactID := createDocument(t, database, "one")
	writer := newProofHistory(t, 101)
	appendUpdates(t, versioned, artifactID, writer.write("one\n"), writer.write("two\n\nmore\n"), writer.write("three\n"))
	before := readStamp(t, versioned, artifactID)
	loaded := loadDocument(t, versioned, artifactID)
	want, wantMarkdown := rawReading(t, loaded.Doc), docMarkdown(loaded.Doc)
	if loaded.Stamp != before || wantMarkdown != "three\n" {
		t.Fatalf("load before compaction = %q under %+v, want three under %+v", wantMarkdown, loaded.Stamp, before)
	}
	if deleted, err := versioned.Compact(context.Background(), artifactID, compactKeep); err != nil || deleted != 2 {
		t.Fatalf("compact: deleted %d (%v), want 2", deleted, err)
	}
	after := readStamp(t, versioned, artifactID)
	if after.Version != before.Version || after.Row == before.Row {
		t.Fatalf("stamp after compaction = %+v, want version %d under another row than %q", after, before.Version, before.Row)
	}
	compacted := loadDocument(t, versioned, artifactID)
	if got := rawReading(t, compacted.Doc); got != want || docMarkdown(compacted.Doc) != wantMarkdown || compacted.Stamp != after {
		t.Fatalf("load after compaction = %s under %+v, want %s under %+v", got, compacted.Stamp, want, after)
	}
}

// LoadDocument hands out the document the fold's read-back decoded, or, for a log of one update,
// the update decoded once: the same document a load's state decodes into, for a log of many
// updates (each pmdoc fixture rewritten into the next and back), a log of one, and a log whose
// fold parks an update and a delete, which the loaded document parks again.
func TestALoadedDocumentIsTheLoadedStateDecoded(t *testing.T) {
	database := storetest.Open(t)
	versioned := NewPgVersioned(database)
	ctx := context.Background()
	sameAsLoad := func(t *testing.T, artifactID string) *crdt.Doc {
		t.Helper()
		loaded := loadDocument(t, versioned, artifactID)
		state, err := versioned.Load(ctx, artifactID)
		if err != nil {
			t.Fatalf("load the state: %v", err)
		}
		decoded := newDocumentCopy()
		if err := crdt.ApplyUpdateV1(decoded, state.Update, nil); err != nil {
			t.Fatalf("decode the loaded state: %v", err)
		}
		if loaded.Stamp.Version != int64(state.Version) {
			t.Fatalf("loaded under version %d, the state is at %d", loaded.Stamp.Version, state.Version)
		}
		for what, pair := range map[string][2]string{
			"tree":     {rawReading(t, loaded.Doc), rawReading(t, decoded)},
			"markdown": {docMarkdown(loaded.Doc), docMarkdown(decoded)},
			"text":     {loaded.Doc.GetText("t").ToString(), decoded.GetText("t").ToString()},
			"pending":  {fmt.Sprint(loaded.Doc.PendingStats()), fmt.Sprint(decoded.PendingStats())},
		} {
			if pair[0] != pair[1] {
				t.Fatalf("the loaded document's %s is\n%s\nthe decoded state's\n%s", what, pair[0], pair[1])
			}
		}
		if !maps.Equal(loaded.Doc.StateVector(), decoded.StateVector()) {
			t.Fatalf("the loaded document's state vector is %v, the decoded state's %v", loaded.Doc.StateVector(), decoded.StateVector())
		}
		return loaded.Doc
	}

	fixtures := readHistoryFixtures(t)
	for index, fixture := range fixtures {
		t.Run("many updates/"+fixture.Name, func(t *testing.T) {
			artifactID := createDocument(t, database, "history")
			appendUpdates(t, versioned, artifactID, documentHistory(t, fixture.update, fixtures[(index+1)%len(fixtures)].update)...)
			sameAsLoad(t, artifactID)
		})
	}
	t.Run("one update", func(t *testing.T) {
		artifactID := createDocument(t, database, "history")
		appendUpdates(t, versioned, artifactID, fixtures[0].update)
		sameAsLoad(t, artifactID)
	})
	t.Run("parked updates", func(t *testing.T) {
		writer := crdt.New(crdt.WithClientID(7))
		text := writer.GetText("t")
		writer.Transact(func(txn *crdt.Transaction) { text.Insert(txn, 0, "base", nil) })
		base := crdt.EncodeStateAsUpdateV1(writer, nil)
		afterBase := writer.StateVector()
		writer.Transact(func(txn *crdt.Transaction) { text.Insert(txn, 4, " lost", nil) })
		lost := crdt.EncodeStateAsUpdateV1(writer, afterBase)
		afterLost := writer.StateVector()
		writer.Transact(func(txn *crdt.Transaction) { text.Insert(txn, 9, " kept", nil) })
		parkedInsert := crdt.EncodeStateAsUpdateV1(writer, afterLost)
		other := crdt.New(crdt.WithClientID(8))
		if err := crdt.ApplyUpdateV1(other, base, nil); err != nil {
			t.Fatalf("open the second writer: %v", err)
		}
		otherText := other.GetText("t")
		other.Transact(func(txn *crdt.Transaction) { otherText.Insert(txn, 0, "gone ", nil) })
		gone := crdt.EncodeStateAsUpdateV1(other, afterBase)
		afterGone := other.StateVector()
		other.Transact(func(txn *crdt.Transaction) { otherText.Delete(txn, 0, 5) })
		parkedDelete := crdt.EncodeStateAsUpdateV1(other, afterGone)

		artifactID := createDocument(t, database, "parked")
		appendUpdates(t, versioned, artifactID, base, parkedInsert, parkedDelete)
		doc := sameAsLoad(t, artifactID)
		if parked := doc.PendingStats(); parked.Items == 0 && parked.DeleteRanges == 0 {
			t.Fatal("the loaded document parks nothing, want the insert and the delete whose dependencies never arrived")
		}
		for _, update := range [][]byte{lost, gone} {
			if err := crdt.ApplyUpdateV1(doc, update, nil); err != nil {
				t.Fatalf("apply a missing update: %v", err)
			}
		}
		if got := doc.GetText("t").ToString(); got != "base lost kept" {
			t.Fatalf("the loaded document, once the missing updates arrive, reads %q, want %q", got, "base lost kept")
		}
	})
}

// A history whose updates merge past ygo's item cap does not decode: LoadDocument names it as the
// one state a rebuild repairs (ErrDocumentUnloadable), and so does a read of it, not wrapped in
// the unavailable service any failed room or store answers.
func TestAnUndecodableHistoryLoadsAsDocumentUnloadable(t *testing.T) {
	database := storetest.Open(t)
	artifactID := createDocument(t, database, "before\n")
	versioned := NewPgVersioned(database)
	appendUpdates(t, versioned, artifactID, oversizedHistory(t)...)
	if _, err := versioned.LoadDocument(context.Background(), artifactID); !errors.Is(err, ErrDocumentUnloadable) {
		t.Fatalf("load an over-cap history: %v, want ErrDocumentUnloadable", err)
	}
	service := newSchemaRepairService(database)
	t.Cleanup(func() { _ = service.Shutdown(context.Background()) })
	if _, err := service.Text(context.Background(), artifactID); !errors.Is(err, ErrDocumentUnloadable) || errors.Is(err, ErrServiceUnavailable) {
		t.Fatalf("read an over-cap history: %v, want ErrDocumentUnloadable and not ErrServiceUnavailable", err)
	}
}

type loadCountingStore struct {
	VersionedStore
	loads atomic.Int64
}

func (s *loadCountingStore) LoadDocument(ctx context.Context, room string) (LoadedDocument, error) {
	s.loads.Add(1)
	return s.VersionedStore.LoadDocument(ctx, room)
}

type gatedLoadStore struct {
	VersionedStore
	entered  chan struct{}
	release  chan struct{}
	loads    atomic.Int64
	canceled atomic.Bool
}

func (s *gatedLoadStore) LoadDocument(ctx context.Context, room string) (LoadedDocument, error) {
	s.loads.Add(1)
	select {
	case s.entered <- struct{}{}:
	default:
	}
	select {
	case <-s.release:
		return s.VersionedStore.LoadDocument(ctx, room)
	case <-ctx.Done():
		s.canceled.Store(true)
		return LoadedDocument{}, ctx.Err()
	}
}

type panickingLoadStore struct {
	VersionedStore
	panics atomic.Bool
}

func (s *panickingLoadStore) LoadDocument(ctx context.Context, room string) (LoadedDocument, error) {
	if s.panics.Load() {
		panic("injected cold document load panic")
	}
	return s.VersionedStore.LoadDocument(ctx, room)
}

type timeoutLoadStore struct {
	VersionedStore
	started chan struct{}
}

func (s *timeoutLoadStore) LoadDocument(ctx context.Context, room string) (LoadedDocument, error) {
	select {
	case s.started <- struct{}{}:
	default:
	}
	<-ctx.Done()
	return LoadedDocument{}, ctx.Err()
}

func newReadCacheService(t *testing.T, database *store.Store, persistence VersionedStore) *Service {
	t.Helper()
	service := New(Deps{
		Store:       database,
		Persistence: persistence,
		Events:      events.NewBroker(),
		Identity:    headerIdentity(database),
		ServerURL:   "https://dispatch.example",
		Settle:      time.Hour,
	})
	t.Cleanup(func() {
		if err := service.Shutdown(context.Background()); err != nil {
			t.Errorf("shutdown read-cache service: %v", err)
		}
	})
	return service
}

type coldReading struct {
	markdown string
	token    string
	blocks   []model.ArtifactBlock
	path     model.BlockPath
}

func readCold(t *testing.T, service *Service, artifactID string) coldReading {
	t.Helper()
	if service.srv.GetDoc(artifactID) != nil {
		t.Fatal("the document is resident, want a cold read")
	}
	markdown, err := service.Text(context.Background(), artifactID)
	if err != nil {
		t.Fatalf("read text: %v", err)
	}
	tokenMarkdown, token, err := service.TextWithToken(context.Background(), artifactID)
	if err != nil {
		t.Fatalf("read text and token: %v", err)
	}
	if tokenMarkdown != markdown {
		t.Fatalf("text = %q, text with token = %q", markdown, tokenMarkdown)
	}
	blocks, err := service.Blocks(context.Background(), artifactID)
	if err != nil {
		t.Fatalf("read blocks: %v", err)
	}
	reading := coldReading{markdown: markdown, token: token, blocks: blocks}
	if len(blocks) != 0 {
		reading.path, err = service.BlockPath(context.Background(), artifactID, blocks[0].ID)
		if err != nil {
			t.Fatalf("read first block's path: %v", err)
		}
	}
	if service.srv.GetDoc(artifactID) != nil {
		t.Fatal("a cold read loaded the document room")
	}
	return reading
}

func cacheEntry(t *testing.T, service *Service, artifactID string) *documentRead {
	t.Helper()
	service.reads.mu.Lock()
	defer service.reads.mu.Unlock()
	element := service.reads.entries[artifactID]
	if element == nil {
		return nil
	}
	return element.Value.(*documentRead)
}

func TestColdReadAfterAnEditSeesTheEdit(t *testing.T) {
	database := storetest.Open(t)
	versioned := NewPgVersioned(database)
	persistence := &loadCountingStore{VersionedStore: versioned}
	artifactID := createDocument(t, database, "before")
	service := newReadCacheService(t, database, persistence)
	seedServiceText(t, service, artifactID, "# Heading\n\nbefore\n")

	before := readCold(t, service, artifactID)
	if before.markdown != "# Heading\n\nbefore\n" || len(before.blocks) < 2 {
		t.Fatalf("before cold read = %#v, want heading and before paragraph", before)
	}
	if cacheEntry(t, service, artifactID) == nil {
		t.Fatal("the first cold read did not populate the rendering cache")
	}
	if _, err := joinedReplaceText(service, artifactID, "# Heading\n\nafter\n", model.Actor{Kind: "user", ID: "alice"}); err != nil {
		t.Fatalf("replace document text: %v", err)
	}
	if err := service.Evict(context.Background(), artifactID); err != nil {
		t.Fatalf("retire edited room: %v", err)
	}
	waitForNoLiveDocument(t, service, artifactID)

	after := readCold(t, service, artifactID)
	if after.markdown != "# Heading\n\nafter\n" || after.token == before.token {
		t.Fatalf("after cold read = %#v, want updated markdown and token distinct from %#v", after, before)
	}
	if len(after.blocks) != len(before.blocks) || after.blocks[1].Token == before.blocks[1].Token {
		t.Fatalf("after blocks = %#v, want a changed paragraph token from %#v", after.blocks, before.blocks)
	}
	if got := persistence.loads.Load(); got != 2 {
		t.Fatalf("cold document loads = %d, want one before and one after the edit", got)
	}
}

func TestRepeatedColdReadsLoadAnUnchangedDocumentOnce(t *testing.T) {
	database := storetest.Open(t)
	persistence := &loadCountingStore{VersionedStore: NewPgVersioned(database)}
	artifactID := createDocument(t, database, "before")
	service := newReadCacheService(t, database, persistence)
	seedServiceText(t, service, artifactID, "# Heading\n\nbefore\n")

	first := readCold(t, service, artifactID)
	if got := persistence.loads.Load(); got != 1 {
		t.Fatalf("loads after first cold read = %d, want 1", got)
	}
	second := readCold(t, service, artifactID)
	if got := persistence.loads.Load(); got != 1 {
		t.Fatalf("loads after repeated cold reads = %d, want 1", got)
	}
	if first.markdown != second.markdown || first.token != second.token || !slices.EqualFunc(first.blocks, second.blocks, func(a, b model.ArtifactBlock) bool {
		return a.ID == b.ID && a.Type == b.Type && a.From == b.From && a.To == b.To && a.Token == b.Token && slices.Equal(a.DescendantIDs, b.DescendantIDs)
	}) || !reflect.DeepEqual(first.path, second.path) {
		t.Fatalf("repeated cold reads changed from %#v to %#v", first, second)
	}
}

func TestCancelledColdReaderLeavesTheFoldForOtherReaders(t *testing.T) {
	database := storetest.Open(t)
	persistence := &gatedLoadStore{
		VersionedStore: NewPgVersioned(database),
		entered:        make(chan struct{}, 3),
		release:        make(chan struct{}),
	}
	artifactID := createDocument(t, database, "before")
	service := newReadCacheService(t, database, persistence)
	seedServiceText(t, service, artifactID, "before\n")

	leaderCtx, cancel := context.WithCancel(context.Background())
	t.Cleanup(cancel)
	leader := make(chan error, 1)
	go func() {
		_, err := service.Text(leaderCtx, artifactID)
		leader <- err
	}()
	select {
	case <-persistence.entered:
	case <-time.After(time.Second):
		t.Fatal("the cold fold never reached LoadDocument")
	}

	var followers sync.WaitGroup
	results := make(chan error, 2)
	for range 2 {
		followers.Add(1)
		go func() {
			defer followers.Done()
			text, err := service.Text(context.Background(), artifactID)
			if err == nil && text != "before\n" {
				err = fmt.Errorf("follower read %q, want before", text)
			}
			results <- err
		}()
	}
	// Give the followers a chance to enter DoChan while the first fold is still blocked.
	time.Sleep(20 * time.Millisecond)
	cancel()
	select {
	case err := <-leader:
		if !errors.Is(err, context.Canceled) {
			t.Fatalf("cancelled leader = %v, want context.Canceled", err)
		}
	case <-time.After(time.Second):
		t.Fatal("the cancelled leader stayed blocked on the shared fold")
	}
	close(persistence.release)
	followers.Wait()
	close(results)
	for err := range results {
		if err != nil {
			t.Fatalf("follower read: %v", err)
		}
	}
	if persistence.canceled.Load() {
		t.Fatal("the caller's cancellation reached the shared cold fold")
	}
	if got := persistence.loads.Load(); got != 1 {
		t.Fatalf("cold folds called LoadDocument %d times, want 1", got)
	}
}

func TestPanickingColdLoadFailsTheRoomAndRecovers(t *testing.T) {
	database := storetest.Open(t)
	persistence := &panickingLoadStore{VersionedStore: NewPgVersioned(database)}
	artifactID := createDocument(t, database, "before")
	service := newReadCacheService(t, database, persistence)
	seedServiceText(t, service, artifactID, "before\n")
	persistence.panics.Store(true)

	if _, err := service.Text(context.Background(), artifactID); !errors.Is(err, ErrServiceUnavailable) || !strings.Contains(err.Error(), "cold document read panicked") {
		t.Fatalf("panicking cold load = %v, want ErrServiceUnavailable naming the panic", err)
	}
	persistence.panics.Store(false)
	waitForDocumentText(t, service, artifactID, "before\n")
}

func TestColdLoadTimeoutIsServiceUnavailable(t *testing.T) {
	database := storetest.Open(t)
	persistence := &timeoutLoadStore{VersionedStore: NewPgVersioned(database), started: make(chan struct{}, 1)}
	artifactID := createDocument(t, database, "before")
	service := newReadCacheService(t, database, persistence)
	seedServiceText(t, service, artifactID, "before\n")
	oldTimeout := coldReadTimeout
	coldReadTimeout = 20 * time.Millisecond
	t.Cleanup(func() { coldReadTimeout = oldTimeout })

	ctx := context.Background()
	_, err := service.Text(ctx, artifactID)
	if !errors.Is(err, ErrServiceUnavailable) || errors.Is(err, context.DeadlineExceeded) {
		t.Fatalf("timed-out cold load = %v, want ErrServiceUnavailable and not context.DeadlineExceeded", err)
	}
	if ctx.Err() != nil {
		t.Fatalf("the live caller context ended with %v", ctx.Err())
	}
	select {
	case <-persistence.started:
	default:
		t.Fatal("the timed cold fold never reached LoadDocument")
	}
}

func TestColdReadSeesTheNewContentAfterPruneAndReappend(t *testing.T) {
	database := storetest.Open(t)
	versioned := NewPgVersioned(database)
	artifactID := createDocument(t, database, "one")
	writer := newProofHistory(t, 101)
	one, two, three := writer.write("one\n"), writer.write("two\n"), writer.write("three\n")
	appendUpdates(t, versioned, artifactID, one, two, three)
	service := newReadCacheService(t, database, versioned)
	if text, err := service.Text(context.Background(), artifactID); err != nil || text != "three\n" {
		t.Fatalf("read before prune = %q (%v), want three", text, err)
	}
	if err := versioned.PruneAfter(context.Background(), artifactID, 2, nil); err != nil {
		t.Fatalf("prune to version 2: %v", err)
	}
	four := newProofHistory(t, 202, one, two).write("four\n")
	appendUpdates(t, versioned, artifactID, four)
	if text, err := service.Text(context.Background(), artifactID); err != nil || text != "four\n" {
		t.Fatalf("read after prune and re-append = %q (%v), want four", text, err)
	}
}

func TestColdReadSeesRebuildAndDeleteBetweenReads(t *testing.T) {
	database := storetest.Open(t)
	versioned := NewPgVersioned(database)
	persistence := &rebuildCaptureStore{VersionedStore: versioned}
	artifactID := createDocument(t, database, "before\n")
	service := newReadCacheService(t, database, persistence)
	seedServiceText(t, service, artifactID, "before\n")
	if text, err := service.Text(context.Background(), artifactID); err != nil || text != "before\n" {
		t.Fatalf("read before rebuild = %q (%v), want before", text, err)
	}
	persistence.invalid = true
	replacement := "rebuilt\n"
	if _, _, err := rebuildDocument(t, service, artifactID, &replacement); err != nil {
		t.Fatalf("rebuild document: %v", err)
	}
	if text, err := service.Text(context.Background(), artifactID); err != nil || text != "rebuilt\n" {
		t.Fatalf("read after rebuild = %q (%v), want rebuilt", text, err)
	}
	if err := versioned.Delete(context.Background(), artifactID); err != nil {
		t.Fatalf("delete rebuilt document: %v", err)
	}
	if text, err := service.Text(context.Background(), artifactID); err != nil || text != "" {
		t.Fatalf("read after delete = %q (%v), want no document", text, err)
	}
	if _, err := service.BlockPath(context.Background(), artifactID, "absent"); !errors.Is(err, pmdoc.ErrTargetNotFound) {
		t.Fatalf("path after delete = %v, want ErrTargetNotFound", err)
	}
}

func TestColdReadOnOneServiceSeesAnotherServicesWrite(t *testing.T) {
	database := storetest.Open(t)
	versioned := NewPgVersioned(database)
	artifactID := createDocument(t, database, "before")
	writer := newReadCacheService(t, database, versioned)
	reader := newReadCacheService(t, database, versioned)
	seedServiceText(t, writer, artifactID, "# Heading\n\nbefore\n")
	if text, err := reader.Text(context.Background(), artifactID); err != nil || text != "# Heading\n\nbefore\n" {
		t.Fatalf("reader before write = %q (%v), want before", text, err)
	}
	if _, err := joinedReplaceText(writer, artifactID, "# Heading\n\nafter\n", model.Actor{Kind: "user", ID: "alice"}); err != nil {
		t.Fatalf("writer replace: %v", err)
	}
	if text, err := reader.Text(context.Background(), artifactID); err != nil || text != "# Heading\n\nafter\n" {
		t.Fatalf("reader after another service's write = %q (%v), want after", text, err)
	}
}

func TestColdReadDoesNotLeakCallerMutations(t *testing.T) {
	database := storetest.Open(t)
	artifactID := createDocument(t, database, "before")
	service := newReadCacheService(t, database, NewPgVersioned(database))
	seedServiceText(t, service, artifactID, "# Heading\n\nbefore\n")
	blocks, err := service.Blocks(context.Background(), artifactID)
	if err != nil || len(blocks) == 0 {
		t.Fatalf("first blocks = %#v (%v), want blocks", blocks, err)
	}
	path, err := service.BlockPath(context.Background(), artifactID, blocks[0].ID)
	if err != nil {
		t.Fatalf("first path: %v", err)
	}
	originalBlock := blocks[0]
	originalPath := append([]model.BlockPathEntry(nil), path.Path...)
	blocks[0].ID = "caller-mutated"
	blocks[0].References = model.BlockReferences{Comments: 99, Asks: 99}
	path.Path[0].ID = "caller-mutated"

	again, err := service.Blocks(context.Background(), artifactID)
	if err != nil || len(again) == 0 {
		t.Fatalf("second blocks = %#v (%v), want blocks", again, err)
	}
	if again[0].ID != originalBlock.ID || again[0].References != originalBlock.References {
		t.Fatalf("caller mutation leaked into blocks: got %#v, want %#v", again[0], originalBlock)
	}
	againPath, err := service.BlockPath(context.Background(), artifactID, originalBlock.ID)
	if err != nil {
		t.Fatalf("second path: %v", err)
	}
	if !slices.Equal(againPath.Path, originalPath) {
		t.Fatalf("caller mutation leaked into path: got %#v, want %#v", againPath.Path, originalPath)
	}
}

func cacheHas(service *Service, artifactID string) bool {
	service.reads.mu.Lock()
	defer service.reads.mu.Unlock()
	_, ok := service.reads.entries[artifactID]
	return ok
}

func TestColdReadCacheEvictsTheLeastRecentlyReadDocument(t *testing.T) {
	database := storetest.Open(t)
	persistence := &loadCountingStore{VersionedStore: NewPgVersioned(database)}
	service := newReadCacheService(t, database, persistence)
	artifactIDs := make([]string, 10)
	for index := range artifactIDs {
		artifactIDs[index] = createIssueDocument(t, database, index+1, "body\n")
		seedServiceText(t, service, artifactIDs[index], "body\n")
	}
	if _, err := service.Text(context.Background(), artifactIDs[0]); err != nil {
		t.Fatalf("measure first cache entry: %v", err)
	}
	weight := cacheEntry(t, service, artifactIDs[0]).weight
	service.reads = newDocumentReads(weight * 8)
	persistence.loads.Store(0)
	for _, artifactID := range artifactIDs[:8] {
		if _, err := service.Text(context.Background(), artifactID); err != nil {
			t.Fatalf("fill read %s: %v", artifactID, err)
		}
		if service.reads.bytes > service.reads.budget {
			t.Fatalf("cache bytes = %d, budget = %d after %s", service.reads.bytes, service.reads.budget, artifactID)
		}
	}
	if _, err := service.Text(context.Background(), artifactIDs[0]); err != nil {
		t.Fatalf("refresh most-recent document: %v", err)
	}
	if _, err := service.Text(context.Background(), artifactIDs[8]); err != nil {
		t.Fatalf("read ninth document: %v", err)
	}
	if service.reads.bytes > service.reads.budget {
		t.Fatalf("cache bytes = %d, budget = %d after eviction", service.reads.bytes, service.reads.budget)
	}
	if !cacheHas(service, artifactIDs[0]) || cacheHas(service, artifactIDs[1]) || !cacheHas(service, artifactIDs[8]) {
		t.Fatalf("cache membership after LRU eviction has doc0=%t doc1=%t doc8=%t, want true false true", cacheHas(service, artifactIDs[0]), cacheHas(service, artifactIDs[1]), cacheHas(service, artifactIDs[8]))
	}
	loadsBefore := persistence.loads.Load()
	if _, err := service.Text(context.Background(), artifactIDs[0]); err != nil {
		t.Fatalf("read retained document: %v", err)
	}
	if got := persistence.loads.Load(); got != loadsBefore {
		t.Fatalf("retained document reloaded: loads = %d, want %d", got, loadsBefore)
	}
	if _, err := service.Text(context.Background(), artifactIDs[1]); err != nil {
		t.Fatalf("read evicted document: %v", err)
	}
	if got := persistence.loads.Load(); got != loadsBefore+1 {
		t.Fatalf("evicted document did not reload: loads = %d, want %d", got, loadsBefore+1)
	}
	service.reads = newDocumentReads(weight * 4)
	if _, err := service.Text(context.Background(), artifactIDs[2]); err != nil {
		t.Fatalf("read oversized-for-budget document: %v", err)
	}
	if cacheHas(service, artifactIDs[2]) {
		t.Fatal("an entry heavier than a quarter-budget was cached, want the budget/eighth rule to reject it")
	}
}
