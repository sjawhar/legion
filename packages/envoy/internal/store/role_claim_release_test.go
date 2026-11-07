package store

import (
	"context"
	"strings"
	"testing"

	natsgo "github.com/nats-io/nats.go"
	"github.com/sjawhar/envoy/internal/bus"
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
// adopts it (RenewRoleClaims) - keeps the rule it was written under: its holder's own session
// deregistration frees it, on the session match alone. That is the carve-out
// (RoleReleaseAuthority.frees) that keeps every claim standing at the deploy from waiting out
// the role claim reaper, and it closes nothing, since no claim written before this build named
// a process for a release to be weighed against.
func TestAClaimThatNamesNoProcessIsFreedByItsSessionsOwnRelease(t *testing.T) {
	const (
		role   = "legion-controller"
		holder = "ses_releasing_unnamed_process"
	)
	for name, authority := range map[string]RoleReleaseAuthority{
		"a release naming a process":         releasedByItsProcess,
		"a release that names none":          {},
		"the administrator's forced release": {Force: true},
	} {
		t.Run(name, func(t *testing.T) {
			reg := releasableRoleClaim(t, role, holder, "")
			if claim := storedRoleClaim(t, reg, role); claim.ClientInstanceID != "" {
				t.Fatalf("the seeded claim names the process %q, want none", claim.ClientInstanceID)
			}

			if err := reg.Remove(context.Background(), holder, nil, authority); err != nil {
				t.Fatalf("release every interest of %s: %v", holder, err)
			}

			assertRoleHolder(t, reg, role, "")
			assertInterestMissing(t, reg, holder)
		})
	}
}

// RoleReleaseAuthority.frees is the whole rule, and it is asked here record by record: against a
// claim that names the process holding it an absent instance is no match, where
// session.InstanceMatches, written for a soft claim's predecessor, admits one; a claim that
// names no process is freed by its own session's release, whatever process asks; a force frees
// whatever the session holds; and no release reaches a claim of another session.
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
		"a named process against a claim that names none": {authority: releasedByItsProcess, claim: unnamed, sessionID: holder, want: true},
		"no process against a claim that names none":      {authority: RoleReleaseAuthority{}, claim: unnamed, sessionID: holder, want: true},
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

// racingEachDeleteKeyValue runs a hook before each of the first racedDeletes conditional deletes
// reaches the bucket, so the claim each one is conditional on can be moved under it. It is the
// wrapper a release that sends more than one delete needs, where racingDeleteKeyValue (kv_test)
// hooks the first delete alone and answers an error of its own: the hook's own writes go through
// this wrapper too and are not hooked again, and every delete is the real one, so the loss it
// suffers is the server's rather than one this wrapper invented.
type racingEachDeleteKeyValue struct {
	natsgo.KeyValue

	deletes      int
	racedDeletes int
	beforeDelete func()
}

func (kv *racingEachDeleteKeyValue) Delete(key string, opts ...natsgo.DeleteOpt) error {
	kv.deletes++
	if kv.deletes <= kv.racedDeletes {
		kv.beforeDelete()
	}
	return kv.KeyValue.Delete(key, opts...)
}

// raceReleaseWith lands the write wins makes against role under each of the first races
// conditional deletes a release sends, which is the race a release meets between its read of the
// claim and the delete conditional on the revision that read returned. It returns the wrapper,
// whose count of deletes is how many the release sent in all.
func raceReleaseWith(t *testing.T, reg *Registry, role string, races int, wins func()) *racingEachDeleteKeyValue {
	t.Helper()
	racing := &racingEachDeleteKeyValue{KeyValue: reg.roleKV.Legacy(), racedDeletes: races, beforeDelete: wins}
	reg.roleKV = bus.NewKeyValue(racing, reg.roleKV.WatchHandle(), reg.roleKV.Streams())
	return racing
}

// The defect this guards: pi-envoy sends its registration outside the task queue a queued task
// hands a role back in, so the holder's own renewal lands between the release's read of the
// claim and its conditional delete. The delete loses that condition, and a release that read
// nats.ErrKeyExists as "a claim written since is kept" freed nothing and answered success,
// leaving the role with the very process that gave it up. A renewal moves RenewedAt and nothing
// a release weighs, so the release reads the claim again, finds the same session and the same
// process, and deletes the revision that renewal left.
//
// The administrator's forced release takes the same path, over the same renewal.
//
// The release names the role's own lane topic, which is the hand-back a client sends: a
// deregistration of every interest at once would reach the claim twice, through the record's
// topics and then through the role claim view (Registry.releaseAllRoleClaims), and the second
// pass - which reads the claim again for its own reasons - would free what the first one lost.
func TestAReleaseThatRacesItsOwnHoldersRenewalFreesTheClaim(t *testing.T) {
	const (
		role   = "legion-controller"
		holder = "ses_released_under_renewal"
	)
	for name, authority := range map[string]RoleReleaseAuthority{
		"the holder's own process":           releasedByItsProcess,
		"the administrator's forced release": {Force: true},
	} {
		t.Run(name, func(t *testing.T) {
			reg := releasableRoleClaim(t, role, holder, roleClaimInstance)
			racing := raceReleaseWith(t, reg, role, 1, func() {
				if err := reg.renewRoleClaim(context.Background(), role, holder, roleClaimInstance); err != nil {
					t.Errorf("the holder's renewal that wins the race: %v", err)
				}
			})

			if err := reg.Remove(context.Background(), holder, []string{contracts.RoleTopicPrefix + role}, authority); err != nil {
				t.Fatalf("hand %s's role back under its own renewal: %v", holder, err)
			}

			if racing.deletes != 2 {
				t.Fatalf("the release sent %d conditional deletes, want two: the one the renewal beat and the one at the revision it left", racing.deletes)
			}
			assertRoleHolder(t, reg, role, "")
			if claim := storedRoleClaim(t, reg, role); claim.Revision != 0 {
				t.Fatalf("the claim stands at revision %d, want none: the release read the renewal and deleted it", claim.Revision)
			}
		})
	}
}

// A claim another claimant landed under the release's read is not the claim the release was
// authorised over, and it is left exactly as that claimant wrote it: the release frees nothing,
// as it frees nothing of a claim it never authorised, and reports no failure - the session is
// removing its interests either way. The row covers both ways another claim can read: another
// session, and the same session id under a second process.
func TestAReleaseThatRacesAnotherClaimantFreesNothing(t *testing.T) {
	const (
		role    = "legion-controller"
		holder  = "ses_released_under_claim"
		another = "ses_winning_claimant"
	)
	for name, winner := range map[string]struct {
		session  string
		instance string
	}{
		"another session's claim":                {session: another, instance: roleClaimSecondInstance},
		"a second process under the holder's id": {session: holder, instance: roleClaimSecondInstance},
	} {
		t.Run(name, func(t *testing.T) {
			reg := releasableRoleClaim(t, role, holder, roleClaimInstance)
			racing := raceReleaseWith(t, reg, role, 1, func() {
				claimRoleAs(t, reg, winner.session, "example-host", role, winner.instance)
			})

			if err := reg.Remove(context.Background(), holder, []string{contracts.RoleTopicPrefix + role}, releasedByItsProcess); err != nil {
				t.Fatalf("hand %s's role back under another claim: %v", holder, err)
			}

			if racing.deletes != 1 {
				t.Fatalf("the release sent %d conditional deletes, want one: a claim that is not the one read is never deleted", racing.deletes)
			}
			assertRoleHolder(t, reg, role, winner.session)
			if claim := storedRoleClaim(t, reg, role); claim.ClientInstanceID != winner.instance {
				t.Fatalf("the standing claim names the process %q, want the winner's %q", claim.ClientInstanceID, winner.instance)
			}
		})
	}
}

// The bound: a claim renewed under every one of the release's deletes is left standing rather
// than looped on, and the release says so rather than answering success. Three deletes is the
// whole of what one release sends (roleClaimReleaseAttempts), because the loop runs inside an
// HTTP handler and a process registering in a tight loop must not hold a deregistration for as
// long as it keeps writing.
func TestAReleaseLeavesAClaimRenewedUnderEveryDeleteStanding(t *testing.T) {
	const (
		role   = "legion-controller"
		holder = "ses_released_under_every_renewal"
	)
	reg := releasableRoleClaim(t, role, holder, roleClaimInstance)
	racing := raceReleaseWith(t, reg, role, roleClaimReleaseAttempts, func() {
		if err := reg.renewRoleClaim(context.Background(), role, holder, roleClaimInstance); err != nil {
			t.Errorf("the holder's renewal that wins the race: %v", err)
		}
	})

	err := reg.Remove(context.Background(), holder, []string{contracts.RoleTopicPrefix + role}, releasedByItsProcess)

	if err == nil {
		t.Fatalf("the release of a claim renewed under every delete answered success, want the failure that says the claim is still held")
	}
	if !strings.Contains(err.Error(), role) {
		t.Fatalf("the release failed with %v, want an error naming the role %s it left standing", err, role)
	}
	if racing.deletes != roleClaimReleaseAttempts {
		t.Fatalf("the release sent %d conditional deletes, want the %d it is bounded at", racing.deletes, roleClaimReleaseAttempts)
	}
	assertRoleHolder(t, reg, role, holder)
}
