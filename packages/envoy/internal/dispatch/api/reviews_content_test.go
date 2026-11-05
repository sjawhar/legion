package api

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"net/http"
	"strconv"
	"testing"

	"github.com/sjawhar/envoy/internal/dispatch/model"
)

func contentHash(text string) string {
	sum := sha256.Sum256([]byte(text))
	return hex.EncodeToString(sum[:])
}

func contentReviews(t *testing.T, handler http.Handler, id string) []model.ArtifactReview {
	t.Helper()
	read := dispatchRequest(t, handler, http.MethodGet, "/api/v1/artifacts/"+id+"/reviews", nil, "alice")
	if read.Code != http.StatusOK {
		t.Fatalf("reviews: %d %s", read.Code, read.Body)
	}
	return decodeBody[[]model.ArtifactReview](t, read)
}

// Dropping the pin from any review/approval/version/event surface breaks this test.
func TestEveryApprovalReadNamesTheApprovedBytes(t *testing.T) {
	handler := approvalCarryHandler(t)
	issue := createInteractionIssue(t, handler, "HASH", "Approve bytes", "Approve me ☕\n")
	id := issue.PrimaryArtifactID
	h1 := contentHash(versionMarkdown(t, handler, id, 1))
	reviewDocument(t, handler, id, "alice", "approved", "")
	reviews := contentReviews(t, handler, id)
	if len(reviews) != 1 || reviews[0].ContentSHA256 == nil || *reviews[0].ContentSHA256 != h1 || reviews[0].ContentSHA256Source == nil || *reviews[0].ContentSHA256Source != "insert" {
		t.Fatalf("reviews = %#v", reviews)
	}
	checkRead := func(state string, version int, hash string) {
		t.Helper()
		read := dispatchRequest(t, handler, http.MethodGet, "/api/v1/artifacts/"+id, nil, "alice")
		if read.Code != http.StatusOK {
			t.Fatalf("read: %d %s", read.Code, read.Body)
		}
		artifact := decodeBody[model.Artifact](t, read)
		approval := artifact.Approval
		if approval == nil || approval.State != state || approval.ContentSHA256 == nil || *approval.ContentSHA256 != h1 || approval.ContentSHA256Source == nil || *approval.ContentSHA256Source != "insert" {
			t.Fatalf("approval = %#v", approval)
		}
		read = dispatchRequest(t, handler, http.MethodGet, "/api/v1/artifacts/"+id+"/versions/"+strconv.Itoa(version), nil, "alice")
		if read.Code != http.StatusOK {
			t.Fatalf("version: %d %s", read.Code, read.Body)
		}
		got := decodeBody[model.Version](t, read)
		if got.SHA256 == nil || *got.SHA256 != hash {
			t.Fatalf("version sha256 = %v, want %s", got.SHA256, hash)
		}
	}
	checkRead("approved", 1, h1)
	found := false
	for _, event := range issueEvents(t, handler, issue.Key) {
		if event.Type != "artifact.approved" {
			continue
		}
		var payload model.ArtifactReviewEventPayload
		if err := json.Unmarshal(event.Payload, &payload); err != nil {
			t.Fatal(err)
		}
		if payload.ContentSHA256 == nil || *payload.ContentSHA256 != h1 || payload.ContentSHA256Source == nil || *payload.ContentSHA256Source != "insert" {
			t.Fatalf("event = %#v", payload)
		}
		found = true
	}
	if !found {
		t.Fatal("no artifact.approved event")
	}
	reviseDocument(t, handler, id, "Approve me ☕", "Changed.", 2)
	h2 := contentHash(versionMarkdown(t, handler, id, 2))
	if h1 == h2 {
		t.Fatal("fixture hashes do not differ")
	}
	checkRead("stale", 2, h2)
	requested := reviewDocument(t, handler, id, "alice", "changes_requested", "Please revise.")
	if requested.ContentSHA256 == nil || *requested.ContentSHA256 != h2 {
		t.Fatalf("changes request = %#v", requested)
	}
	for _, event := range issueEvents(t, handler, issue.Key) {
		if event.Type != "artifact.changes_requested" {
			continue
		}
		var payload model.ArtifactReviewEventPayload
		if err := json.Unmarshal(event.Payload, &payload); err != nil {
			t.Fatal(err)
		}
		if payload.ContentSHA256 == nil || *payload.ContentSHA256 != h2 || payload.ContentSHA256Source == nil || *payload.ContentSHA256Source != "insert" {
			t.Fatalf("changes event = %#v", payload)
		}
		return
	}
	t.Fatal("no artifact.changes_requested event")
}

// A carry pins its destination bytes, not the source approval's bytes.
func TestACarriedApprovalPinsTheVersionItCarriesTo(t *testing.T) {
	handler := approvalCarryHandler(t)
	issue := createInteractionIssue(t, handler, "CARR", "Carry bytes", "Context\n")
	id := issue.PrimaryArtifactID
	reviewDocument(t, handler, id, "alice", "approved", "")
	h1 := contentHash(versionMarkdown(t, handler, id, 1))
	asks := insertDecisionBlocks(t, handler, issue.Key, id, waveAsk, 2, map[string]string{"wave": "How many regions take the first wave?"})
	answerDecisionBlock(t, handler, asks["wave"], "alice", []string{"Two"}, "Two regions first.", nil)
	waitForArtifactVersion(t, handler, id, 3)
	requireCarriedReview(t, handler, id, 3, 1, nil)
	h3 := contentHash(versionMarkdown(t, handler, id, 3))
	reviews := contentReviews(t, handler, id)
	if h3 == h1 || len(reviews) != 2 || reviews[0].ContentSHA256 == nil || *reviews[0].ContentSHA256 != h3 || reviews[0].ContentSHA256Source == nil || *reviews[0].ContentSHA256Source != "insert" {
		t.Fatalf("carried reviews = %#v; hashes %s / %s", reviews, h1, h3)
	}
}
