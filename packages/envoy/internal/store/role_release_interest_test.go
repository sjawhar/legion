// What a release leaves in the session's interest record. One record serves every process
// running under one session id, so the topic a release may not take is the lane topic of a claim
// it did not free: that topic is the interest half of a claim the release left standing.
// The claim half of the same releases is read in role_claim_release_test.go.

package store

import (
	"context"
	"slices"
	"testing"
	"time"

	"github.com/sjawhar/envoy/internal/contracts"
)

// assertRemovedTopics fails unless removed, which Registry.Remove answers, holds exactly want:
// the topics it took out of the record, which POST /v1/interests/unsubscribe reports as its
// removed list.
func assertRemovedTopics(t *testing.T, removed []string, want ...string) {
	t.Helper()
	got := slices.Sorted(slices.Values(removed))
	slices.Sort(want)
	if !slices.Equal(got, want) {
		t.Fatalf("the release answered %v as removed, want %v", got, want)
	}
}

// A release that frees no claim keeps that claim's lane topic in the interest record, and
// removes every other topic it named. The record is shared by every process under one session
// id, so a release from a second process that resumed the holder's id would otherwise take out
// the interest half of a claim it was not allowed to touch, while leaving the holder the role.
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

			removed, err := reg.Remove(context.Background(), holder, []string{roleTopic, releaseKeptTopic}, tc.authority)
			if err != nil {
				t.Fatalf("release %s's interests: %v", holder, err)
			}

			if tc.frees {
				assertRoleHolder(t, reg, role, "")
				// Both topics the request named are gone, which leaves no record at all,
				// and both are what the release answers it removed.
				assertInterestMissing(t, reg, holder)
				assertRemovedTopics(t, removed, roleTopic, releaseKeptTopic)
				return
			}
			assertRoleHolder(t, reg, role, holder)
			assertInterestTopicsForSession(t, reg, holder, roleTopic)
			// The answer is the topics that left the record, so the kept lane topic is not in
			// it even though the request named it.
			assertRemovedTopics(t, removed, releaseKeptTopic)
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

	removed, err := reg.Remove(context.Background(), holder, []string{roleTopic}, RoleReleaseAuthority{ClientInstanceID: roleClaimSecondInstance})
	if err != nil {
		t.Fatalf("release %s's claim of %s from a second process: %v", holder, role, err)
	}

	assertRoleHolder(t, reg, role, holder)
	assertInterestTopicsForSession(t, reg, holder, releaseKeptTopic, roleTopic)
	// Nothing left the record, so the release answers no topic at all.
	assertRemovedTopics(t, removed)
}

// The whole of the incident, on one session id: a first process takes the role and renews it; it
// stops answering and its renewal lapses; a second process under that same session id takes the
// claim (POST /v1/roles/set, which lists the claimant's own session id as supersedable once
// nobody has renewed the standing claim inside the live window); and then the first process's
// release arrives. The second process keeps the claim, keeps its lane topic, and its own
// registrations go on renewing the claim window after window.
//
// Both halves are read, because either one alone leaves the role half-given-up: a release that
// freed the claim would hand the role to nobody, and a release that took the lane topic out of
// the record would leave the second process holding a role its own record no longer names.
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
	removed, err := reg.Remove(ctx, session, []string{roleTopic}, RoleReleaseAuthority{ClientInstanceID: roleClaimInstance})
	if err != nil {
		t.Fatalf("the first process's release of %s: %v", role, err)
	}
	// The second process holds the claim, so the lane topic stays and the release answers that
	// it removed nothing.
	assertRemovedTopics(t, removed)
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
