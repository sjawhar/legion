package store

import (
	"context"
	"testing"

	"github.com/sjawhar/envoy/internal/contracts"
)

// releasedByItsProcess is the release POST /v1/interests/unsubscribe makes for the process that
// holds the claims these tests seed (roleClaimInstance): the holder's own instance and no force,
// which is the only release of that shape that frees them.
var releasedByItsProcess = RoleReleaseAuthority{ClientInstanceID: roleClaimInstance}

// claimRoleAs gives sessionID role through the process instance names, as a client's own claim
// does (SetRoleWithPrevious carries the claimant's client instance), so the stored claim says
// which process holds the role and a release can be weighed against it.
func claimRoleAs(t *testing.T, reg *Registry, sessionID, machineID, role, instance string) {
	t.Helper()
	if _, err := reg.SetRoleWithPrevious(context.Background(), sessionID, machineID, instance, role, "", false, nil); err != nil {
		t.Fatalf("claim %s for %s as %q: %v", role, sessionID, instance, err)
	}
}

// releasableRoleClaim opens a registry over an empty pair of buckets, gives holder role through
// the process instance names, and leaves holder's interest record carrying that role's lane
// topic beside a topic of its own, so a release can be read on both halves at once.
func releasableRoleClaim(t *testing.T, role, holder, instance string) *Registry {
	t.Helper()
	conn, cleanup := connectNATS(t)
	t.Cleanup(cleanup)
	reg, _ := coldRegistry(t, conn)
	if _, err := reg.Upsert(Interest{SessionID: holder, MachineID: "example-host"}, []string{releaseKeptTopic}); err != nil {
		t.Fatalf("register %s's interests: %v", holder, err)
	}
	claimRoleAs(t, reg, holder, "example-host", role, instance)
	assertRoleHolder(t, reg, role, holder)
	assertInterestTopicsForSession(t, reg, holder, releaseKeptTopic, contracts.RoleTopicPrefix+role)
	return reg
}

// releaseKeptTopic is a topic of the holder's own, neither a role lane nor named by any release
// below: the record must still carry it wherever the record survives.
const releaseKeptTopic = "notifications.team.release"

// A release frees a role claim only for the process the claim names. The holder's own process
// frees it, as an administrator's force does; a second process that resumed the holder's session
// id frees nothing, and neither does a release that names no process at all - a session id names
// no process, so those two releases are indistinguishable from the holder's own but for the
// instance they carry.
//
// The interest half is the same work in every case: the topics named are removed, and an empty
// topics deletes the record. Only the claim is scoped to the process.
func TestAReleaseFreesARoleClaimOnlyForTheProcessTheClaimNames(t *testing.T) {
	const (
		role   = "legion-controller"
		holder = "ses_releasing_holder"
	)
	roleTopic := contracts.RoleTopicPrefix + role
	for name, release := range map[string]struct {
		authority RoleReleaseAuthority
		frees     bool
	}{
		"the process the claim names":        {authority: releasedByItsProcess, frees: true},
		"a second process of that session":   {authority: RoleReleaseAuthority{ClientInstanceID: roleClaimSecondInstance}},
		"a release that names no process":    {authority: RoleReleaseAuthority{}},
		"the administrator's forced release": {authority: RoleReleaseAuthority{Force: true}, frees: true},
	} {
		t.Run(name, func(t *testing.T) {
			t.Run("the role's own topic", func(t *testing.T) {
				reg := releasableRoleClaim(t, role, holder, roleClaimInstance)
				if err := reg.Remove(context.Background(), holder, []string{roleTopic}, release.authority); err != nil {
					t.Fatalf("release %s's claim of %s: %v", holder, role, err)
				}
				wantHolder := holder
				if release.frees {
					wantHolder = ""
				}
				assertRoleHolder(t, reg, role, wantHolder)
				assertInterestTopicsForSession(t, reg, holder, releaseKeptTopic)
			})
			t.Run("every topic the session has", func(t *testing.T) {
				reg := releasableRoleClaim(t, role, holder, roleClaimInstance)
				if err := reg.Remove(context.Background(), holder, nil, release.authority); err != nil {
					t.Fatalf("release every interest of %s: %v", holder, err)
				}
				wantHolder := holder
				if release.frees {
					wantHolder = ""
				}
				assertRoleHolder(t, reg, role, wantHolder)
				assertInterestMissing(t, reg, holder)
			})
		})
	}
}

// A claim of another session is no business of this release, whatever process asks: the holder's
// session id is the first thing weighed, and the process is weighed only after it.
func TestAReleaseFreesNoClaimOfAnotherSession(t *testing.T) {
	const (
		role     = "legion-controller"
		holder   = "ses_releasing_other_holder"
		leaving  = "ses_releasing_leaver"
		leavings = "notifications.team.leaving"
	)
	reg := releasableRoleClaim(t, role, holder, roleClaimInstance)
	if _, err := reg.Upsert(Interest{SessionID: leaving, MachineID: "example-host"}, []string{leavings}); err != nil {
		t.Fatalf("register %s's interests: %v", leaving, err)
	}

	if err := reg.Remove(context.Background(), leaving, nil, releasedByItsProcess); err != nil {
		t.Fatalf("release every interest of %s: %v", leaving, err)
	}

	assertRoleHolder(t, reg, role, holder)
	assertInterestMissing(t, reg, leaving)
	assertInterestTopicsForSession(t, reg, holder, releaseKeptTopic, contracts.RoleTopicPrefix+role)
}

// A claim that names no process - one an earlier build wrote, before its holder's registration
// adopts it (RenewRoleClaims) - is matched by no instance at all, so only the administrator's
// forced release frees it. That is the release DELETE /v1/interests/<session_id> makes, and the
// reason it still forces one.
func TestOnlyAForcedReleaseFreesAClaimThatNamesNoProcess(t *testing.T) {
	const (
		role   = "legion-controller"
		holder = "ses_releasing_unnamed_process"
	)
	for name, tc := range map[string]struct {
		authority RoleReleaseAuthority
		frees     bool
	}{
		"a release naming a process":         {authority: releasedByItsProcess},
		"the administrator's forced release": {authority: RoleReleaseAuthority{Force: true}, frees: true},
	} {
		t.Run(name, func(t *testing.T) {
			reg := releasableRoleClaim(t, role, holder, "")
			if claim := storedRoleClaim(t, reg, role); claim.ClientInstanceID != "" {
				t.Fatalf("the seeded claim names the process %q, want none", claim.ClientInstanceID)
			}

			if err := reg.Remove(context.Background(), holder, nil, tc.authority); err != nil {
				t.Fatalf("release every interest of %s: %v", holder, err)
			}

			wantHolder := holder
			if tc.frees {
				wantHolder = ""
			}
			assertRoleHolder(t, reg, role, wantHolder)
			assertInterestMissing(t, reg, holder)
		})
	}
}

// RoleReleaseAuthority.frees is the whole rule, and it is asked here record by record: an absent
// instance is no match on either side, where session.InstanceMatches, written for a soft claim's
// predecessor, admits one; a force frees whatever the session holds; and no release reaches a
// claim of another session.
func TestWhichClaimsAReleaseAuthorityFrees(t *testing.T) {
	const (
		holder = "ses_frees_holder"
		other  = "ses_frees_other"
	)
	named := RoleClaim{HolderSessionID: holder, ClientInstanceID: roleClaimInstance}
	unnamed := RoleClaim{HolderSessionID: holder}
	for name, tc := range map[string]struct {
		authority RoleReleaseAuthority
		claim     RoleClaim
		sessionID string
		want      bool
	}{
		"the process the claim names":                     {authority: releasedByItsProcess, claim: named, sessionID: holder, want: true},
		"another process of the claim's session":          {authority: RoleReleaseAuthority{ClientInstanceID: roleClaimSecondInstance}, claim: named, sessionID: holder},
		"a release that names no process":                 {authority: RoleReleaseAuthority{}, claim: named, sessionID: holder},
		"a named process against a claim that names none": {authority: releasedByItsProcess, claim: unnamed, sessionID: holder},
		"no process against a claim that names none":      {authority: RoleReleaseAuthority{}, claim: unnamed, sessionID: holder},
		"a force against the claim's session":             {authority: RoleReleaseAuthority{Force: true}, claim: named, sessionID: holder, want: true},
		"a force against a claim that names no process":   {authority: RoleReleaseAuthority{Force: true}, claim: unnamed, sessionID: holder, want: true},
		"a force against another session's claim":         {authority: RoleReleaseAuthority{Force: true}, claim: named, sessionID: other},
		"the claim's own process, another session":        {authority: releasedByItsProcess, claim: named, sessionID: other},
	} {
		t.Run(name, func(t *testing.T) {
			if got := tc.authority.frees(tc.claim, tc.sessionID); got != tc.want {
				t.Fatalf("a release %+v frees %+v from %s = %t, want %t", tc.authority, tc.claim, tc.sessionID, got, tc.want)
			}
		})
	}
}

// A release the authority does not admit leaves the stored claim exactly as it stands - the same
// record at the same revision, not a claim rewritten with the same holder - so the holder's own
// renewal and provenance survive a second process's hand-back untouched.
func TestARefusedReleaseLeavesTheStoredClaimAtItsOwnRevision(t *testing.T) {
	const (
		role   = "legion-controller"
		holder = "ses_releasing_untouched"
	)
	reg := releasableRoleClaim(t, role, holder, roleClaimInstance)
	before := storedRoleClaim(t, reg, role)

	if err := reg.Remove(context.Background(), holder, nil, RoleReleaseAuthority{ClientInstanceID: roleClaimSecondInstance}); err != nil {
		t.Fatalf("release every interest of %s: %v", holder, err)
	}

	if after := storedRoleClaim(t, reg, role); after != before {
		t.Fatalf("the claim is now %+v, want it exactly as it stood, %+v", after, before)
	}
}
