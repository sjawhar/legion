package api

import (
	"context"
	"encoding/json"
	"net/http"
	"testing"

	"github.com/sjawhar/envoy/internal/dispatch/store"
)

// approveAdviceDocument approves a document from its header, as a person does, and answers the
// version the review pinned.
func approveAdviceDocument(t *testing.T, handler http.Handler, artifactID string) int {
	t.Helper()
	return reviewAdviceDocument(t, handler, artifactID, map[string]any{"state": "approved"})
}

func reviewAdviceDocument(t *testing.T, handler http.Handler, artifactID string, body map[string]any) int {
	t.Helper()
	response := dispatchRequest(t, handler, http.MethodPost, "/api/v1/artifacts/"+artifactID+"/reviews", body, "alice")
	if response.Code != http.StatusCreated {
		t.Fatalf("review document: status=%d body=%s", response.Code, response.Body.String())
	}
	review := decodeBody[struct {
		Version int `json:"version"`
	}](t, response)
	return review.Version
}

// editAdviceDocument runs one insert at the document's end as a session, the way an agent's
// `dispatch doc-edit` reaches this route, and answers the advice the write came back with.
func editAdviceDocument(t *testing.T, handler http.Handler, artifactID, markdown string) *testWriteAdvice {
	t.Helper()
	response := sessionRequest(t, handler, http.MethodPost, "/api/v1/artifacts/"+artifactID+"/edits", map[string]any{
		"actor": adviceSessionActor("session-editor"),
		"ops":   []map[string]any{{"op": "insert", "markdown": markdown, "after": "end"}},
	})
	return adviceFromResponse(t, response.Code, response.Body.String())
}

// A document edit that stores a version over the one a person approved says so, because that is
// what closes a Legion design gate. The edit after it says nothing: that approval was already
// stale, and the second edit closed nothing.
func TestDocumentEditAdviceNamesTheApprovalItSuperseded(t *testing.T) {
	handler := newTestHandler(t)
	createAdviceProject(t, handler, "GATE")
	spec := "# Transport\n\nThe design, as approved.\n"
	issue := createAdviceIssue(t, handler, "GATE", "Approved spec", &spec)
	approved := approveAdviceDocument(t, handler, issue.PrimaryArtifactID)

	advice := editAdviceDocument(t, handler, issue.PrimaryArtifactID, "A stale sentence, removed.")
	superseded := advice.SupersededApproval
	if superseded == nil {
		t.Fatalf("advice = %#v, want the approval the edit superseded", advice)
	}
	if superseded.ApprovedVersion != approved || superseded.Version != approved+1 || superseded.ApprovedBy != "alice" {
		t.Fatalf("superseded_approval = %#v, want version %d superseding the approval of %d by alice",
			superseded, approved+1, approved)
	}

	again := editAdviceDocument(t, handler, issue.PrimaryArtifactID, "One more line.")
	if again.SupersededApproval != nil {
		t.Fatalf("second edit's superseded_approval = %#v, want none: the approval was already stale", again.SupersededApproval)
	}
}

// The negative that keeps the warning worth reading: a document nobody approved, and one whose
// newest review requested changes, each raise nothing. A changes request had already closed the
// gate, and its reviewer said why.
func TestDocumentEditAdviceSupersedesNoApprovalWhenThereIsNone(t *testing.T) {
	handler := newTestHandler(t)
	createAdviceProject(t, handler, "NOGATE")
	spec := "# Draft\n\nNobody has approved this.\n"
	issue := createAdviceIssue(t, handler, "NOGATE", "Unapproved spec", &spec)

	advice := editAdviceDocument(t, handler, issue.PrimaryArtifactID, "A line.")
	if advice.SupersededApproval != nil {
		t.Fatalf("unapproved document's superseded_approval = %#v, want none", advice.SupersededApproval)
	}

	reviewAdviceDocument(t, handler, issue.PrimaryArtifactID, map[string]any{
		"state": "changes_requested", "reason": "Name the rollback path.",
	})
	afterChanges := editAdviceDocument(t, handler, issue.PrimaryArtifactID, "Another line.")
	if afterChanges.SupersededApproval != nil {
		t.Fatalf("changes-requested document's superseded_approval = %#v, want none", afterChanges.SupersededApproval)
	}
}

// An upload that replaces an approved document supersedes its approval exactly as an edit does:
// this route always writes a version, so a replacement of an approved spec closes its gate too.
func TestDocumentUploadAdviceNamesTheApprovalItSuperseded(t *testing.T) {
	handler := newTestHandler(t)
	createAdviceProject(t, handler, "UPGATE")
	spec := "# Transport\n\nThe design, as approved.\n"
	issue := createAdviceIssue(t, handler, "UPGATE", "Approved spec", &spec)
	approved := approveAdviceDocument(t, handler, issue.PrimaryArtifactID)

	replaced := sessionRequest(t, handler, http.MethodPost, "/api/v1/issues/"+issue.Key+"/artifacts", map[string]any{
		"actor": adviceSessionActor("session-editor"),
		"name":  "spec.md", "content": "# Transport\n\nThe design, rewritten.\n",
	})
	advice := adviceFromResponse(t, replaced.Code, replaced.Body.String())
	superseded := advice.SupersededApproval
	if superseded == nil || superseded.ApprovedVersion != approved || superseded.Version != approved+1 {
		t.Fatalf("upload advice = %#v (superseded %#v), want version %d superseding the approval of %d",
			advice, superseded, approved+1, approved)
	}
}

// The rule itself, over the rows a write leaves, including the one shape a route test cannot
// reach: the approval carry (docs.carryApproval) writes its approved review pinned to the version
// the write stored, so the write superseded nothing and reports nothing - the carry's own
// artifact.approved event reopens the gate. The rows are a real document's and a real person's
// approval; the version argument stands for the version each write stored.
func TestNewestReviewAsApprovalReportsOnlyTheVersionItSuperseded(t *testing.T) {
	handler, database := newTestHandlerWithStore(t)
	createAdviceProject(t, handler, "RULE")
	spec := "# Rule\n\nApproved once.\n"
	issue := createAdviceIssue(t, handler, "RULE", "Rule rows", &spec)
	approved := approveAdviceDocument(t, handler, issue.PrimaryArtifactID)

	for _, test := range []struct {
		name    string
		version int
		want    bool
	}{
		{name: "the version right above the approved one", version: approved + 1, want: true},
		{name: "the approved version itself, as the approval carry writes it", version: approved, want: false},
		{name: "a version two above it, whose approval was already stale", version: approved + 2, want: false},
	} {
		t.Run(test.name, func(t *testing.T) {
			got := newestApprovalOf(t, database, issue.PrimaryArtifactID, test.version)
			if (got != nil) != test.want {
				t.Fatalf("superseded approval at version %d = %#v, want reported=%t", test.version, got, test.want)
			}
			if got != nil && (got.ApprovedVersion != approved || got.ApprovedBy != "alice") {
				t.Fatalf("superseded approval = %#v, want the approval of version %d by alice", got, approved)
			}
		})
	}
}

// The reviews are read in the order their events committed (docs.ReviewOrder), not by
// created_at and id: created_at is the writing transaction's start and id is random, so a review
// that committed later can carry the earlier pair. That is the approval carry's own shape - a
// person's approval of the version below commits while the version write is open, and the carry's
// approval of the version it stored commits after it with an earlier created_at - and reading by
// created_at there reports a gate the write did not close.
func TestNewestReviewAsApprovalReadsTheOrderTheReviewsCommittedIn(t *testing.T) {
	handler, database := newTestHandlerWithStore(t)
	createAdviceProject(t, handler, "ORDER")
	spec := "# Order\n\nApproved once.\n"
	issue := createAdviceIssue(t, handler, "ORDER", "Commit order", &spec)
	approved := approveAdviceDocument(t, handler, issue.PrimaryArtifactID)

	// The carry's approval of the version this write stored: appended after the approval above,
	// so its event sequence is higher, and stamped a second before it, as a transaction that was
	// already open when that approval committed would stamp it.
	if _, err := database.Pool.Exec(context.Background(), `
		insert into artifact_reviews (artifact_id, version, state, actor, event_seq, created_at)
		select $1, $2, 'approved', '{"kind":"user","id":"alice"}'::jsonb,
		       max(event_seq) + 1, min(created_at) - interval '1 second'
		from artifact_reviews where artifact_id = $1
	`, issue.PrimaryArtifactID, approved+1); err != nil {
		t.Fatalf("insert the carry's approval: %v", err)
	}

	if got := newestApprovalOf(t, database, issue.PrimaryArtifactID, approved+1); got != nil {
		t.Fatalf("superseded approval = %#v, want none: the newest review approves the version this write stored", got)
	}
}

func newestApprovalOf(t *testing.T, database *store.Store, artifactID string, version int) *supersededApproval {
	t.Helper()
	approval, err := newestReviewAsApproval(context.Background(), database.Pool, artifactID, version)
	if err != nil {
		t.Fatalf("read the newest review: %v", err)
	}
	return approval
}

// A project document's edit carries no issue state, so its whole advice is the document report:
// the approval it superseded rides it there too, beside the block count.
func TestProjectDocumentEditAdviceCarriesTheApprovalItSuperseded(t *testing.T) {
	handler := newTestHandler(t)
	createAdviceProject(t, handler, "PROJGATE")
	upload := dispatchRequest(t, handler, http.MethodPost, "/api/v1/projects/PROJGATE/artifacts", map[string]string{
		"name": "notes.md", "content": "# Notes\n\nApproved.\n",
	}, "alice")
	if upload.Code != http.StatusCreated {
		t.Fatalf("project upload: status=%d body=%s", upload.Code, upload.Body.String())
	}
	document := decodeBody[struct {
		Artifact struct {
			ID string `json:"id"`
		} `json:"artifact"`
	}](t, upload)
	approved := approveAdviceDocument(t, handler, document.Artifact.ID)

	edited := sessionRequest(t, handler, http.MethodPost, "/api/v1/artifacts/"+document.Artifact.ID+"/edits", map[string]any{
		"actor": adviceSessionActor("session-editor"),
		"ops":   []map[string]any{{"op": "insert", "markdown": "A line.", "after": "end"}},
	})
	if edited.Code != http.StatusOK {
		t.Fatalf("project document edit: status=%d body=%s", edited.Code, edited.Body.String())
	}
	var raw struct {
		Advice struct {
			DecisionBlocksAdded *int                `json:"decision_blocks_added"`
			SupersededApproval  *supersededApproval `json:"superseded_approval"`
		} `json:"advice"`
	}
	if err := json.Unmarshal(edited.Body.Bytes(), &raw); err != nil {
		t.Fatalf("decode project document edit: %v", err)
	}
	if raw.Advice.DecisionBlocksAdded == nil || *raw.Advice.DecisionBlocksAdded != 0 {
		t.Fatalf("project document edit advice = %#v, want the block count beside the approval", raw.Advice)
	}
	if raw.Advice.SupersededApproval == nil || raw.Advice.SupersededApproval.ApprovedVersion != approved {
		t.Fatalf("project document edit superseded_approval = %#v, want the approval of version %d",
			raw.Advice.SupersededApproval, approved)
	}
}
