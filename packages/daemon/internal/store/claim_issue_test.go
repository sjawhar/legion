package store

import (
	"context"
	"testing"

	"github.com/sjawhar/legion/daemon/internal/claim"
	"github.com/sjawhar/legion/daemon/internal/supervise"
)

// A claim's issue is read from the daemon's own claims, in the issue key's own case, which its
// token lowercases; a token no claim holds is refused.
func TestClaimIssueIsTheIssueTheClaimIsOn(t *testing.T) {
	ctx := context.Background()
	s := migratedStore(t)
	if err := s.PutClaim(ctx, supervise.Claim{Token: "legion-legion-legion-209-implementer", Project: "legion", Tree: "LEGION-208", Issue: "LEGION-209",
		Role: claim.RoleImplementer, State: supervise.StateIdle}); err != nil {
		t.Fatal(err)
	}
	if got, err := s.ClaimIssue(ctx, "legion-legion-legion-209-implementer"); err != nil || got != "LEGION-209" {
		t.Errorf("ClaimIssue = %q, %v; want LEGION-209", got, err)
	}
	if got, err := s.ClaimIssue(ctx, "legion-legion-legion-999-implementer"); err == nil {
		t.Errorf("ClaimIssue of a token no claim holds = %q, want a refusal", got)
	}
}
