package api

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"reflect"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/reearth/ygo/crdt"

	"github.com/sjawhar/envoy/internal/contracts"
	"github.com/sjawhar/envoy/internal/dispatch/docs"
	"github.com/sjawhar/envoy/internal/dispatch/events"
	"github.com/sjawhar/envoy/internal/dispatch/identity"
	"github.com/sjawhar/envoy/internal/dispatch/model"
	"github.com/sjawhar/envoy/internal/dispatch/outbox"
	"github.com/sjawhar/envoy/internal/dispatch/store"
)

// An approval request remains one conversation across document versions. A version move keeps its
// ask, carries its existing summary forward, rewords the card to the new version, and pauses it for
// the requesting agent. Handing the revision back updates the same card and makes the human its
// next actor again.
func TestApprovalRequestFollowsDocumentVersions(t *testing.T) {
	var documentService *docs.Service
	handler, _ := newInteractionHandler(t, func(database *store.Store) docs.API {
		documentService = docs.New(docs.Deps{Store: database, Settle: time.Hour})
		t.Cleanup(func() { _ = documentService.Shutdown(context.Background()) })
		return documentService
	})
	issue := createInteractionIssue(t, handler, "TEST", "Moving approval", "A spec")
	type approvalAsk struct {
		ID       string  `json:"id"`
		State    string  `json:"state"`
		Question string  `json:"question"`
		EditedAt *string `json:"edited_at"`
		Approval struct {
			Version          int `json:"version"`
			RequestedVersion int `json:"requested_version"`
		} `json:"approval"`
	}
	type requestResponse struct {
		Ask     approvalAsk `json:"ask"`
		Version int         `json:"version"`
	}
	var askID string

	request := func(summary string) *httptest.ResponseRecorder {
		return sessionRequest(t, handler, http.MethodPost, "/api/v1/artifacts/"+issue.PrimaryArtifactID+"/approval-requests", map[string]any{
			"actor": sessionActor(), "summary": summary,
		})
	}
	readAsk := func() approvalAsk {
		response := dispatchRequest(t, handler, http.MethodGet, "/api/v1/asks/"+askID, nil, "alice")
		if response.Code != http.StatusOK {
			t.Fatalf("read approval ask: status=%d body=%s", response.Code, response.Body.String())
		}
		return decodeBody[struct {
			Ask approvalAsk `json:"ask"`
		}](t, response).Ask
	}

	first := request("Caps retries at three attempts.")
	if first.Code != http.StatusCreated {
		t.Fatalf("request approval: status=%d body=%s", first.Code, first.Body.String())
	}
	opened := decodeBody[requestResponse](t, first).Ask
	askID = opened.ID
	if opened.Approval.Version != 1 || opened.Approval.RequestedVersion != 1 {
		t.Fatalf("opened approval = %#v, want requested version 1", opened)
	}

	if _, err := documentService.ReplaceText(context.Background(), issue.PrimaryArtifactID, "A revised spec", model.Actor{Kind: "session", ID: "session-0123456789abcdef"}); err != nil {
		t.Fatalf("revise document: %v", err)
	}
	if named := dispatchRequest(t, handler, http.MethodPost, "/api/v1/artifacts/"+issue.PrimaryArtifactID+"/versions", map[string]string{"summary": "revised"}, "alice"); named.Code != http.StatusCreated {
		t.Fatalf("name revised version: status=%d body=%s", named.Code, named.Body.String())
	}
	moved := readAsk()
	if moved.ID != askID || moved.State != "open" || moved.Approval.Version != 2 || moved.Approval.RequestedVersion != 1 || moved.Question != "Approve spec.md (version 2)? Caps retries at three attempts." {
		t.Fatalf("moved approval = %#v, want the original ask open at version 2 and requested at version 1", moved)
	}
	inbox := dispatchRequest(t, handler, http.MethodGet, "/api/v1/inbox?project=TEST", nil, "alice")
	if inbox.Code != http.StatusOK || !strings.Contains(inbox.Body.String(), `"id":"`+askID+`"`) || !strings.Contains(inbox.Body.String(), `"waiting_on":"agent"`) {
		t.Fatalf("inbox after version move: status=%d body=%s", inbox.Code, inbox.Body.String())
	}
	if counts := askEventCounts(t, handler, issue.Key, askID); counts["ask.opened"] != 1 || counts["ask.edited"] != 1 || counts["ask.handed_back"] != 0 || counts["ask.resolved"] != 0 {
		t.Fatalf("ask events after move = %#v, want one open, one rewording, no hand-back, and no resolution", counts)
	}

	returnedSummary := "Adds the rollback budget."
	returned := request(returnedSummary)
	if returned.Code != http.StatusCreated {
		t.Fatalf("hand approval back: status=%d body=%s", returned.Code, returned.Body.String())
	}
	handedBack := decodeBody[requestResponse](t, returned).Ask
	if handedBack.ID != askID || handedBack.Approval.Version != 2 || handedBack.Approval.RequestedVersion != 2 || handedBack.Question != "Approve spec.md (version 2)? "+returnedSummary {
		t.Fatalf("handed-back approval = %#v, want the same ask at the requested version with its new summary", handedBack)
	}
	inbox = dispatchRequest(t, handler, http.MethodGet, "/api/v1/inbox?project=TEST", nil, "alice")
	if inbox.Code != http.StatusOK || !strings.Contains(inbox.Body.String(), `"id":"`+askID+`"`) || !strings.Contains(inbox.Body.String(), `"waiting_on":"human"`) {
		t.Fatalf("inbox after hand-back: status=%d body=%s", inbox.Code, inbox.Body.String())
	}
	// The new summary rewords the card, and returning it to the human is the hand-back.
	if counts := askEventCounts(t, handler, issue.Key, askID); counts["ask.opened"] != 1 || counts["ask.edited"] != 2 || counts["ask.handed_back"] != 1 || counts["ask.resolved"] != 0 {
		t.Fatalf("ask events after hand-back = %#v, want one open, two rewordings, one hand-back, and no resolution", counts)
	}

	repeat := request(returnedSummary)
	if repeat.Code != http.StatusOK {
		t.Fatalf("repeat hand-back: status=%d body=%s", repeat.Code, repeat.Body.String())
	}
	if repeated := decodeBody[requestResponse](t, repeat).Ask; repeated.ID != askID || repeated.Question != handedBack.Question {
		t.Fatalf("repeat approval request = %#v, want the same unchanged ask", repeated)
	}
	if counts := askEventCounts(t, handler, issue.Key, askID); counts["ask.opened"] != 1 || counts["ask.edited"] != 2 || counts["ask.handed_back"] != 1 || counts["ask.resolved"] != 0 {
		t.Fatalf("ask events after repeat = %#v, want no additional event", counts)
	}

	approved := dispatchRequest(t, handler, http.MethodPost, "/api/v1/asks/"+askID+"/answer", map[string]any{
		"selected": []string{"Approve"}, "expected_edited_at": handedBack.EditedAt,
	}, "alice")
	if approved.Code != http.StatusOK {
		t.Fatalf("approve moved request: status=%d body=%s", approved.Code, approved.Body.String())
	}
	if got := readApproval(t, handler, issue.PrimaryArtifactID); got.Approval == nil || got.Approval.State != "approved" || got.Approval.Version == nil || *got.Approval.Version != 2 || got.Approval.AskID == nil || *got.Approval.AskID != askID {
		t.Fatalf("approval after moved request = %#v, want v2 approved through %s", got.Approval, askID)
	}
}

// A hand-back returns the approval request to the human whatever else it changes: a new version,
// a new summary, or nothing at all after the thread has moved on. Each records one ask.handed_back,
// and an ask.edited only when its summary rewords the question; a repeat with nothing newer in the
// thread records neither, and the next human reply takes the turn back on every read, the Inbox's
// order and the comment's own event included.
func TestApprovalHandBackWaitsOnTheHuman(t *testing.T) {
	for _, flow := range []struct {
		name     string
		move     bool
		progress bool
		summary  string
		// rewords says whether the first hand-back's summary changes the question.
		rewords bool
	}{
		{name: "after a human comment and version move", move: true, summary: "Clarifies the rollout.", rewords: true},
		{name: "after a human comment at the same version", summary: "Clarifies the rollout.", rewords: true},
		{name: "after a human comment and agent progress before a version move", move: true, progress: true, summary: "Clarifies the rollout.", rewords: true},
		{name: "after a human comment and agent progress with an unchanged summary", progress: true, summary: "Names the existing proposal."},
		{name: "after a human comment with the summary omitted"},
		{name: "after a human comment and version move with the summary repeated", move: true, summary: "Names the existing proposal."},
		{name: "after a human comment and version move with the summary omitted", move: true},
	} {
		t.Run(flow.name, func(t *testing.T) {
			var documentService *docs.Service
			handler, _ := newInteractionHandler(t, func(database *store.Store) docs.API {
				documentService = docs.New(docs.Deps{Store: database, Settle: time.Hour})
				t.Cleanup(func() { _ = documentService.Shutdown(context.Background()) })
				return documentService
			})
			issue := createInteractionIssue(t, handler, "TEST", "Hand-back turn", "A spec")
			// An empty summary is left out of the request, which then keeps the ask's own.
			request := func(summary string) *httptest.ResponseRecorder {
				body := map[string]any{"actor": sessionActor()}
				if summary != "" {
					body["summary"] = summary
				}
				return sessionRequest(t, handler, http.MethodPost,
					"/api/v1/artifacts/"+issue.PrimaryArtifactID+"/approval-requests", body)
			}
			first := request("Names the existing proposal.")
			if first.Code != http.StatusCreated {
				t.Fatalf("request approval: status=%d body=%s", first.Code, first.Body.String())
			}
			askID := decodeBody[struct {
				Ask struct {
					ID string `json:"id"`
				} `json:"ask"`
			}](t, first).Ask.ID
			human := dispatchRequest(t, handler, http.MethodPost, "/api/v1/issues/"+issue.Key+"/comments",
				map[string]any{"ask_id": askID, "body": "Please clarify the rollout."}, "alice")
			if human.Code != http.StatusCreated {
				t.Fatalf("human comment: status=%d body=%s", human.Code, human.Body.String())
			}
			if flow.progress {
				progress := sessionRequest(t, handler, http.MethodPost, "/api/v1/issues/"+issue.Key+"/comments",
					map[string]any{
						"actor": sessionActor(), "ask_id": askID, "body": "Checking the rollout.",
						"turn": "agent",
					})
				if progress.Code != http.StatusCreated {
					t.Fatalf("agent progress: status=%d body=%s", progress.Code, progress.Body.String())
				}
			}
			if flow.move {
				if _, err := documentService.ReplaceText(
					context.Background(), issue.PrimaryArtifactID, "A revised spec",
					model.Actor{Kind: "session", ID: sessionActor()["id"].(string)},
				); err != nil {
					t.Fatalf("revise document: %v", err)
				}
				if named := dispatchRequest(t, handler, http.MethodPost,
					"/api/v1/artifacts/"+issue.PrimaryArtifactID+"/versions",
					map[string]string{"summary": "revised"}, "alice"); named.Code != http.StatusCreated {
					t.Fatalf("name revised version: status=%d body=%s", named.Code, named.Body.String())
				}
			}
			counts := func() map[string]int { return askEventCounts(t, handler, issue.Key, askID) }
			handBack := func(step string, status int) {
				t.Helper()
				if handedBack := request(flow.summary); handedBack.Code != status {
					t.Fatalf("%s: status=%d body=%s, want %d", step, handedBack.Code, handedBack.Body.String(), status)
				}
			}
			wantEvents := func(step string, edited, handedBack int) {
				t.Helper()
				if got := counts(); got["ask.edited"] != edited || got["ask.handed_back"] != handedBack {
					t.Fatalf("ask events after %s = %#v, want %d ask.edited and %d ask.handed_back", step, got, edited, handedBack)
				}
			}
			edited := counts()["ask.edited"]
			if flow.rewords {
				edited++
			}
			beforeHandBack := readAskDetail(t, handler, askID)
			handBack("hand approval back", http.StatusCreated)
			for _, read := range []struct {
				name string
				ask  turnAskRow
			}{
				{name: "ask detail", ask: readAskDetail(t, handler, askID)},
				{name: "Inbox", ask: readInboxRow(t, handler, askID)},
			} {
				if read.ask.WaitingOn != "human" {
					t.Fatalf("%s waiting_on = %q, want human", read.name, read.ask.WaitingOn)
				}
			}
			issueAsks := sessionRequest(t, handler, http.MethodGet, "/api/v1/issues/"+issue.Key+"/asks?state=open", nil)
			if issueAsks.Code != http.StatusOK {
				t.Fatalf("list issue asks: status=%d body=%s", issueAsks.Code, issueAsks.Body.String())
			}
			if asks := decodeBody[[]turnAskRow](t, issueAsks); len(asks) != 1 || asks[0].WaitingOn != "human" {
				t.Fatalf("issue asks = %#v, want the handed-back ask waiting on human", asks)
			}
			openAsks := sessionRequest(t, handler, http.MethodGet, "/api/v1/asks/open?project=TEST", nil)
			if openAsks.Code != http.StatusOK {
				t.Fatalf("list open asks: status=%d body=%s", openAsks.Code, openAsks.Body.String())
			}
			if response := decodeBody[struct {
				Asks []turnAskRow `json:"asks"`
			}](t, openAsks); len(response.Asks) != 1 || response.Asks[0].WaitingOn != "human" {
				t.Fatalf("open asks = %#v, want the handed-back ask waiting on human", response)
			}
			wantEvents("the hand-back", edited, 1)
			if detail := readAskDetail(t, handler, askID); !flow.rewords && !reflect.DeepEqual(detail.EditedAt, beforeHandBack.EditedAt) {
				t.Fatalf("edited_at after a hand-back that rewords nothing = %v, want %v", detail.EditedAt, beforeHandBack.EditedAt)
			}

			// A repeat with nothing newer in the thread is a retry: no event, and the turn stays.
			handBack("repeat the hand-back", http.StatusOK)
			wantEvents("a repeated hand-back", edited, 1)
			if detail := readAskDetail(t, handler, askID); detail.WaitingOn != "human" {
				t.Fatalf("ask detail after a repeated hand-back waiting_on = %q, want human", detail.WaitingOn)
			}

			// Whose turn it is orders the Inbox before recency: a newer question waiting on its
			// agent comes after the handed-back request.
			question := openTurnAsk(t, handler, issue.Key, "Which rollout window?")
			if replied := dispatchRequest(t, handler, http.MethodPost, "/api/v1/issues/"+issue.Key+"/comments",
				map[string]any{"ask_id": question, "body": "Which windows are open?"}, "alice"); replied.Code != http.StatusCreated {
				t.Fatalf("human reply on the question: status=%d body=%s", replied.Code, replied.Body.String())
			}
			inbox := dispatchRequest(t, handler, http.MethodGet, "/api/v1/inbox", nil, "alice")
			if inbox.Code != http.StatusOK {
				t.Fatalf("read inbox: status=%d body=%s", inbox.Code, inbox.Body.String())
			}
			if rows := decodeBody[[]turnAskRow](t, inbox); len(rows) != 2 || rows[0].ID != askID || rows[0].WaitingOn != "human" || rows[1].ID != question || rows[1].WaitingOn != "agent" {
				t.Fatalf("Inbox rows = %#v, want the handed-back request %s waiting on human before the question %s waiting on agent", rows, askID, question)
			}

			// The next human reply takes the turn back, as the comment's own event reports it, and a
			// later hand-back returns it to the human again.
			again := dispatchRequest(t, handler, http.MethodPost, "/api/v1/issues/"+issue.Key+"/comments",
				map[string]any{"ask_id": askID, "body": "One more question."}, "alice")
			if again.Code != http.StatusCreated {
				t.Fatalf("second human comment: status=%d body=%s", again.Code, again.Body.String())
			}
			if response := decodeBody[map[string]any](t, again); response["ask_waiting_on"] != "agent" {
				t.Fatalf("second human comment ask_waiting_on = %#v, want agent", response["ask_waiting_on"])
			}
			if event := latestCommentCreatedPayload(t, handler, issue.Key); event["ask_waiting_on"] != "agent" {
				t.Fatalf("second human comment's comment.created ask_waiting_on = %#v, want agent", event["ask_waiting_on"])
			}
			if row := readInboxRow(t, handler, askID); row.WaitingOn != "agent" {
				t.Fatalf("Inbox after the second human comment waiting_on = %q, want agent", row.WaitingOn)
			}
			// The second hand-back repeats the question the first left, so it only hands back.
			handBack("hand approval back again", http.StatusCreated)
			if row := readInboxRow(t, handler, askID); row.WaitingOn != "human" {
				t.Fatalf("Inbox after the second hand-back waiting_on = %q, want human", row.WaitingOn)
			}
			wantEvents("the second hand-back", edited, 2)
		})
	}
}

// A hand-back that rewords nothing changes only whose turn it is. The question's revision stays,
// so it adds no entry to the card's edit history, and an answer the human started from the card
// before the hand-back is saved rather than refused ASK_EDITED.
func TestARewordlessHandBackKeepsTheQuestionsRevision(t *testing.T) {
	for _, flow := range []struct {
		name    string
		move    bool
		summary string
		version int
	}{
		{name: "after a human comment at the same version with the summary repeated", summary: "Names the existing proposal.", version: 1},
		{name: "after a version move with the summary omitted", move: true, version: 2},
	} {
		t.Run(flow.name, func(t *testing.T) {
			var documentService *docs.Service
			handler, _ := newInteractionHandler(t, func(database *store.Store) docs.API {
				documentService = docs.New(docs.Deps{Store: database, Settle: time.Hour})
				t.Cleanup(func() { _ = documentService.Shutdown(context.Background()) })
				return documentService
			})
			issue := createInteractionIssue(t, handler, "TEST", "Rewordless hand-back", "A spec")
			request := func(summary string) *httptest.ResponseRecorder {
				body := map[string]any{"actor": sessionActor()}
				if summary != "" {
					body["summary"] = summary
				}
				return sessionRequest(t, handler, http.MethodPost,
					"/api/v1/artifacts/"+issue.PrimaryArtifactID+"/approval-requests", body)
			}
			first := request("Names the existing proposal.")
			if first.Code != http.StatusCreated {
				t.Fatalf("request approval: status=%d body=%s", first.Code, first.Body.String())
			}
			askID := decodeBody[struct {
				Ask struct {
					ID string `json:"id"`
				} `json:"ask"`
			}](t, first).Ask.ID
			if human := dispatchRequest(t, handler, http.MethodPost, "/api/v1/issues/"+issue.Key+"/comments",
				map[string]any{"ask_id": askID, "body": "Please clarify the rollout."}, "alice"); human.Code != http.StatusCreated {
				t.Fatalf("human comment: status=%d body=%s", human.Code, human.Body.String())
			}
			if flow.move {
				if _, err := documentService.ReplaceText(
					context.Background(), issue.PrimaryArtifactID, "A revised spec",
					model.Actor{Kind: "session", ID: sessionActor()["id"].(string)},
				); err != nil {
					t.Fatalf("revise document: %v", err)
				}
				if named := dispatchRequest(t, handler, http.MethodPost,
					"/api/v1/artifacts/"+issue.PrimaryArtifactID+"/versions",
					map[string]string{"summary": "revised"}, "alice"); named.Code != http.StatusCreated {
					t.Fatalf("name revised version: status=%d body=%s", named.Code, named.Body.String())
				}
			}
			type detail struct {
				Ask struct {
					Question  string  `json:"question"`
					EditedAt  *string `json:"edited_at"`
					WaitingOn string  `json:"waiting_on"`
				} `json:"ask"`
				Edits []model.AskEdit `json:"edits"`
			}
			read := func() detail {
				t.Helper()
				return decodeBody[detail](t, dispatchRequest(t, handler, http.MethodGet, "/api/v1/asks/"+askID, nil, "alice"))
			}
			// The card the human has open, and the answer they are about to send from it.
			shown := read()
			if shown.Ask.WaitingOn != "agent" {
				t.Fatalf("waiting_on before the hand-back = %q, want agent", shown.Ask.WaitingOn)
			}

			if handedBack := request(flow.summary); handedBack.Code != http.StatusCreated {
				t.Fatalf("hand approval back: status=%d body=%s", handedBack.Code, handedBack.Body.String())
			}
			after := read()
			if after.Ask.WaitingOn != "human" || after.Ask.Question != shown.Ask.Question || !reflect.DeepEqual(after.Ask.EditedAt, shown.Ask.EditedAt) || len(after.Edits) != len(shown.Edits) {
				t.Fatalf("ask after a rewordless hand-back = %#v, want %#v with the same edit history now waiting on human", after, shown)
			}

			answered := dispatchRequest(t, handler, http.MethodPost, "/api/v1/asks/"+askID+"/answer", map[string]any{
				"selected": []string{"Approve"}, "expected_edited_at": shown.Ask.EditedAt,
			}, "alice")
			if answered.Code != http.StatusOK {
				t.Fatalf("answer from the card shown before the hand-back: status=%d body=%s", answered.Code, answered.Body.String())
			}
			if got := readApproval(t, handler, issue.PrimaryArtifactID); got.Approval == nil || got.Approval.State != "approved" || got.Approval.Version == nil || *got.Approval.Version != flow.version || got.Approval.AskID == nil || *got.Approval.AskID != askID {
				t.Fatalf("approval after the answer = %#v, want version %d approved through %s", got.Approval, flow.version, askID)
			}
		})
	}
}

// A reply whose transaction begins while a hand-back holds the owner row waits for that row and
// commits after the hand-back, so it is the thread's newest reply and its turn decides the request.
// The test parks the hand-back after it has taken the owner row, with a lock on the ask row that
// OpenApprovalAsk waits for, then starts the human's comment, which queues on the owner row.
func TestAReplyQueuedBehindAHandBackTakesTheTurn(t *testing.T) {
	ctx := context.Background()
	handler, database := newInteractionHandler(t, nil)
	issue := createInteractionIssue(t, handler, "TEST", "Racing hand-back", "A spec")
	approvalRequest := func() *httptest.ResponseRecorder {
		return sessionRequest(t, handler, http.MethodPost, "/api/v1/artifacts/"+issue.PrimaryArtifactID+"/approval-requests",
			map[string]any{"actor": sessionActor(), "summary": "Caps retries at three attempts."})
	}
	requested := approvalRequest()
	if requested.Code != http.StatusCreated {
		t.Fatalf("request approval: status=%d body=%s", requested.Code, requested.Body.String())
	}
	askID := decodeBody[struct {
		Ask struct {
			ID string `json:"id"`
		} `json:"ask"`
	}](t, requested).Ask.ID
	if human := dispatchRequest(t, handler, http.MethodPost, "/api/v1/issues/"+issue.Key+"/comments",
		map[string]any{"ask_id": askID, "body": "Why three retries?"}, "alice"); human.Code != http.StatusCreated {
		t.Fatalf("first human comment: status=%d body=%s", human.Code, human.Body.String())
	}

	hold, err := database.Pool.Begin(ctx)
	if err != nil {
		t.Fatalf("begin the ask row hold: %v", err)
	}
	defer hold.Rollback(ctx)
	if _, err := hold.Exec(ctx, `select 1 from asks where id = $1 for share`, askID); err != nil {
		t.Fatalf("hold the ask row: %v", err)
	}
	handedBack := make(chan *httptest.ResponseRecorder, 1)
	go func() { handedBack <- approvalRequest() }()
	waitForDatabaseLocks(t, hold, 1)
	commented := make(chan *httptest.ResponseRecorder, 1)
	go func() {
		commented <- dispatchRequest(t, handler, http.MethodPost, "/api/v1/issues/"+issue.Key+"/comments",
			map[string]any{"ask_id": askID, "body": "And the backoff?"}, "alice")
	}()
	waitForDatabaseLocks(t, hold, 2)
	if err := hold.Rollback(ctx); err != nil {
		t.Fatalf("release the ask row: %v", err)
	}
	handBack := awaitResponse(t, handedBack)
	comment := awaitResponse(t, commented)
	if comment.Code != http.StatusCreated {
		t.Fatalf("second human comment: status=%d body=%s", comment.Code, comment.Body.String())
	}
	// The comment is the thread's newest reply, so every read waits on the agent it asked.
	if response := decodeBody[map[string]any](t, comment); response["ask_waiting_on"] != "agent" {
		t.Fatalf("second human comment ask_waiting_on = %#v, want agent", response["ask_waiting_on"])
	}
	if event := latestCommentCreatedPayload(t, handler, issue.Key); event["ask_waiting_on"] != "agent" {
		t.Fatalf("second human comment's comment.created ask_waiting_on = %#v, want agent", event["ask_waiting_on"])
	}
	if detail := readAskDetail(t, handler, askID); detail.WaitingOn != "agent" {
		t.Fatalf("ask detail waiting_on = %q, want agent", detail.WaitingOn)
	}
	if row := readInboxRow(t, handler, askID); row.WaitingOn != "agent" {
		t.Fatalf("Inbox waiting_on = %q, want agent", row.WaitingOn)
	}
	if handBack.Code != http.StatusCreated {
		t.Fatalf("hand approval back: status=%d body=%s", handBack.Code, handBack.Body.String())
	}
	// The interleaving the test means to build: the hand-back committed, then the comment.
	order := []string{}
	for _, event := range issueEvents(t, handler, issue.Key) {
		if event.Type == "ask.handed_back" || event.Type == "comment.created" {
			order = append(order, event.Type)
		}
	}
	if want := []string{"comment.created", "ask.handed_back", "comment.created"}; !slices.Equal(order, want) {
		t.Fatalf("event order = %v, want %v", order, want)
	}
	// The agent answers the newer comment by handing back again, which the thread now owes.
	if again := approvalRequest(); again.Code != http.StatusCreated {
		t.Fatalf("hand approval back again: status=%d body=%s", again.Code, again.Body.String())
	}
	if counts := askEventCounts(t, handler, issue.Key, askID); counts["ask.handed_back"] != 2 {
		t.Fatalf("ask events = %#v, want two hand-backs", counts)
	}
	if row := readInboxRow(t, handler, askID); row.WaitingOn != "human" {
		t.Fatalf("Inbox after the second hand-back waiting_on = %q, want human", row.WaitingOn)
	}
}

// A human can still use the document header while the card has moved to a newer version but has
// not been handed back. The action answers that same open card and pins its review to the version
// the human saw, rather than resolving the card and losing its thread.
func TestHeaderReviewAnswersMovedApprovalAskBeforeTheAgentHandsItBack(t *testing.T) {
	for _, review := range []struct {
		name   string
		state  string
		reason string
		event  string
	}{
		{name: "approve", state: "approved", event: "artifact.approved"},
		{name: "request changes", state: "changes_requested", reason: "Name the rollback path.", event: "artifact.changes_requested"},
	} {
		t.Run(review.name, func(t *testing.T) {
			var documentService *docs.Service
			handler, _ := newInteractionHandler(t, func(database *store.Store) docs.API {
				documentService = docs.New(docs.Deps{Store: database, Settle: time.Hour})
				t.Cleanup(func() { _ = documentService.Shutdown(context.Background()) })
				return documentService
			})
			issue := createInteractionIssue(t, handler, "TEST", "Header review", "A spec")
			requested := sessionRequest(t, handler, http.MethodPost, "/api/v1/artifacts/"+issue.PrimaryArtifactID+"/approval-requests", map[string]any{
				"actor": sessionActor(), "summary": "Caps retries at three attempts.",
			})
			if requested.Code != http.StatusCreated {
				t.Fatalf("request approval: status=%d body=%s", requested.Code, requested.Body.String())
			}
			askID := decodeBody[struct {
				Ask struct {
					ID string `json:"id"`
				} `json:"ask"`
			}](t, requested).Ask.ID
			if _, err := documentService.ReplaceText(context.Background(), issue.PrimaryArtifactID, "A revised spec", model.Actor{Kind: "session", ID: "session-0123456789abcdef"}); err != nil {
				t.Fatalf("revise document: %v", err)
			}
			if named := dispatchRequest(t, handler, http.MethodPost, "/api/v1/artifacts/"+issue.PrimaryArtifactID+"/versions", map[string]string{"summary": "revised"}, "alice"); named.Code != http.StatusCreated {
				t.Fatalf("name revised version: status=%d body=%s", named.Code, named.Body.String())
			}

			header := dispatchRequest(t, handler, http.MethodPost, "/api/v1/artifacts/"+issue.PrimaryArtifactID+"/reviews", map[string]any{
				"state": review.state, "reason": review.reason,
			}, "alice")
			if header.Code != http.StatusCreated {
				t.Fatalf("review from header: status=%d body=%s", header.Code, header.Body.String())
			}
			ask := decodeBody[struct {
				Ask struct {
					State string `json:"state"`
				} `json:"ask"`
			}](t, dispatchRequest(t, handler, http.MethodGet, "/api/v1/asks/"+askID, nil, "alice")).Ask
			if ask.State != "answered" {
				t.Fatalf("header review left moved ask %#v, want it answered", ask)
			}
			if got := readApproval(t, handler, issue.PrimaryArtifactID); got.Approval == nil || got.Approval.State != review.state || got.Approval.Version == nil || *got.Approval.Version != 2 || got.Approval.AskID == nil || *got.Approval.AskID != askID {
				t.Fatalf("header review approval = %#v, want %s at v2 through %s", got.Approval, review.state, askID)
			}
			if counts := askEventCounts(t, handler, issue.Key, askID); counts[review.event] != 1 || counts["ask.resolved"] != 0 {
				t.Fatalf("header review events about %s = %#v, want one %s and no retraction", askID, counts, review.event)
			}
		})
	}
}

// Every path that writes a document version moves its open approval request to that version
// without creating or resolving an Inbox row. The moved request waits on its agent, and its
// ask.edited event reaches followers except the actor who made the version. A write that versions
// nothing leaves the request at the version the human was already shown.
//
// The caller names the actor whose edit moved the request. A route knows its own service write;
// settlement names the latest known browser editor, or SettlementActor when the browser edit was
// ambiguous.
func TestANewVersionMovesTheOpenApprovalAsk(t *testing.T) {
	asker := model.Actor{Kind: "session", ID: sessionActor()["id"].(string)}
	alice := model.Actor{Kind: "user", ID: "alice"}
	bob := model.Actor{Kind: "user", ID: "bob"}
	humanPeer := http.Header{"X-Dispatch-User": []string{"bob"}}
	askerPeer := http.Header{"Authorization": []string{"Bearer agent-token"}, "X-Dispatch-Actor": []string{`{"kind":"session","id":"` + asker.ID + `"}`}}
	edit := func(t *testing.T, handler http.Handler, artifactID string, body map[string]any) {
		t.Helper()
		body["ops"] = []map[string]string{{"op": "replace", "find": "A spec", "with": "A revised spec"}}
		body["actor"] = sessionActor()
		if edited := sessionRequest(t, handler, http.MethodPost, "/api/v1/artifacts/"+artifactID+"/edits", body); edited.Code != http.StatusOK {
			t.Fatalf("edit the document: status=%d body=%s", edited.Code, edited.Body.String())
		}
	}
	type document struct {
		handler         http.Handler
		database        *store.Store
		documentService *docs.Service
		broker          *events.Broker
		issueKey        string
		artifactID      string
		askID           string
		sockets         *servedSockets
		wsURL           string
		peers           []*syncedPeer
	}
	open := func(t *testing.T, settle time.Duration) *document {
		t.Helper()
		doc := &document{broker: events.NewBroker()}
		doc.handler, doc.database = newInteractionHandler(t, func(database *store.Store) docs.API {
			doc.documentService = docs.New(docs.Deps{
				Store: database, Events: doc.broker, Settle: settle, AgentToken: "agent-token",
				Identity: identity.HeaderIdentity{Header: "X-Dispatch-User", AllowedLogins: map[string]struct{}{"bob": {}}},
			})
			t.Cleanup(func() { _ = doc.documentService.Shutdown(context.Background()) })
			return doc.documentService
		})
		issue := createInteractionIssue(t, doc.handler, "TEST", "Moving approval", "A spec")
		doc.issueKey, doc.artifactID = issue.Key, issue.PrimaryArtifactID
		requested := sessionRequest(t, doc.handler, http.MethodPost, "/api/v1/artifacts/"+doc.artifactID+"/approval-requests", map[string]any{
			"actor": sessionActor(), "summary": "Caps retries at three attempts.",
		})
		if requested.Code != http.StatusCreated {
			t.Fatalf("request approval: status=%d body=%s", requested.Code, requested.Body.String())
		}
		doc.askID = decodeBody[struct {
			Ask struct {
				ID string `json:"id"`
			} `json:"ask"`
		}](t, requested).Ask.ID
		doc.sockets = &servedSockets{finished: make(map[string]chan struct{})}
		server := httptest.NewServer(doc.sockets.serve(doc.documentService.ServeHTTP))
		t.Cleanup(server.Close)
		doc.wsURL = "ws" + strings.TrimPrefix(server.URL, "http") + "/ws/doc/" + doc.artifactID
		return doc
	}
	connect := func(t *testing.T, doc *document, headers http.Header) *syncedPeer {
		t.Helper()
		peer := &syncedPeer{wsURL: doc.wsURL, sockets: doc.sockets, headers: headers, artifactID: doc.artifactID, doc: crdt.New()}
		peer.connect(t)
		t.Cleanup(peer.close)
		doc.peers = append(doc.peers, peer)
		return peer
	}
	// typeNote types a paragraph as peer and returns once the room holds it, so the peers the room
	// credits with it are the ones connected now.
	typeNote := func(t *testing.T, peer *syncedPeer, text string) {
		t.Helper()
		peer.appendParagraph(t, text)
		peer.barrier(t)
	}
	type askRead struct {
		Ask struct {
			State    string `json:"state"`
			Approval struct {
				Version          int `json:"version"`
				RequestedVersion int `json:"requested_version"`
			} `json:"approval"`
		} `json:"ask"`
	}

	for _, write := range []struct {
		name   string
		settle time.Duration
		write  func(t *testing.T, doc *document)
		// authors is how many writers version 2 credits, editor is who moves the ask, and
		// askerHears says whether the outbox sends its ask.edited to the asking session.
		authors    int
		editor     model.Actor
		askerHears bool
	}{
		{"an edit", time.Hour, func(t *testing.T, doc *document) {
			edit(t, doc.handler, doc.artifactID, map[string]any{"summary": "revise"})
		}, 1, asker, false},
		{"an edit with no summary", time.Hour, func(t *testing.T, doc *document) {
			edit(t, doc.handler, doc.artifactID, map[string]any{})
		}, 1, asker, false},
		{"a live write settlement versions", 20 * time.Millisecond, func(t *testing.T, doc *document) {
			// A live write that joins no transaction is versioned by settlement alone, so this
			// movement is settlement's and reaches followers once settlement commits.
			published, stop := doc.broker.Subscribe()
			defer stop()
			if _, err := doc.documentService.ReplaceText(context.Background(), doc.artifactID, "A revised spec", alice); err != nil {
				t.Fatalf("revise document: %v", err)
			}
			waitForArtifactVersion(t, doc.handler, doc.artifactID, 2)
			deadline := time.After(5 * time.Second)
			for {
				select {
				case event, ok := <-published:
					if !ok {
						t.Fatal("the document service closed the subscription before settlement published the moved approval")
					}
					if payload, isAsk := event.Payload.(model.AskEditEventPayload); isAsk && event.Type == "ask.edited" && payload.ID == doc.askID {
						return
					}
				case <-deadline:
					t.Fatalf("settlement published no ask.edited for %s", doc.askID)
				}
			}
		}, 1, alice, true},
		{"a named version", time.Hour, func(t *testing.T, doc *document) {
			if _, err := doc.documentService.ReplaceText(context.Background(), doc.artifactID, "A revised spec", alice); err != nil {
				t.Fatalf("revise document: %v", err)
			}
			if named := dispatchRequest(t, doc.handler, http.MethodPost, "/api/v1/artifacts/"+doc.artifactID+"/versions", map[string]string{"summary": "revised"}, "alice"); named.Code != http.StatusCreated {
				t.Fatalf("name a version: status=%d body=%s", named.Code, named.Body.String())
			}
		}, 1, alice, true},
		{"an upload", time.Hour, func(t *testing.T, doc *document) {
			if uploaded := sessionRequest(t, doc.handler, http.MethodPost, "/api/v1/issues/"+doc.issueKey+"/artifacts", map[string]any{
				"actor": sessionActor(), "name": "spec.md", "content": "A revised spec",
			}); uploaded.Code != http.StatusCreated {
				t.Fatalf("upload a version: status=%d body=%s", uploaded.Code, uploaded.Body.String())
			}
		}, 1, asker, false},
		{"two connected peers where the second types", time.Hour, func(t *testing.T, doc *document) {
			connect(t, doc, askerPeer)
			typeNote(t, connect(t, doc, humanPeer), "A note from bob.")
		}, 2, docs.SettlementActor, true},
		{"an earlier author and the next browser editor", time.Hour, func(t *testing.T, doc *document) {
			if _, err := doc.documentService.ReplaceText(context.Background(), doc.artifactID, "A temporary spec", alice); err != nil {
				t.Fatalf("write the earlier edit: %v", err)
			}
			if _, err := doc.documentService.ReplaceText(context.Background(), doc.artifactID, "A spec", alice); err != nil {
				t.Fatalf("undo the earlier edit: %v", err)
			}
			typeNote(t, connect(t, doc, humanPeer), "A note from bob.")
		}, 2, bob, true},
		{"an agent's joined write and a human's typing in one settlement window", time.Hour, func(t *testing.T, doc *document) {
			typist := connect(t, doc, humanPeer)
			ctx := context.Background()
			tx, err := doc.database.Pool.Begin(ctx)
			if err != nil {
				t.Fatalf("begin the agent's write: %v", err)
			}
			defer tx.Rollback(ctx)
			joined, ledger := doc.documentService.Join(ctx, tx)
			defer ledger.Discard()
			if _, err := doc.documentService.ReplaceText(joined, doc.artifactID, "A revised spec", asker); err != nil {
				t.Fatalf("the agent's joined write: %v", err)
			}
			if err := ledger.Commit(ctx); err != nil {
				t.Fatalf("commit the agent's joined write: %v", err)
			}
			waitForLiveText(t, doc.documentService, doc.artifactID, "A revised spec")
			typist.barrier(t)
			typeNote(t, typist, "A note from bob.")
		}, 2, bob, true},
		{"a human's typing and then an agent's edit that versions both", time.Hour, func(t *testing.T, doc *document) {
			typeNote(t, connect(t, doc, humanPeer), "A note from bob.")
			edit(t, doc.handler, doc.artifactID, map[string]any{})
		}, 2, asker, false},
		{"the only connected peer types", time.Hour, func(t *testing.T, doc *document) {
			typeNote(t, connect(t, doc, humanPeer), "A note from bob.")
		}, 1, bob, true},
		{"the asking session alone types over its connection", time.Hour, func(t *testing.T, doc *document) {
			typeNote(t, connect(t, doc, askerPeer), "A note from the asker.")
		}, 1, asker, false},
	} {
		t.Run(write.name, func(t *testing.T) {
			doc := open(t, write.settle)
			write.write(t, doc)
			// The last peer to leave settles the room, which versions whatever the peers wrote.
			for _, peer := range doc.peers {
				peer.close()
			}
			waitForArtifactVersion(t, doc.handler, doc.artifactID, 2)

			version := decodeBody[struct {
				Authors []model.Actor `json:"authors"`
			}](t, dispatchRequest(t, doc.handler, http.MethodGet, "/api/v1/artifacts/"+doc.artifactID+"/versions/2", nil, "alice"))
			if len(version.Authors) != write.authors {
				t.Fatalf("version 2 authors = %#v, want %d", version.Authors, write.authors)
			}
			// The move commits in the transaction that writes the version.
			moved := decodeBody[askRead](t, dispatchRequest(t, doc.handler, http.MethodGet, "/api/v1/asks/"+doc.askID, nil, "alice")).Ask
			if moved.State != "open" || moved.Approval.Version != 2 || moved.Approval.RequestedVersion != 1 {
				t.Fatalf("the ask after %s = %#v, want the same open request at version 2 waiting for its agent", write.name, moved)
			}
			// Whoever wrote the version, the moved request waits on its agent, and the document's
			// approval says so: an agent whose own revision moved it hears no event about that.
			if got := readApproval(t, doc.handler, doc.artifactID); got.Approval.State != "awaiting" || got.Approval.WaitingOn != "agent" || got.Approval.LatestVersion != 2 || got.Approval.AskID == nil || *got.Approval.AskID != doc.askID {
				t.Fatalf("approval after %s = %#v, want awaiting on the moved ask %s, waiting on its agent", write.name, got.Approval, doc.askID)
			}
			var logged []struct {
				Type    string      `json:"type"`
				Actor   model.Actor `json:"actor"`
				Payload struct {
					ID string `json:"id"`
				} `json:"payload"`
			}
			if err := json.NewDecoder(dispatchRequest(t, doc.handler, http.MethodGet, "/api/v1/issues/"+doc.issueKey+"/events", nil, "alice").Body).Decode(&logged); err != nil {
				t.Fatalf("decode events: %v", err)
			}
			edited, resolved := 0, 0
			for _, event := range logged {
				if event.Payload.ID != doc.askID {
					continue
				}
				switch event.Type {
				case "ask.edited":
					edited++
					if !event.Actor.SameAs(write.editor) {
						t.Fatalf("ask.edited actor = %#v, want %#v", event.Actor, write.editor)
					}
				case "ask.resolved":
					resolved++
				}
			}
			if edited != 1 || resolved != 0 {
				t.Fatalf("approval events for %s: edits=%d resolved=%d, want one move and no resolution", doc.askID, edited, resolved)
			}

			deliveries := askEventDeliveries(t, doc.database, doc.documentService, "ask.edited", doc.askID)
			if heard := slices.Contains(deliveries[0].Destinations, contracts.AgentSubject(asker.ID)); heard != write.askerHears {
				t.Fatalf("the outbox published the move to %v; the asking session's topic among them = %t, want %t", deliveries[0].Destinations, heard, write.askerHears)
			}
		})
	}

	t.Run("a write that versions nothing", func(t *testing.T) {
		doc := open(t, time.Hour)
		unchanged := sessionRequest(t, doc.handler, http.MethodPost, "/api/v1/artifacts/"+doc.artifactID+"/edits", map[string]any{
			"ops":     []map[string]string{{"op": "replace", "find": "A spec", "with": "A spec"}},
			"summary": "nothing", "actor": sessionActor(),
		})
		if unchanged.Code != http.StatusOK || !strings.Contains(unchanged.Body.String(), `"changed":false`) {
			t.Fatalf("an edit that changes nothing: status=%d body=%s", unchanged.Code, unchanged.Body.String())
		}
		if still := decodeBody[askRead](t, dispatchRequest(t, doc.handler, http.MethodGet, "/api/v1/asks/"+doc.askID, nil, "alice")).Ask; still.State != "open" {
			t.Fatalf("the ask at the latest version after an edit that versions nothing = %#v, want open", still)
		}
		if got := readApproval(t, doc.handler, doc.artifactID); got.Approval.State != "awaiting" || got.Approval.WaitingOn != "human" || got.Approval.LatestVersion != 1 || got.Approval.AskID == nil || *got.Approval.AskID != doc.askID {
			t.Fatalf("approval after an edit that versions nothing = %#v, want awaiting on ask %s, waiting on the human", got.Approval, doc.askID)
		}
	})

	t.Run("a version no writer is credited with", func(t *testing.T) {
		// Settlement can version a change it credits to nobody, such as one written before a room
		// reload; it is the actor of the moved approval event.
		doc := open(t, time.Hour)
		ctx := context.Background()
		tx, err := doc.database.Pool.Begin(ctx)
		if err != nil {
			t.Fatalf("begin: %v", err)
		}
		defer tx.Rollback(ctx)
		moved, err := docs.MoveApprovalAsk(ctx, tx, events.NewBroker(), doc.artifactID, model.Version{Number: 2}, docs.SettlementActor, "")
		if err != nil {
			t.Fatalf("move with no known writer: %v", err)
		}
		if err := tx.Commit(ctx); err != nil {
			t.Fatalf("commit: %v", err)
		}
		if len(moved) != 1 || !moved[0].Actor.SameAs(docs.SettlementActor) {
			t.Fatalf("moved events = %#v, want one ask.edited by %#v", moved, docs.SettlementActor)
		}
		ask := decodeBody[askRead](t, dispatchRequest(t, doc.handler, http.MethodGet, "/api/v1/asks/"+doc.askID, nil, "alice")).Ask
		if ask.State != "open" || ask.Approval.Version != 2 || ask.Approval.RequestedVersion != 1 {
			t.Fatalf("the ask after a version no writer is credited with = %#v, want an open moved ask", ask)
		}
	})

	// A further version of a request already waiting on its agent changes only the version it
	// names, so it is recorded as a human's unnamed version is: on the issue's own topic with notify
	// off, and on no follower's. Only the move that takes the request from the human wakes anyone,
	// and once the agent hands it back the next version's move wakes again.
	t.Run("a human typing settled versions one after another", func(t *testing.T) {
		doc := open(t, 50*time.Millisecond)
		typist := connect(t, doc, humanPeer)
		typeVersion := func(note string, version int) {
			t.Helper()
			typeNote(t, typist, note)
			waitForArtifactVersion(t, doc.handler, doc.artifactID, version)
		}
		typeVersion("A first note.", 2)
		typeVersion("A second note.", 3)
		typeVersion("A third note.", 4)
		if handedBack := sessionRequest(t, doc.handler, http.MethodPost, "/api/v1/artifacts/"+doc.artifactID+"/approval-requests",
			map[string]any{"actor": sessionActor()}); handedBack.Code != http.StatusCreated {
			t.Fatalf("hand approval back: status=%d body=%s", handedBack.Code, handedBack.Body.String())
		}
		typeVersion("A fourth note.", 5)

		deliveries := askEventDeliveries(t, doc.database, doc.documentService, "ask.edited", doc.askID)
		if len(deliveries) != 4 {
			t.Fatalf("ask.edited events = %#v, want one move to each of versions 2 to 5", deliveries)
		}
		var wakes, routed []int
		for move, delivery := range deliveries {
			if delivery.Notify {
				wakes = append(wakes, move)
			}
			issueTopics := 0
			for _, topic := range delivery.Destinations {
				if strings.HasPrefix(topic, contracts.AgentTopicPrefix) {
					routed = append(routed, move)
				} else {
					issueTopics++
				}
			}
			if issueTopics != 1 {
				t.Fatalf("move %d reached %v, want the issue's own topic once", move, delivery.Destinations)
			}
		}
		if want := []int{0, 3}; !slices.Equal(wakes, want) || !slices.Equal(routed, want) {
			t.Fatalf("moves that woke anyone = %v and reached a session's topic = %v, want %v for both: the first move and the first after the hand-back", wakes, routed, want)
		}
	})
}

// askEventDelivery is what the outbox did with one event: whether it woke anyone, and the topics it
// published the event to.
type askEventDelivery struct {
	Notify       bool
	Destinations []string
}

// askEventDeliveries runs the outbox until it has published every eventType event about askID, and
// returns what each one reached, oldest first.
func askEventDeliveries(t *testing.T, database *store.Store, documentService docs.API, eventType, askID string) []askEventDelivery {
	t.Helper()
	ctx, cancel := context.WithCancel(context.Background())
	stopped := make(chan struct{})
	go func() {
		defer close(stopped)
		outbox.Run(ctx, outbox.Deps{Store: database, Publisher: acceptingPublisher{}, Broker: events.NewBroker(), Docs: documentService})
	}()
	defer func() {
		cancel()
		<-stopped
	}()
	const about = `from events where type = $1 and payload->>'id' = $2`
	deadline := time.Now().Add(5 * time.Second)
	for {
		var unpublished, total int
		if err := database.Pool.QueryRow(context.Background(), `select count(*) filter (where published_at is null), count(*) `+about,
			eventType, askID).Scan(&unpublished, &total); err != nil {
			t.Fatalf("count %s events for %s: %v", eventType, askID, err)
		}
		if total > 0 && unpublished == 0 {
			break
		}
		if time.Now().After(deadline) {
			t.Fatalf("the outbox left %d of %d %s events for %s unpublished", unpublished, total, eventType, askID)
		}
		time.Sleep(10 * time.Millisecond)
	}
	rows, err := database.Pool.Query(context.Background(), `select notify, published_destinations `+about+` order by id`, eventType, askID)
	if err != nil {
		t.Fatalf("read %s deliveries for %s: %v", eventType, askID, err)
	}
	deliveries, err := pgx.CollectRows(rows, func(row pgx.CollectableRow) (askEventDelivery, error) {
		var delivery askEventDelivery
		return delivery, row.Scan(&delivery.Notify, &delivery.Destinations)
	})
	if err != nil {
		t.Fatalf("decode %s deliveries for %s: %v", eventType, askID, err)
	}
	return deliveries
}

// acceptingPublisher takes every envelope the outbox publishes; the outbox records each topic on
// its event.
type acceptingPublisher struct{}

func (acceptingPublisher) Publish(contracts.Envelope) error { return nil }
