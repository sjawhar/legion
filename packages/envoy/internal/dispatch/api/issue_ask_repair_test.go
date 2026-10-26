package api

import (
	"context"
	"fmt"
	"log/slog"
	"net/http"
	"reflect"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/sjawhar/envoy/internal/dispatch/docs"
	"github.com/sjawhar/envoy/internal/dispatch/model"
	"github.com/sjawhar/envoy/internal/dispatch/store"
	"github.com/sjawhar/envoy/internal/dispatch/store/storetest"
)

// The repair pass (RepairIssueAsks) brings the asks of an issue a release older than the
// withdrawal closed or reopened to what the close or the reopen leaves them: withdrawn by its
// latest closer, restored by its reopener, and, on a closed issue whose document another
// process's room wrote, the close's final sweep.

// repairFixture is a server the repair runs against, with the dependencies it is handed.
type repairFixture struct {
	handler   http.Handler
	database  *store.Store
	documents *docs.Service
	deps      Deps
}

func newRepairFixture(t *testing.T) repairFixture {
	t.Helper()
	return newRepairFixtureSettlingAfter(t, 20*time.Millisecond)
}

// newRepairFixtureSettlingAfter is newRepairFixture with a document service whose settle delay is
// settle.
func newRepairFixtureSettlingAfter(t *testing.T, settle time.Duration) repairFixture {
	t.Helper()
	database := storetest.Open(t)
	documents := docs.New(docs.Deps{Store: database, Identity: headerIdentity(database), Settle: settle})
	t.Cleanup(func() { _ = documents.Shutdown(context.Background()) })
	seedPeople(t, database, "alice", "bob")
	deps, err := NewDeps(DepsInput{
		Store:       database,
		Identity:    headerIdentity(database),
		AgentTokens: sharedAgentTokens(t, "agent-token"),
		Docs:        documents,
		ServerURL:   "https://dispatch.example",
	})
	if err != nil {
		t.Fatalf("new API dependencies: %v", err)
	}
	mux := http.NewServeMux()
	Register(mux, deps)
	return repairFixture{handler: mux, database: database, documents: documents, deps: deps}
}

// repairActor is a session actor with every field an actor carries, so a record naming it proves
// the repair kept each one.
func repairActor(id string) model.Actor {
	owner, service := "alice", "legion"
	return model.Actor{
		Kind: "session",
		ID:   id,
		Origin: &model.ActorOrigin{
			Host: "devbox", Machine: "machine-1", Cwd: "/work", Tmux: "main", Pane: "%1", SessionTitle: "Closer",
		},
		Owner:   &owner,
		Service: &service,
	}
}

// closeWithoutWithdrawing closes issue as a release older than the withdrawal did: the issue's
// row reads done and closed at closedAt, an issue.closed event names actor, and every ask and
// block is left as it was. With no actor it appends no event.
func closeWithoutWithdrawing(t *testing.T, f repairFixture, issue string, actor *model.Actor, closedAt time.Time) {
	t.Helper()
	ctx := store.WithTransactionTracking(context.Background())
	if _, err := f.database.Pool.Exec(ctx, `update issues set status = 'done', closed_at = $2, updated_at = now() where key = $1`, issue, closedAt); err != nil {
		t.Fatalf("close %s: %v", issue, err)
	}
	if actor != nil {
		appendIssueEvent(t, f, issue, "issue.closed", "done", *actor)
	}
	f.documents.SetIssueClosed(ctx, issue, true)
}

// reopenWithoutRestoring reopens issue as a release older than the withdrawal did: the issue's
// row reads status, an issue.updated event names actor, and its withdrawn asks stay withdrawn.
func reopenWithoutRestoring(t *testing.T, f repairFixture, issue, status string, actor model.Actor) {
	t.Helper()
	ctx := store.WithTransactionTracking(context.Background())
	if _, err := f.database.Pool.Exec(ctx, `update issues set status = $2, closed_at = null, updated_at = now() where key = $1`, issue, status); err != nil {
		t.Fatalf("reopen %s: %v", issue, err)
	}
	appendIssueEvent(t, f, issue, "issue.updated", status, actor)
	f.documents.SetIssueClosed(ctx, issue, false)
}

// appendIssueEvent appends an eventType event of issue naming actor, its payload the issue's
// key and status.
func appendIssueEvent(t *testing.T, f repairFixture, issue, eventType, status string, actor model.Actor) {
	t.Helper()
	ctx := store.WithTransactionTracking(context.Background())
	tx, err := f.database.Pool.Begin(ctx)
	if err != nil {
		t.Fatalf("begin the %s event: %v", eventType, err)
	}
	defer tx.Rollback(ctx)
	if _, err := f.deps.Events.Append(ctx, tx, issueOwner(issue).event(eventType, actor, map[string]any{"key": issue, "status": status})); err != nil {
		t.Fatalf("append the %s event of %s: %v", eventType, issue, err)
	}
	if err := tx.Commit(ctx); err != nil {
		t.Fatalf("commit the %s event of %s: %v", eventType, issue, err)
	}
}

// eventCount counts every event of issue.
func eventCount(t *testing.T, database *store.Store, issue string) int {
	t.Helper()
	var count int
	if err := database.Pool.QueryRow(context.Background(), `select count(*) from events where issue_key = $1`, issue).Scan(&count); err != nil {
		t.Fatalf("count the events of %s: %v", issue, err)
	}
	return count
}

func repairNow(t *testing.T, f repairFixture) IssueAskRepair {
	t.Helper()
	repair, err := RepairIssueAsks(context.Background(), f.deps, nil)
	if err != nil {
		t.Fatalf("repair: %v", err)
	}
	return repair
}

// captureLogs records what the default logger writes until the test ends.
func captureLogs(t *testing.T) *storetest.LockedLog {
	t.Helper()
	logs := &storetest.LockedLog{}
	previous := slog.Default()
	slog.SetDefault(slog.New(slog.NewTextHandler(logs, nil)))
	t.Cleanup(func() { slog.SetDefault(previous) })
	return logs
}

// A closed issue still holding a plain ask, a decision block and an approval request has each
// withdrawn by the repair, naming the issue's latest closer with every field the close event
// stores, dated when the repair ran. A second repair changes nothing, and a later reopen restores
// them.
func TestRepairWithdrawsTheOpenAsksOfAnIssueClosedWithoutWithdrawing(t *testing.T) {
	f := newRepairFixture(t)
	issue := createInteractionIssue(t, f.handler, "TEST", "Closed by an older release", closeDecision)
	awaitIndexedAskBlock(t, f.handler, issue.PrimaryArtifactID, "close-decision", "Ship it?")
	blockAsk := requireBlockAsk(t, f.handler, issue.Key, issue.PrimaryArtifactID, "close-decision")
	plainAsk := createSessionAsk(t, f.handler, issue.Key, "Which region?")
	approvalAsk := requestApproval(t, f.handler, issue.PrimaryArtifactID).ID
	asks := []string{plainAsk, blockAsk, approvalAsk}
	awaitSettled(t, f.database, issue.PrimaryArtifactID)

	// An earlier closer, then the latest one, whom the records must name.
	earlier := model.Actor{Kind: "user", ID: "bob"}
	closeWithoutWithdrawing(t, f, issue.Key, &earlier, time.Now().Add(-2*time.Hour))
	reopenWithoutRestoring(t, f, issue.Key, "todo", earlier)
	closer := repairActor("session-latest-closer")
	closedAt := time.Now().Add(-time.Hour).UTC()
	closeWithoutWithdrawing(t, f, issue.Key, &closer, closedAt)
	for _, ask := range asks {
		if got := readBlockAsk(t, f.handler, ask); got.State != "open" {
			t.Fatalf("seeded ask %s = %s, want open on the closed issue", ask, got.State)
		}
	}
	requireLiveText(t, f.handler, issue.PrimaryArtifactID, `#close-decision urgency="med" multiple="false" state="open"`)

	started := time.Now()
	repair := repairNow(t, f)
	finished := time.Now()
	if repair.Withdrawn != len(asks) || repair.Restored != 0 {
		t.Fatalf("repair = %+v, want %d withdrawn and none restored", repair, len(asks))
	}
	for _, ask := range asks {
		requireWithdrawn(t, f.handler, issue.Key, ask, closer)
		at := readBlockAsk(t, f.handler, ask).Resolution.At
		if at.Before(started.Add(-time.Second)) || at.After(finished.Add(time.Second)) || !at.After(closedAt) {
			t.Fatalf("ask %s withdrawn at %s, want when the repair ran (%s - %s), not at the close %s", ask, at, started, finished, closedAt)
		}
		if count := askEventCount(t, f.database, ask, "ask.resolved"); count != 1 {
			t.Fatalf("ask %s has %d ask.resolved events, want 1", ask, count)
		}
		if actor := latestAskEventActor(t, f.database, ask, "ask.resolved"); !reflect.DeepEqual(actor, closer) {
			t.Fatalf("ask %s withdrawal event by %#v, want the closer %#v", ask, actor, closer)
		}
	}
	if detail := readIssueDetail(t, f.handler, issue.Key); len(detail.OpenAsks) != 0 {
		t.Fatalf("open asks after the repair = %d, want none", len(detail.OpenAsks))
	}
	if approval := readApproval(t, f.handler, issue.PrimaryArtifactID).Approval; approval != nil && approval.State == "awaiting" {
		t.Fatalf("the spec's approval = %#v, want no request awaiting", approval)
	}
	requireLiveText(t, f.handler, issue.PrimaryArtifactID, `#close-decision urgency="med" multiple="false" state="resolved"`)

	rows := askRowTexts(t, f.database, asks...)
	events := eventCount(t, f.database, issue.Key)
	live, _ := documentTexts(t, f.handler, issue.PrimaryArtifactID)
	if again := repairNow(t, f); again.Changed() {
		t.Fatalf("second repair = %+v, want nothing changed", again)
	}
	if got := eventCount(t, f.database, issue.Key); got != events {
		t.Fatalf("the second repair appended %d events", got-events)
	}
	if got := askRowTexts(t, f.database, asks...); !reflect.DeepEqual(got, rows) {
		t.Fatalf("the second repair changed the asks:\n%v\nwant\n%v", got, rows)
	}
	if got, _ := documentTexts(t, f.handler, issue.PrimaryArtifactID); got != live {
		t.Fatalf("the second repair changed the document:\n%s\nwant\n%s", got, live)
	}

	reopened := dispatchRequest(t, f.handler, http.MethodPatch, "/api/v1/issues/"+issue.Key, map[string]string{"status": "todo"}, "alice")
	if reopened.Code != http.StatusOK {
		t.Fatalf("reopen: status=%d body=%s", reopened.Code, reopened.Body.String())
	}
	if got, want := openAskIDs(readIssueDetail(t, f.handler, issue.Key)), sorted(asks); !reflect.DeepEqual(got, want) {
		t.Fatalf("open asks after the reopen = %v, want the repaired %v", got, want)
	}
	for _, ask := range asks {
		if actor := latestAskEventActor(t, f.database, ask, "ask.opened"); actor.Kind != "user" || actor.ID != "alice" {
			t.Fatalf("restoration of %s credited to %#v, want the reopener", ask, actor)
		}
	}
	requireLiveText(t, f.handler, issue.PrimaryArtifactID, `#close-decision urgency="med" multiple="false" state="open"`)
}

// Two repairs started together serialise on the issue's row: each ask is withdrawn once.
func TestTwoRepairsAtOnceWithdrawEachAskOnce(t *testing.T) {
	f := newRepairFixture(t)
	issue := createInteractionIssue(t, f.handler, "TEST", "Repaired twice at once", closeDecision)
	awaitIndexedAskBlock(t, f.handler, issue.PrimaryArtifactID, "close-decision", "Ship it?")
	asks := []string{
		requireBlockAsk(t, f.handler, issue.Key, issue.PrimaryArtifactID, "close-decision"),
		createSessionAsk(t, f.handler, issue.Key, "Which region?"),
		createSessionAsk(t, f.handler, issue.Key, "Which owner?"),
	}
	awaitSettled(t, f.database, issue.PrimaryArtifactID)
	closer := repairActor("session-closer")
	closeWithoutWithdrawing(t, f, issue.Key, &closer, time.Now().Add(-time.Hour))

	start := make(chan struct{})
	var wait sync.WaitGroup
	repairs := make([]IssueAskRepair, 2)
	failures := make([]error, 2)
	for index := range repairs {
		wait.Add(1)
		go func() {
			defer wait.Done()
			<-start
			repairs[index], failures[index] = RepairIssueAsks(context.Background(), f.deps, nil)
		}()
	}
	close(start)
	wait.Wait()
	for index, err := range failures {
		if err != nil {
			t.Fatalf("repair %d: %v", index, err)
		}
	}
	if withdrawn := repairs[0].Withdrawn + repairs[1].Withdrawn; withdrawn != len(asks) {
		t.Fatalf("repairs = %+v, want %d withdrawn between them", repairs, len(asks))
	}
	for _, ask := range asks {
		requireWithdrawn(t, f.handler, issue.Key, ask, closer)
		if count := askEventCount(t, f.database, ask, "ask.resolved"); count != 1 {
			t.Fatalf("ask %s has %d ask.resolved events, want 1", ask, count)
		}
	}
}

// A closed issue with no issue.closed event has no closer to name: the repair logs it and leaves
// its asks open, writing nothing.
func TestRepairSkipsAClosedIssueWithNoClosingEvent(t *testing.T) {
	f := newRepairFixture(t)
	issue := createInteractionIssue(t, f.handler, "TEST", "Closed with no event", "Context\n")
	ask := createSessionAsk(t, f.handler, issue.Key, "Which region?")
	closeWithoutWithdrawing(t, f, issue.Key, nil, time.Now().Add(-time.Hour))
	rows := askRowTexts(t, f.database, ask)
	events := eventCount(t, f.database, issue.Key)
	logs := captureLogs(t)

	if repair := repairNow(t, f); repair.Changed() {
		t.Fatalf("repair = %+v, want nothing changed", repair)
	}
	if got := askRowTexts(t, f.database, ask); !reflect.DeepEqual(got, rows) {
		t.Fatalf("the repair changed the ask:\n%v\nwant\n%v", got, rows)
	}
	if got := eventCount(t, f.database, issue.Key); got != events {
		t.Fatalf("the repair appended %d events", got-events)
	}
	if logged := logs.String(); !strings.Contains(logged, "no issue.closed event") || !strings.Contains(logged, "issue="+issue.Key) {
		t.Fatalf("the repair logged:\n%s\nwant the skipped issue %s", logged, issue.Key)
	}
}

// An open issue still holding the asks its close withdrew - reopened by a release older than the
// restoration - gets them back, credited to the actor of the event that reopened it, not to a
// later editor.
func TestRepairRestoresTheWithdrawnAsksOfAnIssueReopenedWithoutRestoring(t *testing.T) {
	f := newRepairFixture(t)
	issue := createInteractionIssue(t, f.handler, "TEST", "Reopened by an older release", closeDecision)
	awaitIndexedAskBlock(t, f.handler, issue.PrimaryArtifactID, "close-decision", "Ship it?")
	asks := []string{
		requireBlockAsk(t, f.handler, issue.Key, issue.PrimaryArtifactID, "close-decision"),
		createSessionAsk(t, f.handler, issue.Key, "Which region?"),
	}
	awaitSettled(t, f.database, issue.PrimaryArtifactID)
	if closed := dispatchRequest(t, f.handler, http.MethodPatch, "/api/v1/issues/"+issue.Key, map[string]string{"status": "done"}, "alice"); closed.Code != http.StatusOK {
		t.Fatalf("close: status=%d body=%s", closed.Code, closed.Body.String())
	}
	for _, ask := range asks {
		requireWithdrawn(t, f.handler, issue.Key, ask, model.Actor{Kind: "user", ID: "alice"})
	}
	reopener := repairActor("session-reopener")
	reopenWithoutRestoring(t, f, issue.Key, "in_progress", reopener)
	// A later edit of the reopened issue is no reopen.
	appendIssueEvent(t, f, issue.Key, "issue.updated", "in_progress", model.Actor{Kind: "user", ID: "bob"})
	opened := map[string]int{}
	for _, ask := range asks {
		opened[ask] = askEventCount(t, f.database, ask, "ask.opened")
	}

	repair := repairNow(t, f)
	if repair.Restored != len(asks) || repair.Withdrawn != 0 {
		t.Fatalf("repair = %+v, want %d restored and none withdrawn", repair, len(asks))
	}
	for _, ask := range asks {
		if got := readBlockAsk(t, f.handler, ask); got.State != "open" || got.Resolution != nil || got.Answer != nil {
			t.Fatalf("restored ask %s = %s %#v, want open with no record", ask, got.State, got.Resolution)
		}
		if count := askEventCount(t, f.database, ask, "ask.opened"); count != opened[ask]+1 {
			t.Fatalf("ask %s has %d ask.opened events, want %d", ask, count, opened[ask]+1)
		}
		if actor := latestAskEventActor(t, f.database, ask, "ask.opened"); !reflect.DeepEqual(actor, reopener) {
			t.Fatalf("restoration of %s credited to %#v, want the reopener %#v", ask, actor, reopener)
		}
	}
	if got, want := openAskIDs(readIssueDetail(t, f.handler, issue.Key)), sorted(asks); !reflect.DeepEqual(got, want) {
		t.Fatalf("open asks after the repair = %v, want %v", got, want)
	}
	requireLiveText(t, f.handler, issue.PrimaryArtifactID, `#close-decision urgency="med" multiple="false" state="open"`)

	events := eventCount(t, f.database, issue.Key)
	if again := repairNow(t, f); again.Changed() {
		t.Fatalf("second repair = %+v, want nothing changed", again)
	}
	if got := eventCount(t, f.database, issue.Key); got != events {
		t.Fatalf("the second repair appended %d events", got-events)
	}
}

// A browser on another process's room - which the close's SetIssueClosed never reached - writes
// an ask block into a closed issue's document, leaving a settlement owed that no settlement runs
// while the issue is closed. The repair gives that issue the close's final sweep, so the block
// reads resolved; a closed issue whose document owes no settlement is not touched.
func TestRepairSweepsAClosedIssuesDocumentWrittenByAnotherProcess(t *testing.T) {
	f := newRepairFixture(t)
	swept := createInteractionIssue(t, f.handler, "TEST", "Written after its close", "Context\n")
	untouched := createInteractionIssue(t, f.handler, "OTHER", "Settled after its close", "Context\n")
	awaitSettled(t, f.database, swept.PrimaryArtifactID, untouched.PrimaryArtifactID)
	other := docs.New(docs.Deps{Store: f.database, Identity: headerIdentity(f.database), Settle: time.Hour})
	t.Cleanup(func() { _ = other.Shutdown(context.Background()) })
	peers := map[string]*syncedPeer{
		swept.PrimaryArtifactID:     connectBrowserPeer(t, other, swept.PrimaryArtifactID),
		untouched.PrimaryArtifactID: connectBrowserPeer(t, other, untouched.PrimaryArtifactID),
	}
	for _, issue := range []string{swept.Key, untouched.Key} {
		if closed := dispatchRequest(t, f.handler, http.MethodPatch, "/api/v1/issues/"+issue, map[string]string{"status": "done"}, "alice"); closed.Code != http.StatusOK {
			t.Fatalf("close %s: status=%d body=%s", issue, closed.Code, closed.Body.String())
		}
	}
	block := `:::ask{#other-room urgency="low" multiple="false"}` + "\nWritten in the other room?\n:::\n"
	for document, peer := range peers {
		peer.appendParagraph(t, block)
		peer.barrier(t)
		awaitOwedSettlement(t, f.database, document)
		awaitLiveText(t, f.handler, document, "#other-room")
	}
	// The untouched issue's document no longer owes its settlement.
	if _, err := f.database.Pool.Exec(context.Background(), `delete from doc_settlements_pending where artifact_id = $1`, untouched.PrimaryArtifactID); err != nil {
		t.Fatalf("clear the untouched document's settlement: %v", err)
	}
	requireLiveText(t, f.handler, swept.PrimaryArtifactID, `#other-room urgency="low" multiple="false"`)
	untouchedText, _ := documentTexts(t, f.handler, untouched.PrimaryArtifactID)
	untouchedVersions := versionHashes(t, f.database, untouched.PrimaryArtifactID)
	sweptEvents := eventCount(t, f.database, swept.Key)
	untouchedEvents := eventCount(t, f.database, untouched.Key)

	repair := repairNow(t, f)
	if repair.Swept != 1 || repair.Withdrawn != 0 || repair.Restored != 0 {
		t.Fatalf("repair = %+v, want one block swept", repair)
	}
	requireLiveText(t, f.handler, swept.PrimaryArtifactID, `#other-room urgency="low" multiple="false" state="resolved"`)
	if id := findBlockAsk(t, f.handler, swept.Key, swept.PrimaryArtifactID, "other-room"); id != "" {
		t.Fatalf("the other room's block is ask %s on a closed issue", id)
	}
	if got := eventCount(t, f.database, swept.Key); got != sweptEvents {
		t.Fatalf("the sweep appended %d events", got-sweptEvents)
	}
	if got, _ := documentTexts(t, f.handler, untouched.PrimaryArtifactID); got != untouchedText {
		t.Fatalf("the repair changed the untouched document:\n%s\nwant\n%s", got, untouchedText)
	}
	if got := versionHashes(t, f.database, untouched.PrimaryArtifactID); !reflect.DeepEqual(got, untouchedVersions) {
		t.Fatalf("the untouched document's versions changed:\n%v\nwant\n%v", got, untouchedVersions)
	}
	if got := eventCount(t, f.database, untouched.Key); got != untouchedEvents {
		t.Fatalf("the repair appended %d events to the untouched issue", got-untouchedEvents)
	}
}

// TestAnIntervalRepairSweepsOnlyTheDocumentsMarkedSinceItsLastPass proves the interval pass
// does not read every closed issue's documents again each interval: a document whose owed
// settlement was marked before the pass's bound is left for a full pass, while one marked after
// it is swept.
func TestAnIntervalRepairSweepsOnlyTheDocumentsMarkedSinceItsLastPass(t *testing.T) {
	f := newRepairFixture(t)
	stale := createInteractionIssue(t, f.handler, "TEST", "Marked before the bound", "Context\n")
	fresh := createInteractionIssue(t, f.handler, "OTHER", "Marked after the bound", "Context\n")
	awaitSettled(t, f.database, stale.PrimaryArtifactID, fresh.PrimaryArtifactID)
	other := docs.New(docs.Deps{Store: f.database, Identity: headerIdentity(f.database), Settle: time.Hour})
	t.Cleanup(func() { _ = other.Shutdown(context.Background()) })
	peers := map[string]*syncedPeer{
		stale.PrimaryArtifactID: connectBrowserPeer(t, other, stale.PrimaryArtifactID),
		fresh.PrimaryArtifactID: connectBrowserPeer(t, other, fresh.PrimaryArtifactID),
	}
	for _, issue := range []string{stale.Key, fresh.Key} {
		if closed := dispatchRequest(t, f.handler, http.MethodPatch, "/api/v1/issues/"+issue, map[string]string{"status": "done"}, "alice"); closed.Code != http.StatusOK {
			t.Fatalf("close %s: status=%d body=%s", issue, closed.Code, closed.Body.String())
		}
	}
	block := `:::ask{#other-room urgency="low" multiple="false"}` + "\nWritten in the other room?\n:::\n"
	for document, peer := range peers {
		peer.appendParagraph(t, block)
		peer.barrier(t)
		awaitOwedSettlement(t, f.database, document)
		awaitLiveText(t, f.handler, document, "#other-room")
	}
	if _, err := f.database.Pool.Exec(context.Background(), `
		update doc_settlements_pending set marked_at = now() - interval '1 day' where artifact_id = $1
	`, stale.PrimaryArtifactID); err != nil {
		t.Fatalf("age the stale document's mark: %v", err)
	}
	s := &server{deps: f.deps}
	bound := time.Now().Add(-time.Hour)
	repair, _, err := s.repairIssueAsks(context.Background(), &bound)
	if err != nil {
		t.Fatalf("bounded repair: %v", err)
	}
	if repair.Swept != 1 {
		t.Fatalf("bounded repair = %+v, want the fresh document's block alone swept", repair)
	}
	requireLiveText(t, f.handler, fresh.PrimaryArtifactID, `#other-room urgency="low" multiple="false" state="resolved"`)
	requireLiveText(t, f.handler, stale.PrimaryArtifactID, `#other-room urgency="low" multiple="false" state="open"`)

	if repair := repairNow(t, f); repair.Swept != 1 {
		t.Fatalf("full repair = %+v, want the stale document's block swept", repair)
	}
	requireLiveText(t, f.handler, stale.PrimaryArtifactID, `#other-room urgency="low" multiple="false" state="resolved"`)
}

// A repair pass that finds nothing to change writes nothing, so the interval passes' bound keeps
// what it swept out of the next sweep. One issue is closed by this release, its document owing
// the settlement its close's own block-state write left and its decision block reading resolved;
// another is closed by an older release, holding a plain ask and a decision block a browser wrote
// that no settlement indexed, which the first pass withdraws and sweeps. A second full pass, and
// then an interval pass bounded one interval back, each change nothing, append no update to
// either document, and leave each document's owed settlement marked when it was.
func TestARepairThatChangesNothingAppendsNoUpdateAndMarksNoDocument(t *testing.T) {
	// An hour, so the only settlement is the one the close through PATCH runs.
	f := newRepairFixtureSettlingAfter(t, time.Hour)
	closed := createInteractionIssue(t, f.handler, "TEST", "Closed by this release", closeDecision)
	older := createInteractionIssue(t, f.handler, "OTHER", "Closed by an older release", "Context\n")
	if response := dispatchRequest(t, f.handler, http.MethodPatch, "/api/v1/issues/"+closed.Key, map[string]string{"status": "done"}, "alice"); response.Code != http.StatusOK {
		t.Fatalf("close %s: status=%d body=%s", closed.Key, response.Code, response.Body.String())
	}
	requireLiveText(t, f.handler, closed.PrimaryArtifactID, `#close-decision urgency="med" multiple="false" state="resolved"`)
	plainAsk := createSessionAsk(t, f.handler, older.Key, "Which region?")
	peer := connectBrowserPeer(t, f.documents, older.PrimaryArtifactID)
	peer.appendParagraph(t, `:::ask{#browser-decision urgency="low" multiple="false"}`+"\nWritten in a browser?\n:::\n")
	peer.barrier(t)
	awaitLiveText(t, f.handler, older.PrimaryArtifactID, "#browser-decision")
	if id := findBlockAsk(t, f.handler, older.Key, older.PrimaryArtifactID, "browser-decision"); id != "" {
		t.Fatalf("the browser's block is ask %s, want it unindexed", id)
	}
	closer := repairActor("session-older-closer")
	closeWithoutWithdrawing(t, f, older.Key, &closer, time.Now().Add(-time.Hour))

	if first := repairNow(t, f); first.Withdrawn != 1 || first.Swept != 1 || first.Restored != 0 {
		t.Fatalf("first repair = %+v, want the plain ask withdrawn and the unindexed block swept", first)
	}
	requireWithdrawn(t, f.handler, older.Key, plainAsk, closer)
	requireLiveText(t, f.handler, older.PrimaryArtifactID, `#browser-decision urgency="low" multiple="false" state="resolved"`)
	documents := []string{closed.PrimaryArtifactID, older.PrimaryArtifactID}
	for _, document := range documents {
		awaitOwedSettlement(t, f.database, document)
	}

	bound := time.Now().Add(-IssueAskRepairInterval)
	for _, pass := range []struct {
		name        string
		markedSince *time.Time
	}{
		{name: "a second full pass", markedSince: nil},
		{name: "an interval pass bounded one interval back", markedSince: &bound},
	} {
		writes := documentWrites(t, f.database, documents...)
		events := eventCount(t, f.database, closed.Key) + eventCount(t, f.database, older.Key)
		repair, err := RepairIssueAsks(context.Background(), f.deps, pass.markedSince)
		if err != nil {
			t.Fatalf("%s: %v", pass.name, err)
		}
		if repair != (IssueAskRepair{}) {
			t.Fatalf("%s = %+v, want nothing changed", pass.name, repair)
		}
		if got := documentWrites(t, f.database, documents...); !reflect.DeepEqual(got, writes) {
			t.Fatalf("%s wrote the documents:\n%v\nwant\n%v", pass.name, got, writes)
		}
		if got := eventCount(t, f.database, closed.Key) + eventCount(t, f.database, older.Key); got != events {
			t.Fatalf("%s appended %d events", pass.name, got-events)
		}
	}
}

// awaitOwedSettlement waits until document owes a settlement.
func awaitOwedSettlement(t *testing.T, database *store.Store, document string) {
	t.Helper()
	for deadline := time.Now().Add(10 * time.Second); ; time.Sleep(20 * time.Millisecond) {
		var owed bool
		if err := database.Pool.QueryRow(context.Background(), `
			select exists (select 1 from doc_settlements_pending where artifact_id = $1)
		`, document).Scan(&owed); err != nil {
			t.Fatalf("read the pending settlement: %v", err)
		}
		if owed {
			return
		}
		if time.Now().After(deadline) {
			t.Fatalf("document %s never owed a settlement", document)
		}
	}
}

// awaitLiveText waits until document's live text, as this server reads it, holds part.
func awaitLiveText(t *testing.T, handler http.Handler, document, part string) {
	t.Helper()
	for deadline := time.Now().Add(10 * time.Second); ; time.Sleep(20 * time.Millisecond) {
		live, _ := documentTexts(t, handler, document)
		if strings.Contains(live, part) {
			return
		}
		if time.Now().After(deadline) {
			t.Fatalf("document %s live text never held %q:\n%s", document, part, live)
		}
	}
}

// documentWrites is, for each of documents, how many updates it holds and when its owed
// settlement was marked, or that it owes none.
func documentWrites(t *testing.T, database *store.Store, documents ...string) map[string]string {
	t.Helper()
	writes := make(map[string]string, len(documents))
	for _, document := range documents {
		var updates int
		var marked string
		if err := database.Pool.QueryRow(context.Background(), `
			select
				(select count(*) from doc_updates where artifact_id = $1),
				coalesce((select marked_at::text from doc_settlements_pending where artifact_id = $1), 'none')
		`, document).Scan(&updates, &marked); err != nil {
			t.Fatalf("read the writes of document %s: %v", document, err)
		}
		writes[document] = fmt.Sprintf("updates=%d marked_at=%s", updates, marked)
	}
	return writes
}
