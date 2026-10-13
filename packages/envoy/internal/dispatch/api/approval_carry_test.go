package api

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"reflect"
	"strings"
	"testing"
	"time"

	"github.com/sjawhar/envoy/internal/dispatch/docs"
	"github.com/sjawhar/envoy/internal/dispatch/model"
	"github.com/sjawhar/envoy/internal/dispatch/store"
)

// A human's approval is what a request to approve again follows. A request opened on a document
// with an earlier approval names that approval's version, time and approver in its approval
// (approved_version, approved_at, approved_by), so the human reads what he is approving since the
// version he approved rather than from nothing; a first request names none. And his answer to a
// decision block carries his approval forward: when that answer is the whole change since the
// version he approved, the version it writes gets an approved review in his name, in the same
// transaction (docs.carryApproval, LEGION-573).

// carriedAsk is an approval ask as the carry tests read it: the document version it names and the
// earlier approval it carries, each of the three absent when the document was never approved.
type carriedAsk struct {
	ID       string `json:"id"`
	Approval struct {
		Version          int          `json:"version"`
		RequestedVersion int          `json:"requested_version"`
		ApprovedVersion  *int         `json:"approved_version"`
		ApprovedAt       *string      `json:"approved_at"`
		ApprovedBy       *model.Actor `json:"approved_by"`
	} `json:"approval"`
}

// approvalCarryHandler serves documents that version on an agent's edit in the edit's own
// transaction and settle a human's live write - an answer to a decision block, a resolution - in
// milliseconds, so a test waits for the version that write settles into rather than racing it.
func approvalCarryHandler(t *testing.T) http.Handler {
	t.Helper()
	return settlingCarryHandler(t, 20*time.Millisecond)
}

// settlingCarryHandler is approvalCarryHandler settling settle after a live write. A test that
// needs two writes in one version - two humans' answers, an answer and an agent's edit - uses a
// window wide enough that both land in it.
func settlingCarryHandler(t *testing.T, settle time.Duration) http.Handler {
	t.Helper()
	handler, _ := newInteractionHandler(t, func(database *store.Store) docs.API {
		documentService := docs.New(docs.Deps{Store: database, Settle: settle})
		t.Cleanup(func() { _ = documentService.Shutdown(context.Background()) })
		return documentService
	})
	return handler
}

// reviewDocument reviews the document's latest settled version from its header as login and
// returns the review it wrote.
func reviewDocument(t *testing.T, handler http.Handler, artifactID, login, state, reason string) model.ArtifactReview {
	t.Helper()
	body := map[string]any{"state": state}
	if reason != "" {
		body["reason"] = reason
	}
	reviewed := dispatchRequest(t, handler, http.MethodPost, "/api/v1/artifacts/"+artifactID+"/reviews", body, login)
	if reviewed.Code != http.StatusCreated {
		t.Fatalf("%s reviews %s: status=%d body=%s", login, state, reviewed.Code, reviewed.Body.String())
	}
	return decodeBody[model.ArtifactReview](t, reviewed)
}

// reviseDocument replaces find with with as the asking agent through the edits route, which
// versions the document in the route's own transaction, and waits until that version reads.
func reviseDocument(t *testing.T, handler http.Handler, artifactID, find, with string, number int) {
	t.Helper()
	edited := sessionRequest(t, handler, http.MethodPost, "/api/v1/artifacts/"+artifactID+"/edits", map[string]any{
		"ops":   []map[string]string{{"op": "replace", "find": find, "with": with}},
		"actor": sessionActor(),
	})
	if edited.Code != http.StatusOK {
		t.Fatalf("revise the document to version %d: status=%d body=%s", number, edited.Code, edited.Body.String())
	}
	waitForArtifactVersion(t, handler, artifactID, number)
}

// requestApproval opens the asking agent's approval request on the document and returns the ask
// the route answers with.
func requestApproval(t *testing.T, handler http.Handler, artifactID string) carriedAsk {
	t.Helper()
	requested := sessionRequest(t, handler, http.MethodPost, "/api/v1/artifacts/"+artifactID+"/approval-requests", map[string]any{"actor": sessionActor()})
	if requested.Code != http.StatusCreated {
		t.Fatalf("request approval: status=%d body=%s", requested.Code, requested.Body.String())
	}
	return decodeBody[struct {
		Ask carriedAsk `json:"ask"`
	}](t, requested).Ask
}

// readCarriedAsk reads the ask as GET /api/v1/asks/{id} answers it.
func readCarriedAsk(t *testing.T, handler http.Handler, askID string) carriedAsk {
	t.Helper()
	read := dispatchRequest(t, handler, http.MethodGet, "/api/v1/asks/"+askID, nil, "alice")
	if read.Code != http.StatusOK {
		t.Fatalf("read approval ask: status=%d body=%s", read.Code, read.Body.String())
	}
	return decodeBody[struct {
		Ask carriedAsk `json:"ask"`
	}](t, read).Ask
}

// requireCarriedApproval fails t unless ask carries exactly review: its version, its created_at as
// RFC 3339 text, and its actor.
func requireCarriedApproval(t *testing.T, ask carriedAsk, review model.ArtifactReview) {
	t.Helper()
	carried := ask.Approval
	if carried.ApprovedVersion == nil || *carried.ApprovedVersion != review.Version {
		t.Fatalf("approved_version = %v, want %d", carried.ApprovedVersion, review.Version)
	}
	if carried.ApprovedAt == nil {
		t.Fatalf("approval = %#v, want approved_at", carried)
	}
	approvedAt, err := time.Parse(time.RFC3339, *carried.ApprovedAt)
	if err != nil {
		t.Fatalf("approved_at %q is not RFC 3339: %v", *carried.ApprovedAt, err)
	}
	if !approvedAt.Equal(review.CreatedAt) {
		t.Fatalf("approved_at = %s, want the review's created_at %s", approvedAt.Format(time.RFC3339Nano), review.CreatedAt.Format(time.RFC3339Nano))
	}
	if carried.ApprovedBy == nil || !reflect.DeepEqual(*carried.ApprovedBy, review.Actor) {
		t.Fatalf("approved_by = %#v, want the approving actor %#v", carried.ApprovedBy, review.Actor)
	}
}

// A request on a document a human approved at an earlier version names that approval: the version
// he approved, when, and that it was him.
func TestARequestOnAnApprovedDocumentCarriesTheApproval(t *testing.T) {
	handler := approvalCarryHandler(t)
	issue := createInteractionIssue(t, handler, "TEST", "Approve me again", "A spec")
	review := reviewDocument(t, handler, issue.PrimaryArtifactID, "alice", "approved", "")
	if review.Version != 1 || review.Actor.Kind != "user" || review.Actor.ID != "alice" {
		t.Fatalf("review = %#v, want alice's approval of version 1", review)
	}
	reviseDocument(t, handler, issue.PrimaryArtifactID, "A spec", "A revised spec", 2)

	ask := requestApproval(t, handler, issue.PrimaryArtifactID)
	if ask.Approval.Version != 2 || ask.Approval.RequestedVersion != 2 {
		t.Fatalf("request approval = %#v, want a request at version 2", ask.Approval)
	}
	requireCarriedApproval(t, ask, review)
	requireCarriedApproval(t, readCarriedAsk(t, handler, ask.ID), review)
}

// A first request, on a document nobody has approved, carries none of the three keys: not null
// values, no keys at all.
func TestAFirstRequestCarriesNoEarlierApproval(t *testing.T) {
	handler := approvalCarryHandler(t)
	issue := createInteractionIssue(t, handler, "TEST", "Approve me", "A spec")
	requested := sessionRequest(t, handler, http.MethodPost, "/api/v1/artifacts/"+issue.PrimaryArtifactID+"/approval-requests", map[string]any{"actor": sessionActor()})
	if requested.Code != http.StatusCreated {
		t.Fatalf("request approval: status=%d body=%s", requested.Code, requested.Body.String())
	}
	approval := decodeBody[struct {
		Ask struct {
			Approval map[string]json.RawMessage `json:"approval"`
		} `json:"ask"`
	}](t, requested).Ask.Approval
	if string(approval["version"]) != "1" || string(approval["requested_version"]) != "1" {
		t.Fatalf("approval = %s, want version and requested_version 1", requested.Body.String())
	}
	for _, key := range []string{"approved_version", "approved_at", "approved_by"} {
		if raw, present := approval[key]; present {
			t.Fatalf("a first request carries %s = %s, want the key absent", key, raw)
		}
	}
}

// Two humans approved at different versions: the request names the newest approval, whoever gave
// it, and a later request for changes, which approves nothing, does not displace it.
func TestARequestCarriesTheNewestApprovalWhoeverGaveIt(t *testing.T) {
	handler := approvalCarryHandler(t)
	issue := createInteractionIssue(t, handler, "TEST", "Approved twice", "A spec")
	alices := reviewDocument(t, handler, issue.PrimaryArtifactID, "alice", "approved", "")
	reviseDocument(t, handler, issue.PrimaryArtifactID, "A spec", "A second spec", 2)
	bobs := reviewDocument(t, handler, issue.PrimaryArtifactID, "bob", "approved", "")
	if bobs.Version != 2 || bobs.Actor.ID != "bob" || bobs.Actor.ID == alices.Actor.ID {
		t.Fatalf("second review = %#v, want bob's approval of version 2", bobs)
	}
	reviseDocument(t, handler, issue.PrimaryArtifactID, "A second spec", "A third spec", 3)

	requireCarriedApproval(t, requestApproval(t, handler, issue.PrimaryArtifactID), bobs)

	// Alice answers the open request from the header asking for changes, which is a newer review
	// but no approval; the request the next revision opens still follows bob's approval.
	changes := reviewDocument(t, handler, issue.PrimaryArtifactID, "alice", "changes_requested", "Name the rollback path.")
	if changes.Version != 3 || changes.State != "changes_requested" {
		t.Fatalf("changes requested = %#v, want a changes_requested review of version 3", changes)
	}
	reviseDocument(t, handler, issue.PrimaryArtifactID, "A third spec", "A fourth spec", 4)
	requireCarriedApproval(t, requestApproval(t, handler, issue.PrimaryArtifactID), bobs)
}

// A version that moves the open request rewords it to the new version and re-encodes its whole
// approval, so the earlier approval it carries survives the move unchanged.
func TestAVersionMoveKeepsTheCarriedApproval(t *testing.T) {
	handler := approvalCarryHandler(t)
	issue := createInteractionIssue(t, handler, "TEST", "Moving carried approval", "A spec")
	review := reviewDocument(t, handler, issue.PrimaryArtifactID, "alice", "approved", "")
	reviseDocument(t, handler, issue.PrimaryArtifactID, "A spec", "A revised spec", 2)
	ask := requestApproval(t, handler, issue.PrimaryArtifactID)
	requireCarriedApproval(t, ask, review)

	reviseDocument(t, handler, issue.PrimaryArtifactID, "A revised spec", "A twice revised spec", 3)
	moved := readCarriedAsk(t, handler, ask.ID)
	if moved.ID != ask.ID || moved.Approval.Version != 3 || moved.Approval.RequestedVersion != 2 {
		t.Fatalf("ask after the version move = %#v, want the same request at version 3, requested at version 2", moved)
	}
	requireCarriedApproval(t, moved, review)
}

// The decision blocks the carry tests write: each a block id, a question and two options.
const (
	waveAsk   = ":::ask{#wave urgency=\"med\" multiple=\"false\"}\nHow many regions take the first wave?\n\n- Two: The two with the fewest tenants.\n- Five: Half of them.\n:::\n"
	regionAsk = ":::ask{#region urgency=\"low\" multiple=\"false\"}\nWhich region goes first?\n\n- East: The quiet one.\n- West: The busy one.\n:::\n"
)

// carrySentence is the reason a carried review records, naming the approval it carries forward.
func carrySentence(from int) string {
	return fmt.Sprintf("Carried forward from the approval of version %d: the answer to a decision block was the whole change since it.", from)
}

// insertDecisionBlocks inserts markdown, holding the ask blocks named by blocks (id to question),
// at the end of the document as the asking agent, which versions the document to number in the
// edit's own transaction; it then waits for settlement to index every block and returns each
// block's ask id by block id.
func insertDecisionBlocks(t *testing.T, handler http.Handler, issueKey, artifactID, markdown string, number int, blocks map[string]string) map[string]string {
	t.Helper()
	inserted := sessionRequest(t, handler, http.MethodPost, "/api/v1/artifacts/"+artifactID+"/edits", map[string]any{
		"actor": sessionActor(),
		"ops":   []map[string]string{{"op": "insert", "after": "end", "markdown": markdown}},
	})
	if inserted.Code != http.StatusOK {
		t.Fatalf("insert the decision blocks: status=%d body=%s", inserted.Code, inserted.Body.String())
	}
	waitForArtifactVersion(t, handler, artifactID, number)
	return indexedDecisionBlocks(t, handler, issueKey, artifactID, blocks)
}

// indexedDecisionBlocks waits for settlement to index each block of blocks (id to question) on the
// document and returns each block's ask id by block id.
func indexedDecisionBlocks(t *testing.T, handler http.Handler, issueKey, artifactID string, blocks map[string]string) map[string]string {
	t.Helper()
	asks := make(map[string]string, len(blocks))
	for blockID, question := range blocks {
		awaitIndexedAskBlock(t, handler, artifactID, blockID, question)
		asks[blockID] = blockAskID(t, handler, issueKey, blockID)
	}
	return asks
}

// answerDecisionBlock answers the block ask as login with selected and text, on the revision a
// block ask nobody reworded carries (no edited_at); replaces, when set, is the `at` of the answer
// this one changes.
func answerDecisionBlock(t *testing.T, handler http.Handler, askID, login string, selected []string, text string, replaces *time.Time) {
	t.Helper()
	body := map[string]any{"selected": selected, "expected_edited_at": nil}
	if text != "" {
		body["text"] = text
	}
	if replaces != nil {
		body["expected_answer_at"] = replaces.UTC().Format(time.RFC3339Nano)
	}
	answered := dispatchRequest(t, handler, http.MethodPost, "/api/v1/asks/"+askID+"/answer", body, login)
	if answered.Code != http.StatusOK {
		t.Fatalf("%s answers the decision block: status=%d body=%s", login, answered.Code, answered.Body.String())
	}
}

// servedReview is one review as GET /api/v1/artifacts/{id}/reviews serves it, its actor kept as
// the JSON the route wrote so two reviews' actors compare byte for byte.
type servedReview struct {
	ID      string          `json:"id"`
	Version int             `json:"version"`
	State   string          `json:"state"`
	Actor   json.RawMessage `json:"actor"`
	Reason  *string         `json:"reason"`
	AskID   *string         `json:"ask_id"`
}

// documentReviews is the document's reviews, newest first.
func documentReviews(t *testing.T, handler http.Handler, artifactID string) []servedReview {
	t.Helper()
	listed := dispatchRequest(t, handler, http.MethodGet, "/api/v1/artifacts/"+artifactID+"/reviews", nil, "alice")
	if listed.Code != http.StatusOK {
		t.Fatalf("list reviews: status=%d body=%s", listed.Code, listed.Body.String())
	}
	return decodeBody[[]servedReview](t, listed)
}

// requireCarriedReview fails t unless the document's newest review is the carry of the approval
// of version from to version: approved, in the approving actor's name byte for byte, with the
// carry's reason and askID, and unless the document now reads approved at version, which is its
// latest. It returns that review.
func requireCarriedReview(t *testing.T, handler http.Handler, artifactID string, version, from int, askID *string) servedReview {
	t.Helper()
	reviews := documentReviews(t, handler, artifactID)
	var approval *servedReview
	for index := range reviews {
		if reviews[index].Version == from && reviews[index].State == "approved" {
			approval = &reviews[index]
			break
		}
	}
	if approval == nil {
		t.Fatalf("reviews = %+v, want an approval of version %d among them", reviews, from)
	}
	carried := reviews[0]
	if carried.Version != version || carried.State != "approved" {
		t.Fatalf("newest review = %+v, want an approval of version %d", carried, version)
	}
	if string(carried.Actor) != string(approval.Actor) {
		t.Fatalf("carried review actor = %s, want the approval's %s", carried.Actor, approval.Actor)
	}
	if carried.Reason == nil || *carried.Reason != carrySentence(from) {
		t.Fatalf("carried review reason = %v, want %q", carried.Reason, carrySentence(from))
	}
	if !reflect.DeepEqual(carried.AskID, askID) {
		t.Fatalf("carried review ask_id = %v, want %v", carried.AskID, askID)
	}
	got := readApproval(t, handler, artifactID)
	if got.Approval == nil || got.Approval.State != "approved" || got.Approval.LatestVersion != version || got.Approval.Version == nil || *got.Approval.Version != version {
		t.Fatalf("approval after the carry = %#v, want approved at version %d, the latest", got.Approval, version)
	}
	return carried
}

// requireNoCarry fails t unless the document still has exactly reviews reviews and reads state.
func requireNoCarry(t *testing.T, handler http.Handler, artifactID string, reviews int, state string) {
	t.Helper()
	if listed := documentReviews(t, handler, artifactID); len(listed) != reviews {
		t.Fatalf("reviews = %+v, want the %d written before the answer and no carried one", listed, reviews)
	}
	if got := readApproval(t, handler, artifactID); got.Approval == nil || got.Approval.State != state {
		t.Fatalf("approval = %#v, want %s", got.Approval, state)
	}
}

// carryEvent is one of the issue's events as the carry tests read them: its order on the issue,
// its actor and its payload.
type carryEvent struct {
	Seq     int             `json:"seq"`
	Type    string          `json:"type"`
	Actor   model.Actor     `json:"actor"`
	Payload json.RawMessage `json:"payload"`
}

// carryEvents is the issue's event log, oldest first.
func carryEvents(t *testing.T, handler http.Handler, issueKey string) []carryEvent {
	t.Helper()
	listed := dispatchRequest(t, handler, http.MethodGet, "/api/v1/issues/"+issueKey+"/events?limit=200", nil, "alice")
	if listed.Code != http.StatusOK {
		t.Fatalf("read events: status=%d body=%s", listed.Code, listed.Body.String())
	}
	return decodeBody[[]carryEvent](t, listed)
}

// decodePayload decodes event's payload into T.
func decodePayload[T any](t *testing.T, event carryEvent) T {
	t.Helper()
	var payload T
	if err := json.Unmarshal(event.Payload, &payload); err != nil {
		t.Fatalf("decode %s payload %s: %v", event.Type, event.Payload, err)
	}
	return payload
}

// A human approves the spec; the agent writes a decision block into it; the human answers. His
// answer is the whole change since the version he approved, so the version it settles into carries
// his approval: one approved review in his name at that version, with the carry's reason and no
// ask, appended before the version's own event, and no approval request opened for it.
func TestAnAnswerThatIsTheWholeChangeSinceHisApprovalCarriesIt(t *testing.T) {
	handler := approvalCarryHandler(t)
	issue := createInteractionIssue(t, handler, "TEST", "Carry the approval",
		"# Plan\n\nThe service ships behind a flag.\n\n> Rollback is a flag flip.\n\n- Announce the dates.\n- Pick the regions.\n")
	reviewDocument(t, handler, issue.PrimaryArtifactID, "alice", "approved", "")
	asks := insertDecisionBlocks(t, handler, issue.Key, issue.PrimaryArtifactID, waveAsk, 2, map[string]string{"wave": "How many regions take the first wave?"})
	if got := readApproval(t, handler, issue.PrimaryArtifactID); got.Approval.State != "stale" {
		t.Fatalf("approval after the block insert = %#v, want stale", got.Approval)
	}

	answerDecisionBlock(t, handler, asks["wave"], "alice", []string{"Two"}, "Two regions first.", nil)
	waitForArtifactVersion(t, handler, issue.PrimaryArtifactID, 3)
	requireCarriedReview(t, handler, issue.PrimaryArtifactID, 3, 1, nil)
	if reviews := documentReviews(t, handler, issue.PrimaryArtifactID); len(reviews) != 2 {
		t.Fatalf("reviews = %+v, want the approval and its carry", reviews)
	}

	approvedSeq, versionSeq := 0, 0
	for _, event := range carryEvents(t, handler, issue.Key) {
		switch event.Type {
		case "artifact.approved":
			payload := decodePayload[struct {
				Version int     `json:"version"`
				Reason  *string `json:"reason"`
				AskID   *string `json:"ask_id"`
			}](t, event)
			if payload.Version == 3 {
				if event.Actor != (model.Actor{Kind: "user", ID: "alice"}) || payload.Reason == nil || *payload.Reason != carrySentence(1) || payload.AskID != nil {
					t.Fatalf("carried artifact.approved = actor %#v payload %s", event.Actor, event.Payload)
				}
				approvedSeq = event.Seq
			}
		case "artifact.version":
			if decodePayload[struct {
				Version model.Version `json:"version"`
			}](t, event).Version.Number == 3 {
				versionSeq = event.Seq
			}
		case "ask.opened":
			if decodePayload[struct {
				Kind string `json:"kind"`
			}](t, event).Kind == "approval" {
				t.Fatalf("the carry opened an approval request: %s", event.Payload)
			}
		}
	}
	if approvedSeq == 0 || versionSeq == 0 || approvedSeq >= versionSeq {
		t.Fatalf("artifact.approved seq %d, artifact.version seq %d, want both for version 3 with the approval first", approvedSeq, versionSeq)
	}
}

// An agent's edit through the edits route versions the document in its own transaction, so a
// human's answer still waiting for its settlement lands in that version too. The edit is inside a
// second, still-open block's body, which the comparison leaves out: the version carries.
func TestAnAnswerCarriesWhenAnAgentEditInsideAnotherBlockLandsInItsVersion(t *testing.T) {
	handler := settlingCarryHandler(t, time.Second)
	issue := createInteractionIssue(t, handler, "TEST", "Answer beside an edit", "Context\n")
	asks := insertDecisionBlocks(t, handler, issue.Key, issue.PrimaryArtifactID, waveAsk+"\n"+regionAsk, 2,
		map[string]string{"wave": "How many regions take the first wave?", "region": "Which region goes first?"})
	reviewDocument(t, handler, issue.PrimaryArtifactID, "alice", "approved", "")

	answerDecisionBlock(t, handler, asks["wave"], "alice", []string{"Two"}, "", nil)
	reviseDocument(t, handler, issue.PrimaryArtifactID, "Which region goes first?", "Which cloud region goes first?", 3)
	version := dispatchRequest(t, handler, http.MethodGet, "/api/v1/artifacts/"+issue.PrimaryArtifactID+"/versions/3", nil, "alice")
	markdown := decodeBody[struct {
		Markdown string `json:"markdown"`
	}](t, version).Markdown
	if !strings.Contains(markdown, `answered_by="alice"`) || !strings.Contains(markdown, "Which cloud region goes first?") {
		t.Fatalf("version 3 = %s, want it to hold both the answer and the agent's edit", markdown)
	}
	requireCarriedReview(t, handler, issue.PrimaryArtifactID, 3, 2, nil)
}

// The human changes his answer. The block's answered_at moves, so the block is newly answered
// again, and the version he approved is now the carried one: the change carries again.
func TestAChangedAnswerByTheSameHumanCarriesAgain(t *testing.T) {
	handler := approvalCarryHandler(t)
	issue := createInteractionIssue(t, handler, "TEST", "Change the answer", "Context\n")
	reviewDocument(t, handler, issue.PrimaryArtifactID, "alice", "approved", "")
	asks := insertDecisionBlocks(t, handler, issue.Key, issue.PrimaryArtifactID, waveAsk, 2, map[string]string{"wave": "How many regions take the first wave?"})
	answerDecisionBlock(t, handler, asks["wave"], "alice", []string{"Two"}, "", nil)
	waitForArtifactVersion(t, handler, issue.PrimaryArtifactID, 3)
	requireCarriedReview(t, handler, issue.PrimaryArtifactID, 3, 1, nil)

	first := readBlockAsk(t, handler, asks["wave"])
	if first.Answer == nil {
		t.Fatalf("ask after the answer = %#v, want an answer", first)
	}
	answerDecisionBlock(t, handler, asks["wave"], "alice", []string{"Five"}, "Half of them.", &first.Answer.At)
	waitForArtifactVersion(t, handler, issue.PrimaryArtifactID, 4)
	requireCarriedReview(t, handler, issue.PrimaryArtifactID, 4, 3, nil)
	if reviews := documentReviews(t, handler, issue.PrimaryArtifactID); len(reviews) != 3 {
		t.Fatalf("reviews = %+v, want the approval and two carries", reviews)
	}
}

// Stale: the agent changed a paragraph outside the blocks before the answer, so the answer is not
// the whole change since the approval.
func TestAParagraphEditBeforeTheAnswerDoesNotCarry(t *testing.T) {
	handler := approvalCarryHandler(t)
	issue := createInteractionIssue(t, handler, "TEST", "Paragraph changed", "The service ships behind a flag.\n")
	reviewDocument(t, handler, issue.PrimaryArtifactID, "alice", "approved", "")
	asks := insertDecisionBlocks(t, handler, issue.Key, issue.PrimaryArtifactID, waveAsk, 2, map[string]string{"wave": "How many regions take the first wave?"})
	reviseDocument(t, handler, issue.PrimaryArtifactID, "behind a flag.", "behind a flag and a canary.", 3)

	answerDecisionBlock(t, handler, asks["wave"], "alice", []string{"Two"}, "", nil)
	waitForArtifactVersion(t, handler, issue.PrimaryArtifactID, 4)
	requireNoCarry(t, handler, issue.PrimaryArtifactID, 1, "stale")
}

// Stale: only another human approved the document; the answerer has no approval to carry.
func TestAnAnswerOnADocumentOnlyAnotherHumanApprovedDoesNotCarry(t *testing.T) {
	handler := approvalCarryHandler(t)
	issue := createInteractionIssue(t, handler, "TEST", "Approved by another", "Context\n")
	reviewDocument(t, handler, issue.PrimaryArtifactID, "bob", "approved", "")
	asks := insertDecisionBlocks(t, handler, issue.Key, issue.PrimaryArtifactID, waveAsk, 2, map[string]string{"wave": "How many regions take the first wave?"})

	answerDecisionBlock(t, handler, asks["wave"], "alice", []string{"Two"}, "", nil)
	waitForArtifactVersion(t, handler, issue.PrimaryArtifactID, 3)
	requireNoCarry(t, handler, issue.PrimaryArtifactID, 1, "stale")
}

// Changes requested: the answerer's latest review of the document asks for changes, so his
// earlier approval is not his standing word on it.
func TestAnAnswerByAHumanWhoseLatestReviewRequestsChangesDoesNotCarry(t *testing.T) {
	handler := approvalCarryHandler(t)
	issue := createInteractionIssue(t, handler, "TEST", "Answerer requested changes", "Context\n")
	reviewDocument(t, handler, issue.PrimaryArtifactID, "alice", "approved", "")
	asks := insertDecisionBlocks(t, handler, issue.Key, issue.PrimaryArtifactID, waveAsk, 2, map[string]string{"wave": "How many regions take the first wave?"})
	reviewDocument(t, handler, issue.PrimaryArtifactID, "alice", "changes_requested", "Name the rollback path.")

	answerDecisionBlock(t, handler, asks["wave"], "alice", []string{"Two"}, "", nil)
	waitForArtifactVersion(t, handler, issue.PrimaryArtifactID, 3)
	requireNoCarry(t, handler, issue.PrimaryArtifactID, 2, "changes_requested")
}

// Changes requested: a second human asked for changes after the answerer's approval, so carrying
// it would approve over a standing request for changes.
func TestAnAnswerAfterAnotherHumanRequestedChangesDoesNotCarry(t *testing.T) {
	handler := approvalCarryHandler(t)
	issue := createInteractionIssue(t, handler, "TEST", "Another requested changes", "Context\n")
	reviewDocument(t, handler, issue.PrimaryArtifactID, "alice", "approved", "")
	asks := insertDecisionBlocks(t, handler, issue.Key, issue.PrimaryArtifactID, waveAsk, 2, map[string]string{"wave": "How many regions take the first wave?"})
	reviewDocument(t, handler, issue.PrimaryArtifactID, "bob", "changes_requested", "Name the rollback path.")

	answerDecisionBlock(t, handler, asks["wave"], "alice", []string{"Two"}, "", nil)
	waitForArtifactVersion(t, handler, issue.PrimaryArtifactID, 3)
	requireNoCarry(t, handler, issue.PrimaryArtifactID, 2, "changes_requested")
}

// Stale: a second block added since his approval is still open, so the version holds a question
// he has not decided.
func TestAnAnswerBesideASecondBlockLeftOpenDoesNotCarry(t *testing.T) {
	handler := approvalCarryHandler(t)
	issue := createInteractionIssue(t, handler, "TEST", "Second block open", "Context\n")
	reviewDocument(t, handler, issue.PrimaryArtifactID, "alice", "approved", "")
	asks := insertDecisionBlocks(t, handler, issue.Key, issue.PrimaryArtifactID, waveAsk+"\n"+regionAsk, 2,
		map[string]string{"wave": "How many regions take the first wave?", "region": "Which region goes first?"})

	answerDecisionBlock(t, handler, asks["wave"], "alice", []string{"Two"}, "", nil)
	waitForArtifactVersion(t, handler, issue.PrimaryArtifactID, 3)
	requireNoCarry(t, handler, issue.PrimaryArtifactID, 1, "stale")
}

// Stale: a second block added since his approval was resolved rather than answered by him, so
// the version holds a decision that is not his.
func TestAnAnswerBesideASecondBlockResolvedDoesNotCarry(t *testing.T) {
	handler := approvalCarryHandler(t)
	issue := createInteractionIssue(t, handler, "TEST", "Second block resolved", "Context\n")
	reviewDocument(t, handler, issue.PrimaryArtifactID, "alice", "approved", "")
	asks := insertDecisionBlocks(t, handler, issue.Key, issue.PrimaryArtifactID, waveAsk+"\n"+regionAsk, 2,
		map[string]string{"wave": "How many regions take the first wave?", "region": "Which region goes first?"})
	resolved := sessionRequest(t, handler, http.MethodPost, "/api/v1/asks/"+asks["region"]+"/resolve", map[string]any{
		"kind": "resolved", "reason": "Decided in the kickoff.", "actor": sessionActor(),
	})
	if resolved.Code != http.StatusOK {
		t.Fatalf("resolve the second block: status=%d body=%s", resolved.Code, resolved.Body.String())
	}
	waitForArtifactVersion(t, handler, issue.PrimaryArtifactID, 3)

	answerDecisionBlock(t, handler, asks["wave"], "alice", []string{"Two"}, "", nil)
	waitForArtifactVersion(t, handler, issue.PrimaryArtifactID, 4)
	requireNoCarry(t, handler, issue.PrimaryArtifactID, 1, "stale")
}

// Stale: a block the approved version held was removed by the agent before the answer. The cut
// text is the same either way, so the comparison alone would carry; the blocks of the approved
// version must all still stand.
func TestAnAnswerAfterABlockHeApprovedWasRemovedDoesNotCarry(t *testing.T) {
	handler := approvalCarryHandler(t)
	issue := createInteractionIssue(t, handler, "TEST", "Approved block removed", "Context\n\n"+regionAsk)
	indexedDecisionBlocks(t, handler, issue.Key, issue.PrimaryArtifactID, map[string]string{"region": "Which region goes first?"})
	asks := insertDecisionBlocks(t, handler, issue.Key, issue.PrimaryArtifactID, waveAsk, 2, map[string]string{"wave": "How many regions take the first wave?"})
	reviewDocument(t, handler, issue.PrimaryArtifactID, "alice", "approved", "")
	deleted := sessionRequest(t, handler, http.MethodPost, "/api/v1/artifacts/"+issue.PrimaryArtifactID+"/edits", map[string]any{
		"actor": sessionActor(),
		"ops":   []map[string]string{{"op": "delete", "block": "region"}},
	})
	if deleted.Code != http.StatusOK {
		t.Fatalf("delete the approved block: status=%d body=%s", deleted.Code, deleted.Body.String())
	}
	waitForArtifactVersion(t, handler, issue.PrimaryArtifactID, 3)

	answerDecisionBlock(t, handler, asks["wave"], "alice", []string{"Two"}, "", nil)
	waitForArtifactVersion(t, handler, issue.PrimaryArtifactID, 4)
	requireNoCarry(t, handler, issue.PrimaryArtifactID, 1, "stale")
}

// Stale: two humans answered two blocks in one version, so the version is not one person's
// answer and carries nobody's approval.
func TestTwoHumansAnswersInOneVersionDoNotCarry(t *testing.T) {
	handler := settlingCarryHandler(t, time.Second)
	issue := createInteractionIssue(t, handler, "TEST", "Two answerers", "Context\n\n"+waveAsk+"\n"+regionAsk)
	asks := indexedDecisionBlocks(t, handler, issue.Key, issue.PrimaryArtifactID,
		map[string]string{"wave": "How many regions take the first wave?", "region": "Which region goes first?"})
	reviewDocument(t, handler, issue.PrimaryArtifactID, "alice", "approved", "")

	answerDecisionBlock(t, handler, asks["wave"], "alice", []string{"Two"}, "", nil)
	answerDecisionBlock(t, handler, asks["region"], "bob", []string{"East"}, "", nil)
	waitForArtifactVersion(t, handler, issue.PrimaryArtifactID, 2)
	version := dispatchRequest(t, handler, http.MethodGet, "/api/v1/artifacts/"+issue.PrimaryArtifactID+"/versions/2", nil, "alice")
	markdown := decodeBody[struct {
		Markdown string `json:"markdown"`
	}](t, version).Markdown
	if !strings.Contains(markdown, `answered_by="alice"`) || !strings.Contains(markdown, `answered_by="bob"`) {
		t.Fatalf("version 2 = %s, want both answers in it", markdown)
	}
	requireNoCarry(t, handler, issue.PrimaryArtifactID, 1, "stale")
}

// A record copy of the spec shows the spec's block as a copy, answered from the spec. The human
// approved both documents; his answer carries his approval of the spec, whose block he answered,
// and not of the copy, whose block is nobody's decision there.
func TestACopiedBlockAnsweredThroughItsSourceCarriesOnTheSourceOnly(t *testing.T) {
	handler := approvalCarryHandler(t)
	issue := createInteractionIssue(t, handler, "TEST", "Copied block", "Context\n\n"+waveAsk)
	asks := indexedDecisionBlocks(t, handler, issue.Key, issue.PrimaryArtifactID, map[string]string{"wave": "How many regions take the first wave?"})
	uploaded := sessionRequest(t, handler, http.MethodPost, "/api/v1/issues/"+issue.Key+"/artifacts", map[string]any{
		"actor": sessionActor(), "name": "spec-record.md", "content": documentMarkdown(t, handler, issue.PrimaryArtifactID),
	})
	if uploaded.Code != http.StatusCreated {
		t.Fatalf("upload the record copy: status=%d body=%s", uploaded.Code, uploaded.Body.String())
	}
	copied := decodeBody[struct {
		Artifact model.Artifact `json:"artifact"`
	}](t, uploaded).Artifact.ID
	awaitDocumentText(t, handler, copied, "its open source", contains(`state="open" copied_from="`+asks["wave"]+`"`), nil)
	copyVersions := documentVersions(t, handler, copied)
	copyLatest := copyVersions[len(copyVersions)-1].Number
	reviewDocument(t, handler, copied, "alice", "approved", "")
	reviewDocument(t, handler, issue.PrimaryArtifactID, "alice", "approved", "")

	answerDecisionBlock(t, handler, asks["wave"], "alice", []string{"Two"}, "", nil)
	waitForArtifactVersion(t, handler, issue.PrimaryArtifactID, 2)
	waitForArtifactVersion(t, handler, copied, copyLatest+1)
	awaitDocumentText(t, handler, copied, "its source's answer", contains(`state="answered" answered_by="alice"`, `copied_from="`+asks["wave"]+`"`), nil)
	requireCarriedReview(t, handler, issue.PrimaryArtifactID, 2, 1, nil)
	requireNoCarry(t, handler, copied, 1, "stale")
}

// An approval request is open when the answer settles: the request follows the version first, and
// the carry then answers it Approve in the human's name with the carry's reason, so the review
// names it and nothing reopens or rewords it afterwards.
func TestAnOpenApprovalRequestIsAnsweredByTheCarry(t *testing.T) {
	handler := approvalCarryHandler(t)
	issue := createInteractionIssue(t, handler, "TEST", "Request open at the carry", "Context\n")
	reviewDocument(t, handler, issue.PrimaryArtifactID, "alice", "approved", "")
	asks := insertDecisionBlocks(t, handler, issue.Key, issue.PrimaryArtifactID, waveAsk, 2, map[string]string{"wave": "How many regions take the first wave?"})
	request := requestApproval(t, handler, issue.PrimaryArtifactID)
	if request.Approval.Version != 2 || request.Approval.ApprovedVersion == nil || *request.Approval.ApprovedVersion != 1 {
		t.Fatalf("request = %#v, want one at version 2 following the approval of version 1", request.Approval)
	}

	answerDecisionBlock(t, handler, asks["wave"], "alice", []string{"Two"}, "", nil)
	waitForArtifactVersion(t, handler, issue.PrimaryArtifactID, 3)
	requireCarriedReview(t, handler, issue.PrimaryArtifactID, 3, 1, &request.ID)
	if got := readApproval(t, handler, issue.PrimaryArtifactID); got.Approval.AskID == nil || *got.Approval.AskID != request.ID {
		t.Fatalf("approval = %#v, want it to name the answered request", got.Approval)
	}
	answered := readBlockAsk(t, handler, request.ID)
	if answered.State != "answered" || answered.Answer == nil || answered.Answer.User != "alice" ||
		!reflect.DeepEqual(answered.Answer.Selected, []string{"Approve"}) || answered.Answer.Text == nil || *answered.Answer.Text != carrySentence(1) {
		t.Fatalf("request after the carry = state %q answer %#v, want Approve by alice with the carry's reason", answered.State, answered.Answer)
	}
	if answered.Approval == nil || answered.Approval.Version != 3 {
		t.Fatalf("request after the carry names version %#v, want 3", answered.Approval)
	}
	answeredSeq, editedAfter := 0, 0
	for _, event := range carryEvents(t, handler, issue.Key) {
		if event.Type != "ask.answered" && event.Type != "ask.edited" {
			continue
		}
		if decodePayload[struct {
			ID string `json:"id"`
		}](t, event).ID != request.ID {
			continue
		}
		if event.Type == "ask.answered" {
			if answeredSeq != 0 || event.Actor != (model.Actor{Kind: "user", ID: "alice"}) {
				t.Fatalf("ask.answered for the request: seq %d after %d, actor %#v; want one, by alice", event.Seq, answeredSeq, event.Actor)
			}
			answeredSeq = event.Seq
		} else if answeredSeq != 0 {
			editedAfter++
		}
	}
	if answeredSeq == 0 || editedAfter != 0 {
		t.Fatalf("ask.answered seq %d, %d ask.edited after it; want the answer and nothing after", answeredSeq, editedAfter)
	}
}

// A document approved at version 1, whose very first edit is the agent's block insert: the version
// before the answer's is the insert's, holding the block open, and the approved version holds no
// block at all. The answer carries.
func TestADocumentApprovedAtVersionOneCarriesOnItsFirstBlocksAnswer(t *testing.T) {
	handler := approvalCarryHandler(t)
	issue := createInteractionIssue(t, handler, "TEST", "Approved at version one", "Context\n")
	review := reviewDocument(t, handler, issue.PrimaryArtifactID, "alice", "approved", "")
	if review.Version != 1 {
		t.Fatalf("review = %#v, want an approval of version 1", review)
	}
	asks := insertDecisionBlocks(t, handler, issue.Key, issue.PrimaryArtifactID, waveAsk, 2, map[string]string{"wave": "How many regions take the first wave?"})

	answerDecisionBlock(t, handler, asks["wave"], "alice", []string{"Two"}, "", nil)
	waitForArtifactVersion(t, handler, issue.PrimaryArtifactID, 3)
	requireCarriedReview(t, handler, issue.PrimaryArtifactID, 3, 1, nil)
}
