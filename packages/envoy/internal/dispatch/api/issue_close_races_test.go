package api

import (
	"context"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/sjawhar/envoy/internal/dispatch/docs"
	"github.com/sjawhar/envoy/internal/dispatch/model"
	"github.com/sjawhar/envoy/internal/dispatch/store"
)

// An issue's close races the writers of its asks and documents. Each race below ends in one of
// the orders the issue's row allows, never in a mixture of them, and never in a deadlock.

// raceDeadline bounds every request a race starts: a deadlock fails the test here instead of
// hanging it.
const raceDeadline = 10 * time.Second

// A resolution of an open ask and the close of its issue, released together, serialise on the
// issue's row. Either the resolution commits first, so the ask keeps the resolver's record and the
// close has nothing of it to withdraw, or the close commits first, so the resolution is refused
// 409 ISSUE_CLOSED and the ask reads withdrawn naming the closer. Either way the ask has exactly
// one ask.resolved event, and a decision block's block reads resolved, as its row does.
func TestClosingAnIssueAgainstAConcurrentResolveLeavesOneRecord(t *testing.T) {
	resolver := model.Actor{Kind: "session", ID: sessionActor()["id"].(string)}
	for _, block := range []bool{false, true} {
		name := "a plain ask"
		if block {
			name = "a decision block's ask"
		}
		t.Run(name, func(t *testing.T) {
			f := newCloseAsksFixture(t)
			ask := f.personAsk
			if block {
				ask = f.blockAsk
			}
			closer := closers[0]

			start := make(chan struct{})
			var resolved, closed *httptest.ResponseRecorder
			var requests sync.WaitGroup
			requests.Add(2)
			go func() {
				defer requests.Done()
				<-start
				resolved = resolveAsSession(t, f.handler, ask)
			}()
			go func() {
				defer requests.Done()
				<-start
				closed = closer.send(t, f.handler, f.issue, map[string]any{"status": "done"})
			}()
			awaitRequests(t, &requests, start, "the resolve and the close")

			if closed.Code != http.StatusOK {
				t.Fatalf("close: status=%d body=%s", closed.Code, closed.Body.String())
			}
			closedBy := latestEventActor(t, f.database, f.issue, "issue.closed")
			recorded := readBlockAsk(t, f.handler, ask)
			switch {
			case resolved.Code == http.StatusOK:
				t.Log("the resolve committed first")
				if recorded.State != "resolved" || recorded.Resolution == nil || recorded.Resolution.Kind != "resolved" ||
					recorded.Resolution.Reason != sessionResolution ||
					recorded.Resolution.Actor.Kind != resolver.Kind || recorded.Resolution.Actor.ID != resolver.ID {
					t.Fatalf("ask %s after its resolve answered 200 = %s %#v, want the resolver's record", ask, recorded.State, recorded.Resolution)
				}
				if actor := latestAskEventActor(t, f.database, ask, "ask.resolved"); actor.Kind != resolver.Kind || actor.ID != resolver.ID {
					t.Fatalf("ask %s ask.resolved actor = %#v, want the resolver", ask, actor)
				}
			case resolved.Code == http.StatusConflict && strings.Contains(resolved.Body.String(), `"code":"ISSUE_CLOSED"`):
				t.Log("the close committed first")
				requireWithdrawn(t, f.handler, f.issue, ask, closedBy)
				if actor := latestAskEventActor(t, f.database, ask, "ask.resolved"); actor.Kind != closedBy.Kind || actor.ID != closedBy.ID {
					t.Fatalf("ask %s ask.resolved actor = %#v, want the closer", ask, actor)
				}
			default:
				t.Fatalf("resolve: status=%d body=%s, want 200 or 409 ISSUE_CLOSED", resolved.Code, resolved.Body.String())
			}
			if count := askEventCount(t, f.database, ask, "ask.resolved"); count != 1 {
				t.Fatalf("ask %s has %d ask.resolved events, want 1", ask, count)
			}
			if detail := readIssueDetail(t, f.handler, f.issue); len(detail.OpenAsks) != 0 {
				t.Fatalf("open asks after the close = %v, want none", openAskIDs(detail))
			}
			if block {
				requireLiveText(t, f.handler, f.spec, `#close-decision urgency="med" multiple="false" state="resolved"`)
				requireLiveText(t, f.handler, f.copy, `#close-decision urgency="med" multiple="false" state="resolved" copied_from="`+f.blockAsk+`"`)
			}
		})
	}
}

// afterCloseSettlementDocs runs after once, right after the close's settlement of the issue's
// documents and before its transaction takes the issue's row: the window in which a write the
// close no longer settles still commits ahead of it.
type afterCloseSettlementDocs struct {
	docs.API
	after func()
	once  sync.Once
}

func (d *afterCloseSettlementDocs) SettleIssueDocuments(ctx context.Context, issueKey string) error {
	err := d.API.SettleIssueDocuments(ctx, issueKey)
	if d.after != nil {
		d.once.Do(d.after)
	}
	return err
}

// A decision resolved after the close's settlement ran and before the close took the issue's row
// is the order a resolve released beside a close commits in when it wins. The resolution marks
// the decision's copy in the issue's other document owed a settlement, which never runs once the
// issue is closed, and the close withdraws nothing of an ask already resolved. The close's sweep
// writes the copy from its source's row instead, so it reads resolved, as the source does.
func TestADecisionResolvedJustBeforeTheCloseTakesTheIssueLeavesItsCopyResolved(t *testing.T) {
	var documents *docs.Service
	wrapper := &afterCloseSettlementDocs{}
	handler, database := newInteractionHandler(t, func(database *store.Store) docs.API {
		// An hour, so the only settlements are the ones this test runs: the one the resolution
		// arms for the copy never fires while the test runs.
		documents = docs.New(docs.Deps{Store: database, Settle: time.Hour})
		t.Cleanup(func() { _ = documents.Shutdown(context.Background()) })
		wrapper.API = documents
		return wrapper
	})
	issue, decision, copied := decisionWithCopy(t, handler, documents)

	wrapper.after = func() {
		if resolved := resolveAsSession(t, handler, decision); resolved.Code != http.StatusOK {
			t.Errorf("resolve between the close's settlement and its transaction: status=%d body=%s", resolved.Code, resolved.Body.String())
		}
	}
	closed := dispatchRequest(t, handler, http.MethodPatch, "/api/v1/issues/"+issue.Key, map[string]string{"status": "done"}, "alice")
	if closed.Code != http.StatusOK {
		t.Fatalf("close: status=%d body=%s", closed.Code, closed.Body.String())
	}
	requireResolvedDecisionAndCopy(t, handler, database, issue.PrimaryArtifactID, decision, copied)
}

// A decision resolved within the settle delay before a release older than the withdrawal closed
// its issue leaves its copy in the issue's other document owing the settlement that shows the
// resolution, which never runs once the issue is closed. The repair's sweep of that closed issue
// writes the copy from its source's row, as the close's own sweep does, so it reads resolved.
func TestTheRepairSweepLeavesTheCopyOfAResolvedDecisionResolved(t *testing.T) {
	// An hour, so the settlement the resolution arms for the copy never fires while the test runs.
	f := newRepairFixtureSettlingAfter(t, time.Hour)
	issue, decision, copied := decisionWithCopy(t, f.handler, f.documents)
	if resolved := resolveAsSession(t, f.handler, decision); resolved.Code != http.StatusOK {
		t.Fatalf("resolve: status=%d body=%s", resolved.Code, resolved.Body.String())
	}
	closer := repairActor("session-closer")
	closeWithoutWithdrawing(t, f, issue.Key, &closer, time.Now())
	requireLiveText(t, f.handler, copied, `#close-decision urgency="med" multiple="false" state="open" copied_from="`+decision+`"`)

	if repair := repairNow(t, f); repair.Withdrawn != 0 || repair.Restored != 0 || repair.Swept != 0 {
		t.Fatalf("repair = %+v, want no ask withdrawn or restored and no unindexed block swept", repair)
	}
	requireResolvedDecisionAndCopy(t, f.handler, f.database, issue.PrimaryArtifactID, decision, copied)
}

// A decision answered after the close's settlement ran and before the close took the issue's row
// is the order an answer released beside a close commits in when it wins. The answer writes the
// decision's own block and marks its copy in the issue's other document owed a settlement, which
// never runs once the issue is closed, and the close withdraws nothing of an ask already answered.
// The close's sweep writes the copy from its source's row instead, so it reads answered, naming
// who answered and when, with the answer itself left out; the decision's own block is left as the
// answer route wrote it. The settlement the reopen leaves owed writes the answer into the copy.
func TestADecisionAnsweredJustBeforeTheCloseTakesTheIssueLeavesItsCopyAnswered(t *testing.T) {
	var documents *docs.Service
	wrapper := &afterCloseSettlementDocs{}
	handler, database := newInteractionHandler(t, func(database *store.Store) docs.API {
		// An hour, so the only settlements are the ones this test runs: the one the answer arms
		// for the copy never fires while the test runs.
		documents = docs.New(docs.Deps{Store: database, Settle: time.Hour})
		t.Cleanup(func() { _ = documents.Shutdown(context.Background()) })
		wrapper.API = documents
		return wrapper
	})
	issue, decision, copied := decisionWithCopy(t, handler, documents)

	var answered answeredDecision
	wrapper.after = func() {
		answered = answerDecision(t, handler, database, issue.PrimaryArtifactID, decision)
	}
	closed := dispatchRequest(t, handler, http.MethodPatch, "/api/v1/issues/"+issue.Key, map[string]string{"status": "done"}, "alice")
	if closed.Code != http.StatusOK {
		t.Fatalf("close: status=%d body=%s", closed.Code, closed.Body.String())
	}
	requireAnsweredDecisionAndCopy(t, handler, database, issue.PrimaryArtifactID, decision, copied, answered)

	if reopened := dispatchRequest(t, handler, http.MethodPatch, "/api/v1/issues/"+issue.Key, map[string]string{"status": "todo"}, "alice"); reopened.Code != http.StatusOK {
		t.Fatalf("reopen: status=%d body=%s", reopened.Code, reopened.Body.String())
	}
	if err := documents.SettleIssueDocuments(context.Background(), issue.Key); err != nil {
		t.Fatalf("settle the reopened issue's documents: %v", err)
	}
	requireLiveText(t, handler, copied, `state="answered"`, `answered_by="alice"`, decisionSelected, `answer="`+decisionAnswer+`"`, `copied_from="`+decision+`"`)
}

// A decision answered within the settle delay before a release older than the withdrawal closed
// its issue leaves its copy in the issue's other document owing the settlement that shows the
// answer, which never runs once the issue is closed. The repair's sweep of that closed issue
// writes the copy from its source's row, as the close's own sweep does, so it reads answered with
// the answer left out, and leaves the decision's own block as the answer route wrote it.
func TestTheRepairSweepLeavesTheCopyOfAnAnsweredDecisionAnswered(t *testing.T) {
	// An hour, so the settlement the answer arms for the copy never fires while the test runs.
	f := newRepairFixtureSettlingAfter(t, time.Hour)
	issue, decision, copied := decisionWithCopy(t, f.handler, f.documents)
	answered := answerDecision(t, f.handler, f.database, issue.PrimaryArtifactID, decision)
	closer := repairActor("session-closer")
	closeWithoutWithdrawing(t, f, issue.Key, &closer, time.Now())
	requireLiveText(t, f.handler, copied, `#close-decision urgency="med" multiple="false" state="open" copied_from="`+decision+`"`)

	if repair := repairNow(t, f); repair.Withdrawn != 0 || repair.Restored != 0 || repair.Swept != 0 {
		t.Fatalf("repair = %+v, want no ask withdrawn or restored and no unindexed block swept", repair)
	}
	requireAnsweredDecisionAndCopy(t, f.handler, f.database, issue.PrimaryArtifactID, decision, copied, answered)
}

// A decision answered and settled before its issue's close has a copy holding the answer in full.
// The close's sweep writes an answered decision's copies with the answer left out only where they
// do not already hold it, so this copy keeps its answer.
func TestClosingAnIssueLeavesTheSettledAnswerInADecisionsCopy(t *testing.T) {
	var documents *docs.Service
	handler, database := newInteractionHandler(t, func(database *store.Store) docs.API {
		// An hour, so the only settlements are the ones this test runs.
		documents = docs.New(docs.Deps{Store: database, Settle: time.Hour})
		t.Cleanup(func() { _ = documents.Shutdown(context.Background()) })
		return documents
	})
	issue, decision, copied := decisionWithCopy(t, handler, documents)
	answerDecision(t, handler, database, issue.PrimaryArtifactID, decision)
	if err := documents.SettleIssueDocuments(context.Background(), issue.Key); err != nil {
		t.Fatalf("settle the answer's copy: %v", err)
	}
	settled, _ := documentTexts(t, handler, copied)
	if !contains(`state="answered"`, decisionSelected, `answer="`+decisionAnswer+`"`)(settled) {
		t.Fatalf("copy %s after the answer's settlement:\n%s\nwant it to hold the answer", copied, settled)
	}

	closed := dispatchRequest(t, handler, http.MethodPatch, "/api/v1/issues/"+issue.Key, map[string]string{"status": "done"}, "alice")
	if closed.Code != http.StatusOK {
		t.Fatalf("close: status=%d body=%s", closed.Code, closed.Body.String())
	}
	if live, _ := documentTexts(t, handler, copied); live != settled {
		t.Fatalf("copy %s after the close:\n%s\nwant it as settled:\n%s", copied, live, settled)
	}
}

// decisionAnswer is the text answerDecision answers closeDecision with, beside selecting Yes.
const decisionAnswer = "Ship it this week."

// decisionSelected is the selected attribute of a block answered as answerDecision answers, as a
// document's markdown renders it: the selection's JSON, its quotes escaped.
const decisionSelected = `selected="[&#x22;Yes&#x22;]"`

// answeredDecision is what the answer route left of an answered decision: its ask's row as the
// database stores it and the live text of the document its block is on.
type answeredDecision struct {
	row, spec string
}

// answerDecision answers decision, closeDecision's ask on spec, as alice, selecting Yes with
// decisionAnswer, and reads what the answer route left of it.
func answerDecision(t *testing.T, handler http.Handler, database *store.Store, spec, decision string) answeredDecision {
	t.Helper()
	read := readBlockAsk(t, handler, decision)
	answered := dispatchRequest(t, handler, http.MethodPost, "/api/v1/asks/"+decision+"/answer", map[string]any{
		"selected": []string{"Yes"}, "text": decisionAnswer, "expected_edited_at": read.EditedAt,
	}, "alice")
	if answered.Code != http.StatusOK {
		t.Fatalf("answer the decision: status=%d body=%s", answered.Code, answered.Body.String())
	}
	live, _ := documentTexts(t, handler, spec)
	requireLiveText(t, handler, spec, `state="answered"`, `answered_by="alice"`, `answer="`+decisionAnswer+`"`)
	return answeredDecision{row: askRowTexts(t, database, decision)[0], spec: live}
}

// requireAnsweredDecisionAndCopy fails t unless decision's row, and its block on spec, are as the
// answer route left them (answered), with no ask.resolved event, and its copy on copied reads
// answered, naming its answerer, with no selection and no answer.
func requireAnsweredDecisionAndCopy(t *testing.T, handler http.Handler, database *store.Store, spec, decision, copied string, answered answeredDecision) {
	t.Helper()
	if row := askRowTexts(t, database, decision)[0]; row != answered.row {
		t.Fatalf("decision row = %s, want it as the answer left it: %s", row, answered.row)
	}
	if count := askEventCount(t, database, decision, "ask.resolved"); count != 0 {
		t.Fatalf("decision has %d ask.resolved events, want none", count)
	}
	if live, _ := documentTexts(t, handler, spec); live != answered.spec {
		t.Fatalf("spec %s live text:\n%s\nwant it as the answer left it:\n%s", spec, live, answered.spec)
	}
	live, _ := documentTexts(t, handler, copied)
	if !contains(`state="answered"`, `answered_by="alice"`, `answered_at="`, `copied_from="`+decision+`"`)(live) ||
		strings.Contains(live, `selected=`) || strings.Contains(live, ` answer=`) {
		t.Fatalf("copy %s live text:\n%s\nwant it answered by alice with no selection and no answer", copied, live)
	}
}

// sessionResolution is the reason resolveAsSession resolves an ask with.
const sessionResolution = "Settled in the meeting."

// decisionWithCopy creates an issue whose spec holds closeDecision, settled as an open ask, and a
// second document holding a copy of it, settled as that copy. documents' settle delay is an hour:
// it settles them here, on demand. It answers the issue, the decision's ask and the copy's
// document.
func decisionWithCopy(t *testing.T, handler http.Handler, documents *docs.Service) (issue struct {
	Key               string `json:"key"`
	PrimaryArtifactID string `json:"primary_artifact_id"`
}, decision, copied string) {
	t.Helper()
	issue = createInteractionIssue(t, handler, "TEST", "A decision with a copy", closeDecision)
	if err := documents.SettleIssueDocuments(context.Background(), issue.Key); err != nil {
		t.Fatalf("settle the spec: %v", err)
	}
	decision = requireBlockAsk(t, handler, issue.Key, issue.PrimaryArtifactID, "close-decision")
	uploaded := sessionRequest(t, handler, http.MethodPost, "/api/v1/issues/"+issue.Key+"/artifacts", map[string]any{
		"actor": sessionActor(), "name": "spec-record.md", "content": closeDecision,
	})
	if uploaded.Code != http.StatusCreated {
		t.Fatalf("upload the copy: status=%d body=%s", uploaded.Code, uploaded.Body.String())
	}
	copied = decodeBody[struct {
		Artifact model.Artifact `json:"artifact"`
	}](t, uploaded).Artifact.ID
	if err := documents.SettleIssueDocuments(context.Background(), issue.Key); err != nil {
		t.Fatalf("settle the copy: %v", err)
	}
	requireLiveText(t, handler, copied, `#close-decision urgency="med" multiple="false" state="open" copied_from="`+decision+`"`)
	return issue, decision, copied
}

// resolveAsSession resolves ask as a session does, with sessionResolution.
func resolveAsSession(t *testing.T, handler http.Handler, ask string) *httptest.ResponseRecorder {
	t.Helper()
	return sessionRequest(t, handler, http.MethodPost, "/api/v1/asks/"+ask+"/resolve", map[string]any{
		"kind": "resolved", "reason": sessionResolution, "actor": sessionActor(),
	})
}

// requireResolvedDecisionAndCopy fails t unless decision holds the resolver's record, with one
// ask.resolved event, and its block on spec and its copy on copied both read resolved.
func requireResolvedDecisionAndCopy(t *testing.T, handler http.Handler, database *store.Store, spec, decision, copied string) {
	t.Helper()
	if recorded := readBlockAsk(t, handler, decision); recorded.State != "resolved" || recorded.Resolution == nil ||
		recorded.Resolution.Kind != "resolved" || recorded.Resolution.Reason != sessionResolution {
		t.Fatalf("decision = %s %#v, want the resolver's record", recorded.State, recorded.Resolution)
	}
	if count := askEventCount(t, database, decision, "ask.resolved"); count != 1 {
		t.Fatalf("decision has %d ask.resolved events, want 1", count)
	}
	requireLiveText(t, handler, spec, `#close-decision urgency="med" multiple="false" state="resolved"`)
	requireLiveText(t, handler, copied, `#close-decision urgency="med" multiple="false" state="resolved" copied_from="`+decision+`"`)
}

// A decision block written within the settle delay, whose settlement is in flight - holding the
// issue's row - when the issue is closed: the close waits for it on that row, so the settlement
// indexes the block as an ask credited to its writer, and the close then withdraws that ask,
// naming the closer. The close's own settlement, behind it, finds the document settled, so the
// two write at most one version of it between them.
func TestClosingAnIssueWaitsForTheSettlementOfItsDocumentInFlight(t *testing.T) {
	var documents *docs.Service
	// hold names the document whose next settlement is held at its owner row, until release.
	var hold atomic.Pointer[string]
	held := make(chan struct{})
	release := make(chan struct{})
	var releaseOnce sync.Once
	releaseSettlement := func() { releaseOnce.Do(func() { close(release) }) }
	handler, database := newInteractionHandler(t, func(database *store.Store) docs.API {
		documents = docs.New(docs.Deps{Store: database, Settle: 20 * time.Millisecond})
		documents.AfterSettleLockForTest(func(room string) {
			if target := hold.Load(); target != nil && *target == room && hold.CompareAndSwap(target, nil) {
				close(held)
				<-release
			}
		})
		t.Cleanup(func() { _ = documents.Shutdown(context.Background()) })
		// Registered after the shutdown, so it runs before it: a test that fails while the
		// settlement is held still lets it finish.
		t.Cleanup(releaseSettlement)
		return documents
	})
	issue := createInteractionIssue(t, handler, "TEST", "A decision settling as it closes", "Context\n")
	spec := issue.PrimaryArtifactID
	awaitSettled(t, database, spec)
	versions := len(versionHashes(t, database, spec))

	hold.Store(&spec)
	edited := sessionRequest(t, handler, http.MethodPost, "/api/v1/artifacts/"+spec+"/edits", map[string]any{
		"actor": sessionActor(),
		"ops": []map[string]string{{
			"op": "insert", "after": "end",
			"markdown": ":::ask{#late-decision urgency=\"high\" multiple=\"false\"}\nShip late?\n:::\n",
		}},
	})
	if edited.Code != http.StatusOK {
		t.Fatalf("write the decision block: status=%d body=%s", edited.Code, edited.Body.String())
	}
	select {
	case <-held:
	case <-time.After(raceDeadline):
		t.Fatal("the decision block's settlement never started")
	}
	if id := findBlockAsk(t, handler, issue.Key, spec, "late-decision"); id != "" {
		t.Fatalf("the decision block is ask %s while its settlement is held", id)
	}

	var closed *httptest.ResponseRecorder
	closing := make(chan struct{})
	go func() {
		defer close(closing)
		closed = dispatchRequest(t, handler, http.MethodPatch, "/api/v1/issues/"+issue.Key, map[string]string{"status": "done"}, "alice")
	}()
	// The close's own settlement of the document waits on the issue's row, which the held
	// settlement owns.
	awaitLockWaiters(t, database, 1)
	select {
	case <-closing:
		t.Fatalf("the close answered %d while the settlement held the issue's row: body=%s", closed.Code, closed.Body.String())
	default:
	}
	releaseSettlement()
	select {
	case <-closing:
	case <-time.After(raceDeadline):
		t.Fatalf("the close did not answer within %v of the settlement's release", raceDeadline)
	}
	if closed.Code != http.StatusOK {
		t.Fatalf("close: status=%d body=%s", closed.Code, closed.Body.String())
	}

	asks := documentBlockAsks(t, handler, issue.Key, spec)
	if len(asks) != 1 || asks[0].BlockID == nil || *asks[0].BlockID != "late-decision" {
		t.Fatalf("the document's block asks = %#v, want the decision block's one", asks)
	}
	ask := asks[0]
	if ask.Author.Kind != "session" || ask.Author.ID != sessionActor()["id"] {
		t.Fatalf("the decision's author = %#v, want the session that wrote it", ask.Author)
	}
	requireWithdrawn(t, handler, issue.Key, ask.ID, model.Actor{Kind: "user", ID: "alice"})
	if count := askEventCount(t, database, ask.ID, "ask.resolved"); count != 1 {
		t.Fatalf("ask %s has %d ask.resolved events, want 1", ask.ID, count)
	}
	requireLiveText(t, handler, spec, `#late-decision urgency="high" multiple="false" state="resolved"`)
	if written := len(versionHashes(t, database, spec)) - versions; written > 1 {
		t.Fatalf("the settlement and the close wrote %d versions of the document, want at most one", written)
	}
}

// awaitRequests releases start, then waits up to raceDeadline for requests, failing the test
// when they do not all answer: a deadlock between them.
func awaitRequests(t *testing.T, requests *sync.WaitGroup, start chan struct{}, what string) {
	t.Helper()
	answered := make(chan struct{})
	go func() {
		requests.Wait()
		close(answered)
	}()
	close(start)
	select {
	case <-answered:
	case <-time.After(raceDeadline):
		t.Fatalf("%s did not both answer within %v", what, raceDeadline)
	}
}
