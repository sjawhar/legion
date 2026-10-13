package api

import (
	"context"
	"encoding/json"
	"net/http"
	"reflect"
	"testing"
	"time"

	"github.com/sjawhar/envoy/internal/dispatch/docs"
	"github.com/sjawhar/envoy/internal/dispatch/model"
	"github.com/sjawhar/envoy/internal/dispatch/store"
)

// A human's approval is what a request to approve again follows. A request opened on a document
// with an earlier approval names that approval's version, time and approver in its approval
// (approved_version, approved_at, approved_by), so the human reads what he is approving since the
// version he approved rather than from nothing; a first request names none (LEGION-573).

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
// transaction and settle on no timer a test could race.
func approvalCarryHandler(t *testing.T) http.Handler {
	t.Helper()
	handler, _ := newInteractionHandler(t, func(database *store.Store) docs.API {
		documentService := docs.New(docs.Deps{Store: database, Settle: time.Hour})
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
