package docs

import (
	"context"
	"fmt"
	"maps"
	"net/http"
	"net/http/httptest"
	"reflect"
	"runtime"
	"sync"
	"testing"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/reearth/ygo/crdt"

	"github.com/sjawhar/envoy/internal/dispatch/docs/docstest"
	"github.com/sjawhar/envoy/internal/dispatch/events"
	"github.com/sjawhar/envoy/internal/dispatch/model"
	"github.com/sjawhar/envoy/internal/dispatch/pmdoc"
	"github.com/sjawhar/envoy/internal/dispatch/store"
	"github.com/sjawhar/envoy/internal/dispatch/store/storetest"
)

// An ask block a session writes just before its issue closes - inside the settle delay, so no
// settlement indexed it yet - is still that session's ask once the issue reopens and the document
// settles: the asking session can reword it, and readers can tell who asked.
func TestAnAskWrittenJustBeforeItsIssueClosedKeepsItsAuthorAfterAReopen(t *testing.T) {
	service, artifactID := newTestService(t)
	service.settle = time.Hour // the issue closes before the edit's own settlement runs
	actor := model.Actor{Kind: "session", ID: "asking-session"}
	seedServiceText(t, service, artifactID, "# Decision\n\nContext.\n")

	writeAskBeforeClose(t, service, artifactID, actor)
	closeTestIssue(t, service)
	reopenTestIssue(t, service)
	service.settle = 20 * time.Millisecond
	service.ScheduleSettlement(artifactID)
	waitForAskAuthor(t, service, artifactID, actor)
}

// The author persisted for a document that closes before its settlement also survives a process
// restart between the close and the reopen, where the old service's room state is unavailable.
func TestAnAskWrittenJustBeforeItsIssueClosedKeepsItsAuthorAfterRestartAndReopen(t *testing.T) {
	service, artifactID := newTestService(t)
	service.settle = time.Hour // the issue closes before the edit's own settlement runs
	ctx := context.Background()
	actor := model.Actor{Kind: "session", ID: "restarted-asking-session"}
	seedServiceText(t, service, artifactID, "# Decision\n\nContext.\n")

	writeAskBeforeClose(t, service, artifactID, actor)
	closeTestIssue(t, service)
	if err := service.Shutdown(ctx); err != nil {
		t.Fatalf("stop the service between close and reopen: %v", err)
	}
	restarted := New(Deps{Store: service.store, Events: events.NewBroker(), Settle: 20 * time.Millisecond})
	t.Cleanup(func() {
		if err := restarted.Shutdown(context.Background()); err != nil {
			t.Errorf("shutdown restarted document service: %v", err)
		}
	})
	reopenTestIssue(t, restarted)
	if err := restarted.resumeOwedSettlements(ctx, 0); err != nil {
		t.Fatalf("resume the reopened document's settlement: %v", err)
	}
	waitForAskAuthor(t, restarted, artifactID, actor)
}

// A request can be canceled immediately after its transaction commits. That must not leave the
// document's durable pending-settlement credit naming the earlier seed author instead of this
// committed edit's author.
func TestCommittedLedgerWritePersistsSettlementCreditWhenCallerContextCancelsAfterCommit(t *testing.T) {
	service, artifactID := newTestService(t)
	service.settle = time.Hour
	seedServiceText(t, service, artifactID, "# Decision\n\nContext.\n")

	ctx, cancel := context.WithCancel(context.Background())
	tx, err := service.store.Pool.Begin(ctx)
	if err != nil {
		t.Fatalf("begin edit: %v", err)
	}
	defer tx.Rollback(context.Background())
	joined, ledger := service.Join(ctx, cancelAfterCommitTx{Tx: tx, cancel: cancel})
	defer ledger.Discard()
	actor := model.Actor{Kind: "session", ID: "canceled-after-commit-session"}
	if _, err := service.ApplyOps(joined, artifactID, []model.EditOp{{
		Op: "insert", After: "end", Markdown: ":::ask{#canceled-credit urgency=\"med\" multiple=\"false\"}\nWho wrote this?\n:::\n",
	}}, actor, nil); err != nil {
		t.Fatalf("write the ask block: %v", err)
	}
	if err := ledger.Commit(ctx); err != nil {
		t.Fatalf("commit the ask block: %v", err)
	}

	owed, credit, err := pendingSettlementCredit(context.Background(), service.store.Pool, artifactID)
	if err != nil {
		t.Fatalf("read durable settlement credit: %v", err)
	}
	if !owed || credit.LastActor == nil || *credit.LastActor != actor {
		t.Fatalf("durable settlement credit after the canceled-context commit = %+v, want last actor %+v", credit, actor)
	}
}

// Once a settlement consumes its final credit, closing the document's issue must not recreate a
// pending settlement from the already-consumed last actor.
func TestCloseAfterSettlementDoesNotRecreatePendingSettlementFromConsumedLastActor(t *testing.T) {
	service, artifactID := newTestService(t)
	service.settle = 20 * time.Millisecond
	actor := model.Actor{Kind: "session", ID: "settled-before-close-session"}
	seedServiceText(t, service, artifactID, "# Decision\n\nContext.\n")

	writeAskBeforeClose(t, service, artifactID, actor)
	waitForAskAuthor(t, service, artifactID, actor)
	waitFor(t, 30*time.Second, "the ask settlement to clear its pending row", func() bool {
		owed, _, err := pendingSettlementCredit(context.Background(), service.store.Pool, artifactID)
		return err == nil && !owed
	})

	closeTestIssue(t, service)
	owed, _, err := pendingSettlementCredit(context.Background(), service.store.Pool, artifactID)
	if err != nil {
		t.Fatalf("read settlement row after close: %v", err)
	}
	if owed {
		t.Fatal("closing an already settled document recreated a pending settlement from its consumed last actor")
	}
}

// A pending-settlement row whose pending authors are JSON null, as an append that carried no credit
// could store, still lets its document load and settle without failing on the corrupted row: the
// ask settles to the author its own introducing write registered (registerAskAuthors), here the
// seed, which the room tracks independently of the row's pending-credit bookkeeping and takes
// precedence over the row's last_actor fallback - that fallback is for a block no write
// registered (LEGION-503). The row's own null-pending shape is what this test exercises: without
// the jsonb_typeof guard releaseSettlementCredit shares with upsertSettlementCredit, the
// settlement's own release of this credit would roll back on "cannot delete from scalar".
func TestADocumentLoadsAPendingSettlementRowWithNullPendingAuthors(t *testing.T) {
	service, artifactID := newTestService(t)
	service.settle = time.Hour // the seed's own settlement never runs
	ctx := context.Background()
	seed := model.Actor{Kind: "user", ID: "seed"}
	seedServiceText(t, service, artifactID, ":::ask{#null-pending-ask urgency=\"med\" multiple=\"false\"}\nWho asked?\n:::\n")
	writer := model.Actor{Kind: "session", ID: "null-pending-writer"}
	if _, err := service.store.Pool.Exec(ctx, `
		update doc_settlements_pending
		set settlement_authors = jsonb_build_object('pending', null, 'last_actor', $2::jsonb)
		where artifact_id = $1
	`, artifactID, writer); err != nil {
		t.Fatalf("store the pending-settlement row: %v", err)
	}
	service.settle = 20 * time.Millisecond
	if err := service.warmLiveDocument(ctx, artifactID); err != nil {
		t.Fatalf("load the document: %v", err)
	}
	var author model.Actor
	waitFor(t, 30*time.Second, "the loaded document's settlement to index its ask", func() bool {
		return service.store.Pool.QueryRow(ctx, `
			select author from asks where block_artifact_id = $1 and block_id = 'null-pending-ask'
		`, artifactID).Scan(&author) == nil
	})
	if author != seed {
		t.Fatalf("the ask settled from a row with null pending authors is %+v's, want %+v's, its seed", author, seed)
	}
}

// A version written while the document's pending-settlement row holds a stored null pending - the
// same defensive shape an append that carried no credit, or a hand-written row, could leave -
// still commits: releaseSettlementCredit must guard the scalar the same way upsertSettlementCredit
// does, or the whole transaction rolls back on "cannot delete from scalar".
func TestANamedVersionReleasesAuthorsOverANullPendingRow(t *testing.T) {
	service, artifactID := newTestService(t)
	service.settle = time.Hour
	ctx := context.Background()
	seedServiceText(t, service, artifactID, "# Decision\n\nContext.\n")
	if _, err := service.store.Pool.Exec(ctx, `
		update doc_settlements_pending
		set settlement_authors = jsonb_build_object('pending', null, 'last_actor', $2::jsonb)
		where artifact_id = $1
	`, artifactID, model.Actor{Kind: "session", ID: "earlier-writer"}); err != nil {
		t.Fatalf("store a null-pending settlement row: %v", err)
	}
	writer := model.Actor{Kind: "session", ID: "versioned-writer"}
	writeThroughLedger(t, service, artifactID, writer, "Versioned writer's paragraph.\n", withNamedVersion)

	if authors := latestVersionAuthors(t, service, artifactID); !reflect.DeepEqual(authors, []model.Actor{writer}) {
		t.Fatalf("version written over a null-pending row credits %+v, want %+v alone", authors, writer)
	}
	credit := readSettlementCredit(t, service.store, artifactID)
	if len(credit.Pending) != 0 {
		t.Fatalf("pending settlement credit after a version released a null-pending row = %+v, want empty", credit.Pending)
	}
}

// Two sessions that each write a document inside the settle delay are both credited on the version
// a later process settles, when the process that took their writes ended without settling them.
func TestTwoCreditedWritersBothSurviveACrashRestart(t *testing.T) {
	service, artifactID := newTestService(t)
	service.settle = time.Hour // the writing process never settles them
	ctx := context.Background()
	seedServiceText(t, service, artifactID, "# Decision\n\nContext.\n")
	first := model.Actor{Kind: "session", ID: "first-writer-session"}
	second := model.Actor{Kind: "session", ID: "second-writer-session"}
	writeThroughLedger(t, service, artifactID, first, "First writer's paragraph.\n", withoutVersion)
	writeThroughLedger(t, service, artifactID, second, "Second writer's paragraph.\n", withoutVersion)

	restarted := New(Deps{Store: service.store, Events: events.NewBroker(), Settle: 20 * time.Millisecond})
	t.Cleanup(func() {
		if err := restarted.Shutdown(context.Background()); err != nil {
			t.Errorf("shutdown restarted document service: %v", err)
		}
	})
	if err := restarted.resumeOwedSettlements(ctx, 0); err != nil {
		t.Fatalf("resume the document's settlement: %v", err)
	}
	waitFor(t, 30*time.Second, "the resumed settlement to commit", func() bool {
		owed, _, err := pendingSettlementCredit(ctx, service.store.Pool, artifactID)
		return err == nil && !owed
	})
	if authors := latestVersionAuthors(t, service, artifactID); !reflect.DeepEqual(authors, []model.Actor{first, second}) {
		t.Fatalf("resumed settlement version authors = %+v, want %+v and %+v", authors, first, second)
	}
}

// The issue route closes an issue's rooms on its request's context, which the pool tracks as one
// caller holding at most one connection. Every closing document still records its settlement credit
// and releases its state, however many documents the issue has.
func TestClosingAnIssueOnATrackedContextPersistsEveryDocumentsCredit(t *testing.T) {
	for _, procs := range []int{4, 16} {
		t.Run(fmt.Sprintf("GOMAXPROCS=%d", procs), func(t *testing.T) {
			defer runtime.GOMAXPROCS(runtime.GOMAXPROCS(procs))
			service, database := newRoomReleaseService(t)
			service.settle = time.Hour
			ctx := context.Background()
			ids := createDocuments(t, database, 500)
			writer := model.Actor{Kind: "session", ID: "closing-writer"}
			for _, id := range ids {
				service.recordActor(id, writer)
			}
			if _, err := database.Pool.Exec(ctx, `update issues set closed_at = now() where key = 'DOC-1'`); err != nil {
				t.Fatalf("close the documents' issue: %v", err)
			}
			// What PATCH /api/v1/issues/{key} hands it (api/issue_patch.go).
			service.SetIssueClosed(context.WithoutCancel(store.WithTransactionTracking(ctx)), "DOC-1", true)

			var persisted int
			if err := database.Pool.QueryRow(ctx, `
				select count(*) from doc_settlements_pending
				where artifact_id::text = any($1) and settlement_authors ? 'last_actor'
			`, ids).Scan(&persisted); err != nil {
				t.Fatalf("read the documents' settlement credit: %v", err)
			}
			retained := 0
			service.rooms.Range(func(_, _ any) bool {
				retained++
				return true
			})
			if persisted != len(ids) || retained != 0 {
				t.Fatalf("closing %d documents persisted %d credits and kept %d states, want %d and 0",
					len(ids), persisted, retained, len(ids))
			}
		})
	}
}

// ledgerWriteVersion is the version, if any, a test's ledger write commits with its edit.
type ledgerWriteVersion int

const (
	withoutVersion ledgerWriteVersion = iota
	// withSnapshotVersion is the version POST /edits writes for an edit without a summary.
	withSnapshotVersion
	// withNamedVersion is the version POST /edits writes for an edit with a summary.
	withNamedVersion
)

// writeThroughLedger writes markdown at the document's end as actor in one transaction, as POST
// /edits does, with the version the same transaction writes, if any.
func writeThroughLedger(t *testing.T, service *Service, artifactID string, actor model.Actor, markdown string, version ledgerWriteVersion) {
	t.Helper()
	ctx := context.Background()
	tx, err := service.store.Pool.Begin(ctx)
	if err != nil {
		t.Fatalf("begin %s's write: %v", actor.ID, err)
	}
	defer tx.Rollback(ctx)
	joined, ledger := service.Join(ctx, tx)
	defer ledger.Discard()
	if _, err := service.ApplyOps(joined, artifactID, []model.EditOp{{Op: "insert", After: "end", Markdown: markdown}}, actor, nil); err != nil {
		t.Fatalf("apply %s's write: %v", actor.ID, err)
	}
	switch version {
	case withSnapshotVersion:
		_, err = service.SnapshotVersion(joined, artifactID, actor)
	case withNamedVersion:
		_, err = service.NamedVersion(joined, artifactID, actor.ID+"'s version", actor)
	}
	if err != nil {
		t.Fatalf("write %s's version: %v", actor.ID, err)
	}
	if err := ledger.Commit(ctx); err != nil {
		t.Fatalf("commit %s's write: %v", actor.ID, err)
	}
}

// An author whose edit wrote its own version is not owed that credit again: the next version after
// the document's issue closes and reopens credits only the author who wrote it.
func TestAReopenedDocumentsNextVersionCreditsOnlyItsOwnAuthor(t *testing.T) {
	service, artifactID := newTestService(t)
	service.settle = time.Hour // no settlement runs between the edits
	seedServiceText(t, service, artifactID, "# Decision\n\nContext.\n")
	alpha := model.Actor{Kind: "session", ID: "alpha-session"}
	delta := model.Actor{Kind: "session", ID: "delta-session"}

	writeThroughLedger(t, service, artifactID, alpha, "Alpha's paragraph.\n", withSnapshotVersion)
	closeTestIssue(t, service)
	reopenTestIssue(t, service)
	writeThroughLedger(t, service, artifactID, delta, "Delta's paragraph.\n", withSnapshotVersion)

	if authors := latestVersionAuthors(t, service, artifactID); !reflect.DeepEqual(authors, []model.Actor{delta}) {
		t.Fatalf("the reopened document's next version credits %+v, want %+v alone", authors, delta)
	}
}

// A session that named a version for its edit is not credited again once the issue closes and
// reopens: the settlement credits the later writer's ask and version to that writer alone.
func TestAVersionedAuthorIsNotCreditedAgainAfterAReopen(t *testing.T) {
	service, artifactID := newTestService(t)
	service.settle = time.Hour // the issue closes before any settlement runs
	seedServiceText(t, service, artifactID, "# Decision\n\nContext.\n")
	versioned := model.Actor{Kind: "session", ID: "versioned-session"}
	asking := model.Actor{Kind: "session", ID: "asking-session"}

	writeThroughLedger(t, service, artifactID, versioned, "The versioned session's paragraph.\n", withNamedVersion)
	writeAskBeforeClose(t, service, artifactID, asking)
	closeTestIssue(t, service)
	reopenTestIssue(t, service)
	service.settle = 20 * time.Millisecond
	service.ScheduleSettlement(artifactID)

	waitForAskAuthor(t, service, artifactID, asking)
	waitFor(t, 30*time.Second, "the reopened document's settlement to commit", func() bool {
		owed, _, err := pendingSettlementCredit(context.Background(), service.store.Pool, artifactID)
		return err == nil && !owed
	})
	if authors := latestVersionAuthors(t, service, artifactID); !reflect.DeepEqual(authors, []model.Actor{asking}) {
		t.Fatalf("the reopened settlement's version credits %+v, want %+v alone", authors, asking)
	}
}

// The same holds when the process that took both writes ends without settling them: the next
// process's settlement credits the ask to the session that wrote it, not to one already versioned.
func TestAVersionedAuthorIsNotCreditedAgainAfterACrashRestart(t *testing.T) {
	service, artifactID := newTestService(t)
	service.settle = time.Hour // the writing process never settles them
	ctx := context.Background()
	seedServiceText(t, service, artifactID, "# Decision\n\nContext.\n")
	versioned := model.Actor{Kind: "session", ID: "versioned-session"}
	asking := model.Actor{Kind: "user", ID: "asking-user"}

	writeThroughLedger(t, service, artifactID, versioned, "The versioned session's paragraph.\n", withNamedVersion)
	writeAskBeforeClose(t, service, artifactID, asking)

	restarted := New(Deps{Store: service.store, Events: events.NewBroker(), Settle: 20 * time.Millisecond})
	t.Cleanup(func() {
		if err := restarted.Shutdown(context.Background()); err != nil {
			t.Errorf("shutdown restarted document service: %v", err)
		}
	})
	if err := restarted.resumeOwedSettlements(ctx, 0); err != nil {
		t.Fatalf("resume the document's settlement: %v", err)
	}
	waitForAskAuthor(t, restarted, artifactID, asking)
}

// A browser edit the room observes while an API edit's own version is still open is credited only
// once. The version's capture already read carol from the room's pending authors (she is in
// `state.pending` as soon as the room observes her edit, before her own durable append runs), and
// its release takes her back out of the durable row; carol's own durable append - queued behind
// the version's transaction, which holds the document's advisory lock for its whole duration - must
// not then replay the stale pending snapshot it captured before the release ran. Reproduces the
// round-6 regression: without the release-sequence watermark, the reopened document's next version
// credited carol again alongside delta.
func TestABrowserEditQueuedBehindAnOpenVersionIsNotCreditedAgainAfterAReopen(t *testing.T) {
	service, artifactID := newTestService(t)
	service.settle = time.Hour // no settlement runs between the edits
	ctx := context.Background()
	seedServiceText(t, service, artifactID, "# Decision\n\nContext.\n")
	httpServer := httptest.NewServer(http.HandlerFunc(service.ServeHTTP))
	t.Cleanup(httpServer.Close)

	agent := model.Actor{Kind: "session", ID: "agent-session"}
	delta := model.Actor{Kind: "session", ID: "delta-session"}

	// Hold the document's advisory lock for the whole agent transaction, as ApplyOps does for a
	// joined write (lockDocumentRoom), so carol's durable append queues behind it.
	tx, err := service.store.Pool.Begin(ctx)
	if err != nil {
		t.Fatalf("begin the agent's transaction: %v", err)
	}
	defer tx.Rollback(ctx)
	joined, ledger := service.Join(ctx, tx)
	defer ledger.Discard()
	if _, err := service.ApplyOps(joined, artifactID, []model.EditOp{{Op: "insert", After: "end", Markdown: "Agent's paragraph.\n"}}, agent, nil); err != nil {
		t.Fatalf("apply the agent's edit: %v", err)
	}

	// Carol, a connected browser, edits while the agent's transaction still holds the document's
	// advisory lock, so her durable append - which the room's persistence worker drives
	// asynchronously - queues behind it. She stays connected until the issue's close ends her
	// connection, so no last-browser settlement runs before delta's version.
	carol := connectBrowser(t, httpServer.URL, artifactID, "carol")
	editAsBrowser(t, service, artifactID, carol, "# Decision\n\nContext.\n\nCarol's paragraph.\n")
	if !service.hasDurableAppend(artifactID) {
		t.Fatal("carol's durable append finished before the agent's transaction released the document lock")
	}

	result, err := service.SnapshotVersion(joined, artifactID, agent)
	if err != nil {
		t.Fatalf("snapshot the agent's version: %v", err)
	}
	if authors := result.Version.Authors; len(authors) < 2 {
		t.Fatalf("agent's version authors = %+v, want at least the agent and carol", authors)
	}
	if err := ledger.Commit(ctx); err != nil {
		t.Fatalf("commit the agent's transaction: %v", err)
	}

	// Carol's queued append can now take the lock the commit released.
	waitForPersistedUpdates(t, service, artifactID, 2)

	closeTestIssue(t, service)
	reopenTestIssue(t, service)
	writeThroughLedger(t, service, artifactID, delta, "Delta's paragraph.\n", withSnapshotVersion)

	if authors := latestVersionAuthors(t, service, artifactID); !reflect.DeepEqual(authors, []model.Actor{delta}) {
		t.Fatalf("the reopened document's next version credits %+v, want %+v alone", authors, delta)
	}
}

// connectBrowser connects login's browser to artifactID's room (connectPeer) and returns once the
// browser holds the room's document. The browser stays connected until the caller closes it or the
// room closes it. A room's last browser leaving settles what the room is owed at once
// (settleLastPeer), so a test that needs credit left pending keeps a browser connected until the
// issue's close, which stops that settlement first.
func connectBrowser(t *testing.T, serverURL, artifactID, login string) *docstest.Peer {
	t.Helper()
	browser := connectPeer(t, serverURL, artifactID, login)
	if err := browser.AskForDocument(); err != nil {
		t.Fatalf("ask the room for its document: %v", err)
	}
	select {
	case <-browser.Answers:
	case <-browser.Ended:
		t.Fatalf("%s's connection closed before the room sent the document", login)
	case <-time.After(untilTestDeadline(t)):
		t.Fatalf("the room did not send %s the document before the test's deadline", login)
	}
	return browser
}

// editAsBrowser replaces the document's text from browser's own copy and sends the room the update,
// as a keystroke does: the copy's tree is read and rewritten in the one transaction that makes the
// update, under the copy's own lock, which the browser's reader applies the room's updates under.
// ygo applies the update to the room in its own transaction, under the room's lock - the path every
// browser edit takes. It returns once the room's update observer has run for that update -
// creditContentChange credited it and recordUpdateClass counted its durable append - whether or not
// the append has reached storage. The signal is an observer this helper registers on the room after
// the service's own: ygo fires a transaction's update observers in the order they were registered
// (reearth/ygo crdt/doc.go, the onUpdate snapshot loop), so this one runs after the service's has
// finished with the same update.
func editAsBrowser(t *testing.T, service *Service, artifactID string, browser *docstest.Peer, markdown string) {
	t.Helper()
	// These take the copy's lock themselves, so they are read before the transaction that holds it.
	fragment := browser.Doc.GetXmlFragment(fragmentName)
	client := browser.Doc.ClientID()
	sentFrom := browser.Doc.StateVector().Clock(client)
	room := service.srv.GetDoc(artifactID)
	if room == nil {
		t.Fatal("the room is not resident while its browser is connected")
	}
	recorded := make(chan struct{})
	var once sync.Once
	unsubscribe := room.OnUpdate(func([]byte, any) {
		if room.StateVector().Clock(client) > sentFrom {
			once.Do(func() { close(recorded) })
		}
	})
	defer unsubscribe()
	var changeErr error
	if _, err := browser.Send(func(txn *crdt.Transaction) {
		current, err := treeOfTransaction(txn, fragment)
		if err != nil {
			changeErr = fmt.Errorf("read the browser's copy of the document: %w", err)
			return
		}
		target, err := parseReplacing(current, markdown)
		if err != nil {
			changeErr = fmt.Errorf("parse the browser's edit: %w", err)
			return
		}
		changeErr = pmdoc.Update(txn, fragment, target)
	}); err != nil || changeErr != nil {
		t.Fatalf("send the browser's edit: %v %v", err, changeErr)
	}
	select {
	case <-recorded:
	case <-time.After(untilTestDeadline(t)):
		t.Fatal("the room did not record the browser's edit before the test's deadline")
	}
}

// untilTestDeadline is how long a wait may run: up to the test binary's deadline, less a margin
// to fail by name, rather than a fixed bound a loaded machine outruns.
func untilTestDeadline(t *testing.T) time.Duration {
	if deadline, bounded := t.Deadline(); bounded {
		return time.Until(deadline) - 2*time.Second
	}
	return time.Minute
}

// connectedBrowsers is how many browsers the room has registered (state.connected).
func connectedBrowsers(service *Service, artifactID string) int {
	state := service.lockExistingState(artifactID)
	if state == nil {
		return 0
	}
	defer service.unlockState(artifactID, state)
	return len(state.connected)
}

// Two connected peers' edits around an open version are each credited once: carol's edit, which the
// agent's version captured and releases, does not ride back in on dave's edit made while that
// version is still open. A browser edit's credit names the peers connected for it
// (state.connected), never the room's whole pending map, which still holds carol until the
// version's commit takes her out of the room.
func TestATwoPeerBrowserEditQueuedBehindAnOpenVersionIsNotCreditedAgainAfterAReopen(t *testing.T) {
	service, artifactID := newTestService(t)
	service.settle = time.Hour // no settlement runs between the edits
	ctx := context.Background()
	seedServiceText(t, service, artifactID, "# Decision\n\nContext.\n")
	httpServer := httptest.NewServer(http.HandlerFunc(service.ServeHTTP))
	t.Cleanup(httpServer.Close)

	dave := model.Actor{Kind: "user", ID: "dave"}
	agent := model.Actor{Kind: "session", ID: "agent-session"}
	delta := model.Actor{Kind: "session", ID: "delta-session"}

	carol := connectBrowser(t, httpServer.URL, artifactID, "carol")
	editAsBrowser(t, service, artifactID, carol, "# Decision\n\nContext.\n\nCarol's paragraph.\n")

	// Hold the document's advisory lock for the whole agent transaction, as ApplyOps does for a
	// joined write, so a connected peer's durable append queues behind it.
	tx, err := service.store.Pool.Begin(ctx)
	if err != nil {
		t.Fatalf("begin the agent's transaction: %v", err)
	}
	defer tx.Rollback(ctx)
	joined, ledger := service.Join(ctx, tx)
	defer ledger.Discard()
	if _, err := service.ApplyOps(joined, artifactID, []model.EditOp{{Op: "insert", After: "end", Markdown: "Agent's paragraph.\n"}}, agent, nil); err != nil {
		t.Fatalf("apply the agent's edit: %v", err)
	}
	result, err := service.SnapshotVersion(joined, artifactID, agent)
	if err != nil {
		t.Fatalf("snapshot the agent's version: %v", err)
	}
	if authors := result.Version.Authors; len(authors) < 2 {
		t.Fatalf("agent's version authors = %+v, want at least the agent and carol", authors)
	}

	// Dave connects, then carol leaves, so dave is the sole connected browser for his edit and
	// carol's leaving is not the room's last (which would settle at once, settleLastPeer). Dave
	// edits while the agent's transaction still holds the document's advisory lock, so his durable
	// append queues behind it too; he stays connected until the issue's close ends his connection.
	daveBrowser := connectBrowser(t, httpServer.URL, artifactID, "dave")
	carol.Close()
	waitFor(t, 5*time.Second, "the room to forget carol's connection", func() bool {
		return connectedBrowsers(service, artifactID) == 1
	})
	editAsBrowser(t, service, artifactID, daveBrowser, "# Decision\n\nContext.\n\nCarol's paragraph.\n\nDave's paragraph.\n")
	if !service.hasDurableAppend(artifactID) {
		t.Fatal("dave's durable append finished before the agent's transaction released the document lock")
	}

	if err := ledger.Commit(ctx); err != nil {
		t.Fatalf("commit the agent's transaction: %v", err)
	}

	// Carol's and dave's queued appends can now take the lock the commit released.
	waitForPersistedUpdates(t, service, artifactID, 3)

	closeTestIssue(t, service)
	reopenTestIssue(t, service)
	writeThroughLedger(t, service, artifactID, delta, "Delta's paragraph.\n", withSnapshotVersion)

	// Dave's own edit was never captured by any version before the close, so delta's version
	// legitimately credits him too; the bug under test is carol riding back in, not dave's own
	// still-pending credit. The version's authors are sorted by actor key (actorSlice), so the
	// session delta sorts ahead of the user dave.
	want := []model.Actor{delta, dave}
	if authors := latestVersionAuthors(t, service, artifactID); !reflect.DeepEqual(authors, want) {
		t.Fatalf("the reopened document's next version credits %+v, want %+v (dave's own pending credit, not carol's)", authors, want)
	}
}

// A browser edit made after an open version captured its authors, and before that version
// commits, stays owed in the room and in the pending-settlement row alike. The version's release
// takes out only the credit its capture read: carol credited again since is a later edit the
// version does not hold, which the row keeps (its release watermark lets the later credit through)
// and so must the room.
func TestABrowserEditAfterAVersionsCaptureStaysOwedInTheRoomAndTheRow(t *testing.T) {
	service, artifactID := newTestService(t)
	service.settle = time.Hour // no settlement runs between the edits
	ctx := context.Background()
	seedServiceText(t, service, artifactID, "# Decision\n\nContext.\n")
	httpServer := httptest.NewServer(http.HandlerFunc(service.ServeHTTP))
	t.Cleanup(httpServer.Close)
	carol := model.Actor{Kind: "user", ID: "carol"}
	agent := model.Actor{Kind: "session", ID: "agent-session"}

	browser := connectBrowser(t, httpServer.URL, artifactID, carol.ID)
	editAsBrowser(t, service, artifactID, browser, "# Decision\n\nContext.\n\nCarol's first paragraph.\n")

	tx, err := service.store.Pool.Begin(ctx)
	if err != nil {
		t.Fatalf("begin the agent's transaction: %v", err)
	}
	defer tx.Rollback(ctx)
	joined, ledger := service.Join(ctx, tx)
	defer ledger.Discard()
	if _, err := service.ApplyOps(joined, artifactID, []model.EditOp{{Op: "insert", After: "end", Markdown: "Agent's paragraph.\n"}}, agent, nil); err != nil {
		t.Fatalf("apply the agent's edit: %v", err)
	}
	if _, err := service.SnapshotVersion(joined, artifactID, agent); err != nil {
		t.Fatalf("snapshot the agent's version: %v", err)
	}
	editAsBrowser(t, service, artifactID, browser, "# Decision\n\nContext.\n\nCarol's first paragraph.\n\nCarol's second paragraph.\n")
	if err := ledger.Commit(ctx); err != nil {
		t.Fatalf("commit the agent's transaction: %v", err)
	}

	requireOwedInRoomAndRow(t, service, artifactID, carol)
}

// An upload's version credits its uploader alone, but its write may have changed or removed any
// edit visible as of its last read of the room, so its release clears every pending author the
// room held at that point, not just the uploader - and the durable row must agree, not keep a
// browser's credit the room has already let go: a stale row entry resurrects into the room on its
// next load (onLoadDocument, mergeSettlementCreditLocked). LEGION-513.
func TestAnUploadsFullReleaseClearsTheDurableRowOfAPendingAuthorItWasNeverCreditedWith(t *testing.T) {
	service, artifactID := newTestService(t)
	service.settle = time.Hour // no settlement runs between the edit and the upload
	ctx := context.Background()
	seedServiceText(t, service, artifactID, "First.\n\nSecond.\n")
	httpServer := httptest.NewServer(http.HandlerFunc(service.ServeHTTP))
	t.Cleanup(httpServer.Close)
	bob := model.Actor{Kind: "user", ID: "bob"}
	uploader := model.Actor{Kind: "session", ID: "uploader-session"}

	browser := connectBrowser(t, httpServer.URL, artifactID, bob.ID)
	editAsBrowser(t, service, artifactID, browser, "First.\n\nSecond, bob.\n")
	t.Cleanup(browser.Close)
	waitCtx, waitCancel := context.WithTimeout(context.Background(), 10*time.Second)
	if err := service.waitForDurableAppends(waitCtx, artifactID); err != nil {
		waitCancel()
		t.Fatalf("wait for bob's edit to become durable: %v", err)
	}
	waitCancel()
	if owed, credit, err := pendingSettlementCredit(context.Background(), service.store.Pool, artifactID); err != nil {
		t.Fatalf("read the pending-settlement row before the upload: %v", err)
	} else if !owed || len(credit.Pending) == 0 {
		t.Fatalf("the row owes nothing after bob's edit (owed=%v, pending=%+v); the test's premise needs his credit durable first", owed, credit.Pending)
	}

	tx, err := service.store.Pool.Begin(ctx)
	if err != nil {
		t.Fatalf("begin the upload transaction: %v", err)
	}
	defer tx.Rollback(ctx)
	joined, ledger := service.Join(ctx, tx)
	defer ledger.Discard()
	if _, err := service.ReplaceText(joined, artifactID, "First.\n\nSecond, uploaded.\n", uploader); err != nil {
		t.Fatalf("upload the replacement text: %v", err)
	}
	var nextNumber int
	if err := tx.QueryRow(ctx, `select coalesce(max(number), 0) + 1 from artifact_versions where artifact_id = $1`, artifactID).Scan(&nextNumber); err != nil {
		t.Fatalf("read the next version number: %v", err)
	}
	if _, err := tx.Exec(ctx, `
		insert into artifact_versions (artifact_id, number, markdown, authors)
		values ($1, $2, $3, $4)
	`, artifactID, nextNumber, "First.\n\nSecond, uploaded.\n", []model.Actor{uploader}); err != nil {
		t.Fatalf("insert the upload's version row: %v", err)
	}
	ledger.WroteVersion(artifactID, model.Version{Number: nextNumber, Authors: []model.Actor{uploader}})
	if err := ledger.Commit(ctx); err != nil {
		t.Fatalf("commit the upload transaction: %v", err)
	}

	ctx2, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	if err := service.waitForDurableAppends(ctx2, artifactID); err != nil {
		t.Fatalf("wait for the room's updates to become durable: %v", err)
	}
	room := map[string]model.Actor{}
	if state := service.lockExistingState(artifactID); state != nil {
		for _, entry := range state.pending {
			room[settlementCreditKey(entry.actor)] = entry.actor
		}
		service.unlockState(artifactID, state)
	}
	_, credit, err := pendingSettlementCredit(ctx2, service.store.Pool, artifactID)
	if err != nil {
		t.Fatalf("read the pending-settlement row: %v", err)
	}
	row := credit.Pending
	if row == nil {
		row = map[string]model.Actor{}
	}
	if len(room) != 0 {
		t.Fatalf("room pending = %+v after the upload's full release, want empty", room)
	}
	if len(row) != 0 {
		t.Fatalf("durable row pending = %+v after the upload's full release, want empty: bob's credit survived although the room released it", row)
	}
}

// releaseSettlementCredit's full release takes out only an entry whose own pending_seq is at or
// before the release's point: one credited earlier survives if it is swept, one credited later
// does not survive if it is kept. A controlled, direct probe of the SQL itself, with no room or
// ledger involved - Rev's finding named this scenario exactly (LEGION-513).
func TestReleaseSettlementCreditKeepsAnEntryCreditedAfterItsOwnCapture(t *testing.T) {
	database := storetest.Open(t)
	artifactID := createDocuments(t, database, 1)[0]
	early := model.Actor{Kind: "session", ID: "early-writer"}
	late := model.Actor{Kind: "user", ID: "late-writer"}

	appendSettlementCredit(t, database, artifactID, creditAt(10, early), 0)
	appendSettlementCredit(t, database, artifactID, creditAt(50, late), 0)

	setup := readSettlementCredit(t, database, artifactID)
	if _, got := setup.Pending[settlementCreditKey(early)]; !got {
		t.Fatalf("setup: early missing before the release, pending = %+v", setup.Pending)
	}
	if _, got := setup.Pending[settlementCreditKey(late)]; !got {
		t.Fatalf("setup: late missing before the release, pending = %+v", setup.Pending)
	}

	ctx := context.Background()
	tx, err := database.Pool.Begin(ctx)
	if err != nil {
		t.Fatalf("begin the release transaction: %v", err)
	}
	defer tx.Rollback(ctx)
	if err := releaseSettlementCredit(ctx, tx, artifactID, nil, 20, true); err != nil {
		t.Fatalf("release every entry at or before sequence 20: %v", err)
	}
	if err := tx.Commit(ctx); err != nil {
		t.Fatalf("commit the release: %v", err)
	}

	after := readSettlementCredit(t, database, artifactID)
	if _, kept := after.Pending[settlementCreditKey(early)]; kept {
		t.Fatalf("early (credited at 10) survived a full release at 20, pending = %+v, want it swept", after.Pending)
	}
	if _, kept := after.Pending[settlementCreditKey(late)]; !kept {
		t.Fatalf("late (credited at 50) was swept by a full release at 20, pending = %+v, want it kept", after.Pending)
	}
}

// creditAt builds a credit naming one pending author at creditSeq, the entry's own PendingSeq
// (not the credit's aggregate CreditSeq, which appendSettlementCredit sets separately).
func creditAt(creditSeq uint64, actor model.Actor) settlementCredit {
	return settlementCreditFor(map[string]model.Actor{actorKey(actor): actor}, nil, creditSeq)
}

// An upload's forkLive reads the room's forkSeq without state.mu, well before its transaction
// commits and releases (livewrite.go's forkLive against Ledger.commit): a browser edit credited,
// and durably committed, inside that window is credited at a sequence the upload's own capture
// never saw. The upload's full release must not sweep it - the room's own release
// (releaseAllPendingLocked) already does not, since the entry's own creditSeq is newer than the
// upload's forkSeq - or the row diverges from the room in the opposite direction from round 15's
// bug: the room keeps the edit owed, but the row has wiped it (LEGION-513).
func TestAnUploadsFullReleaseKeepsABrowserEditCreditedWhileItsTransactionWasOpen(t *testing.T) {
	service, artifactID := newTestService(t)
	service.settle = time.Hour // no settlement runs during the race window
	ctx := context.Background()
	seedServiceText(t, service, artifactID, "First.\n\nSecond.\n")
	httpServer := httptest.NewServer(http.HandlerFunc(service.ServeHTTP))
	t.Cleanup(httpServer.Close)
	bob := model.Actor{Kind: "user", ID: "bob"}
	uploader := model.Actor{Kind: "session", ID: "uploader-session"}

	tx, err := service.store.Pool.Begin(ctx)
	if err != nil {
		t.Fatalf("begin the upload transaction: %v", err)
	}
	defer tx.Rollback(ctx)
	joined, ledger := service.Join(ctx, tx)
	defer ledger.Discard()
	if _, err := service.ReplaceText(joined, artifactID, "First.\n\nSecond, uploaded.\n", uploader); err != nil {
		t.Fatalf("upload the replacement text: %v", err)
	}

	// The upload's forkLive has already read a low forkSeq; its transaction is still open,
	// uncommitted. A real browser edit lands here: ygo applies it to the live room, which
	// credits it in-memory (creditContentChange) at a newer sequence the upload's forkSeq never
	// saw - editAsBrowser itself waits for that credit to land before it returns (round 12's own
	// fix). Its durable append cannot complete yet: it needs the document's advisory lock the
	// upload's own transaction already holds, so it queues behind this transaction's eventual
	// commit rather than racing it - waiting for it here would deadlock against the open
	// transaction below, so the only wait is the final one, after that commit.
	browser := connectBrowser(t, httpServer.URL, artifactID, bob.ID)
	t.Cleanup(browser.Close)
	editAsBrowser(t, service, artifactID, browser, "First.\n\nSecond, bob.\n")

	var nextNumber int
	if err := tx.QueryRow(ctx, `select coalesce(max(number), 0) + 1 from artifact_versions where artifact_id = $1`, artifactID).Scan(&nextNumber); err != nil {
		t.Fatalf("read the next version number: %v", err)
	}
	if _, err := tx.Exec(ctx, `
		insert into artifact_versions (artifact_id, number, markdown, authors)
		values ($1, $2, $3, $4)
	`, artifactID, nextNumber, "First.\n\nSecond, uploaded.\n", []model.Actor{uploader}); err != nil {
		t.Fatalf("insert the upload's version row: %v", err)
	}
	ledger.WroteVersion(artifactID, model.Version{Number: nextNumber, Authors: []model.Actor{uploader}})
	if err := ledger.Commit(ctx); err != nil {
		t.Fatalf("commit the upload transaction: %v", err)
	}

	ctx2, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	if err := service.waitForDurableAppends(ctx2, artifactID); err != nil {
		t.Fatalf("wait for the room's updates to become durable: %v", err)
	}
	room := map[string]model.Actor{}
	if state := service.lockExistingState(artifactID); state != nil {
		for _, entry := range state.pending {
			room[settlementCreditKey(entry.actor)] = entry.actor
		}
		service.unlockState(artifactID, state)
	}
	_, credit, err := pendingSettlementCredit(ctx2, service.store.Pool, artifactID)
	if err != nil {
		t.Fatalf("read the pending-settlement row: %v", err)
	}
	row := credit.Pending
	if row == nil {
		row = map[string]model.Actor{}
	}
	if _, owed := room[settlementCreditKey(bob)]; !owed {
		t.Fatalf("room pending = %+v after the race, want bob still owed - the upload's forkSeq never saw his edit", room)
	}
	if _, owed := row[settlementCreditKey(bob)]; !owed {
		t.Fatalf("durable row pending = %+v after the race, want bob still owed: the upload's full release wiped an entry credited after its own forkSeq", row)
	}
}

// A room's creditSeq restarts at zero on every load (a fresh roomState's fresh atomic.Uint64),
// but the row's released_through watermark and each pending entry's own credit_seq, from before
// the load, do not - unless resetSettlementCreditSequence resets them. Without that reset, a
// released_through left over from several versions' releases before the reload would make
// upsertSettlementCredit's gate discard a genuinely new, low-sequence credit as already consumed
// (LEGION-513).
func TestANewCreditAfterARoomReloadIsNotDiscardedByAStaleWatermark(t *testing.T) {
	service, artifactID := newTestService(t)
	service.settle = time.Hour
	seedServiceText(t, service, artifactID, "First.\n\nSecond.\n")
	httpServer := httptest.NewServer(http.HandlerFunc(service.ServeHTTP))
	t.Cleanup(httpServer.Close)
	alice := model.Actor{Kind: "user", ID: "alice"}

	// Drive several versioned releases, each raising the row's released_through, well past
	// what a freshly-restarted counter would reach right after a reload.
	for round := range 5 {
		agent := model.Actor{Kind: "session", ID: fmt.Sprintf("agent-%d", round)}
		writeThroughLedger(t, service, artifactID, agent, fmt.Sprintf("Round %d.\n", round), withSnapshotVersion)
	}
	waitCtx, waitCancel := context.WithTimeout(context.Background(), 10*time.Second)
	if err := service.waitForDurableAppends(waitCtx, artifactID); err != nil {
		waitCancel()
		t.Fatalf("wait for the setup rounds to become durable: %v", err)
	}
	waitCancel()

	if err := service.srv.CloseRoom(artifactID, true); err != nil {
		t.Fatalf("close the room: %v", err)
	}
	waitForNoLiveDocument(t, service, artifactID)

	browser := connectBrowser(t, httpServer.URL, artifactID, alice.ID)
	t.Cleanup(browser.Close)
	editAsBrowser(t, service, artifactID, browser, "First.\n\nSecond, alice.\n")
	ctx2, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	if err := service.waitForDurableAppends(ctx2, artifactID); err != nil {
		t.Fatalf("wait for alice's post-reload edit to become durable: %v", err)
	}
	_, credit, err := pendingSettlementCredit(ctx2, service.store.Pool, artifactID)
	if err != nil {
		t.Fatalf("read the pending-settlement row after reload: %v", err)
	}
	if _, owed := credit.Pending[settlementCreditKey(alice)]; !owed {
		t.Fatalf("pending settlement credit after a post-reload edit = %+v, want alice owed: a stale released_through from before the reload discarded her genuinely new credit", credit.Pending)
	}
}

// requireOwedInRoomAndRow requires the room's pending authors and its pending-settlement row's to
// be want alone, once every update the room has queued is durable.
func requireOwedInRoomAndRow(t *testing.T, service *Service, artifactID string, want model.Actor) {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	if err := service.waitForDurableAppends(ctx, artifactID); err != nil {
		t.Fatalf("wait for the room's updates to become durable: %v", err)
	}
	room := map[string]model.Actor{}
	if state := service.lockExistingState(artifactID); state != nil {
		for _, entry := range state.pending {
			room[settlementCreditKey(entry.actor)] = entry.actor
		}
		service.unlockState(artifactID, state)
	}
	_, credit, err := pendingSettlementCredit(ctx, service.store.Pool, artifactID)
	if err != nil {
		t.Fatalf("read the pending-settlement row: %v", err)
	}
	row := credit.Pending
	if row == nil {
		row = map[string]model.Actor{}
	}
	wantKeyed := map[string]model.Actor{settlementCreditKey(want): want}
	if !reflect.DeepEqual(room, wantKeyed) || !reflect.DeepEqual(row, wantKeyed) {
		t.Fatalf("room owes %v and the row owes %v, want both to owe %v alone", room, row, wantKeyed)
	}
}

// Across repeated rounds of a browser's edit and an agent's versioned edit racing each other, the
// room's pending authors and the durable pending-settlement row agree once every round's updates
// are durable: a version's release and a browser's credit can land in either order relative to
// each other, but neither ever leaves the room holding a credit the row does not, or the row
// holding one the room has already released - the exact window Deep's probe isolates for one
// round, repeated here under -race to catch a divergence neither the room-pending state nor the
// durable row alone would surface (LEGION-513).
func TestConcurrentBrowserAndVersionedWritesAgreeWithTheDurableRowAfterEveryRound(t *testing.T) {
	service, artifactID := newTestService(t)
	service.settle = time.Hour // no settlement runs between the edits
	seedServiceText(t, service, artifactID, "# Decision\n\nContext.\n")
	httpServer := httptest.NewServer(http.HandlerFunc(service.ServeHTTP))
	t.Cleanup(httpServer.Close)

	const rounds = 8
	for round := range rounds {
		login := fmt.Sprintf("round%d-browser", round)
		browser := connectBrowser(t, httpServer.URL, artifactID, login)
		agent := model.Actor{Kind: "session", ID: fmt.Sprintf("round%d-agent", round)}

		var wg sync.WaitGroup
		wg.Add(2)
		go func() {
			defer wg.Done()
			editAsBrowser(t, service, artifactID, browser, fmt.Sprintf("# Decision\n\nContext.\n\nRound %d browser.\n", round))
		}()
		go func() {
			defer wg.Done()
			writeThroughLedger(t, service, artifactID, agent, fmt.Sprintf("Round %d agent.\n", round), withSnapshotVersion)
		}()
		wg.Wait()
		browser.Close()
		waitFor(t, 10*time.Second, "the round's browser to disconnect", func() bool {
			return connectedBrowsers(service, artifactID) == 0
		})

		ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
		err := service.waitForDurableAppends(ctx, artifactID)
		cancel()
		if err != nil {
			t.Fatalf("round %d: wait for durable appends: %v", round, err)
		}

		room := map[string]model.Actor{}
		if state := service.lockExistingState(artifactID); state != nil {
			for _, entry := range state.pending {
				room[settlementCreditKey(entry.actor)] = entry.actor
			}
			service.unlockState(artifactID, state)
		}
		_, credit, err := pendingSettlementCredit(context.Background(), service.store.Pool, artifactID)
		if err != nil {
			t.Fatalf("round %d: read the pending-settlement row: %v", round, err)
		}
		row := credit.Pending
		if row == nil {
			row = map[string]model.Actor{}
		}
		if !reflect.DeepEqual(room, row) {
			t.Fatalf("round %d: room pending = %+v, durable row = %+v, want equal", round, room, row)
		}
	}
}

// A document update that carries no credit, such as a browser change that renders nothing new,
// keeps every pending author already recorded for the document's settlement.
func TestSettlementCreditKeepsExistingPendingAcrossUncreditedAppend(t *testing.T) {
	database := storetest.Open(t)
	artifactID := createDocuments(t, database, 1)[0]
	writer := model.Actor{Kind: "session", ID: "credited-writer"}

	appendSettlementCredit(t, database, artifactID, creditOf(&writer, writer), 0)
	appendSettlementCredit(t, database, artifactID, settlementCredit{}, 0)

	credit := readSettlementCredit(t, database, artifactID)
	if _, kept := credit.Pending[settlementCreditKey(writer)]; !kept {
		t.Fatalf("pending settlement credit after an uncredited append = %+v, want credited actor %+v retained", credit, writer)
	}
}

// Each append merges its credit into the document's pending-settlement row: pending authors
// accumulate, a later credit for the same actor replaces the earlier one, and a credit naming
// pending authors carries the room's last actor, none after an edit no one actor can be credited
// with, while an append with no credit keeps the row's.
func TestSettlementCreditMergesPendingAuthors(t *testing.T) {
	alice := model.Actor{Kind: "session", ID: "alice"}
	bob := model.Actor{Kind: "user", ID: "bob"}
	bobFromLaptop := model.Actor{Kind: "user", ID: "bob", Origin: &model.ActorOrigin{Host: "laptop"}}
	carol := model.Actor{Kind: "session", ID: "carol"}
	cases := []struct {
		name     string
		existing settlementCredit
		// storedAuthors, when set, replaces what the existing append stored, as a row written by
		// another build - or a null pending an uncredited append could leave - can hold.
		storedAuthors string
		next          settlementCredit
		want          settlementCredit
	}{
		{name: "empty then empty", want: creditOf(nil)},
		{name: "existing then none", existing: creditOf(&alice, alice, bob), want: creditOf(&alice, alice, bob)},
		{name: "none then new", next: creditOf(&carol, carol), want: creditOf(&carol, carol)},
		{
			name:     "existing then new with an overlapping actor",
			existing: creditOf(&alice, alice, bob),
			next:     creditOf(&carol, bobFromLaptop, carol),
			want:     creditOf(&carol, alice, bobFromLaptop, carol),
		},
		{
			name:     "existing then authors no one actor wrote",
			existing: creditOf(&alice, alice),
			next:     creditOf(nil, bob, carol),
			want:     creditOf(nil, alice, bob, carol),
		},
		{
			name:          "a stored null pending then new",
			storedAuthors: `{"pending": null, "last_actor": {"kind": "session", "id": "alice"}}`,
			next:          creditOf(&carol, carol),
			want:          creditOf(&carol, carol),
		},
	}
	database := storetest.Open(t)
	artifactIDs := createDocuments(t, database, len(cases))
	for index, test := range cases {
		t.Run(test.name, func(t *testing.T) {
			artifactID := artifactIDs[index]
			appendSettlementCredit(t, database, artifactID, test.existing, 0)
			if test.storedAuthors != "" {
				if _, err := database.Pool.Exec(context.Background(), `
					update doc_settlements_pending set settlement_authors = $2::jsonb where artifact_id = $1
				`, artifactID, test.storedAuthors); err != nil {
					t.Fatalf("store the existing settlement authors: %v", err)
				}
			}
			appendSettlementCredit(t, database, artifactID, test.next, 0)

			got := readSettlementCredit(t, database, artifactID)
			if !maps.EqualFunc(got.Pending, test.want.Pending, func(a, b model.Actor) bool { return reflect.DeepEqual(a, b) }) ||
				!reflect.DeepEqual(got.LastActor, test.want.LastActor) {
				t.Fatalf("merged settlement credit = %+v (last actor %+v), want %+v (last actor %+v)",
					got.Pending, got.LastActor, test.want.Pending, test.want.LastActor)
			}
		})
	}
}

func creditOf(lastActor *model.Actor, pending ...model.Actor) settlementCredit {
	authors := make(map[string]model.Actor, len(pending))
	for _, actor := range pending {
		authors[actorKey(actor)] = actor
	}
	return settlementCreditFor(authors, lastActor, 0)
}

// appendSettlementCredit records credit as a document update's own transaction does. creditSeq is
// the room's creditSeq the credit was captured at (0 when the test does not exercise the
// release-gating watermark).
func appendSettlementCredit(t *testing.T, database *store.Store, artifactID string, credit settlementCredit, creditSeq uint64) {
	t.Helper()
	ctx := context.Background()
	tx, err := database.Pool.Begin(ctx)
	if err != nil {
		t.Fatalf("begin the append: %v", err)
	}
	defer tx.Rollback(ctx)
	credit.CreditSeq = creditSeq
	if err := markSettlementPending(ctx, tx, artifactID, credit); err != nil {
		t.Fatalf("record the append's settlement credit: %v", err)
	}
	if err := tx.Commit(ctx); err != nil {
		t.Fatalf("commit the append: %v", err)
	}
}

func readSettlementCredit(t *testing.T, database *store.Store, artifactID string) settlementCredit {
	t.Helper()
	owed, credit, err := pendingSettlementCredit(context.Background(), database.Pool, artifactID)
	if err != nil {
		t.Fatalf("read the pending settlement credit: %v", err)
	}
	if !owed {
		t.Fatal("the document owes no settlement after an append")
	}
	return credit
}

type cancelAfterCommitTx struct {
	pgx.Tx
	cancel context.CancelFunc
}

func (tx cancelAfterCommitTx) Commit(ctx context.Context) error {
	err := tx.Tx.Commit(ctx)
	if err == nil {
		tx.cancel()
	}
	return err
}

func writeAskBeforeClose(t *testing.T, service *Service, artifactID string, actor model.Actor) {
	t.Helper()
	writeThroughLedger(t, service, artifactID, actor, ":::ask{#closing-ask urgency=\"med\" multiple=\"false\"}\nWho is asking?\n:::\n", withoutVersion)
	service.ScheduleSettlement(artifactID)
}

func closeTestIssue(t *testing.T, service *Service) {
	t.Helper()
	ctx := context.Background()
	if _, err := service.store.Pool.Exec(ctx, `update issues set closed_at = now() where key = 'DOC-1'`); err != nil {
		t.Fatalf("close the issue: %v", err)
	}
	service.SetIssueClosed(ctx, "DOC-1", true)
	waitFor(t, 30*time.Second, "the closed issue's room to close", func() bool {
		return len(service.srv.Rooms()) == 0
	})
}

func reopenTestIssue(t *testing.T, service *Service) {
	t.Helper()
	ctx := context.Background()
	if _, err := service.store.Pool.Exec(ctx, `update issues set closed_at = null where key = 'DOC-1'`); err != nil {
		t.Fatalf("reopen the issue: %v", err)
	}
	service.SetIssueClosed(ctx, "DOC-1", false)
}

func waitForAskAuthor(t *testing.T, service *Service, artifactID string, want model.Actor) {
	t.Helper()
	ctx := context.Background()
	var author model.Actor
	waitFor(t, 30*time.Second, "the reopened document's settlement to index the ask", func() bool {
		return service.store.Pool.QueryRow(ctx, `select author from asks where block_artifact_id = $1 and block_id = 'closing-ask'`, artifactID).Scan(&author) == nil
	})
	if author != want {
		t.Fatalf("the ask block %s wrote just before its issue closed is indexed as %+v's after the reopen, want %+v's", want.ID, author, want)
	}
}
