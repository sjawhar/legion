package docs

import (
	"context"
	"errors"
	"fmt"
	"sort"
	"sync"
	"testing"
	"time"

	"github.com/sjawhar/envoy/internal/dispatch/events"
	"github.com/sjawhar/envoy/internal/dispatch/refs"
	"github.com/sjawhar/envoy/internal/dispatch/store"
	"github.com/sjawhar/envoy/internal/dispatch/store/storetest"
	"github.com/sjawhar/envoy/internal/pglisten"
)

// unreadableDocumentStore fails every read of the documents in unreadable with an error that is
// neither of the two the reconciliation records as no block (ErrDocumentUnloadable,
// ErrDocOutsideSchema), and records when each such read was made.
type unreadableDocumentStore struct {
	VersionedStore
	unreadable map[string]bool
	reads      *failedReads
}

func newUnreadableDocumentStore(database *store.Store, unreadable ...string) unreadableDocumentStore {
	documents := make(map[string]bool, len(unreadable))
	for _, document := range unreadable {
		documents[document] = true
	}
	return unreadableDocumentStore{VersionedStore: NewPgVersioned(database), unreadable: documents, reads: &failedReads{}}
}

func (s unreadableDocumentStore) DocumentStamp(ctx context.Context, room string) (DocumentStamp, error) {
	if s.unreadable[room] {
		s.reads.record(room)
		return DocumentStamp{}, errors.New("injected: the store cannot read this document")
	}
	return s.VersionedStore.DocumentStamp(ctx, room)
}

func (s unreadableDocumentStore) LoadDocument(ctx context.Context, room string) (LoadedDocument, error) {
	if s.unreadable[room] {
		s.reads.record(room)
		return LoadedDocument{}, errors.New("injected: the store cannot read this document")
	}
	return s.VersionedStore.LoadDocument(ctx, room)
}

// failedReads is when each read of an unreadable document failed.
type failedReads struct {
	mu sync.Mutex
	at map[string][]time.Time
}

func (r *failedReads) record(room string) {
	r.mu.Lock()
	defer r.mu.Unlock()
	if r.at == nil {
		r.at = make(map[string][]time.Time)
	}
	r.at[room] = append(r.at[room], time.Now())
}

// attempts is when the reconciliation tried to read room: the failed reads, with the reads one
// attempt makes (a stamp, then a load) taken together.
func (r *failedReads) attempts(room string) []time.Time {
	r.mu.Lock()
	defer r.mu.Unlock()
	var attempts []time.Time
	for _, at := range r.at[room] {
		if len(attempts) == 0 || at.Sub(attempts[len(attempts)-1]) > 100*time.Millisecond {
			attempts = append(attempts, at)
		}
	}
	return attempts
}

// owedDocuments commits n project documents, each with one mention owed an excerpt, and returns
// them in the id order the reconciliation reads them in.
func owedDocuments(t *testing.T, database *store.Store, n int) []string {
	t.Helper()
	documents := make([]string, n)
	for index := range documents {
		documents[index] = createProjectDocument(t, database, fmt.Sprintf("# Document %d\n\nSee dispatch://CORE-%d here.\n", index, index+1))
		if _, err := database.Pool.Exec(context.Background(), `insert into refs (from_kind, from_id, to_kind, to_id) values ('artifact', $1, 'issue', $2)`,
			documents[index], fmt.Sprintf("CORE-%d", index+1)); err != nil {
			t.Fatalf("index document %d: %v", index, err)
		}
	}
	sort.Strings(documents)
	return documents
}

// owedMentions lists, in id order, the documents with a mention still owed an excerpt.
func owedMentions(t *testing.T, database *store.Store) []string {
	t.Helper()
	rows, err := database.Pool.Query(context.Background(), `select distinct from_id from refs where excerpt_version is null order by from_id`)
	if err != nil {
		t.Fatal(err)
	}
	defer rows.Close()
	var owed []string
	for rows.Next() {
		var fromID string
		if err := rows.Scan(&fromID); err != nil {
			t.Fatal(err)
		}
		owed = append(owed, fromID)
	}
	if err := rows.Err(); err != nil {
		t.Fatal(err)
	}
	return owed
}

// isOwed reports whether the document has a mention still owed an excerpt.
func isOwed(t *testing.T, database *store.Store, document string) bool {
	t.Helper()
	var owed bool
	if err := database.Pool.QueryRow(context.Background(), `
		select exists(select 1 from refs where from_kind = 'artifact' and from_id = $1 and excerpt_version is null)
	`, document).Scan(&owed); err != nil {
		t.Fatal(err)
	}
	return owed
}

// backoffAttempts is how many failures the document's backoff row counts, at which version; zero
// attempts when it has none.
func backoffAttempts(t *testing.T, database *store.Store, document string) (version, attempts int) {
	t.Helper()
	if err := database.Pool.QueryRow(context.Background(), `
		select coalesce(max(version), 0), coalesce(max(attempts), 0) from reference_excerpt_backoff where artifact_id = $1::uuid
	`, document).Scan(&version, &attempts); err != nil {
		t.Fatal(err)
	}
	return version, attempts
}

// newExcerptService is a document service on database reading documents through persistence (the
// database's own when nil).
func newExcerptService(t *testing.T, database *store.Store, persistence VersionedStore) *Service {
	t.Helper()
	service := New(Deps{Store: database, Persistence: persistence, Events: events.NewBroker(), ServerURL: "https://dispatch.example", Settle: time.Hour})
	t.Cleanup(func() { _ = service.Shutdown(context.Background()) })
	return service
}

// runExcerpts runs the reconciliation loop run until the test ends, and fails the test if it does
// not return promptly once its context ends.
func runExcerpts(t *testing.T, run func(ctx context.Context)) {
	t.Helper()
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan struct{})
	go func() {
		defer close(done)
		run(ctx)
	}()
	t.Cleanup(func() {
		cancel()
		select {
		case <-done:
		case <-time.After(10 * time.Second):
			t.Error("the reconciliation did not return within 10s of its context ending")
		}
	})
}

// One document the reconciliation cannot read for any other reason than the two it records as no
// block is deferred; the other nineteen of its batch still get their excerpts, and the batch
// reports no error, so the document that sorts first never holds the rest back. The next batch
// leaves it alone until its retry is due rather than reading it again.
func TestAnUnreadableDocumentIsDeferredAndDoesNotHoldBackItsExcerptBatch(t *testing.T) {
	database := storetest.Open(t)
	ctx := context.Background()
	documents := owedDocuments(t, database, referenceExcerptBatch)
	first := documents[0]
	persistence := newUnreadableDocumentStore(database, first)
	service := newExcerptService(t, database, persistence)

	reconciled, full, err := service.ReconcileReferenceExcerpts(ctx)
	if err != nil {
		t.Fatalf("a batch with one unreadable document failed: %v", err)
	}
	if reconciled != referenceExcerptBatch-1 || !full {
		t.Fatalf("reconciled %d documents, full %v; want the %d the store can read, and a full batch that moved", reconciled, full, referenceExcerptBatch-1)
	}
	// These documents have a version row and no stored Yjs state, so the reconciliation reads no
	// blocks and records each mention with no excerpt: recorded is excerpt_version set, owed null.
	if owed := owedMentions(t, database); len(owed) != 1 || owed[0] != first {
		t.Fatalf("owed mentions = %v; want only the unreadable document %q's", owed, first)
	}
	if version, attempts := backoffAttempts(t, database, first); version != 1 || attempts != 1 {
		t.Fatalf("the unreadable document's backoff = version %d, %d attempts; want version 1, 1 attempt", version, attempts)
	}

	reconciled, full, err = service.ReconcileReferenceExcerpts(ctx)
	if err != nil || reconciled != 0 || full {
		t.Fatalf("the next batch = %d documents, full %v, err %v; want nothing to do", reconciled, full, err)
	}
	if attempts := persistence.reads.attempts(first); len(attempts) != 1 {
		t.Fatalf("the unreadable document was read %d times; want once, its retry not yet due", len(attempts))
	}
}

// A document the reconciliation reads but cannot record - here a trigger refuses its refs rows'
// update - is deferred and stays owed; the others of its batch are recorded, the batch reports no
// error, and the next batch does not try it again before its retry is due.
func TestADocumentTheExcerptReconciliationCannotRecordIsDeferredAndStaysOwed(t *testing.T) {
	database := storetest.Open(t)
	ctx := context.Background()
	documents := owedDocuments(t, database, 3)
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
	service := newExcerptService(t, database, nil)

	reconciled, _, err := service.ReconcileReferenceExcerpts(ctx)
	if err != nil {
		t.Fatalf("a batch with one document it cannot record failed: %v", err)
	}
	if reconciled != len(documents)-1 {
		t.Fatalf("reconciled %d documents; want the %d it could record", reconciled, len(documents)-1)
	}
	if owed := owedMentions(t, database); len(owed) != 1 || owed[0] != refused {
		t.Fatalf("owed mentions = %v; want only the refused document %q's", owed, refused)
	}
	if version, attempts := backoffAttempts(t, database, refused); version != 1 || attempts != 1 {
		t.Fatalf("the refused document's backoff = version %d, %d attempts; want version 1, 1 attempt", version, attempts)
	}
	if reconciled, full, err := service.ReconcileReferenceExcerpts(ctx); err != nil || reconciled != 0 || full {
		t.Fatalf("the next batch = %d documents, full %v, err %v; want nothing to do", reconciled, full, err)
	}
	if _, attempts := backoffAttempts(t, database, refused); attempts != 1 {
		t.Fatalf("the refused document was tried again before its retry was due: %d attempts", attempts)
	}
}

// A document that keeps failing is tried again when its retry falls due, each wait twice the one
// before - not on every pass, which here may run every 50 ms - and the pass that defers it
// returns that due time as the loop's one timer.
func TestAFailingDocumentsExcerptsAreRetriedAtGrowingIntervals(t *testing.T) {
	database := storetest.Open(t)
	failing := owedDocuments(t, database, 1)[0]
	persistence := newUnreadableDocumentStore(database, failing)
	service := newExcerptService(t, database, persistence)
	leases := excerptLeases(t, database)
	const base = 400 * time.Millisecond
	runExcerpts(t, func(ctx context.Context) {
		service.runReferenceExcerptReconciliation(ctx, leases, nil, excerptSchedule{minGap: 50 * time.Millisecond, retryBase: base})
	})

	waitFor(t, 15*time.Second, "the failing document's first attempt and three retries", func() bool {
		return len(persistence.reads.attempts(failing)) >= 4
	})
	attempts := persistence.reads.attempts(failing)
	for index := 1; index < 4; index++ {
		gap, want := attempts[index].Sub(attempts[index-1]), base<<(index-1)
		if gap < want-20*time.Millisecond || gap > want+want/5+time.Second {
			t.Fatalf("retry %d came %v after the attempt before; want about %v (the base doubled per failure, plus up to a fifth)", index, gap, want)
		}
	}
	if !isOwed(t, database, failing) {
		t.Fatal("the failing document's mention is no longer owed")
	}
}

// Twenty documents that fail, sorting before a twenty-first, fill one batch; deferring them lets
// the next batch reach the twenty-first at once, and none of the twenty is read again before its
// retry is due.
func TestTwentyFailingDocumentsDoNotHoldBackATwentyFirstsExcerpts(t *testing.T) {
	database := storetest.Open(t)
	documents := owedDocuments(t, database, referenceExcerptBatch+1)
	failing, last := documents[:referenceExcerptBatch], documents[referenceExcerptBatch]
	persistence := newUnreadableDocumentStore(database, failing...)
	service := newExcerptService(t, database, persistence)
	leases := excerptLeases(t, database)
	runExcerpts(t, func(ctx context.Context) {
		service.runReferenceExcerptReconciliation(ctx, leases, nil, excerptSchedule{minGap: 100 * time.Millisecond, retryBase: time.Hour})
	})

	waitFor(t, 10*time.Second, "the twenty-first document's excerpt", func() bool { return !isOwed(t, database, last) })
	time.Sleep(500 * time.Millisecond)
	for _, document := range failing {
		if attempts := persistence.reads.attempts(document); len(attempts) != 1 {
			t.Fatalf("failing document %s was read %d times; want once, its retry an hour away", document, len(attempts))
		}
		if version, attempts := backoffAttempts(t, database, document); version != 1 || attempts != 1 {
			t.Fatalf("failing document %s's backoff = version %d, %d attempts; want version 1, 1 attempt", document, version, attempts)
		}
	}
}

// Once the reconciliation has stored every owed excerpt, it makes no statement at all - no batch
// on a timer, no lease attempt - over a window longer than the two-second step the timer it
// replaced took, and its last pass asked for no timer.
func TestTheExcerptReconciliationMakesNoStatementOnceDrained(t *testing.T) {
	database := storetest.Open(t)
	documents := owedDocuments(t, database, 3)
	counted, counter := countedStore(t, database)
	service := newExcerptService(t, counted, nil)
	leases := excerptLeases(t, counted)
	runExcerpts(t, func(ctx context.Context) { service.RunReferenceExcerptReconciliation(ctx, leases, nil) })

	waitFor(t, 15*time.Second, "every owed excerpt stored", func() bool { return len(owedMentions(t, database)) == 0 })
	awaitQuiet(t, counter, time.Second, 10*time.Second)
	requireNoStatements(t, counter, 3*time.Second, "the reconciliation with nothing owed")
	if next := service.reconcileExcerptPass(context.Background(), referenceExcerptSchedule); !next.IsZero() {
		t.Fatalf("a pass with nothing owed asked to run again at %v; want no timer", next)
	}
	for _, document := range documents {
		if version, attempts := backoffAttempts(t, database, document); attempts != 0 {
			t.Fatalf("recorded document %s holds a backoff row (version %d, %d attempts)", document, version, attempts)
		}
	}
}

// A mention left owed while the reconciliation is idle wakes it through its notification on a
// real pglisten.Hub, and its excerpt is stored within 3 seconds: one ClearExcerpts leaves, and
// one an old-style direct update leaves, as the previous release's code and rebuild-refs write
// them, with no call in Go to announce them.
func TestAMentionOwedAnExcerptWakesTheReconciliation(t *testing.T) {
	database := storetest.Open(t)
	document := owedDocuments(t, database, 1)[0]
	hub := &pglisten.Hub{Owner: "reference excerpts test", Dial: database.Pool.Listener}
	wake := hub.Wake(ExcerptsOwedChannel)
	connected := make(chan struct{})
	hub.OnConnected(sync.OnceFunc(func() { close(connected) }))
	hubCtx, stopHub := context.WithCancel(context.Background())
	hubDone := make(chan struct{})
	go func() {
		defer close(hubDone)
		hub.Run(hubCtx)
	}()
	t.Cleanup(func() {
		stopHub()
		<-hubDone
	})
	counted, counter := countedStore(t, database)
	service := newExcerptService(t, counted, nil)
	leases := excerptLeases(t, counted)
	started := time.Now()
	runExcerpts(t, func(ctx context.Context) { service.RunReferenceExcerptReconciliation(ctx, leases, wake) })
	select {
	case <-connected:
	case <-time.After(10 * time.Second):
		t.Fatal("the hub did not connect within 10s")
	}
	waitFor(t, 10*time.Second, "the boot pass to store the excerpt", func() bool { return !isOwed(t, database, document) })
	// The hub's signal after its LISTEN earns one more pass, no sooner than the gap after the
	// boot pass began; after it the loop is idle.
	time.Sleep(time.Until(started.Add(referenceExcerptSchedule.minGap + time.Second)))
	awaitQuiet(t, counter, 500*time.Millisecond, 10*time.Second)

	ctx := context.Background()
	for name, leaveOwed := range map[string]func() error{
		"ClearExcerpts": func() error { return refs.ClearExcerpts(ctx, database.Pool, document) },
		"a direct update": func() error {
			_, err := database.Pool.Exec(ctx, `update refs set excerpt_version = null where from_kind = 'artifact' and from_id = $1`, document)
			return err
		},
	} {
		if err := leaveOwed(); err != nil {
			t.Fatalf("%s: %v", name, err)
		}
		if !isOwed(t, database, document) {
			t.Fatalf("%s left nothing owed", name)
		}
		waitFor(t, 3*time.Second, "the excerpt "+name+" left owed to be stored", func() bool { return !isOwed(t, database, document) })
	}
}
