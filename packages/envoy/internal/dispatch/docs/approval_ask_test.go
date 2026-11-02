package docs

import (
	"testing"

	"github.com/sjawhar/envoy/internal/dispatch/model"
)

func TestApprovalAskSummaryRoundTripsTheSharedQuestion(t *testing.T) {
	documents := []model.AskApprovalDocument{{ArtifactID: "artifact-1", Name: "spec.md", Version: 2, RequestedVersion: 2}}
	ask := model.Ask{
		ID:       "approval-1",
		Question: ApprovalQuestion(documents, "Names the rollback plan."),
		Approval: &model.AskApproval{Name: "spec.md", Version: 2, Documents: documents},
	}
	if summary, err := ApprovalAskSummary(ask); err != nil || summary != "Names the rollback plan." {
		t.Fatalf("approval summary = %q, %v", summary, err)
	}
}

// A request naming a spec and its issue's Verification document asks for both, each at its own
// version, and the summary parses back out of that question too.
func TestApprovalAskSummaryRoundTripsATwoDocumentQuestion(t *testing.T) {
	documents := []model.AskApprovalDocument{
		{ArtifactID: "artifact-1", Name: "spec.md", Version: 3, RequestedVersion: 3},
		{ArtifactID: "artifact-2", Name: "verification.md", Version: 2, RequestedVersion: 2},
	}
	question := ApprovalQuestion(documents, "Both describe the live sync.")
	if question != "Approve spec.md (version 3) and verification.md (version 2)? Both describe the live sync." {
		t.Fatalf("two-document question = %q", question)
	}
	ask := model.Ask{ID: "approval-2", Question: question, Approval: &model.AskApproval{Documents: documents}}
	ask.Approval.Lead()
	if summary, err := ApprovalAskSummary(ask); err != nil || summary != "Both describe the live sync." {
		t.Fatalf("two-document summary = %q, %v", summary, err)
	}
	// The release before the documents list rewords such a request to name its first document
	// alone; the summary still parses back out, so a move or a hand-back after it keeps working.
	ask.Question = ApprovalQuestion(documents[:1], "Both describe the live sync.")
	if summary, err := ApprovalAskSummary(ask); err != nil || summary != "Both describe the live sync." {
		t.Fatalf("summary after an older release reworded the question = %q, %v", summary, err)
	}
}
