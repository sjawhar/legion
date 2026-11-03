package api

import (
	"fmt"
	"net/http"
	"reflect"
	"strings"
	"testing"

	"github.com/sjawhar/envoy/internal/dispatch/docs"
	"github.com/sjawhar/envoy/internal/dispatch/model"
)

// A version whose text is the text of the version a person approved holds nothing he has not
// agreed to: an accidental edit undone, or a version written over a document that changed
// nothing. Dispatch carries the document's newest approval to that version in the transaction
// that writes it (docs.carryApproval), as it carries one to the version a person's own answer to a
// decision block writes, so the approval reads approved there, the write reports no approval it
// superseded, and a request for approval finds nothing to ask. Text one character away from the
// approved text is a change, and a version holding the approved text around a changed decision
// block is judged as it was before restored text carried anything: by the answer carry alone.

// restoreSpec is the approved text the restore tests edit and restore.
const restoreSpec = "# Plan\n\nThe service ships behind a flag.\n\n- Announce the dates.\n- Pick the regions.\n"

// restoreSentence is the reason a review carried to a version restoring the approved text records.
func restoreSentence(from int) string {
	return fmt.Sprintf("Carried forward from the approval of version %d: this version's text is that version's, unchanged.", from)
}

// agentEdit applies ops through the edits route as the asking agent, in one batch that versions
// the document in the route's own transaction, waits until that version, number, reads, and
// answers the advice the write came back with.
func agentEdit(t *testing.T, handler http.Handler, artifactID string, number int, ops ...map[string]string) *testWriteAdvice {
	t.Helper()
	edited := sessionRequest(t, handler, http.MethodPost, "/api/v1/artifacts/"+artifactID+"/edits", map[string]any{
		"actor": sessionActor(),
		"ops":   ops,
	})
	advice := adviceFromResponse(t, edited.Code, edited.Body.String())
	waitForArtifactVersion(t, handler, artifactID, number)
	return advice
}

// replaceOp is one edit operation replacing find with with.
func replaceOp(find, with string) map[string]string {
	return map[string]string{"op": "replace", "find": find, "with": with}
}

// approvedDocument is a document approveThenChange approved and then changed: its issue, its id and
// the ask of each decision block it was written with, by block id.
type approvedDocument struct {
	issueKey, artifactID string
	asks                 map[string]string
}

// approveThenChange writes restoreSpec followed by extra into a new issue's spec, waits for
// settlement to index each decision block blocks names (id to question), approves version 1 from
// the header as alice, then has the agent change its paragraph at version 2, which stales that
// approval.
func approveThenChange(t *testing.T, handler http.Handler, title, extra string, blocks map[string]string) approvedDocument {
	t.Helper()
	issue := createInteractionIssue(t, handler, "TEST", title, restoreSpec+extra)
	asks := indexedDecisionBlocks(t, handler, issue.Key, issue.PrimaryArtifactID, blocks)
	if review := reviewDocument(t, handler, issue.PrimaryArtifactID, "alice", "approved", ""); review.Version != 1 {
		t.Fatalf("review = %#v, want an approval of version 1", review)
	}
	agentEdit(t, handler, issue.PrimaryArtifactID, 2, replaceOp("behind a flag.", "behind a flag and a canary."))
	requireNoCarry(t, handler, issue.PrimaryArtifactID, 1, "stale")
	return approvedDocument{issueKey: issue.Key, artifactID: issue.PrimaryArtifactID, asks: asks}
}

// waveBlock is the one decision block of the restore tests that hold one, waveAsk, by its id.
var waveBlock = map[string]string{"wave": "How many regions take the first wave?"}

// (a) The agent edits the approved spec by accident and undoes the edit: version 3's text is
// version 1's, byte for byte, so version 3 carries alice's approval of version 1 - one approved
// review in her name at version 3 with the restore's reason and no ask, and its artifact.approved
// before version 3's own artifact.version. The undo supersedes no approval, so its advice names
// none, and a request for approval returns the approval and opens nothing.
func TestAVersionRestoringTheApprovedTextCarriesTheApproval(t *testing.T) {
	handler := approvalCarryHandler(t)
	document := approveThenChange(t, handler, "Restore the approved text", "", nil)
	artifactID := document.artifactID

	advice := agentEdit(t, handler, artifactID, 3, replaceOp("behind a flag and a canary.", "behind a flag."))
	if restored, approved := versionMarkdown(t, handler, artifactID, 3), versionMarkdown(t, handler, artifactID, 1); restored != approved {
		t.Fatalf("version 3 = %q, want version 1's text %q", restored, approved)
	}
	requireReviewCarriedWith(t, handler, artifactID, 3, 1, restoreSentence(1), nil)
	if reviews := documentReviews(t, handler, artifactID); len(reviews) != 2 {
		t.Fatalf("reviews = %+v, want the approval and its carry", reviews)
	}
	if advice.SupersededApproval != nil {
		t.Fatalf("the undo's superseded_approval = %#v, want none: its version carries the approval", advice.SupersededApproval)
	}

	approvedSeq, versionSeq := 0, 0
	for _, event := range carryEvents(t, handler, document.issueKey) {
		switch event.Type {
		case "artifact.approved":
			payload := decodePayload[struct {
				Version int     `json:"version"`
				Reason  *string `json:"reason"`
				AskID   *string `json:"ask_id"`
			}](t, event)
			if payload.Version == 3 {
				if event.Actor != (model.Actor{Kind: "user", ID: "alice"}) || payload.Reason == nil || *payload.Reason != restoreSentence(1) || payload.AskID != nil {
					t.Fatalf("restored artifact.approved = actor %#v payload %s", event.Actor, event.Payload)
				}
				approvedSeq = event.Seq
			}
		case "artifact.version":
			if decodePayload[struct {
				Version model.Version `json:"version"`
			}](t, event).Version.Number == 3 {
				versionSeq = event.Seq
			}
		}
	}
	if approvedSeq == 0 || versionSeq == 0 || approvedSeq >= versionSeq {
		t.Fatalf("artifact.approved seq %d, artifact.version seq %d, want both for version 3 with the approval first", approvedSeq, versionSeq)
	}

	requested := sessionRequest(t, handler, http.MethodPost, "/api/v1/artifacts/"+artifactID+"/approval-requests", map[string]any{"actor": sessionActor()})
	if requested.Code != http.StatusOK || !strings.Contains(requested.Body.String(), `"ask":null`) || !strings.Contains(requested.Body.String(), `"state":"approved"`) {
		t.Fatalf("request after the undo: status=%d body=%s, want the approval at version 3 and no ask", requested.Code, requested.Body.String())
	}
}

// An approval request the agent opened on the accidental version follows the document to the
// undo's version, and the restore then answers it Approve in alice's name with the restore's
// reason, so the review names it and she is asked nothing.
func TestAnOpenApprovalRequestIsAnsweredByTheVersionRestoringTheApprovedText(t *testing.T) {
	handler := approvalCarryHandler(t)
	artifactID := approveThenChange(t, handler, "Request open at the undo", "", nil).artifactID
	request := requestApproval(t, handler, artifactID)
	if request.Approval.Version != 2 || request.Approval.Approved == nil || request.Approval.Approved.Version != 1 {
		t.Fatalf("request = %#v, want one at version 2 following the approval of version 1", request.Approval)
	}

	agentEdit(t, handler, artifactID, 3, replaceOp("behind a flag and a canary.", "behind a flag."))
	requireReviewCarriedWith(t, handler, artifactID, 3, 1, restoreSentence(1), &request.ID)
	answered := readBlockAsk(t, handler, request.ID)
	if answered.State != "answered" || answered.Answer == nil || answered.Answer.User != "alice" ||
		!reflect.DeepEqual(answered.Answer.Selected, []string{"Approve"}) || answered.Answer.Text == nil || *answered.Answer.Text != restoreSentence(1) {
		t.Fatalf("request after the undo = state %q answer %#v, want Approve by alice with the restore's reason", answered.State, answered.Answer)
	}
	if answered.Approval == nil || answered.Approval.Version != 3 {
		t.Fatalf("request after the undo names version %#v, want 3", answered.Approval)
	}
}

// A version written over an approved document that changed nothing - a named version, which the
// versions route writes whatever changed - holds the approved text, so the approval stays current.
func TestANamedVersionThatChangesNothingKeepsTheApprovalCurrent(t *testing.T) {
	handler := approvalCarryHandler(t)
	issue := createInteractionIssue(t, handler, "TEST", "Name an unchanged version", restoreSpec)
	reviewDocument(t, handler, issue.PrimaryArtifactID, "alice", "approved", "")

	named := dispatchRequest(t, handler, http.MethodPost, "/api/v1/artifacts/"+issue.PrimaryArtifactID+"/versions",
		map[string]string{"summary": "nothing changed"}, "alice")
	if named.Code != http.StatusCreated {
		t.Fatalf("name a version: status=%d body=%s", named.Code, named.Body.String())
	}
	waitForArtifactVersion(t, handler, issue.PrimaryArtifactID, 2)
	requireReviewCarriedWith(t, handler, issue.PrimaryArtifactID, 2, 1, restoreSentence(1), nil)
}

// The upload route writes its version itself, outside the document service's version write, and
// restores an approval as the edit route does: an agent that undoes its edit by uploading the
// approved text again gets version 3 approved in alice's name, and the upload supersedes nothing.
func TestAnUploadOfTheApprovedTextCarriesTheApproval(t *testing.T) {
	handler := approvalCarryHandler(t)
	document := approveThenChange(t, handler, "Upload the approved text again", "", nil)

	uploaded := sessionRequest(t, handler, http.MethodPost, "/api/v1/issues/"+document.issueKey+"/artifacts", map[string]any{
		"actor": sessionActor(), "name": "spec.md", "content": versionMarkdown(t, handler, document.artifactID, 1),
	})
	advice := adviceFromResponse(t, uploaded.Code, uploaded.Body.String())
	waitForArtifactVersion(t, handler, document.artifactID, 3)
	if restored, approved := versionMarkdown(t, handler, document.artifactID, 3), versionMarkdown(t, handler, document.artifactID, 1); restored != approved {
		t.Fatalf("version 3 = %q, want version 1's text %q", restored, approved)
	}
	requireReviewCarriedWith(t, handler, document.artifactID, 3, 1, restoreSentence(1), nil)
	if advice.SupersededApproval != nil {
		t.Fatalf("the upload's superseded_approval = %#v, want none: its version carries the approval", advice.SupersededApproval)
	}
}

// (b) The undo misses by one character: version 3 is version 1 with its period an exclamation
// mark, which is a change, so the approval stays stale, the undo supersedes nothing (the approval
// was already stale) and a request for approval opens one.
func TestAVersionOneCharacterFromTheApprovedTextLeavesTheApprovalStale(t *testing.T) {
	handler := approvalCarryHandler(t)
	artifactID := approveThenChange(t, handler, "Nearly restore the approved text", "", nil).artifactID

	advice := agentEdit(t, handler, artifactID, 3, replaceOp("behind a flag and a canary.", "behind a flag!"))
	approved, nearly := versionMarkdown(t, handler, artifactID, 1), versionMarkdown(t, handler, artifactID, 3)
	if want := strings.Replace(approved, "behind a flag.", "behind a flag!", 1); nearly != want || len(nearly) != len(approved) {
		t.Fatalf("version 3 = %q, want version 1 with one character changed, %q", nearly, want)
	}
	requireNoCarry(t, handler, artifactID, 1, "stale")
	if advice.SupersededApproval != nil {
		t.Fatalf("the near undo's superseded_approval = %#v, want none: the approval was already stale", advice.SupersededApproval)
	}
	request := requestApproval(t, handler, artifactID)
	if request.Approval.Version != 3 || request.Approval.Approved == nil || request.Approval.Approved.Version != 1 {
		t.Fatalf("request = %#v, want one at version 3 following the approval of version 1", request.Approval)
	}
}

// (c) Version 3 holds version 1's text outside its decision block, restored, and a decision block
// changed since. Such a version is judged by the rule that judged it before restored text carried
// anything, the answer carry (docs.carryVerdict): it carries an approval only when every block it
// changed is the approver's own answer, given since her approval. An agent's reword of the open
// block is no answer, so the approval stays stale.
func TestARestoredTextAroundADecisionBlockTheAgentRewordedLeavesTheApprovalStale(t *testing.T) {
	handler := approvalCarryHandler(t)
	artifactID := approveThenChange(t, handler, "Restore around a reworded block", "\n"+waveAsk, waveBlock).artifactID

	agentEdit(t, handler, artifactID, 3,
		replaceOp("behind a flag and a canary.", "behind a flag."),
		replaceOp("How many regions take the first wave?", "How many regions go in the first wave?"))
	requireRestoredAroundAChangedBlock(t, handler, artifactID, 3)
	requireNoCarry(t, handler, artifactID, 1, "stale")
}

// (c) The other half of the answer carry's rule: alice answers the open block while the paragraph
// is still changed, which carries nothing, and the agent then restores the paragraph. Version 4 is
// version 1's text around her answer, given since her approval, so the answer carry carries her
// approval to it, with the answer carry's reason rather than the restore's.
func TestARestoredTextAroundTheApproversOwnAnswerCarriesByTheAnswerCarry(t *testing.T) {
	handler := approvalCarryHandler(t)
	document := approveThenChange(t, handler, "Restore around her answer", "\n"+waveAsk, waveBlock)
	artifactID := document.artifactID

	answerDecisionBlock(t, handler, document.asks["wave"], "alice", []string{"Two"}, "", nil)
	waitForArtifactVersion(t, handler, artifactID, 3)
	requireNoCarry(t, handler, artifactID, 1, "stale")

	agentEdit(t, handler, artifactID, 4, replaceOp("behind a flag and a canary.", "behind a flag."))
	requireRestoredAroundAChangedBlock(t, handler, artifactID, 4)
	requireCarriedReview(t, handler, artifactID, 4, 1, nil)
}

// requireRestoredAroundAChangedBlock fails t unless version number of the document holds version
// 1's text outside its decision blocks and is not version 1's text.
func requireRestoredAroundAChangedBlock(t *testing.T, handler http.Handler, artifactID string, number int) {
	t.Helper()
	approved, restored := versionMarkdown(t, handler, artifactID, 1), versionMarkdown(t, handler, artifactID, number)
	approvedCut, err := docs.CutDecisionBlocks(approved)
	if err != nil {
		t.Fatalf("cut version 1: %v", err)
	}
	restoredCut, err := docs.CutDecisionBlocks(restored)
	if err != nil {
		t.Fatalf("cut version %d: %v", number, err)
	}
	if restoredCut != approvedCut || restored == approved {
		t.Fatalf("version %d = %q, want version 1's text %q outside a changed decision block", number, restored, approved)
	}
}
