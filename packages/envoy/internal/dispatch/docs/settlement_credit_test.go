package docs

import (
	"context"
	"fmt"
	"maps"
	"reflect"
	"runtime"
	"testing"
	"time"

	"github.com/jackc/pgx/v5"

	"github.com/sjawhar/envoy/internal/dispatch/events"
	"github.com/sjawhar/envoy/internal/dispatch/model"
	"github.com/sjawhar/envoy/internal/dispatch/store"
	"github.com/sjawhar/envoy/internal/dispatch/store/storetest"
)

// An ask block a session writes just before its issue closes - inside the settle delay, so no
// settlement indexed it yet - is still that session's ask once the issue reopens and the document
// settles: the asking session can reword it, and readers can tell who asked (AGENTC-150).
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
// could store, still lets its document load and settle, crediting the row's last actor.
func TestADocumentLoadsAPendingSettlementRowWithNullPendingAuthors(t *testing.T) {
	service, artifactID := newTestService(t)
	service.settle = time.Hour // the seed's own settlement never runs
	ctx := context.Background()
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
	if author != writer {
		t.Fatalf("the ask settled from a row with null pending authors is %+v's, want %+v's", author, writer)
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

	carol := model.Actor{Kind: "session", ID: "carol-session"}
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

	// Carol's direct write takes the room path a browser edit takes (creditContentChange), while
	// the agent's transaction still holds the document's advisory lock, so her durable append -
	// which the room's persistence worker drives asynchronously - queues behind it.
	if _, err := service.ReplaceText(context.Background(), artifactID, "# Decision\n\nContext.\n\nCarol's paragraph.\n", carol); err != nil {
		t.Fatalf("replace text as carol: %v", err)
	}
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
	return settlementCreditFor(authors, lastActor)
}

// appendSettlementCredit records credit as a document update's own transaction does. sequence is
// the room's creditVersion the credit was captured at (0 when the test does not exercise the
// release-gating watermark).
func appendSettlementCredit(t *testing.T, database *store.Store, artifactID string, credit settlementCredit, sequence uint64) {
	t.Helper()
	ctx := context.Background()
	tx, err := database.Pool.Begin(ctx)
	if err != nil {
		t.Fatalf("begin the append: %v", err)
	}
	defer tx.Rollback(ctx)
	if err := markSettlementPending(ctx, tx, artifactID, credit, sequence); err != nil {
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
