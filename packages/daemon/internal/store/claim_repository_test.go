package store

import (
	"context"
	"testing"

	"github.com/sjawhar/legion/daemon/internal/claim"
	"github.com/sjawhar/legion/daemon/internal/supervise"
)

// A claim's repository owner is the owner of the repository its issue's record names, read from
// the daemon's own record and nothing a pane can write: what a tmux pane adopted after a restart
// holds its write token for. A claim whose issue the workflow does not record, and a token no claim
// holds, has none.
func TestClaimRepositoryOwnerIsTheOwnerItsIssuesRecordNames(t *testing.T) {
	ctx := context.Background()
	s := migratedStore(t)
	if _, err := s.pool.Exec(ctx, `insert into issues (key, tree, project, title, phase, generation, status, rank, last_dispatch_seq, repo, default_branch)
		values ('LEGION-209', 'LEGION-208', 'LEGION', 'child', 'implementing', 1, 'in_progress', 'A', 1, 'Zeta/Tools', 'release/v7.1')`); err != nil {
		t.Fatal(err)
	}
	for _, c := range []supervise.Claim{
		{Token: "legion-legion-legion-209-implementer", Project: "legion", Tree: "LEGION-208", Issue: "LEGION-209", Role: claim.RoleImplementer, State: supervise.StateIdle},
		{Token: "legion-legion-legion-300-implementer", Project: "legion", Tree: "LEGION-300", Issue: "LEGION-300", Role: claim.RoleImplementer, State: supervise.StateIdle},
	} {
		if err := s.PutClaim(ctx, c); err != nil {
			t.Fatal(err)
		}
	}
	for _, tc := range []struct {
		token claim.Token
		want  string
	}{
		{"legion-legion-legion-209-implementer", "Zeta"},
		{"legion-legion-legion-300-implementer", ""},
		{"legion-legion-legion-999-implementer", ""},
	} {
		if got, err := s.ClaimRepositoryOwner(ctx, tc.token); err != nil || got != tc.want {
			t.Errorf("ClaimRepositoryOwner(%s) = %q, %v; want %q", tc.token, got, err, tc.want)
		}
	}
}
