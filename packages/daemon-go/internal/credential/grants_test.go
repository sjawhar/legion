package credential

import (
	"errors"
	"testing"
	"time"

	"github.com/sjawhar/legion/daemon/internal/claim"
	"github.com/sjawhar/legion/daemon/internal/supervise"
)

func TestGrantsExpireAndCannotBeRedeemedTwice(t *testing.T) {
	now := time.Date(2026, 9, 23, 12, 0, 0, 0, time.UTC)
	grants := New(func() time.Time { return now })
	issued, err := grants.Mint(supervise.Claim{
		Token:          "legion-legion-legion-208-implementer",
		Project:        "legion",
		Tree:           "LEGION-208",
		Issue:          "LEGION-208",
		Role:           claim.RoleImplementer,
		CapabilityHash: []byte("live-session-capability"),
	})
	if err != nil {
		t.Fatalf("Mint: %v", err)
	}
	if issued.ExpiresAt != now.Add(60*time.Second) {
		t.Fatalf("ExpiresAt = %s, want %s", issued.ExpiresAt, now.Add(60*time.Second))
	}
	if _, err := grants.Redeem(issued.ID); err != nil {
		t.Fatalf("first Redeem: %v", err)
	}
	if _, err := grants.Redeem(issued.ID); !errors.Is(err, ErrUsed) {
		t.Fatalf("second Redeem = %v, want ErrUsed", err)
	}

	expired, err := grants.Mint(supervise.Claim{
		Token:          "legion-legion-legion-209-reviewer",
		Project:        "legion",
		Tree:           "LEGION-209",
		Issue:          "LEGION-209",
		Role:           claim.RoleReviewer,
		CapabilityHash: []byte("another-live-session-capability"),
	})
	if err != nil {
		t.Fatalf("Mint expired candidate: %v", err)
	}
	now = now.Add(60 * time.Second)
	if _, err := grants.Redeem(expired.ID); !errors.Is(err, ErrExpired) {
		t.Fatalf("expired Redeem = %v, want ErrExpired", err)
	}
}

func TestGrantsRequireAnAuthenticatedClaim(t *testing.T) {
	grants := New(func() time.Time { return time.Unix(0, 0) })
	if _, err := grants.Mint(supervise.Claim{Role: claim.RoleImplementer}); !errors.Is(err, ErrUnauthenticated) {
		t.Fatalf("Mint unauthenticated claim = %v, want ErrUnauthenticated", err)
	}
}

func TestGrantStillMatchesTheClaimThatMintedIt(t *testing.T) {
	grants := New(func() time.Time { return time.Unix(0, 0) })
	issued, err := grants.Mint(supervise.Claim{
		Token:          "legion-legion-legion-208-tester",
		Project:        "legion",
		Tree:           "LEGION-208",
		Issue:          "LEGION-208",
		Role:           claim.RoleTester,
		CapabilityHash: []byte("capability-at-mint"),
	})
	if err != nil {
		t.Fatalf("Mint: %v", err)
	}
	if !grants.StillMatches(issued, supervise.Claim{Token: issued.Claim, CapabilityHash: []byte("capability-at-mint")}) {
		t.Fatal("StillMatches rejected the unchanged authenticated claim")
	}
	if grants.StillMatches(issued, supervise.Claim{Token: issued.Claim, CapabilityHash: []byte("replacement-capability")}) {
		t.Fatal("StillMatches accepted a claim whose capability changed after the grant was minted")
	}
}
