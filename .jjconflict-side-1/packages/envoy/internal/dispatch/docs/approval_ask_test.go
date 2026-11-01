package docs

import (
	"testing"

	"github.com/sjawhar/envoy/internal/dispatch/model"
)

func TestApprovalAskSummaryRoundTripsTheSharedQuestion(t *testing.T) {
	ask := model.Ask{
		ID:       "approval-1",
		Question: ApprovalQuestion("spec.md", 2, "Names the rollback plan."),
		Approval: &model.AskApproval{Name: "spec.md", Version: 2},
	}
	if summary, err := ApprovalAskSummary(ask); err != nil || summary != "Names the rollback plan." {
		t.Fatalf("approval summary = %q, %v", summary, err)
	}
}
