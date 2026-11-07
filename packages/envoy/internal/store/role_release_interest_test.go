// What a release leaves in the session's interest record. One record serves every process
// running under one session id, so the topic a release may not take is the lane topic of a claim
// it did not free: the process that still holds the role takes the role's messages through it.
// The claim half of the same releases is read in role_claim_release_test.go.

package store

import (
	"context"
	"testing"
	"time"

	"github.com/sjawhar/envoy/internal/contracts"
)

// A release that frees no claim keeps that claim's lane topic in the interest record, and
// removes every other topic it named. The record is shared by every process under one session
// id, so a release from a second process that resumed the holder's id would otherwise take the
// lane topic the live holder's deliveries ride on while leaving it the role.
//
// The release the claim's own process makes is the control: it frees the claim, so the lane
// topic goes with it, exactly as it always did.
func TestAReleaseKeepsTheLaneTopicOfAClaimItDidNotFree(t *testing.T) {
	const (
		role   = "legion-controller"
		holder = "ses_release_keeps_topic"
	)
	roleTopic := contracts.RoleTopicPrefix + role
	for name, tc := range map[string]struct {
		authority RoleReleaseAuthority
		frees     bool
	}{
		"a second process of the holder's session id": {authority: RoleReleaseAuthority{ClientInstanceID: roleClaimSecondInstance}},
		"the process the claim names":                 {authority: releasedByItsProcess, frees: true},
	} {
		t.Run(name, func(t *testing.T) {
			reg := releasableRoleClaim(t, role, holder, roleClaimInstance)

			if err := reg.Remove(context.Background(), holder, []string{roleTopic, releaseKeptTopic}, tc.authority); err != nil {
				t.Fatalf("release %s's interests: %v", holder, err)
			}

			if tc.frees {
				assertRoleHolder(t, reg, role, "")
				// Both topics the request named are gone, which leaves no record at all.
				assertInterestMissing(t, reg, holder)
				return
			}
			assertRoleHolder(t, reg, role, holder)
			assertInterestTopicsForSession(t, reg, holder, roleTopic)
		})
	}
}

// A release whose every named topic is kept makes no interest write at all, rather than one with
// an empty list: an empty list is how removeInterestTopics is told to delete the whole record,
// which would cost the session every topic it has over a release that took nothing.
func TestAReleaseThatKeepsEveryTopicItNamedLeavesTheRecordWhole(t *testing.T) {
	const (
		role   = "legion-controller"
		holder = "ses_release_keeps_record"
	)
	roleTopic := contracts.RoleTopicPrefix + role
	reg := releasableRoleClaim(t, role, holder, roleClaimInstance)

	if err := reg.Remove(context.Background(), holder, []string{roleTopic}, RoleReleaseAuthority{ClientInstanceID: roleClaimSecondInstance}); err != nil {
		t.Fatalf("release %s's claim of %s from a second process: %v", holder, role, err)
	}

	assertRoleHolder(t, reg, role, holder)
	assertInterestTopicsForSession(t, reg, holder, releaseKeptTopic, roleTopic)
}

// The whole of the incident, on one session id: a first process takes the role and renews it; it
// stops answering and its renewal lapses; a second process under that same session id takes the
// claim (POST /v1/roles/set, which lists the claimant's own session id as supersedable once
// nobody has renewed the standing claim inside the live window); and then the first process's
// release arrives. The second process keeps the claim, keeps its lane topic, and its own
// registrations go on renewing the claim window after window.
//
// Both halves are read, because either one alone leaves the role broken: a release that freed
// the claim would hand the role to nobody, and a release that took the lane topic out of the
// record would leave the second process holding a role whose messages no longer reach it.
func TestASecondProcessThatTookALapsedClaimKeepsItThroughTheFirstsRelease(t *testing.T) {
	const (
		role       = "legion-controller"
		session    = "ses_resumed_under_one_id"
		liveWindow = time.Minute
		beat       = 20 * time.Second
	)
	roleTopic := contracts.RoleTopicPrefix + role
	ctx := context.Background()
	reg, advance := renewableRoleClaim(t, role, session, roleClaimInstance)
	if _, err := reg.Upsert(Interest{SessionID: session, MachineID: "example-host"}, []string{releaseKeptTopic}); err != nil {
		t.Fatalf("register %s's interests: %v", session, err)
	}
	claimed := storedRoleClaim(t, reg, role).ClaimedAt

	// 1. The first process holds the role, and its registration renews the claim.
	since := beat
	advance(since)
	if err := reg.RenewRoleClaims(ctx, session, roleClaimInstance); err != nil {
		t.Fatalf("the first process's registration: %v", err)
	}
	if renewed := storedRoleClaim(t, reg, role); renewed.RenewedAt != claimed+since.Milliseconds() {
		t.Fatalf("the claim was renewed at %d, want the registration's own moment %d", renewed.RenewedAt, claimed+since.Milliseconds())
	}

	// 2. The first process stops answering, and a live window passes with no renewal of its own.
	since += liveWindow + beat
	advance(since)
	if lapsed := storedRoleClaim(t, reg, role); reg.ClaimRenewedWithin(lapsed, liveWindow) {
		t.Fatalf("the claim %+v still reads as renewed inside %s, so nothing has lapsed and the case that follows proves nothing", lapsed, liveWindow)
	}

	// 3. A second process under the same session id takes the role.
	claimRoleAs(t, reg, session, "example-host", role, roleClaimSecondInstance)

	// 4. The first process's release arrives, naming itself.
	if err := reg.Remove(ctx, session, []string{roleTopic}, RoleReleaseAuthority{ClientInstanceID: roleClaimInstance}); err != nil {
		t.Fatalf("the first process's release of %s: %v", role, err)
	}
	taken := storedRoleClaim(t, reg, role)
	if taken.HolderSessionID != session || taken.ClientInstanceID != roleClaimSecondInstance || taken.HandedBack {
		t.Fatalf("the claim after the first process's release is %+v, want %s's claim of process %q, unmarked", taken, session, roleClaimSecondInstance)
	}
	assertRoleHolder(t, reg, role, session)
	assertInterestTopicsForSession(t, reg, session, releaseKeptTopic, roleTopic)

	// 5. The second process goes on renewing the claim it holds, window after window.
	for window := range 3 {
		since += liveWindow
		advance(since)
		if err := reg.RenewRoleClaims(ctx, session, roleClaimSecondInstance); err != nil {
			t.Fatalf("the second process's registration in window %d: %v", window+1, err)
		}
		renewed := storedRoleClaim(t, reg, role)
		if renewed.RenewedAt != claimed+since.Milliseconds() {
			t.Fatalf("the claim was renewed at %d in window %d, want the registration's own moment %d", renewed.RenewedAt, window+1, claimed+since.Milliseconds())
		}
		if !reg.ClaimRenewedWithin(renewed, liveWindow) {
			t.Fatalf("the claim %+v reads as stale in window %d, so the role it holds would be reaped from under it", renewed, window+1)
		}
		if renewed.ClientInstanceID != roleClaimSecondInstance || renewed.HandedBack {
			t.Fatalf("the claim in window %d is %+v, want it still the second process's and unmarked", window+1, renewed)
		}
	}
	assertRoleHolder(t, reg, role, session)
	assertInterestTopicsForSession(t, reg, session, releaseKeptTopic, roleTopic)
}
