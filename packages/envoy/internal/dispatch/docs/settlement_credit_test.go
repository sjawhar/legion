package docs

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"reflect"
	"runtime"
	"slices"
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
// document's pending settlement naming the earlier seed author instead of this committed edit's
// author, or its pending authors without this edit's author.
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

	owed, lastActor, err := readOwedSettlement(context.Background(), service.store.Pool, artifactID)
	if err != nil {
		t.Fatalf("read the document's pending settlement: %v", err)
	}
	if !owed || lastActor == nil || *lastActor != actor {
		t.Fatalf("pending settlement after the canceled-context commit names %+v (owed %t), want last actor %+v", lastActor, owed, actor)
	}
	if pending := pendingAuthorRows(t, service.store, artifactID); !slices.Contains(pending, actor) {
		t.Fatalf("pending authors after the canceled-context commit = %+v, want %+v", pending, actor)
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
		owed, err := settlementPending(context.Background(), service.store.Pool, artifactID)
		return err == nil && !owed
	})

	closeTestIssue(t, service)
	owed, err := settlementPending(context.Background(), service.store.Pool, artifactID)
	if err != nil {
		t.Fatalf("read settlement row after close: %v", err)
	}
	if owed {
		t.Fatal("closing an already settled document recreated a pending settlement from its consumed last actor")
	}
}

// A pending-settlement row naming a latest edit source other than an ask block's own author still
// lets its document load and settle, and the ask settles to the author its own introducing write
// registered (registerAskAuthors), here the seed: the room tracks that independently of the row,
// and it takes precedence over the row's last_actor, the fallback for a block no write registered
// (LEGION-503).
func TestADocumentLoadsAPendingSettlementRowNamingAnotherLastActor(t *testing.T) {
	service, artifactID := newTestService(t)
	service.settle = time.Hour // the seed's own settlement never runs
	ctx := context.Background()
	seed := model.Actor{Kind: "user", ID: "seed"}
	seedServiceText(t, service, artifactID, ":::ask{#null-pending-ask urgency=\"med\" multiple=\"false\"}\nWho asked?\n:::\n")
	writer := model.Actor{Kind: "session", ID: "null-pending-writer"}
	if _, err := service.store.Pool.Exec(ctx, `
		update doc_settlements_pending set last_actor = $2::jsonb where artifact_id = $1
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
		t.Fatalf("the ask settled from a row naming another last actor is %+v's, want %+v's, its seed", author, seed)
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
		owed, err := settlementPending(ctx, service.store.Pool, artifactID)
		return err == nil && !owed
	})
	if authors := latestVersionAuthors(t, service, artifactID); !reflect.DeepEqual(authors, []model.Actor{first, second}) {
		t.Fatalf("resumed settlement version authors = %+v, want %+v and %+v", authors, first, second)
	}
}

// The issue route closes an issue's rooms on its request's context, which the pool tracks as one
// caller holding at most one connection. Every closing document still records its latest edit
// source and releases its state, however many documents the issue has.
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
				service.recordActor(t, id, writer)
			}
			if _, err := database.Pool.Exec(ctx, `update issues set closed_at = now() where key = 'DOC-1'`); err != nil {
				t.Fatalf("close the documents' issue: %v", err)
			}
			// What PATCH /api/v1/issues/{key} hands it (api/issue_patch.go).
			service.SetIssueClosed(context.WithoutCancel(store.WithTransactionTracking(ctx)), "DOC-1", true)

			var persisted int
			if err := database.Pool.QueryRow(ctx, `
				select count(*) from doc_settlements_pending
				where artifact_id::text = any($1) and last_actor is not null
			`, ids).Scan(&persisted); err != nil {
				t.Fatalf("read the documents' latest edit sources: %v", err)
			}
			retained := 0
			service.rooms.Range(func(_, _ any) bool {
				retained++
				return true
			})
			if persisted != len(ids) || retained != 0 {
				t.Fatalf("closing %d documents persisted %d latest edit sources and kept %d states, want %d and 0",
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
		owed, err := settlementPending(context.Background(), service.store.Pool, artifactID)
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

// A browser edit the room observes while an API edit's own version is still open is listed only
// once. The version reads carol's in-flight credit (the room holds it from the moment it observes
// her edit, before her own durable append runs) and marks it consumed as it commits; carol's own
// durable append - queued behind the version's transaction, which holds the document's advisory
// lock for its whole duration - then finds it consumed and records nothing. Without that, the
// reopened document's next version would list carol again alongside delta.
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

// Two connected peers' edits around an open version are each listed once: carol's edit, which the
// agent's version read and marks consumed, does not ride back in on dave's edit made while that
// version is still open. A browser edit's credit names the peers connected for it
// (state.connected), never another edit's authors.
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

// A browser edit made after an open version read its authors, and before that version commits,
// stays owed: the version deletes the pending authors it read and marks the in-flight credits it
// read consumed, and carol's later edit is neither - its append, queued behind the version's lock,
// records her once the version has committed.
func TestABrowserEditAfterAVersionsCaptureStaysOwed(t *testing.T) {
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

	requireOwedAlone(t, service, artifactID, carol)
}

// An upload's version lists its uploader alone, but its write may have changed or removed any
// edit its read of the room held, so its commit clears every author owed before that read, not
// just the uploader: bob's landed edit owes nothing once the upload commits (LEGION-513).
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
	if owed := pendingAuthorRows(t, service.store, artifactID); !slices.Contains(owed, bob) {
		t.Fatalf("pending authors before the upload = %+v; the test's premise needs bob's edit durable first", owed)
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

	waitForLandedAppends(t, service, artifactID)
	if owed := pendingAuthors(t, service, artifactID); len(owed) != 0 {
		t.Fatalf("pending authors after the upload = %+v, want none: its read of the room held bob's edit", owed)
	}
}

// An upload's version clears only what its read of the room held (liveWrite.forkSeq): a browser
// edit the room observes after that read, while the upload's transaction is still open, stays
// owed once the upload commits. The upload holds the document's advisory lock from before that
// read, so the edit's append lands only after the commit and records bob then (LEGION-513).
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
	// saw - editAsBrowser itself waits for that credit to land before it returns. Its durable
	// append cannot complete yet: it needs the document's advisory lock the
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

	waitForLandedAppends(t, service, artifactID)
	if owed := pendingAuthorRows(t, service.store, artifactID); !slices.Contains(owed, bob) {
		t.Fatalf("pending authors after the upload = %+v, want bob: the upload's read of the room never held his edit", owed)
	}
}

// newTestServiceInstance is one more *Service sharing database with whatever other instances a
// test already built: a second (or third) Dispatch task's own process, for tests that model a
// rolling deploy's overlap.
func newTestServiceInstance(t *testing.T, database *store.Store) (*Service, *httptest.Server) {
	t.Helper()
	service := New(Deps{
		Store:     database,
		Events:    events.NewBroker(),
		Identity:  headerIdentity(database),
		ServerURL: "https://dispatch.example",
		Settle:    time.Hour,
	})
	t.Cleanup(func() {
		if err := service.Shutdown(context.Background()); err != nil {
			t.Errorf("shutdown document service: %v", err)
		}
	})
	server := httptest.NewServer(http.HandlerFunc(service.ServeHTTP))
	t.Cleanup(server.Close)
	return service, server
}

// versionAuthors is the authors artifactID's version number credits.
func versionAuthors(t *testing.T, database *store.Store, artifactID string, number int) []model.Actor {
	t.Helper()
	var raw []byte
	if err := database.Pool.QueryRow(context.Background(), `
		select authors from artifact_versions where artifact_id = $1 and number = $2
	`, artifactID, number).Scan(&raw); err != nil {
		t.Fatalf("read version %d's authors: %v", number, err)
	}
	var authors []model.Actor
	if err := json.Unmarshal(raw, &authors); err != nil {
		t.Fatalf("decode version %d's authors: %v", number, err)
	}
	return authors
}

// nextVersionNumber is the number writeThroughLedger's next call on artifactID will take.
func nextVersionNumber(t *testing.T, database *store.Store, artifactID string) int {
	t.Helper()
	var number int
	if err := database.Pool.QueryRow(context.Background(), `
		select coalesce(max(number), 0) + 1 from artifact_versions where artifact_id = $1
	`, artifactID).Scan(&number); err != nil {
		t.Fatalf("read the next version number: %v", err)
	}
	return number
}

// A browser edit made after its room reloads is owed like any other: the reloaded room starts its
// own in-flight credits afresh, and the edit's append records its author.
func TestABrowserEditAfterARoomReloadIsOwed(t *testing.T) {
	service, artifactID := newTestService(t)
	service.settle = time.Hour
	seedServiceText(t, service, artifactID, "First.\n\nSecond.\n")
	httpServer := httptest.NewServer(http.HandlerFunc(service.ServeHTTP))
	t.Cleanup(httpServer.Close)
	alice := model.Actor{Kind: "user", ID: "alice"}

	// Several versioned writes before the reload, each taking its own author out of what the
	// document owes.
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
	waitForLandedAppends(t, service, artifactID)
	if owed := pendingAuthorRows(t, service.store, artifactID); !slices.Contains(owed, alice) {
		t.Fatalf("pending authors after a post-reload edit = %+v, want alice", owed)
	}
}

// requireOwedAlone requires want to be the only author the document owes once every update the
// room has queued is durable.
func requireOwedAlone(t *testing.T, service *Service, artifactID string, want model.Actor) {
	t.Helper()
	waitForLandedAppends(t, service, artifactID)
	if owed := pendingAuthors(t, service, artifactID); !reflect.DeepEqual(owed, []model.Actor{want}) {
		t.Fatalf("pending authors = %+v, want %+v alone", owed, want)
	}
}

// Across repeated rounds of a browser's edit and an agent's versioned edit racing each other,
// every author is in exactly one place once each round's updates are durable: listed on one
// committed version, or owed. A version's commit and a browser's append can land in either order,
// and neither lists an author twice nor loses one (LEGION-513).
func TestConcurrentBrowserAndVersionedWritesListEveryAuthorOnceAfterEveryRound(t *testing.T) {
	service, artifactID := newTestService(t)
	service.settle = time.Hour // no settlement runs between the edits
	seedServiceText(t, service, artifactID, "# Decision\n\nContext.\n")
	httpServer := httptest.NewServer(http.HandlerFunc(service.ServeHTTP))
	t.Cleanup(httpServer.Close)

	var authors []model.Actor
	const rounds = 8
	for round := range rounds {
		login := fmt.Sprintf("round%d-browser", round)
		browser := connectBrowser(t, httpServer.URL, artifactID, login)
		agent := model.Actor{Kind: "session", ID: fmt.Sprintf("round%d-agent", round)}
		authors = append(authors, model.Actor{Kind: "user", ID: login}, agent)

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
		waitForLandedAppends(t, service, artifactID)

		owed := pendingAuthors(t, service, artifactID)
		for _, author := range authors {
			listed := countVersionAuthor(t, service.store, artifactID, author)
			isOwed := slices.Contains(owed, author)
			if places := listed + map[bool]int{true: 1}[isOwed]; places != 1 {
				t.Fatalf("round %d: %s is listed on %d versions and owed %t, want exactly one of the two", round, author.ID, listed, isOwed)
			}
		}
	}
}

// Each browser append records its credit in the document's pending authors and pending-settlement
// row: authors accumulate, a later credit for the same actor replaces the earlier one, a credit
// naming authors sets the row's latest edit source to its own, none after an edit no one actor can
// be credited with, and an append no one is credited with keeps both.
func TestAnAppendRecordsItsCreditInThePendingAuthors(t *testing.T) {
	alice := model.Actor{Kind: "session", ID: "alice"}
	bob := model.Actor{Kind: "user", ID: "bob"}
	bobFromLaptop := model.Actor{Kind: "user", ID: "bob", Origin: &model.ActorOrigin{Host: "laptop"}}
	carol := model.Actor{Kind: "session", ID: "carol"}
	cases := []struct {
		name           string
		existing, next *testCredit
		want           []model.Actor
		wantLastActor  *model.Actor
	}{
		{name: "uncredited then uncredited"},
		{name: "existing then uncredited", existing: creditOf(&alice, alice, bob), want: []model.Actor{alice, bob}, wantLastActor: &alice},
		{name: "uncredited then new", next: creditOf(&carol, carol), want: []model.Actor{carol}, wantLastActor: &carol},
		{
			name:          "existing then new with an overlapping actor",
			existing:      creditOf(&alice, alice, bob),
			next:          creditOf(&carol, bobFromLaptop, carol),
			want:          []model.Actor{alice, bobFromLaptop, carol},
			wantLastActor: &carol,
		},
		{
			name:     "existing then authors no one actor wrote",
			existing: creditOf(&alice, alice),
			next:     creditOf(nil, bob, carol),
			want:     []model.Actor{alice, bob, carol},
		},
	}
	database := storetest.Open(t)
	artifactIDs := createDocuments(t, database, len(cases))
	service, _ := newTestServiceInstance(t, database)
	for index, test := range cases {
		t.Run(test.name, func(t *testing.T) {
			artifactID := artifactIDs[index]
			appendCredited(t, service, artifactID, test.existing)
			appendCredited(t, service, artifactID, test.next)

			owed, lastActor, err := readOwedSettlement(context.Background(), service.store.Pool, artifactID)
			if err != nil || !owed {
				t.Fatalf("the document owes a settlement = %t (%v) after two appends, want true", owed, err)
			}
			got := make(map[string]model.Actor)
			for _, author := range pendingAuthorRows(t, service.store, artifactID) {
				got[actorKey(author)] = author
			}
			want := make(map[string]model.Actor)
			for _, author := range test.want {
				want[actorKey(author)] = author
			}
			if !reflect.DeepEqual(got, want) || !reflect.DeepEqual(lastActor, test.wantLastActor) {
				t.Fatalf("pending authors = %+v (last actor %+v), want %+v (last actor %+v)", got, lastActor, want, test.wantLastActor)
			}
		})
	}
}

// testCredit is the authors and latest edit source of one browser edit's in-flight credit.
type testCredit struct {
	lastActor *model.Actor
	authors   []model.Actor
}

func creditOf(lastActor *model.Actor, authors ...model.Actor) *testCredit {
	return &testCredit{lastActor: lastActor, authors: authors}
}

// appendCredited appends an update to artifactID through the store as a room's persistence does
// (AppendUpdateWithCredit), with an in-flight credit for credited, or none when it is nil.
func appendCredited(t *testing.T, service *Service, artifactID string, credited *testCredit) {
	t.Helper()
	var credit *UpdateCredit
	if credited != nil {
		state := service.lockState(artifactID)
		record := &inflightCredit{seq: state.creditSeq.Add(1), authors: make(map[string]model.Actor), lastActor: credited.lastActor}
		for _, author := range credited.authors {
			record.authors[actorKey(author)] = author
		}
		state.inflight[record.seq] = record
		service.unlockState(artifactID, state)
		credit = &UpdateCredit{service: service, room: artifactID, state: state, record: record}
	}
	doc := crdt.New()
	doc.GetXmlFragment(fragmentName)
	if _, err := service.persistence.AppendUpdateWithCredit(context.Background(), artifactID, crdt.EncodeStateAsUpdateV1(doc, nil), true, credit); err != nil {
		t.Fatalf("append the update: %v", err)
	}
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
