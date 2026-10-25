package requests

import (
	"context"
	"errors"
	"fmt"
	"sync"
	"sync/atomic"
	"testing"

	"github.com/sjawhar/envoy/internal/broker/policy"
	"github.com/sjawhar/envoy/internal/broker/record"
	"github.com/sjawhar/envoy/internal/broker/secrets"
)

// TestAKindSecretIsNeverReleasedToAnotherOperator: an agent-tier kind secret its owner's sessions
// get at once is, for another operator's session, a request its owner must approve, which that
// operator cannot approve; and that session cannot read the owner's live grant.
func TestAKindSecretIsNeverReleasedToAnotherOperator(t *testing.T) {
	machine, enrollmentID, key, _ := newFixture(t)
	retag(t, machine, requestKindSecret("OWNERS_APP_KEY", policy.KindGitHubApp, policy.TierAgent, ""))
	var calls atomic.Int32
	machine.Minters = map[string]Minter{
		policy.KindGitHubApp: func(context.Context, secrets.MintRequest) (string, error) { calls.Add(1); return "minted-token", nil },
	}
	const other = "bob@example.com"
	otherEnr, otherKey := newEnrollment(t, machine.Store, "session", "box-other-"+t.Name(), new(other), nil)

	theirs, err := machine.Create(t.Context(), otherEnr, signRequest(t, machine, otherKey, "read the App key", "OWNERS_APP_KEY"), "")
	if err != nil || theirs.State != "pending" || theirs.GrantID != nil || theirs.RecordID == nil {
		t.Fatalf("Create by %s = %+v, %v; want pending on its owner's approval", other, theirs, err)
	}
	if decision, err := machine.ApplyDecision(t.Context(), *theirs.RecordID, true, other); !errors.Is(err, record.ErrNotApprover) || decision.GrantID != "" {
		t.Fatalf("ApplyDecision by %s = %+v, %v; want ErrNotApprover", other, decision, err)
	}
	owners, err := machine.Create(t.Context(), enrollmentID, signRequest(t, machine, key, "read the App key", "OWNERS_APP_KEY"), "")
	if err != nil || owners.GrantID == nil {
		t.Fatalf("Create by the owner = %+v, %v; want a grant", owners, err)
	}
	if values, _, err := machine.Values(t.Context(), *owners.GrantID, otherEnr); !errors.Is(err, ErrNotYours) || values != nil {
		t.Fatalf("Values of the owner's grant from %s's session = %v, %v; want ErrNotYours", other, values, err)
	}
	if n := calls.Load(); n != 0 {
		t.Fatalf("minter calls = %d; want none for %s", n, other)
	}
}

// TestAHumanTierGitHubAppSecretWaitsForItsOwnerThenMints: a human-tier github-app secret's
// request waits on its owner, releases nothing meanwhile, and once approved mints for the owners
// its github-owners tag lists.
func TestAHumanTierGitHubAppSecretWaitsForItsOwnerThenMints(t *testing.T) {
	machine, enrollmentID, key, approver := newFixture(t)
	retag(t, machine, requestKindSecret("HUMAN_APP_KEY", policy.KindGitHubApp, policy.TierHuman, ""))
	var owners []string
	machine.Minters = map[string]Minter{
		policy.KindGitHubApp: func(_ context.Context, req secrets.MintRequest) (string, error) {
			owners = req.GitHubOwners
			return "minted-token", nil
		},
	}
	request, err := machine.Create(t.Context(), enrollmentID, signRequest(t, machine, key, "read the App key", "HUMAN_APP_KEY"), "")
	if err != nil || request.State != "pending" || request.GrantID != nil || request.RecordID == nil {
		t.Fatalf("Create = %+v, %v; want pending", request, err)
	}
	decision, err := machine.ApplyDecision(t.Context(), *request.RecordID, true, approver)
	if err != nil || decision.GrantID == "" {
		t.Fatalf("ApplyDecision = %+v, %v; want a grant", decision, err)
	}
	values, _, err := machine.Values(t.Context(), decision.GrantID, enrollmentID)
	if err != nil || values["HUMAN_APP_KEY"] != "minted-token" {
		t.Fatalf("Values = %v, %v; want the minted token", values, err)
	}
	if fmt.Sprint(owners) != "[acme ada]" {
		t.Fatalf("minter's GitHubOwners = %v; want [acme ada]", owners)
	}
}

// TestConcurrentReleasesOfOneKindSecretAllMint: many releases of one grant at once each mint and
// each succeeds, and none sees another's token.
func TestConcurrentReleasesOfOneKindSecretAllMint(t *testing.T) {
	machine, enrollmentID, key, _ := newFixture(t)
	retag(t, machine, requestKindSecret("BUSY_APP_KEY", policy.KindGitHubApp, policy.TierAgent, ""))
	var calls atomic.Int32
	machine.Minters = map[string]Minter{
		policy.KindGitHubApp: func(context.Context, secrets.MintRequest) (string, error) {
			return fmt.Sprintf("minted-token-%d", calls.Add(1)), nil
		},
	}
	request, err := machine.Create(t.Context(), enrollmentID, signRequest(t, machine, key, "read the App key", "BUSY_APP_KEY"), "")
	if err != nil || request.GrantID == nil {
		t.Fatalf("Create = %+v, %v", request, err)
	}
	const releases = 16
	tokens := make([]string, releases)
	errs := make([]error, releases)
	var wg sync.WaitGroup
	for i := range releases {
		wg.Go(func() {
			values, _, err := machine.Values(t.Context(), *request.GrantID, enrollmentID)
			tokens[i], errs[i] = values["BUSY_APP_KEY"], err
		})
	}
	wg.Wait()
	seen := map[string]bool{}
	for i := range releases {
		if errs[i] != nil || tokens[i] == "" || seen[tokens[i]] {
			t.Fatalf("release %d = %q, %v; want its own minted token (seen %v)", i, tokens[i], errs[i], seen)
		}
		seen[tokens[i]] = true
	}
	if n := calls.Load(); n != releases {
		t.Fatalf("minter calls = %d; want %d", n, releases)
	}
}
