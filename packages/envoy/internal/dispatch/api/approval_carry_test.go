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
// (approval.approved: version, at, by), so the human reads what he is approving since the version
// he approved rather than from nothing; a first request names none. And his answer to a decision
// block carries his approval forward: when that answer is the whole change since the version he
// approved, the version it writes gets an approved review in his name, in the same transaction
// (docs.carryApproval, LEGION-573).

// carriedAsk is an approval ask as the carry tests read it: the document version it names and the
// earlier approval it carries, absent when the document was never approved.
type carriedAsk struct {
	ID       string `json:"id"`
	Approval struct {
		Version          int                        `json:"version"`
		RequestedVersion int                        `json:"requested_version"`
		Approved         *model.AskApprovalApproved `json:"approved"`
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
	handler, _, _ := settlingCarryDocuments(t, settle)
	return handler
}

// settlingCarryDocuments is settlingCarryHandler with the store and the document service it
// serves, for a test that writes a document as the server's own code does, beside the routes.
func settlingCarryDocuments(t *testing.T, settle time.Duration) (http.Handler, *store.Store, docs.API) {
	t.Helper()
	var documentService docs.API
	handler, database := newInteractionHandler(t, func(database *store.Store) docs.API {
		service := docs.New(docs.Deps{Store: database, Settle: settle})
		t.Cleanup(func() { _ = service.Shutdown(context.Background()) })
		documentService = service
		return service
	})
	return handler, database, documentService
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
	carried := ask.Approval.Approved
	if carried == nil {
		t.Fatalf("approval = %#v, want approved", ask.Approval)
	}
	if carried.Version != review.Version {
		t.Fatalf("approved.version = %d, want %d", carried.Version, review.Version)
	}
	approvedAt, err := time.Parse(time.RFC3339, carried.At)
	if err != nil {
		t.Fatalf("approved.at %q is not RFC 3339: %v", carried.At, err)
	}
	if !approvedAt.Equal(review.CreatedAt) {
		t.Fatalf("approved.at = %s, want the review's created_at %s", approvedAt.Format(time.RFC3339Nano), review.CreatedAt.Format(time.RFC3339Nano))
	}
	if !reflect.DeepEqual(carried.By, review.Actor) {
		t.Fatalf("approved.by = %#v, want the approving actor %#v", carried.By, review.Actor)
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

// A first request, on a document nobody has approved, carries no approved key: not a null value,
// no key at all.
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
	if raw, present := approval["approved"]; present {
		t.Fatalf("a first request carries approved = %s, want the key absent", raw)
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
	markdown := versionMarkdown(t, handler, issue.PrimaryArtifactID, 3)
	if !strings.Contains(markdown, `answered_by="alice"`) || !strings.Contains(markdown, "Which cloud region goes first?") {
		t.Fatalf("version 3 = %s, want it to hold both the answer and the agent's edit", markdown)
	}
	requireCarriedReview(t, handler, issue.PrimaryArtifactID, 3, 2, nil)
}

// versionMarkdown is the markdown of the document's version number.
func versionMarkdown(t *testing.T, handler http.Handler, artifactID string, number int) string {
	t.Helper()
	version := dispatchRequest(t, handler, http.MethodGet, fmt.Sprintf("/api/v1/artifacts/%s/versions/%d", artifactID, number), nil, "alice")
	if version.Code != http.StatusOK {
		t.Fatalf("read version %d: status=%d body=%s", number, version.Code, version.Body.String())
	}
	return decodeBody[struct {
		Markdown string `json:"markdown"`
	}](t, version).Markdown
}

// Stale: the agent rewords an answered block in the version the human's answer writes. The human
// answered the first block and approved; the agent added a second, which the human answers, and
// the agent's edit of the first block's question, through the edits route, versions in the route's
// own transaction while the answer still waits for its settlement, so one version holds both. The
// cut leaves both blocks out and every block is his, but the first now shows his answer under
// wording nobody decided: a decided block must ask what its row was decided on, so nothing is
// carried. Settlement then writes the decided wording back, and that version - the one he approved
// plus his answer to the second block - carries.
func TestARewordOfAnAnsweredBlockInTheAnswersVersionDoesNotCarry(t *testing.T) {
	handler := settlingCarryHandler(t, time.Second)
	issue := createInteractionIssue(t, handler, "TEST", "Reword under the answer", "Context\n\n"+regionAsk)
	asks := indexedDecisionBlocks(t, handler, issue.Key, issue.PrimaryArtifactID, map[string]string{"region": "Which region goes first?"})
	answerDecisionBlock(t, handler, asks["region"], "alice", []string{"East"}, "", nil)
	waitForArtifactVersion(t, handler, issue.PrimaryArtifactID, 2)
	reviewDocument(t, handler, issue.PrimaryArtifactID, "alice", "approved", "")
	for id, ask := range insertDecisionBlocks(t, handler, issue.Key, issue.PrimaryArtifactID, waveAsk, 3, map[string]string{"wave": "How many regions take the first wave?"}) {
		asks[id] = ask
	}

	answerDecisionBlock(t, handler, asks["wave"], "alice", []string{"Two"}, "", nil)
	reviseDocument(t, handler, issue.PrimaryArtifactID, "Which region goes first?", "Which region goes last?", 4)
	markdown := versionMarkdown(t, handler, issue.PrimaryArtifactID, 4)
	if strings.Count(markdown, `answered_by="alice"`) != 2 || !strings.Contains(markdown, "Which region goes last?") {
		t.Fatalf("version 4 = %s, want it to hold both answers and the agent's reword", markdown)
	}
	requireNoCarry(t, handler, issue.PrimaryArtifactID, 1, "stale")

	waitForArtifactVersion(t, handler, issue.PrimaryArtifactID, 5)
	restored := versionMarkdown(t, handler, issue.PrimaryArtifactID, 5)
	if strings.Count(restored, `answered_by="alice"`) != 2 || !strings.Contains(restored, "Which region goes first?") || strings.Contains(restored, "goes last?") {
		t.Fatalf("version 5 = %s, want settlement to have written the decided wording back under both answers", restored)
	}
	requireCarriedReview(t, handler, issue.PrimaryArtifactID, 5, 2, nil)
}

// Stale: a server-owned attribute forged on a decided block in the version the human's answer
// writes - each attribute settlement writes from the row (askServerAttributeNames), forged alone.
// The human answered the first block and approved; the agent added a second, which the human
// answers, and at once a write through the server's own live attribute path - the one the answer
// route writes an answer with - puts another value on the first block, leaving every other
// attribute as the row has it; the agent then names a version, in the route's own transaction,
// while the answer still waits for its settlement, so one version holds both. The cut leaves both
// blocks out and every block is his, but the first is not as settlement would write it from its
// row: a block's whole server-attribute set must be as settlement's writer leaves it, so nothing
// is carried. Settlement then writes the row back over the forgery, and that version - the one he
// approved plus his answer to the second block - carries; for `invalid`, which no rendering writes,
// settlement's repair changes no markdown and versions nothing, so the approval stays stale.
func TestAForgedServerAttributeOnADecidedBlockCarriesNothing(t *testing.T) {
	for _, tc := range []struct {
		attribute string
		forged    any
		rendered  string // what version 4's markdown shows for the forgery; "" for an attribute no rendering writes
	}{
		{"state", "resolved", `state="resolved"`},
		{"answered_by", "mallory", `answered_by="mallory"`},
		{"answered_at", "2026-01-01T00:00:00Z", `answered_at="2026-01-01T00:00:00Z"`},
		{"selected", []string{"West"}, `selected="[&#x22;West&#x22;]"`},
		{"answer", "Pick West.", `answer="Pick West."`},
		{"invalid", "forged", ""},
		{"copied_from", "00000000-0000-0000-0000-000000000000", `copied_from="00000000-0000-0000-0000-000000000000"`},
		{"copied_from_document", "dispatch://TEST/artifact/other", `copied_from_document="dispatch://TEST/artifact/other"`},
	} {
		t.Run(tc.attribute, func(t *testing.T) {
			handler, database, documents := settlingCarryDocuments(t, time.Second)
			issue := createInteractionIssue(t, handler, "TEST", "Forged "+tc.attribute+" under the answer", "Context\n\n"+regionAsk)
			asks := indexedDecisionBlocks(t, handler, issue.Key, issue.PrimaryArtifactID, map[string]string{"region": "Which region goes first?"})
			answerDecisionBlock(t, handler, asks["region"], "alice", []string{"East"}, "", nil)
			waitForArtifactVersion(t, handler, issue.PrimaryArtifactID, 2)
			reviewDocument(t, handler, issue.PrimaryArtifactID, "alice", "approved", "")
			for id, ask := range insertDecisionBlocks(t, handler, issue.Key, issue.PrimaryArtifactID, waveAsk, 3, map[string]string{"wave": "How many regions take the first wave?"}) {
				asks[id] = ask
			}

			answerDecisionBlock(t, handler, asks["wave"], "alice", []string{"Two"}, "", nil)
			setLiveBlockAttributes(t, database, documents, issue.PrimaryArtifactID, "region", map[string]any{tc.attribute: tc.forged})
			named := sessionRequest(t, handler, http.MethodPost, "/api/v1/artifacts/"+issue.PrimaryArtifactID+"/versions", map[string]any{"summary": "Name the forged " + tc.attribute, "actor": sessionActor()})
			if named.Code != http.StatusCreated {
				t.Fatalf("name the version holding the forged %s: status=%d body=%s", tc.attribute, named.Code, named.Body.String())
			}
			waitForArtifactVersion(t, handler, issue.PrimaryArtifactID, 4)
			markdown := versionMarkdown(t, handler, issue.PrimaryArtifactID, 4)
			if !strings.Contains(markdown, "How many regions take the first wave?") || !strings.Contains(markdown, `selected="[&#x22;Two&#x22;]"`) {
				t.Fatalf("version 4 = %s, want it to hold the answer to the second block", markdown)
			}
			if tc.rendered != "" && !strings.Contains(markdown, tc.rendered) {
				t.Fatalf("version 4 = %s, want it to show the forged %s", markdown, tc.attribute)
			}
			requireNoCarry(t, handler, issue.PrimaryArtifactID, 1, "stale")

			if tc.rendered == "" {
				// Settlement takes the forged attribute off the live block, but a repair no rendering
				// writes changes no markdown, so it versions nothing: the approval stays stale and the
				// forgery costs the carry, which the human's next approval replaces. Nothing is approved.
				time.Sleep(3 * time.Second)
				if latest := readApproval(t, handler, issue.PrimaryArtifactID).Approval.LatestVersion; latest != 4 {
					t.Fatalf("latest version = %d, want settlement's repair of %s to write none", latest, tc.attribute)
				}
				requireNoCarry(t, handler, issue.PrimaryArtifactID, 1, "stale")
				return
			}
			waitForArtifactVersion(t, handler, issue.PrimaryArtifactID, 5)
			restored := versionMarkdown(t, handler, issue.PrimaryArtifactID, 5)
			if strings.Count(restored, `answered_by="alice"`) != 2 || !strings.Contains(restored, `selected="[&#x22;East&#x22;]"`) || (tc.rendered != "" && strings.Contains(restored, tc.rendered)) {
				t.Fatalf("version 5 = %s, want settlement to have written the row back over the forged %s under both answers", restored, tc.attribute)
			}
			requireCarriedReview(t, handler, issue.PrimaryArtifactID, 5, 2, nil)
		})
	}
}

// setLiveBlockAttributes writes attributes onto the typed block blockID of the document through
// the server's own live attribute write, as the answer route writes an answer into its block:
// inside a transaction joined with Docs.Join, committed through its ledger, as the agent's
// session.
func setLiveBlockAttributes(t *testing.T, database *store.Store, documents docs.API, artifactID, blockID string, attributes map[string]any) {
	t.Helper()
	ctx := context.Background()
	tx, err := database.Pool.Begin(ctx)
	if err != nil {
		t.Fatalf("begin the attribute write: %v", err)
	}
	defer tx.Rollback(ctx)
	documentCtx, ledger := documents.Join(ctx, tx)
	defer ledger.Discard()
	if err := documents.SetBlockAttributes(documentCtx, artifactID, blockID, attributes, model.Actor{Kind: "session", ID: "session-0123456789abcdef"}); err != nil {
		t.Fatalf("set block %q attributes %v: %v", blockID, attributes, err)
	}
	if err := ledger.Commit(ctx); err != nil {
		t.Fatalf("commit the attribute write: %v", err)
	}
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

// Stale: the agent deletes a block the human answered and approved, then inserts it again with
// its id and wording. Its row stays answered - settlement retracts only an open ask - so the row
// says the block he answered is back, with no action of his. The inserted block shows open until
// settlement writes the row's answer back into it, and a block that shows other than its row's
// state carries nothing; once it does, the version holds every block as the approved version held
// it, which is nothing new of his. Neither version carries.
func TestADeletedAndRestoredAnsweredBlockCarriesNothing(t *testing.T) {
	handler := approvalCarryHandler(t)
	issue := createInteractionIssue(t, handler, "TEST", "Answered block restored", "Context\n\n"+regionAsk)
	asks := indexedDecisionBlocks(t, handler, issue.Key, issue.PrimaryArtifactID, map[string]string{"region": "Which region goes first?"})
	answerDecisionBlock(t, handler, asks["region"], "alice", []string{"East"}, "", nil)
	waitForArtifactVersion(t, handler, issue.PrimaryArtifactID, 2)
	reviewDocument(t, handler, issue.PrimaryArtifactID, "alice", "approved", "")
	deleted := sessionRequest(t, handler, http.MethodPost, "/api/v1/artifacts/"+issue.PrimaryArtifactID+"/edits", map[string]any{
		"actor": sessionActor(),
		"ops":   []map[string]string{{"op": "delete", "block": "region"}},
	})
	if deleted.Code != http.StatusOK {
		t.Fatalf("delete the answered block: status=%d body=%s", deleted.Code, deleted.Body.String())
	}
	waitForArtifactVersion(t, handler, issue.PrimaryArtifactID, 3)
	requireNoCarry(t, handler, issue.PrimaryArtifactID, 1, "stale")

	restored := sessionRequest(t, handler, http.MethodPost, "/api/v1/artifacts/"+issue.PrimaryArtifactID+"/edits", map[string]any{
		"actor": sessionActor(),
		"ops":   []map[string]string{{"op": "insert", "after": "end", "markdown": regionAsk}},
	})
	if restored.Code != http.StatusOK {
		t.Fatalf("insert the answered block again: status=%d body=%s", restored.Code, restored.Body.String())
	}
	waitForArtifactVersion(t, handler, issue.PrimaryArtifactID, 4)
	if markdown := versionMarkdown(t, handler, issue.PrimaryArtifactID, 4); !strings.Contains(markdown, `:::ask{#region urgency="low" multiple="false" state="open"}`) {
		t.Fatalf("version 4 = %s, want the block inserted again, showing open", markdown)
	}
	requireNoCarry(t, handler, issue.PrimaryArtifactID, 1, "stale")
	awaitDocumentText(t, handler, issue.PrimaryArtifactID, "the row's answer written back into the restored block",
		contains(`:::ask{#region urgency="low" multiple="false" state="answered" answered_by="alice"`), nil)
	if ask := readBlockAsk(t, handler, asks["region"]); ask.State != "answered" || ask.Answer == nil || ask.Answer.User != "alice" {
		t.Fatalf("the block's ask after the delete and the insert = state %q answer %#v, want alice's answer kept", ask.State, ask.Answer)
	}
	requireNoCarry(t, handler, issue.PrimaryArtifactID, 1, "stale")
}

// Stale: a block the human answered before his approval, which the approved version did not
// hold. The human answered the block; the agent deleted it; the human approved the document
// without it; the agent inserts the block's markdown again. Its row stayed answered - settlement
// retracts only an open ask - so settlement writes his answer back into the inserted block, and
// the version it writes holds a block the approved version did not, answered by him: not as the
// approved version held it, and his. But the answer predates his approval, so no action of his
// since it put the block there: a changed block's answer must be newer than the approval it would
// carry. Neither the insert's version, showing the block open, nor settlement's carries.
func TestARestoredBlockAnsweredBeforeTheApprovalCarriesNothing(t *testing.T) {
	handler := approvalCarryHandler(t)
	issue := createInteractionIssue(t, handler, "TEST", "Restored block answered before the approval", "Context\n\n"+regionAsk)
	asks := indexedDecisionBlocks(t, handler, issue.Key, issue.PrimaryArtifactID, map[string]string{"region": "Which region goes first?"})
	answerDecisionBlock(t, handler, asks["region"], "alice", []string{"East"}, "", nil)
	waitForArtifactVersion(t, handler, issue.PrimaryArtifactID, 2)
	deleted := sessionRequest(t, handler, http.MethodPost, "/api/v1/artifacts/"+issue.PrimaryArtifactID+"/edits", map[string]any{
		"actor": sessionActor(),
		"ops":   []map[string]string{{"op": "delete", "block": "region"}},
	})
	if deleted.Code != http.StatusOK {
		t.Fatalf("delete the answered block: status=%d body=%s", deleted.Code, deleted.Body.String())
	}
	waitForArtifactVersion(t, handler, issue.PrimaryArtifactID, 3)
	review := reviewDocument(t, handler, issue.PrimaryArtifactID, "alice", "approved", "")
	if review.Version != 3 {
		t.Fatalf("review = %#v, want an approval of version 3, without the block", review)
	}

	restored := sessionRequest(t, handler, http.MethodPost, "/api/v1/artifacts/"+issue.PrimaryArtifactID+"/edits", map[string]any{
		"actor": sessionActor(),
		"ops":   []map[string]string{{"op": "insert", "after": "end", "markdown": regionAsk}},
	})
	if restored.Code != http.StatusOK {
		t.Fatalf("insert the answered block again: status=%d body=%s", restored.Code, restored.Body.String())
	}
	waitForArtifactVersion(t, handler, issue.PrimaryArtifactID, 4)
	if markdown := versionMarkdown(t, handler, issue.PrimaryArtifactID, 4); !strings.Contains(markdown, `:::ask{#region urgency="low" multiple="false" state="open"}`) {
		t.Fatalf("version 4 = %s, want the block inserted again, showing open", markdown)
	}
	requireNoCarry(t, handler, issue.PrimaryArtifactID, 1, "stale")

	waitForArtifactVersion(t, handler, issue.PrimaryArtifactID, 5)
	if markdown := versionMarkdown(t, handler, issue.PrimaryArtifactID, 5); !strings.Contains(markdown, `:::ask{#region urgency="low" multiple="false" state="answered" answered_by="alice"`) {
		t.Fatalf("version 5 = %s, want settlement to have written his answer back into the restored block", markdown)
	}
	if ask := readBlockAsk(t, handler, asks["region"]); ask.Answer == nil || !ask.Answer.At.Before(review.CreatedAt) {
		t.Fatalf("the block's answer = %#v, want one given before the approval at %s", ask.Answer, review.CreatedAt.Format(time.RFC3339Nano))
	}
	requireNoCarry(t, handler, issue.PrimaryArtifactID, 1, "stale")
}

// Stale: a block the approved version held open was answered by another human after his
// approval, and his own answer to a block added since would carry that decision into his name.
// The cut text is the same and every added block is his, so only the comparison of each block
// with the approved version's state keeps the carry from approving the other answer unread.
func TestAnAnswerBesideABlockAnotherHumanAnsweredSinceHisApprovalDoesNotCarry(t *testing.T) {
	handler := approvalCarryHandler(t)
	issue := createInteractionIssue(t, handler, "TEST", "Other answer since approval", "Context\n\n"+regionAsk)
	asks := indexedDecisionBlocks(t, handler, issue.Key, issue.PrimaryArtifactID, map[string]string{"region": "Which region goes first?"})
	reviewDocument(t, handler, issue.PrimaryArtifactID, "alice", "approved", "")
	for id, ask := range insertDecisionBlocks(t, handler, issue.Key, issue.PrimaryArtifactID, waveAsk, 2, map[string]string{"wave": "How many regions take the first wave?"}) {
		asks[id] = ask
	}
	answerDecisionBlock(t, handler, asks["region"], "bob", []string{"East"}, "", nil)
	waitForArtifactVersion(t, handler, issue.PrimaryArtifactID, 3)
	requireNoCarry(t, handler, issue.PrimaryArtifactID, 1, "stale")

	answerDecisionBlock(t, handler, asks["wave"], "alice", []string{"Two"}, "", nil)
	waitForArtifactVersion(t, handler, issue.PrimaryArtifactID, 4)
	requireNoCarry(t, handler, issue.PrimaryArtifactID, 1, "stale")
}

// Carry: a block the approved version held open, answered by the approver himself with nothing
// else changed, is his answer to a block of the version he approved.
func TestHisAnswerToABlockTheApprovedVersionHeldOpenCarries(t *testing.T) {
	handler := approvalCarryHandler(t)
	issue := createInteractionIssue(t, handler, "TEST", "Approved block answered", "Context\n\n"+regionAsk)
	asks := indexedDecisionBlocks(t, handler, issue.Key, issue.PrimaryArtifactID, map[string]string{"region": "Which region goes first?"})
	reviewDocument(t, handler, issue.PrimaryArtifactID, "alice", "approved", "")

	answerDecisionBlock(t, handler, asks["region"], "alice", []string{"East"}, "", nil)
	waitForArtifactVersion(t, handler, issue.PrimaryArtifactID, 2)
	requireCarriedReview(t, handler, issue.PrimaryArtifactID, 2, 1, nil)
}

// Carry: a block another human answered before his approval, untouched since, is content he
// approved; his answer to a block added since is still the whole change.
func TestAnotherHumansAnswerHeApprovedStaysOutOfTheCarrysTest(t *testing.T) {
	handler := approvalCarryHandler(t)
	issue := createInteractionIssue(t, handler, "TEST", "Other answer before approval", "Context\n\n"+regionAsk)
	asks := indexedDecisionBlocks(t, handler, issue.Key, issue.PrimaryArtifactID, map[string]string{"region": "Which region goes first?"})
	answerDecisionBlock(t, handler, asks["region"], "bob", []string{"East"}, "", nil)
	waitForArtifactVersion(t, handler, issue.PrimaryArtifactID, 2)
	reviewDocument(t, handler, issue.PrimaryArtifactID, "alice", "approved", "")
	for id, ask := range insertDecisionBlocks(t, handler, issue.Key, issue.PrimaryArtifactID, waveAsk, 3, map[string]string{"wave": "How many regions take the first wave?"}) {
		asks[id] = ask
	}

	answerDecisionBlock(t, handler, asks["wave"], "alice", []string{"Two"}, "", nil)
	waitForArtifactVersion(t, handler, issue.PrimaryArtifactID, 4)
	requireCarriedReview(t, handler, issue.PrimaryArtifactID, 4, 2, nil)
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
	markdown := versionMarkdown(t, handler, issue.PrimaryArtifactID, 2)
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

// Stale: a copy planted since his approval. Another document of the issue holds a block the human
// answered; the agent inserts into the spec he approved a block with that block's id and wording,
// which settlement shows as a copy answered by him, then adds a question of its own, which he
// answers. The copy is answered in his name and is no row of the spec's, so the spec carries
// nothing: a block no row of this document backs is a decision the carry cannot read.
func TestACopiedBlockPlantedSinceHisApprovalDoesNotCarry(t *testing.T) {
	handler := approvalCarryHandler(t)
	issue := createInteractionIssue(t, handler, "TEST", "Planted copy", "Context\n")
	reviewDocument(t, handler, issue.PrimaryArtifactID, "alice", "approved", "")
	uploaded := sessionRequest(t, handler, http.MethodPost, "/api/v1/issues/"+issue.Key+"/artifacts", map[string]any{
		"actor": sessionActor(), "name": "decisions.md", "content": "Decisions\n\n" + waveAsk,
	})
	if uploaded.Code != http.StatusCreated {
		t.Fatalf("upload the other document: status=%d body=%s", uploaded.Code, uploaded.Body.String())
	}
	other := decodeBody[struct {
		Artifact model.Artifact `json:"artifact"`
	}](t, uploaded).Artifact.ID
	asks := indexedDecisionBlocks(t, handler, issue.Key, other, map[string]string{"wave": "How many regions take the first wave?"})
	answerDecisionBlock(t, handler, asks["wave"], "alice", []string{"Two"}, "", nil)
	awaitDocumentText(t, handler, other, "alice's answer", contains(`state="answered" answered_by="alice"`), nil)

	planted := sessionRequest(t, handler, http.MethodPost, "/api/v1/artifacts/"+issue.PrimaryArtifactID+"/edits", map[string]any{
		"actor": sessionActor(),
		"ops":   []map[string]string{{"op": "insert", "after": "end", "markdown": waveAsk}},
	})
	if planted.Code != http.StatusOK {
		t.Fatalf("plant the copy: status=%d body=%s", planted.Code, planted.Body.String())
	}
	awaitDocumentText(t, handler, issue.PrimaryArtifactID, "the planted copy", contains(`state="answered" answered_by="alice"`, `copied_from="`+asks["wave"]+`"`), nil)
	versions := documentVersions(t, handler, issue.PrimaryArtifactID)
	latest := versions[len(versions)-1].Number
	regions := insertDecisionBlocks(t, handler, issue.Key, issue.PrimaryArtifactID, regionAsk, latest+1, map[string]string{"region": "Which region goes first?"})

	answerDecisionBlock(t, handler, regions["region"], "alice", []string{"East"}, "", nil)
	waitForArtifactVersion(t, handler, issue.PrimaryArtifactID, latest+2)
	requireNoCarry(t, handler, issue.PrimaryArtifactID, 1, "stale")
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
	if request.Approval.Version != 2 || request.Approval.Approved == nil || request.Approval.Approved.Version != 1 {
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
