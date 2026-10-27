package docs

import (
	"context"
	"errors"
	"fmt"
	"slices"
	"sync/atomic"
	"testing"
	"time"

	"github.com/sjawhar/envoy/internal/dispatch/events"
	"github.com/sjawhar/envoy/internal/dispatch/store"
	"github.com/sjawhar/envoy/internal/dispatch/store/storetest"
)

// runExcerptReconciliation runs service's reconciliation loop, under the reference-excerpts lease
// on database's leases pool, until the test ends.
func runExcerptReconciliation(t *testing.T, service *Service, database *store.Store) {
	t.Helper()
	leases := excerptLeases(t, database)
	ctx, stop := context.WithCancel(context.Background())
	done := make(chan struct{})
	go func() {
		defer close(done)
		service.RunReferenceExcerptReconciliation(ctx, leases)
	}()
	t.Cleanup(func() {
		stop()
		<-done
	})
}

// excerptVersionOf is the excerpt_version of the mention document writes, or nil while it is owed.
func excerptVersionOf(t *testing.T, database *store.Store, document string) *int {
	t.Helper()
	var version *int
	if err := database.Pool.QueryRow(context.Background(),
		`select excerpt_version from refs where from_kind = 'artifact' and from_id = $1`, document).Scan(&version); err != nil {
		t.Fatalf("read the excerpt version of %s: %v", document, err)
	}
	return version
}

// Two builds serve one database during a release, and the build holding the reference-excerpts
// lease cannot read a document holding an element only the newer build knows. It passes over such
// a document and leaves its mentions owed, for a build that reads it, and its batches move past
// it: twenty-five such documents, more than one batch, sort before a document it reads, and that
// document still gets its excerpt.
func TestTheExcerptLeaseHolderPassesOverDocumentsHoldingNewerContent(t *testing.T) {
	database := storetest.Open(t)
	ctx := context.Background()
	const newer = 25
	documents := make([]string, newer+1)
	for index := range documents {
		documents[index] = createProjectDocument(t, database, fmt.Sprintf("# Document %d\n\nSee dispatch://CORE-%d here.\n", index, index+1))
		if _, err := database.Pool.Exec(ctx, `insert into refs (from_kind, from_id, to_kind, to_id) values ('artifact', $1, 'issue', $2)`,
			documents[index], fmt.Sprintf("CORE-%d", index+1)); err != nil {
			t.Fatalf("index document %d: %v", index, err)
		}
	}
	slices.Sort(documents)
	readable := documents[newer]
	service := New(Deps{Store: database, Events: events.NewBroker(), ServerURL: "https://dispatch.example", Settle: time.Hour})
	t.Cleanup(func() { _ = service.Shutdown(context.Background()) })
	for _, document := range documents[:newer] {
		if err := service.InjectUnknownElementForTest(ctx, document); err != nil {
			t.Fatalf("write an element this build does not know into %s: %v", document, err)
		}
		if err := service.Evict(ctx, document); err != nil {
			t.Fatalf("evict %s: %v", document, err)
		}
		if _, _, err := service.TextWithBlocks(ctx, document); !errors.Is(err, ErrDocNewerContent) {
			t.Fatalf("read %s: %v, want ErrDocNewerContent", document, err)
		}
	}

	runExcerptReconciliation(t, service, database)
	for deadline := time.Now().Add(20 * time.Second); excerptVersionOf(t, database, readable) == nil; {
		if time.Now().After(deadline) {
			t.Fatalf("the document after %d documents holding newer content was never reconciled", newer)
		}
		time.Sleep(20 * time.Millisecond)
	}
	for _, document := range documents[:newer] {
		if version := excerptVersionOf(t, database, document); version != nil {
			t.Fatalf("a document holding newer content got an excerpt at version %d, want its mention owed", *version)
		}
	}
}

// A version a build that stores no excerpt wrote leaves its mentions' excerpts behind, and the
// reconciliation marks them owed and cuts them again however long ago its process started: two
// builds serve one database during every release, not only in the first hour of a process. Here
// the clock moves two hours at each reading, so the tick reads the process's start as two hours
// before it.
func TestAReconciliationStartedMoreThanAnHourAgoStillMarksAStaleExcerpt(t *testing.T) {
	database := storetest.Open(t)
	ctx := context.Background()
	document := createProjectDocument(t, database, "# Document\n\nSee dispatch://CORE-1 here.\n")
	if _, err := database.Pool.Exec(ctx, `
		insert into refs (from_kind, from_id, to_kind, to_id, excerpt_version) values ('artifact', $1, 'issue', 'CORE-1', 1)
	`, document); err != nil {
		t.Fatalf("index the document's mention at version 1: %v", err)
	}
	if _, err := database.Pool.Exec(ctx, `
		insert into artifact_versions (artifact_id, number, markdown, authors) values ($1, 2, '# Document\n\nSee dispatch://CORE-1 again.\n', '[]')
	`, document); err != nil {
		t.Fatalf("write version 2 as a build that stores no excerpt: %v", err)
	}
	service := New(Deps{Store: database, Events: events.NewBroker(), ServerURL: "https://dispatch.example", Settle: time.Hour})
	t.Cleanup(func() { _ = service.Shutdown(context.Background()) })
	started := time.Now()
	var readings atomic.Int64
	service.now = func() time.Time { return started.Add(time.Duration(readings.Add(1)) * 2 * time.Hour) }

	runExcerptReconciliation(t, service, database)
	for deadline := time.Now().Add(20 * time.Second); ; time.Sleep(20 * time.Millisecond) {
		if version := excerptVersionOf(t, database, document); version != nil && *version == 2 {
			return
		}
		if time.Now().After(deadline) {
			t.Fatal("the excerpt version 1 left behind was never marked owed and cut again at version 2")
		}
	}
}

// unreadableDocumentStore fails every read of one document with an error that is neither of the
// two the reconciliation records as no block (ErrDocumentUnloadable, ErrDocOutsideSchema).
type unreadableDocumentStore struct {
	VersionedStore
	unreadable string
}

func (s unreadableDocumentStore) DocumentStamp(ctx context.Context, room string) (DocumentStamp, error) {
	if room == s.unreadable {
		return DocumentStamp{}, errors.New("injected: the store cannot read this document")
	}
	return s.VersionedStore.DocumentStamp(ctx, room)
}

func (s unreadableDocumentStore) LoadDocument(ctx context.Context, room string) (LoadedDocument, error) {
	if room == s.unreadable {
		return LoadedDocument{}, errors.New("injected: the store cannot read this document")
	}
	return s.VersionedStore.LoadDocument(ctx, room)
}

// One document the reconciliation cannot read for any other reason than the two it records as no
// block is skipped for that pass; the other nineteen of its batch still get their excerpts, and
// the tick reports no error, so the document that sorts first never holds the rest back.
func TestOneUnreadableDocumentDoesNotHoldBackItsBatch(t *testing.T) {
	database := storetest.Open(t)
	ctx := context.Background()
	documents := make([]string, referenceExcerptBatch)
	for index := range documents {
		documents[index] = createProjectDocument(t, database, fmt.Sprintf("# Document %d\n\nSee dispatch://CORE-%d here.\n", index, index+1))
		if _, err := database.Pool.Exec(ctx, `insert into refs (from_kind, from_id, to_kind, to_id) values ('artifact', $1, 'issue', $2)`,
			documents[index], fmt.Sprintf("CORE-%d", index+1)); err != nil {
			t.Fatalf("index document %d: %v", index, err)
		}
	}
	var first string
	if err := database.Pool.QueryRow(ctx, `select min(from_id) from refs where excerpt_version is null`).Scan(&first); err != nil {
		t.Fatal(err)
	}
	service := New(Deps{
		Store:       database,
		Persistence: unreadableDocumentStore{VersionedStore: NewPgVersioned(database), unreadable: first},
		Events:      events.NewBroker(),
		ServerURL:   "https://dispatch.example",
		Settle:      time.Hour,
	})
	t.Cleanup(func() { _ = service.Shutdown(context.Background()) })

	reconciled, _, err := service.ReconcileReferenceExcerpts(ctx)
	if err != nil {
		t.Fatalf("a tick with one unreadable document failed: %v", err)
	}
	if reconciled != referenceExcerptBatch-1 {
		t.Fatalf("reconciled %d documents; want the %d the store can read", reconciled, referenceExcerptBatch-1)
	}
	// These documents have a version row and no stored Yjs state, so the reconciliation reads no
	// blocks and records each mention with no excerpt: recorded is excerpt_version set, owed null.
	var recorded, owed int
	if err := database.Pool.QueryRow(ctx, `
		select count(*) filter (where excerpt_version is not null), count(*) filter (where excerpt_version is null) from refs
	`).Scan(&recorded, &owed); err != nil {
		t.Fatal(err)
	}
	if recorded != referenceExcerptBatch-1 || owed != 1 {
		t.Fatalf("recorded %d mentions and left %d owed; want %d recorded and the unreadable document's one owed", recorded, owed, referenceExcerptBatch-1)
	}
	var stillOwed string
	if err := database.Pool.QueryRow(ctx, `select from_id from refs where excerpt_version is null`).Scan(&stillOwed); err != nil || stillOwed != first {
		t.Fatalf("owed mention = %q (%v); want the unreadable document %q's", stillOwed, err, first)
	}
}

// A document the reconciliation reads but cannot record - here a trigger refuses its refs rows'
// update - is skipped for that pass and stays owed; the others of its batch are recorded, and the
// tick reports no error.
func TestADocumentTheReconciliationCannotRecordStaysOwed(t *testing.T) {
	database := storetest.Open(t)
	ctx := context.Background()
	documents := make([]string, 3)
	for index := range documents {
		documents[index] = createProjectDocument(t, database, fmt.Sprintf("# Document %d\n\nSee dispatch://CORE-%d here.\n", index, index+1))
		if _, err := database.Pool.Exec(ctx, `insert into refs (from_kind, from_id, to_kind, to_id) values ('artifact', $1, 'issue', $2)`,
			documents[index], fmt.Sprintf("CORE-%d", index+1)); err != nil {
			t.Fatalf("index document %d: %v", index, err)
		}
	}
	refused := documents[1]
	if _, err := database.Pool.Exec(ctx, `
		create function refuse_excerpt_record() returns trigger language plpgsql as $$
		begin
			raise exception 'injected: refs refuses this document''s excerpts';
		end $$`); err != nil {
		t.Fatalf("create the refusing function: %v", err)
	}
	if _, err := database.Pool.Exec(ctx, fmt.Sprintf(`
		create trigger refuse_excerpt_record before update on refs for each row
		when (old.from_id = '%s') execute function refuse_excerpt_record()`, refused)); err != nil {
		t.Fatalf("create the refusing trigger: %v", err)
	}
	service := New(Deps{Store: database, Events: events.NewBroker(), ServerURL: "https://dispatch.example", Settle: time.Hour})
	t.Cleanup(func() { _ = service.Shutdown(context.Background()) })

	reconciled, _, err := service.ReconcileReferenceExcerpts(ctx)
	if err != nil {
		t.Fatalf("a tick with one document it cannot record failed: %v", err)
	}
	if reconciled != len(documents)-1 {
		t.Fatalf("reconciled %d documents; want the %d it could record", reconciled, len(documents)-1)
	}
	var stillOwed []string
	rows, err := database.Pool.Query(ctx, `select from_id from refs where excerpt_version is null order by from_id`)
	if err != nil {
		t.Fatal(err)
	}
	for rows.Next() {
		var fromID string
		if err := rows.Scan(&fromID); err != nil {
			t.Fatal(err)
		}
		stillOwed = append(stillOwed, fromID)
	}
	rows.Close()
	if len(stillOwed) != 1 || stillOwed[0] != refused {
		t.Fatalf("owed mentions = %v; want only the refused document %q's", stillOwed, refused)
	}
}
