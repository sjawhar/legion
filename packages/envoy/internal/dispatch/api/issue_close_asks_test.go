package api

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"reflect"
	"slices"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/sjawhar/envoy/internal/dispatch/docs"
	"github.com/sjawhar/envoy/internal/dispatch/model"
	"github.com/sjawhar/envoy/internal/dispatch/store"
)

// Closing an issue withdraws every open ask it holds, whoever closes it: each is recorded resolved
// as withdrawn by the close, naming the closer, and stops reading open in the documents too.
// Reopening the issue opens exactly those asks again.

// closeDecision is the decision block the closing tests' spec holds, and a copy of it a second
// document of the issue holds.
const closeDecision = "Context\n\n:::ask{#close-decision urgency=\"med\" multiple=\"false\"}\nShip it?\n\n- Yes: Now.\n- No: Later.\n:::\n"

// closeAsksFixture is an issue holding one open ask of every kind - a plain ask waiting on a
// person, a plain ask waiting on its agent, a decision block in the spec with a copy of it in a
// second document, an approval request on the spec - beside an answered and a retracted ask.
type closeAsksFixture struct {
	handler   http.Handler
	database  *store.Store
	documents *docs.Service
	issue     string
	spec      string
	copy      string
	// open are the open asks: the person's, the agent's, the decision block's, the approval's.
	personAsk, agentAsk, blockAsk, approvalAsk string
	answeredAsk, retractedAsk                  string
}

func (f closeAsksFixture) open() []string {
	return []string{f.personAsk, f.agentAsk, f.blockAsk, f.approvalAsk}
}

func newCloseAsksFixture(t *testing.T) closeAsksFixture {
	t.Helper()
	var documents *docs.Service
	handler, database := newInteractionHandler(t, func(database *store.Store) docs.API {
		documents = docs.New(docs.Deps{Store: database, Settle: 20 * time.Millisecond})
		t.Cleanup(func() { _ = documents.Shutdown(context.Background()) })
		return documents
	})
	issue := createInteractionIssue(t, handler, "TEST", "Closing with asks", closeDecision)
	f := closeAsksFixture{handler: handler, database: database, documents: documents, issue: issue.Key, spec: issue.PrimaryArtifactID}
	awaitIndexedAskBlock(t, handler, f.spec, "close-decision", "Ship it?")
	f.blockAsk = requireBlockAsk(t, handler, f.issue, f.spec, "close-decision")
	uploaded := sessionRequest(t, handler, http.MethodPost, "/api/v1/issues/"+f.issue+"/artifacts", map[string]any{
		"actor": sessionActor(), "name": "spec-record.md", "content": closeDecision,
	})
	if uploaded.Code != http.StatusCreated {
		t.Fatalf("upload the copy: status=%d body=%s", uploaded.Code, uploaded.Body.String())
	}
	f.copy = decodeBody[struct {
		Artifact model.Artifact `json:"artifact"`
	}](t, uploaded).Artifact.ID
	awaitDocumentText(t, handler, f.copy, "the copy of the open decision", contains(
		`#close-decision urgency="med" multiple="false" state="open" copied_from="`+f.blockAsk+`"`,
	), nil)
	awaitSettled(t, database, f.spec, f.copy)
	f.approvalAsk = requestApproval(t, handler, f.spec).ID

	f.personAsk = createSessionAsk(t, handler, f.issue, "Which region?")
	f.agentAsk = createSessionAsk(t, handler, f.issue, "Which owner?")
	if reply := dispatchRequest(t, handler, http.MethodPost, "/api/v1/issues/"+f.issue+"/comments", map[string]any{
		"body": "Who are the candidates?", "ask_id": f.agentAsk,
	}, "alice"); reply.Code != http.StatusCreated {
		t.Fatalf("reply to the agent's ask: status=%d body=%s", reply.Code, reply.Body.String())
	}
	f.answeredAsk = createSessionAsk(t, handler, f.issue, "Ship on Monday?")
	if answered := dispatchRequest(t, handler, http.MethodPost, "/api/v1/asks/"+f.answeredAsk+"/answer", map[string]any{
		"text": "Yes.",
	}, "alice"); answered.Code != http.StatusOK {
		t.Fatalf("answer: status=%d body=%s", answered.Code, answered.Body.String())
	}
	f.retractedAsk = createSessionAsk(t, handler, f.issue, "Still needed?")
	if retracted := sessionRequest(t, handler, http.MethodPost, "/api/v1/asks/"+f.retractedAsk+"/resolve", map[string]any{
		"kind": "retracted", "reason": "Not needed.", "actor": sessionActor(),
	}); retracted.Code != http.StatusOK {
		t.Fatalf("retract: status=%d body=%s", retracted.Code, retracted.Body.String())
	}
	for ask, want := range map[string]string{f.personAsk: "human", f.agentAsk: "agent", f.blockAsk: "human", f.approvalAsk: "human"} {
		if got := readAskRecord(t, handler, ask); got.State != "open" || got.WaitingOn != want {
			t.Fatalf("ask %s = %s waiting on %q, want open waiting on %s", ask, got.State, got.WaitingOn, want)
		}
	}
	awaitSettled(t, database, f.spec, f.copy)
	return f
}

// closer is one of the writers that close issues, as their requests reach PATCH
// /api/v1/issues/{key}.
type closer struct {
	name  string
	actor model.Actor
	send  func(t *testing.T, handler http.Handler, issue string, body map[string]any) *httptest.ResponseRecorder
}

var closers = []closer{
	{
		name:  "a person",
		actor: model.Actor{Kind: "user", ID: "alice"},
		send: func(t *testing.T, handler http.Handler, issue string, body map[string]any) *httptest.ResponseRecorder {
			return dispatchRequest(t, handler, http.MethodPatch, "/api/v1/issues/"+issue, body, "alice")
		},
	},
	sessionCloser("an agent session", "session-0123456789abcdef"),
	// The Legion daemon's request (packages/daemon/internal/dispatch/client.go SetStatus).
	sessionCloser("the Legion daemon", "legion-daemon:TEST"),
}

func sessionCloser(name, id string) closer {
	return closer{
		name:  name,
		actor: model.Actor{Kind: "session", ID: id},
		send: func(t *testing.T, handler http.Handler, issue string, body map[string]any) *httptest.ResponseRecorder {
			withActor := map[string]any{"actor": map[string]any{"kind": "session", "id": id}}
			for key, value := range body {
				withActor[key] = value
			}
			return sessionRequest(t, handler, http.MethodPatch, "/api/v1/issues/"+issue, withActor)
		},
	}
}

func TestClosingAnIssueWithdrawsItsOpenAsksAndReopeningRestoresThem(t *testing.T) {
	for _, closer := range closers {
		t.Run(closer.name, func(t *testing.T) {
			f := newCloseAsksFixture(t)
			preClose := readIssueDetail(t, f.handler, f.issue)
			if len(preClose.OpenAsks) != 4 {
				t.Fatalf("open asks before the close = %d, want 4", len(preClose.OpenAsks))
			}
			settledRows := askRowTexts(t, f.database, f.answeredAsk, f.retractedAsk)
			versions := versionHashes(t, f.database, f.spec, f.copy)
			reviews := reviewCount(t, f.database, f.spec, f.copy)
			decisions := decisionEventCount(t, f.database, f.issue)
			openedBefore := map[string]int{}
			recordsBefore := map[string]model.Ask{}
			for _, ask := range f.open() {
				openedBefore[ask] = askEventCount(t, f.database, ask, "ask.opened")
				recordsBefore[ask] = readAskRecord(t, f.handler, ask)
			}
			if inbox := inboxAskIDs(t, f.handler); !containsAll(inbox, f.open()) {
				t.Fatalf("inbox before the close = %v, want every open ask %v", inbox, f.open())
			}

			closed := closer.send(t, f.handler, f.issue, map[string]any{"status": "done"})
			if closed.Code != http.StatusOK {
				t.Fatalf("close: status=%d body=%s", closed.Code, closed.Body.String())
			}
			response := decodeBody[model.Issue](t, closed)
			closedBy := latestEventActor(t, f.database, f.issue, "issue.closed")
			if closedBy.Kind != closer.actor.Kind || closedBy.ID != closer.actor.ID {
				t.Fatalf("issue.closed actor = %#v, want %s %s", closedBy, closer.actor.Kind, closer.actor.ID)
			}

			detail := readIssueDetail(t, f.handler, f.issue)
			if len(detail.OpenAsks) != 0 {
				t.Fatalf("open asks after the close = %d, want none", len(detail.OpenAsks))
			}
			if response.LastSeq != detail.LastSeq {
				t.Fatalf("close response last_seq = %d, a read of the issue answers %d", response.LastSeq, detail.LastSeq)
			}
			for _, ask := range f.open() {
				requireWithdrawn(t, f.handler, f.issue, ask, closedBy)
				if count := askEventCount(t, f.database, ask, "ask.resolved"); count != 1 {
					t.Fatalf("ask %s has %d ask.resolved events, want 1", ask, count)
				}
			}
			requireLiveText(t, f.handler, f.spec, `#close-decision urgency="med" multiple="false" state="resolved"`)
			requireLiveText(t, f.handler, f.copy, `#close-decision urgency="med" multiple="false" state="resolved" copied_from="`+f.blockAsk+`"`)
			if got := askRowTexts(t, f.database, f.answeredAsk, f.retractedAsk); !reflect.DeepEqual(got, settledRows) {
				t.Fatalf("the answered and retracted asks changed:\n%v\nwant\n%v", got, settledRows)
			}
			if got := decisionEventCount(t, f.database, f.issue); got != decisions {
				t.Fatalf("the close appended %d answer or review events", got-decisions)
			}
			if got := reviewCount(t, f.database, f.spec, f.copy); got != reviews {
				t.Fatalf("the close wrote %d reviews", got-reviews)
			}
			if approval := readApproval(t, f.handler, f.spec).Approval; approval != nil && approval.State == "awaiting" {
				t.Fatalf("the spec's approval = %#v, want no request awaiting", approval)
			}
			if got := versionHashes(t, f.database, f.spec, f.copy); !reflect.DeepEqual(got, versions) {
				t.Fatalf("the documents' versions changed:\n%v\nwant\n%v", got, versions)
			}

			stale := dispatchRequest(t, f.handler, http.MethodPatch, "/api/v1/issues/"+f.issue, map[string]any{
				"status": "todo", "expected_sequence": preClose.LastSeq,
			}, "alice")
			if stale.Code != http.StatusConflict || !strings.Contains(stale.Body.String(), `"code":"ISSUE_CHANGED"`) {
				t.Fatalf("reopen against the pre-close sequence: status=%d body=%s", stale.Code, stale.Body.String())
			}
			reopened := dispatchRequest(t, f.handler, http.MethodPatch, "/api/v1/issues/"+f.issue, map[string]any{
				"status": "todo", "expected_sequence": detail.LastSeq,
			}, "alice")
			if reopened.Code != http.StatusOK {
				t.Fatalf("reopen against the close's sequence: status=%d body=%s", reopened.Code, reopened.Body.String())
			}
			reopenResponse := decodeBody[model.Issue](t, reopened)

			detail = readIssueDetail(t, f.handler, f.issue)
			if got, want := openAskIDs(detail), sorted(f.open()); !reflect.DeepEqual(got, want) {
				t.Fatalf("open asks after the reopen = %v, want exactly the withdrawn %v", got, want)
			}
			if reopenResponse.LastSeq != detail.LastSeq {
				t.Fatalf("reopen response last_seq = %d, a read of the issue answers %d", reopenResponse.LastSeq, detail.LastSeq)
			}
			for _, ask := range f.open() {
				got := readAskRecord(t, f.handler, ask)
				before := recordsBefore[ask]
				if got.State != "open" || got.Resolution != nil || got.Answer != nil || got.WaitingOn != before.WaitingOn {
					t.Fatalf("restored ask %s = %s %#v waiting on %q, want open with no record waiting on %q",
						ask, got.State, got.Resolution, got.WaitingOn, before.WaitingOn)
				}
				if !reflect.DeepEqual(got.Approval, before.Approval) {
					t.Fatalf("restored ask %s approval = %#v, want %#v", ask, got.Approval, before.Approval)
				}
				if count := askEventCount(t, f.database, ask, "ask.opened"); count != openedBefore[ask]+1 {
					t.Fatalf("ask %s has %d ask.opened events, want %d", ask, count, openedBefore[ask]+1)
				}
				if actor := latestAskEventActor(t, f.database, ask, "ask.opened"); actor.Kind != "user" || actor.ID != "alice" {
					t.Fatalf("restoration of %s credited to %#v, want the reopener", ask, actor)
				}
			}
			if got := askRowTexts(t, f.database, f.answeredAsk, f.retractedAsk); !reflect.DeepEqual(got, settledRows) {
				t.Fatalf("the reopen changed the answered and retracted asks:\n%v\nwant\n%v", got, settledRows)
			}
			if approval := readApproval(t, f.handler, f.spec).Approval; approval == nil || approval.State != "awaiting" {
				t.Fatalf("the spec's approval after the reopen = %#v, want awaiting", approval)
			}
			requireLiveText(t, f.handler, f.spec, `#close-decision urgency="med" multiple="false" state="open"`)
			requireLiveText(t, f.handler, f.copy, `#close-decision urgency="med" multiple="false" state="open" copied_from="`+f.blockAsk+`"`)
			if inbox := inboxAskIDs(t, f.handler); !containsAll(inbox, f.open()) {
				t.Fatalf("inbox after the reopen = %v, want every restored ask %v", inbox, f.open())
			}
			// The settlement the close and the reopen left owed, as the room's next load or the
			// resumption runs it.
			if err := f.documents.SettleIssueDocuments(context.Background(), f.issue); err != nil {
				t.Fatalf("settle the reopened issue's documents: %v", err)
			}
			awaitSettled(t, f.database, f.spec, f.copy)
			if got := versionHashes(t, f.database, f.spec, f.copy); !reflect.DeepEqual(got, versions) {
				t.Fatalf("the documents' versions after the reopen settled:\n%v\nwant\n%v", got, versions)
			}
		})
	}
}

// failingSettlement is a document service whose settlement of an issue's documents before its
// close fails, leaving the blocks written since the last settlement without an ask.
type failingSettlement struct {
	docs.API
}

func (failingSettlement) SettleIssueDocuments(context.Context, string) error {
	return errors.New("settlement failed")
}

// A decision block written a moment before the close, which its settlement (an hour away) has not
// indexed, is still withdrawn by the close: the close settles the issue's documents first, so the
// block is an ask, credited to whoever wrote it. When that settlement fails, the block is no ask
// to withdraw, but the close still leaves it reading resolved, and the reopen's settlement opens
// it as its author's ask.
func TestClosingAnIssueWithdrawsADecisionBlockNotYetSettled(t *testing.T) {
	for _, settlementFails := range []bool{false, true} {
		name := "settled by the close"
		if settlementFails {
			name = "settlement fails"
		}
		t.Run(name, func(t *testing.T) {
			var documentService *docs.Service
			handler, _ := newInteractionHandler(t, func(database *store.Store) docs.API {
				documentService = docs.New(docs.Deps{Store: database, Settle: time.Hour})
				t.Cleanup(func() { _ = documentService.Shutdown(context.Background()) })
				if settlementFails {
					return failingSettlement{API: documentService}
				}
				return documentService
			})
			issue := createInteractionIssue(t, handler, "TEST", "A late decision", "Context\n")
			edited := sessionRequest(t, handler, http.MethodPost, "/api/v1/artifacts/"+issue.PrimaryArtifactID+"/edits", map[string]any{
				"actor": sessionActor(),
				"ops": []map[string]string{{
					"op": "insert", "after": "end",
					"markdown": ":::ask{#late-decision urgency=\"high\" multiple=\"false\"}\nShip late?\n:::\n",
				}},
			})
			if edited.Code != http.StatusOK {
				t.Fatalf("write the decision block: status=%d body=%s", edited.Code, edited.Body.String())
			}
			if id := findBlockAsk(t, handler, issue.Key, issue.PrimaryArtifactID, "late-decision"); id != "" {
				t.Fatalf("the decision block is ask %s before any settlement", id)
			}
			closed := dispatchRequest(t, handler, http.MethodPatch, "/api/v1/issues/"+issue.Key, map[string]string{"status": "done"}, "alice")
			if closed.Code != http.StatusOK {
				t.Fatalf("close: status=%d body=%s", closed.Code, closed.Body.String())
			}
			requireLiveText(t, handler, issue.PrimaryArtifactID, `#late-decision urgency="high" multiple="false" state="resolved"`)
			id := findBlockAsk(t, handler, issue.Key, issue.PrimaryArtifactID, "late-decision")
			if !settlementFails {
				if id == "" {
					t.Fatal("the close's settlement never indexed the decision block")
				}
				ask := readAskRecord(t, handler, id)
				if ask.Author.Kind != "session" || ask.Author.ID != sessionActor()["id"] {
					t.Fatalf("withdrawn decision's author = %#v, want the session that wrote it", ask.Author)
				}
				requireWithdrawn(t, handler, issue.Key, id, model.Actor{Kind: "user", ID: "alice"})
				return
			}
			if id != "" {
				t.Fatalf("the decision block is ask %s although its settlement failed", id)
			}
			if reopened := dispatchRequest(t, handler, http.MethodPatch, "/api/v1/issues/"+issue.Key, map[string]string{"status": "todo"}, "alice"); reopened.Code != http.StatusOK {
				t.Fatalf("reopen: status=%d body=%s", reopened.Code, reopened.Body.String())
			}
			if err := documentService.SettleIssueDocuments(context.Background(), issue.Key); err != nil {
				t.Fatalf("settle the reopened issue's documents: %v", err)
			}
			id = findBlockAsk(t, handler, issue.Key, issue.PrimaryArtifactID, "late-decision")
			if id == "" {
				t.Fatal("the reopen's settlement never indexed the decision block")
			}
			ask := readAskRecord(t, handler, id)
			if ask.State != "open" || ask.Author.Kind != "session" || ask.Author.ID != sessionActor()["id"] {
				t.Fatalf("indexed decision = %s by %#v, want open by the session that wrote it", ask.State, ask.Author)
			}
			requireLiveText(t, handler, issue.PrimaryArtifactID, `#late-decision urgency="high" multiple="false" state="open"`)
		})
	}
}

// peerInsertDocs runs insert once, right after the close's transaction has written the issue's
// documents and before it commits: the window a browser connected to a document's room can still
// write in.
type peerInsertDocs struct {
	docs.API
	insert func()
	once   sync.Once
}

func (d *peerInsertDocs) WriteIssueAskStates(ctx context.Context, issueKey string, asks []model.Ask, actor model.Actor, markUnindexed bool) ([]docs.BlockRef, error) {
	written, err := d.API.WriteIssueAskStates(ctx, issueKey, asks, actor, markUnindexed)
	if err == nil && !markUnindexed && d.insert != nil {
		d.once.Do(d.insert)
	}
	return written, err
}

// A browser writing a decision block into a document after the close's transaction forked it and
// before it committed leaves the block in the room, where no settlement will index it while the
// issue is closed; the close's sweep, once the rooms are closed, leaves it reading resolved.
func TestABrowsersDecisionBlockWrittenDuringTheCloseReadsResolved(t *testing.T) {
	var documentService *docs.Service
	wrapper := &peerInsertDocs{}
	handler, database := newInteractionHandler(t, func(database *store.Store) docs.API {
		documentService = docs.New(docs.Deps{Store: database, Identity: headerIdentity(database), Settle: 20 * time.Millisecond})
		t.Cleanup(func() { _ = documentService.Shutdown(context.Background()) })
		wrapper.API = documentService
		return wrapper
	})
	issue := createInteractionIssue(t, handler, "TEST", "A browser writes during the close", closeDecision)
	awaitIndexedAskBlock(t, handler, issue.PrimaryArtifactID, "close-decision", "Ship it?")
	awaitSettled(t, database, issue.PrimaryArtifactID)
	peer := connectBrowserPeer(t, documentService, issue.PrimaryArtifactID)
	wrapper.insert = func() {
		peer.appendParagraph(t, ":::ask{#peer-decision urgency=\"low\" multiple=\"false\"}\nWritten during the close?\n:::\n")
		peer.barrier(t)
	}
	closed := dispatchRequest(t, handler, http.MethodPatch, "/api/v1/issues/"+issue.Key, map[string]string{"status": "done"}, "alice")
	if closed.Code != http.StatusOK {
		t.Fatalf("close: status=%d body=%s", closed.Code, closed.Body.String())
	}
	requireLiveText(t, handler, issue.PrimaryArtifactID,
		`#close-decision urgency="med" multiple="false" state="resolved"`,
		`#peer-decision urgency="low" multiple="false" state="resolved"`,
	)
	if id := findBlockAsk(t, handler, issue.Key, issue.PrimaryArtifactID, "peer-decision"); id != "" {
		t.Fatalf("the browser's block is ask %s on a closed issue", id)
	}
}

// Every close withdraws the asks open at that close, and every reopen restores them, once each.
func TestEachCloseAndReopenWithdrawsAndRestoresOnce(t *testing.T) {
	handler, database := newInteractionHandler(t, func(database *store.Store) docs.API {
		service := docs.New(docs.Deps{Store: database, Settle: 20 * time.Millisecond})
		t.Cleanup(func() { _ = service.Shutdown(context.Background()) })
		return service
	})
	issue := createInteractionIssue(t, handler, "TEST", "Closed and reopened twice", closeDecision)
	awaitIndexedAskBlock(t, handler, issue.PrimaryArtifactID, "close-decision", "Ship it?")
	asks := []string{requireBlockAsk(t, handler, issue.Key, issue.PrimaryArtifactID, "close-decision"), createSessionAsk(t, handler, issue.Key, "Which region?")}
	opened := map[string]int{}
	for _, ask := range asks {
		opened[ask] = askEventCount(t, database, ask, "ask.opened")
	}
	for cycle := 1; cycle <= 2; cycle++ {
		closer := closers[cycle]
		if closed := closer.send(t, handler, issue.Key, map[string]any{"status": "done"}); closed.Code != http.StatusOK {
			t.Fatalf("close %d: status=%d body=%s", cycle, closed.Code, closed.Body.String())
		}
		closedBy := latestEventActor(t, database, issue.Key, "issue.closed")
		for _, ask := range asks {
			requireWithdrawn(t, handler, issue.Key, ask, closedBy)
			if count := askEventCount(t, database, ask, "ask.resolved"); count != cycle {
				t.Fatalf("after close %d ask %s has %d ask.resolved events, want %d", cycle, ask, count, cycle)
			}
		}
		requireLiveText(t, handler, issue.PrimaryArtifactID, `#close-decision urgency="med" multiple="false" state="resolved"`)
		if reopened := dispatchRequest(t, handler, http.MethodPatch, "/api/v1/issues/"+issue.Key, map[string]string{"status": "in_progress"}, "alice"); reopened.Code != http.StatusOK {
			t.Fatalf("reopen %d: status=%d body=%s", cycle, reopened.Code, reopened.Body.String())
		}
		for _, ask := range asks {
			if got := readAskRecord(t, handler, ask); got.State != "open" || got.Resolution != nil {
				t.Fatalf("after reopen %d ask %s = %s %#v, want open", cycle, ask, got.State, got.Resolution)
			}
			if count := askEventCount(t, database, ask, "ask.opened"); count != opened[ask]+cycle {
				t.Fatalf("after reopen %d ask %s has %d ask.opened events, want %d", cycle, ask, count, opened[ask]+cycle)
			}
		}
		requireLiveText(t, handler, issue.PrimaryArtifactID, `#close-decision urgency="med" multiple="false" state="open"`)
	}
}

// requireWithdrawn fails t unless ask reads as withdrawn by the close of issue, made by closedBy.
func requireWithdrawn(t *testing.T, handler http.Handler, issue, ask string, closedBy model.Actor) {
	t.Helper()
	got := readAskRecord(t, handler, ask)
	if got.State != "resolved" || got.Resolution == nil {
		t.Fatalf("ask %s = %s %#v, want resolved", ask, got.State, got.Resolution)
	}
	if got.Resolution.Kind != "withdrawn" || got.Resolution.Reason != "Withdrawn by the close of "+issue+"." {
		t.Fatalf("ask %s resolution = %#v, want withdrawn by the close of %s", ask, got.Resolution, issue)
	}
	if !reflect.DeepEqual(got.Resolution.Actor, closedBy) {
		t.Fatalf("ask %s withdrawn by %#v, want the closer %#v", ask, got.Resolution.Actor, closedBy)
	}
	if got.Resolution.At.IsZero() {
		t.Fatalf("ask %s resolution has no time", ask)
	}
}

// readAskRecord reads the ask as GET /api/v1/asks/{id} answers it.
func readAskRecord(t *testing.T, handler http.Handler, ask string) model.Ask {
	t.Helper()
	read := dispatchRequest(t, handler, http.MethodGet, "/api/v1/asks/"+ask, nil, "alice")
	if read.Code != http.StatusOK {
		t.Fatalf("read ask %s: status=%d body=%s", ask, read.Code, read.Body.String())
	}
	return decodeBody[struct {
		Ask model.Ask `json:"ask"`
	}](t, read).Ask
}

func createSessionAsk(t *testing.T, handler http.Handler, issue, question string) string {
	t.Helper()
	created := sessionRequest(t, handler, http.MethodPost, "/api/v1/issues/"+issue+"/asks", map[string]any{
		"question": question, "actor": sessionActor(),
	})
	if created.Code != http.StatusCreated {
		t.Fatalf("create ask %q: status=%d body=%s", question, created.Code, created.Body.String())
	}
	return decodeBody[model.Ask](t, created).ID
}

// findBlockAsk is the id of the ask indexed for blockID on document, "" when none is.
func findBlockAsk(t *testing.T, handler http.Handler, issue, document, blockID string) string {
	t.Helper()
	listed := dispatchRequest(t, handler, http.MethodGet, "/api/v1/issues/"+issue+"/asks?state=all", nil, "alice")
	if listed.Code != http.StatusOK {
		t.Fatalf("list asks: status=%d body=%s", listed.Code, listed.Body.String())
	}
	for _, ask := range decodeBody[[]model.Ask](t, listed) {
		if ask.BlockID != nil && *ask.BlockID == blockID && ask.BlockArtifact != nil && ask.BlockArtifact.ID == document {
			return ask.ID
		}
	}
	return ""
}

func requireBlockAsk(t *testing.T, handler http.Handler, issue, document, blockID string) string {
	t.Helper()
	id := findBlockAsk(t, handler, issue, document, blockID)
	if id == "" {
		t.Fatalf("no ask for block %s on %s", blockID, document)
	}
	return id
}

type issueDetailRead struct {
	LastSeq  int `json:"last_seq"`
	OpenAsks []struct {
		ID string `json:"id"`
	} `json:"open_asks"`
}

func readIssueDetail(t *testing.T, handler http.Handler, issue string) issueDetailRead {
	t.Helper()
	read := dispatchRequest(t, handler, http.MethodGet, "/api/v1/issues/"+issue, nil, "alice")
	if read.Code != http.StatusOK {
		t.Fatalf("read issue: status=%d body=%s", read.Code, read.Body.String())
	}
	return decodeBody[issueDetailRead](t, read)
}

func openAskIDs(detail issueDetailRead) []string {
	ids := make([]string, 0, len(detail.OpenAsks))
	for _, ask := range detail.OpenAsks {
		ids = append(ids, ask.ID)
	}
	return sorted(ids)
}

func sorted(values []string) []string {
	values = slices.Clone(values)
	slices.Sort(values)
	return values
}

func containsAll(have, want []string) bool {
	for _, value := range want {
		if !slices.Contains(have, value) {
			return false
		}
	}
	return true
}

func inboxAskIDs(t *testing.T, handler http.Handler) []string {
	t.Helper()
	inbox := dispatchRequest(t, handler, http.MethodGet, "/api/v1/inbox", nil, "alice")
	if inbox.Code != http.StatusOK {
		t.Fatalf("read inbox: status=%d body=%s", inbox.Code, inbox.Body.String())
	}
	var ids []string
	for _, ask := range decodeBody[[]model.Ask](t, inbox) {
		ids = append(ids, ask.ID)
	}
	return ids
}

// requireLiveText fails t unless document's live text holds every one of parts.
func requireLiveText(t *testing.T, handler http.Handler, document string, parts ...string) {
	t.Helper()
	live, _ := documentTexts(t, handler, document)
	if !contains(parts...)(live) {
		t.Fatalf("document %s live text:\n%s\nwant it to hold %q", document, live, parts)
	}
}

// awaitSettled waits until none of documents owes a settlement.
func awaitSettled(t *testing.T, database *store.Store, documents ...string) {
	t.Helper()
	for deadline := time.Now().Add(10 * time.Second); ; time.Sleep(20 * time.Millisecond) {
		var pending int
		if err := database.Pool.QueryRow(context.Background(), `
			select count(*) from doc_settlements_pending where artifact_id = any($1::uuid[])
		`, documents).Scan(&pending); err != nil {
			t.Fatalf("read pending settlements: %v", err)
		}
		if pending == 0 {
			return
		}
		if time.Now().After(deadline) {
			t.Fatalf("documents %v still owe %d settlements", documents, pending)
		}
	}
}

// askRowTexts is each ask's whole row as the database stores it.
func askRowTexts(t *testing.T, database *store.Store, asks ...string) []string {
	t.Helper()
	rows := make([]string, 0, len(asks))
	for _, ask := range asks {
		var row string
		if err := database.Pool.QueryRow(context.Background(), `select to_jsonb(a)::text from asks a where id = $1`, ask).Scan(&row); err != nil {
			t.Fatalf("read ask row %s: %v", ask, err)
		}
		rows = append(rows, row)
	}
	return rows
}

// versionHashes is every stored version of documents, each with its markdown's hash.
func versionHashes(t *testing.T, database *store.Store, documents ...string) []string {
	t.Helper()
	rows, err := database.Pool.Query(context.Background(), `
		select artifact_id::text || ' v' || number || ' ' || md5(coalesce(markdown, ''))
		from artifact_versions where artifact_id = any($1::uuid[]) order by artifact_id, number
	`, documents)
	if err != nil {
		t.Fatalf("read versions: %v", err)
	}
	defer rows.Close()
	var hashes []string
	for rows.Next() {
		var hash string
		if err := rows.Scan(&hash); err != nil {
			t.Fatalf("read version: %v", err)
		}
		hashes = append(hashes, hash)
	}
	if err := rows.Err(); err != nil {
		t.Fatalf("read versions: %v", err)
	}
	if len(hashes) == 0 {
		t.Fatalf("documents %v have no versions", documents)
	}
	return hashes
}

func reviewCount(t *testing.T, database *store.Store, documents ...string) int {
	t.Helper()
	var count int
	if err := database.Pool.QueryRow(context.Background(), `
		select count(*) from artifact_reviews where artifact_id = any($1::uuid[])
	`, documents).Scan(&count); err != nil {
		t.Fatalf("count reviews: %v", err)
	}
	return count
}

// decisionEventCount counts issue's events that record an answer or a review.
func decisionEventCount(t *testing.T, database *store.Store, issue string) int {
	t.Helper()
	var count int
	if err := database.Pool.QueryRow(context.Background(), `
		select count(*) from events
		where issue_key = $1 and type in ('ask.answered', 'artifact.approved', 'artifact.changes_requested')
	`, issue).Scan(&count); err != nil {
		t.Fatalf("count decision events: %v", err)
	}
	return count
}

func askEventCount(t *testing.T, database *store.Store, ask, eventType string) int {
	t.Helper()
	var count int
	if err := database.Pool.QueryRow(context.Background(), `
		select count(*) from events where type = $2 and payload->>'id' = $1
	`, ask, eventType).Scan(&count); err != nil {
		t.Fatalf("count %s events of %s: %v", eventType, ask, err)
	}
	return count
}

func latestEventActor(t *testing.T, database *store.Store, issue, eventType string) model.Actor {
	t.Helper()
	return scanEventActor(t, database, `
		select actor from events where issue_key = $1 and type = $2 order by seq desc limit 1
	`, issue, eventType)
}

func latestAskEventActor(t *testing.T, database *store.Store, ask, eventType string) model.Actor {
	t.Helper()
	return scanEventActor(t, database, `
		select actor from events where payload->>'id' = $1 and type = $2 order by id desc limit 1
	`, ask, eventType)
}

func scanEventActor(t *testing.T, database *store.Store, query string, args ...any) model.Actor {
	t.Helper()
	var raw []byte
	if err := database.Pool.QueryRow(context.Background(), query, args...).Scan(&raw); err != nil {
		t.Fatalf("read event actor: %v", err)
	}
	var actor model.Actor
	if err := json.Unmarshal(raw, &actor); err != nil {
		t.Fatalf("decode event actor: %v", err)
	}
	return actor
}
