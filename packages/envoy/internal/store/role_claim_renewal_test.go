package store

import (
	"context"
	"fmt"
	"slices"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	natsgo "github.com/nats-io/nats.go"
	"github.com/sjawhar/envoy/internal/bus"
	"github.com/sjawhar/envoy/internal/contracts"
)

// roleClaimInstance is the client instance a test claim names: the process that holds it, as a
// session registration names one (session.SessionEntry.ClientInstanceID).
const roleClaimInstance = "4f1c2a9e8b7d6c5a4f1c2a9e8b7d6c5a"

// A claim this build writes names the process that holds it and the moment that process last
// answered for it, and both survive the round trip through the bucket. The two stamps a claim
// write makes are one reading of the registry's clock, so a claim is renewed as of the instant it
// lands and LastAnsweredAt reads it there.
func TestAClaimThisBuildWritesNamesItsHoldersProcessAndWhenItLastAnswered(t *testing.T) {
	conn, cleanup := connectNATS(t)
	defer cleanup()

	reg, _ := coldRegistry(t, conn)
	claimed := time.Date(2026, 11, 6, 9, 30, 0, 0, time.UTC)
	reg.now = func() time.Time { return claimed }
	const (
		role   = "legion-controller"
		holder = "ses_claiming_process"
	)
	claim := RoleClaimRequest{SessionID: holder, MachineID: "example-host", ClientInstanceID: roleClaimInstance, Role: role}
	if _, err := reg.SetRoleWithPrevious(context.Background(), claim); err != nil {
		t.Fatalf("claim %s for %s: %v", role, holder, err)
	}

	stored, err := reg.RoleClaim(context.Background(), role)
	if err != nil {
		t.Fatalf("read %s's claim back: %v", role, err)
	}
	if stored.HolderSessionID != holder || stored.ClientInstanceID != roleClaimInstance {
		t.Fatalf("the stored claim is held by %q of instance %q, want %q of %q: a claim names the process that holds it, not the session id alone",
			stored.HolderSessionID, stored.ClientInstanceID, holder, roleClaimInstance)
	}
	if want := claimed.UnixMilli(); stored.ClaimedAt != want || stored.RenewedAt != want {
		t.Fatalf("the stored claim was claimed at %d and renewed at %d, want both %d: one reading of the clock stamps both", stored.ClaimedAt, stored.RenewedAt, want)
	}
	if got := stored.LastAnsweredAt(); got != stored.RenewedAt {
		t.Fatalf("the stored claim last answered at %d, want its renewal %d", got, stored.RenewedAt)
	}
}

// A record that carries no client instance and no renewal is one an earlier build wrote, and it
// is read as it is rather than refused: both fields are zero, and the claim last answered when it
// was claimed, which is what keeps it inside a live window its holder is still inside. The bare
// session id that build also stored carries neither a holder's process nor a time at all.
func TestARecordWithNoProcessOrRenewalDecodesWithBothZeroAndIsReadAtItsClaimTime(t *testing.T) {
	claimed := time.Date(2026, 11, 6, 9, 30, 0, 0, time.UTC).UnixMilli()
	for name, older := range map[string]struct {
		record   string
		answered int64
	}{
		"a structured claim an earlier build wrote": {
			record:   fmt.Sprintf(`{"holder_session_id":"ses_older","claimed_at":%d,"previous_session_id":"ses_oldest"}`, claimed),
			answered: claimed,
		},
		"the bare session id an earlier build stored": {record: "ses_older"},
	} {
		t.Run(name, func(t *testing.T) {
			claim, err := DecodeRoleClaim([]byte(older.record))
			if err != nil {
				t.Fatalf("decode %q: %v; want it read, since a missing field is not a record this build cannot read", older.record, err)
			}
			if claim.HolderSessionID != "ses_older" {
				t.Fatalf("the decoded claim is held by %q, want ses_older", claim.HolderSessionID)
			}
			if claim.ClientInstanceID != "" || claim.RenewedAt != 0 {
				t.Fatalf("the decoded claim names instance %q and renewal %d, want both zero: the record carries neither", claim.ClientInstanceID, claim.RenewedAt)
			}
			if got := claim.LastAnsweredAt(); got != older.answered {
				t.Fatalf("the decoded claim last answered at %d, want %d: a claim with no renewal answered when it was claimed", got, older.answered)
			}
		})
	}
}

// No reader of the role bucket reports a record of either older shape unreadable: the registry's
// own read answers the holder each one names, the watched copy the role lane and RolesHeldBy
// answer from gives that holder the role, and nothing is logged at WARN, which is how a claim
// this build cannot read is reported (roleCache.Apply).
func TestNoReaderOfTheRoleBucketReportsARecordWithNoProcessOrRenewalUnreadable(t *testing.T) {
	logs := captureRegistryLogs(t)
	conn, cleanup := connectNATS(t)
	defer cleanup()
	js, err := conn.JetStream()
	if err != nil {
		t.Fatalf("jetstream: %v", err)
	}
	rawRoles, err := js.CreateKeyValue(&natsgo.KeyValueConfig{Bucket: bus.RoleClaimBucket, Storage: natsgo.FileStorage})
	if err != nil {
		t.Fatalf("create the role bucket: %v", err)
	}
	claimed := time.Date(2026, 11, 6, 9, 30, 0, 0, time.UTC).UnixMilli()
	records := map[string]string{
		"structured": fmt.Sprintf(`{"holder_session_id":"ses_structured","claimed_at":%d,"previous_session_id":""}`, claimed),
		"legacy":     "ses_legacy",
	}
	for role, record := range records {
		if _, err := rawRoles.Put(role, []byte(record)); err != nil {
			t.Fatalf("store %s's record %q: %v", role, record, err)
		}
	}

	reg, err := Open(conn, WithReplicas(1))
	if err != nil {
		t.Fatalf("Open: %v", err)
	}
	t.Cleanup(reg.StopWatch)
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	if err := reg.WaitForCacheReady(ctx); err != nil {
		t.Fatalf("WaitForCacheReady: %v", err)
	}

	for role, holder := range map[string]string{"structured": "ses_structured", "legacy": "ses_legacy"} {
		claim, err := reg.RoleClaim(ctx, role)
		if err != nil {
			t.Fatalf("read %s's claim: %v; want the holder it names", role, err)
		}
		if claim.HolderSessionID != holder {
			t.Fatalf("%s's claim is held by %q, want %q", role, claim.HolderSessionID, holder)
		}
		if got := heldRoles(t, reg, holder); !slices.Equal(got, []string{role}) {
			t.Fatalf("the claim view gives %s the roles %v, want [%s]", holder, got, role)
		}
	}
	if out := logs.String(); strings.Contains(out, "level=WARN") {
		t.Fatalf("a record carrying no client instance and no renewal was reported unreadable:\n%s", out)
	}
}

// renewedRoleClaim gives holder role at a fixed moment, through a registry whose clock the
// returned advance moves that far past the claim: nothing ever registers holder's session, so
// every isAlive these tests pass answers that the holder is gone and the claim's own renewal is
// all that keeps it.
func renewedRoleClaim(t *testing.T, role, holder string) (*Registry, func(time.Duration)) {
	t.Helper()
	conn, cleanup := connectNATS(t)
	t.Cleanup(cleanup)
	reg, _ := coldRegistry(t, conn)
	claimed := time.Date(2026, 11, 6, 9, 30, 0, 0, time.UTC)
	var elapsed atomic.Int64
	reg.now = func() time.Time { return claimed.Add(time.Duration(elapsed.Load())) }
	claim := RoleClaimRequest{SessionID: holder, MachineID: "example-host", ClientInstanceID: roleClaimInstance, Role: role}
	if _, err := reg.SetRoleWithPrevious(context.Background(), claim); err != nil {
		t.Fatalf("claim %s for %s: %v", role, holder, err)
	}
	return reg, func(since time.Duration) { elapsed.Store(int64(since)) }
}

// The incident this guards: a second process resumed the holder's session id and deregistered it,
// so the holder's session record is gone while the holder itself goes on answering for its claim.
// The claim's own renewal is inside the live window, so the sweep keeps the claim whatever the
// session bucket says of its holder - the renewal is the one thing that says the holder is still
// there, and the sweep reads it before it asks anything else (ReapRoleClaims).
func TestAClaimRenewedInsideTheLiveWindowIsKeptThoughItsHoldersSessionRecordIsGone(t *testing.T) {
	const (
		role       = "legion-controller"
		holder     = "ses_renewing_holder"
		liveWindow = time.Minute
	)
	reg, advance := renewedRoleClaim(t, role, holder)
	advance(liveWindow - time.Second)
	gone := func(string) bool { return false }

	// The renewal the sweep weighs, read off the claim as the sweep's own scan carries it.
	claim, err := reg.RoleClaim(context.Background(), role)
	if err != nil {
		t.Fatalf("read %s's claim: %v", role, err)
	}
	if !reg.ClaimRenewedWithin(claim, liveWindow) {
		t.Fatalf("a claim renewed %s ago reads unrenewed within %s; want it renewed", liveWindow-time.Second, liveWindow)
	}

	sweep, err := reg.ReapRoleClaims(context.Background(), gone, 0, liveWindow)
	if err != nil || sweep.Reaped != 0 {
		t.Fatalf("the sweep freed %+v, %v; want none: the holder still renews its claim", sweep, err)
	}
	assertRoleHolder(t, reg, role, holder)
}

// The same claim once its holder has stopped answering: its renewal is as old as the live window,
// the holder's session record is gone, and both the release and a sweep free the role for the
// next claimant.
func TestAClaimWhoseRenewalIsOlderThanTheLiveWindowIsFreed(t *testing.T) {
	const (
		role       = "legion-controller"
		liveWindow = time.Minute
	)
	t.Run("the release frees it", func(t *testing.T) {
		const holder = "ses_lapsed_holder"
		reg, advance := renewedRoleClaim(t, role, holder)
		advance(liveWindow)
		claim, err := reg.RoleClaim(context.Background(), role)
		if err != nil {
			t.Fatalf("read %s's claim to release: %v", role, err)
		}
		release, err := reg.releaseReadExpiredRoleClaim(context.Background(), role, claim, 0)
		if err != nil || release != ExpiredRoleClaimReleased {
			t.Fatalf("release a claim renewed one live window ago = %v, %v; want it released", release, err)
		}
		assertRoleHolder(t, reg, role, "")
	})
	t.Run("the sweep frees it", func(t *testing.T) {
		const holder = "ses_lapsed_sweep"
		reg, advance := renewedRoleClaim(t, role, holder)
		advance(liveWindow + time.Second)
		sweep, err := reg.ReapRoleClaims(context.Background(), func(string) bool { return false }, 0, liveWindow)
		if err != nil || sweep.Reaped != 1 {
			t.Fatalf("the sweep freed %+v, %v; want the one claim whose holder stopped answering", sweep, err)
		}
		assertRoleHolder(t, reg, role, "")
	})
}

// Registry.ClaimRenewedWithin is the one test of a claim's renewal, and the sweep and POST
// /v1/roles/set weighing a claim held under the claimant's own session id both read a claim
// through it, against this registry's own clock: there is one clock for the two of them, and a
// test that moves it moves both readings. A claim an earlier build wrote carries no renewal and
// is weighed at the moment it was claimed, so it is renewed while that moment is inside the
// window and stale once it is older; a window of zero holds nothing renewed, and neither does a
// record carrying no time at all.
func TestClaimRenewedWithinWeighsEveryClaimAgainstTheWindowItIsGiven(t *testing.T) {
	now := time.Date(2026, 11, 6, 9, 30, 0, 0, time.UTC)
	const liveWindow = time.Minute
	at := func(age time.Duration) int64 { return now.Add(-age).UnixMilli() }
	reg := &Registry{now: func() time.Time { return now }}
	for name, tc := range map[string]struct {
		claim      RoleClaim
		liveWindow time.Duration
		want       bool
	}{
		"a claim renewed inside the window":               {claim: RoleClaim{ClaimedAt: at(time.Hour), RenewedAt: at(time.Second)}, liveWindow: liveWindow, want: true},
		"a claim renewed one window ago":                  {claim: RoleClaim{ClaimedAt: at(time.Hour), RenewedAt: at(liveWindow)}, liveWindow: liveWindow},
		"a claim with no renewal, claimed inside it":      {claim: RoleClaim{ClaimedAt: at(time.Second)}, liveWindow: liveWindow, want: true},
		"a claim with no renewal, claimed one window ago": {claim: RoleClaim{ClaimedAt: at(liveWindow)}, liveWindow: liveWindow},
		"a record carrying no time at all":                {claim: RoleClaim{}, liveWindow: liveWindow},
		"a caller that configured no window":              {claim: RoleClaim{ClaimedAt: at(time.Second), RenewedAt: at(time.Second)}},
	} {
		t.Run(name, func(t *testing.T) {
			if got := reg.ClaimRenewedWithin(tc.claim, tc.liveWindow); got != tc.want {
				t.Fatalf("a claim claimed at %d and renewed at %d reads renewed=%t within %s, want %t",
					tc.claim.ClaimedAt, tc.claim.RenewedAt, got, tc.liveWindow, tc.want)
			}
		})
	}
}

// roleClaimSecondInstance is the second process one session id can be claimed under: another
// client instance that resumed the session the claim names, which a renewal must not let move
// the holder's claim.
const roleClaimSecondInstance = "9b8a7c6d5e4f3a2b9b8a7c6d5e4f3a2b"

// roleClaimPredecessor is the session a seeded claim continues, carried on the claim as
// provenance: a renewal leaves it exactly as the claim that took the role left it.
const roleClaimPredecessor = "ses_renewal_predecessor"

// renewableRoleClaim opens a registry whose role claim view answers, since RenewRoleClaims takes
// the roles it renews from that view, and gives holder role through the process instance names.
// The claim is a soft one continuing a predecessor, so what a renewal leaves alone - the holder,
// the moment the role was taken, and whom it was taken from - is all on the record to read back.
// The returned advance moves the registry's clock that far past the claim, which is where every
// renewal these cases make lands.
func renewableRoleClaim(t *testing.T, role, holder, instance string) (*Registry, func(time.Duration)) {
	t.Helper()
	conn, cleanup := connectNATS(t)
	t.Cleanup(cleanup)
	claimed := time.Date(2026, 11, 6, 9, 30, 0, 0, time.UTC)
	var elapsed atomic.Int64
	reg, err := Open(conn, WithReplicas(1), WithClock(func() time.Time { return claimed.Add(time.Duration(elapsed.Load())) }))
	if err != nil {
		t.Fatalf("Open: %v", err)
	}
	t.Cleanup(reg.StopWatch)
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	if err := reg.WaitForCacheReady(ctx); err != nil {
		t.Fatalf("WaitForCacheReady: %v", err)
	}
	claim := RoleClaimRequest{
		SessionID:         holder,
		MachineID:         "example-host",
		ClientInstanceID:  instance,
		Role:              role,
		PreviousSessionID: roleClaimPredecessor,
		Soft:              true,
	}
	if _, err := reg.SetRoleWithPrevious(ctx, claim); err != nil {
		t.Fatalf("claim %s for %s: %v", role, holder, err)
	}
	return reg, func(since time.Duration) { elapsed.Store(int64(since)) }
}

// storedRoleClaim reads role's claim back from the bucket, with the revision the record stands at.
func storedRoleClaim(t *testing.T, reg *Registry, role string) RoleClaim {
	t.Helper()
	claim, err := reg.RoleClaim(context.Background(), role)
	if err != nil {
		t.Fatalf("read %s's claim: %v", role, err)
	}
	return claim
}

// A registration by the very process a claim names moves that claim's renewal to the moment it
// registered, which is what makes RenewedAt the holder's heartbeat rather than the instant the
// role was taken. Everything else on the record is the claim's own provenance and is left as the
// claim that took the role left it.
func TestARenewalFromTheProcessTheClaimNamesMovesTheRenewalAndNothingElse(t *testing.T) {
	const (
		role     = "legion-controller"
		holder   = "ses_renewing_holder"
		beat     = 2 * time.Minute
		instance = roleClaimInstance
	)
	reg, advance := renewableRoleClaim(t, role, holder, instance)
	claimed := storedRoleClaim(t, reg, role)
	advance(beat)

	if err := reg.RenewRoleClaims(context.Background(), holder, instance); err != nil {
		t.Fatalf("renew %s's role claims: %v", holder, err)
	}

	renewed := storedRoleClaim(t, reg, role)
	if want := claimed.ClaimedAt + beat.Milliseconds(); renewed.RenewedAt != want {
		t.Fatalf("the renewed claim was renewed at %d, want %d: the registration's own moment", renewed.RenewedAt, want)
	}
	if renewed.HolderSessionID != holder || renewed.ClaimedAt != claimed.ClaimedAt || renewed.PreviousSessionID != roleClaimPredecessor {
		t.Fatalf("the renewed claim is %+v, want %s's claim of %d continuing %s: a renewal writes the renewal alone",
			renewed, holder, claimed.ClaimedAt, roleClaimPredecessor)
	}
	if renewed.ClientInstanceID != instance {
		t.Fatalf("the renewed claim names the process %q, want %q", renewed.ClientInstanceID, instance)
	}
	if renewed.Revision == claimed.Revision {
		t.Fatalf("the claim still stands at revision %d: the renewal never reached the bucket", renewed.Revision)
	}
	if !slices.Equal(heldRoles(t, reg, holder), []string{role}) {
		t.Fatalf("the claim view no longer gives %s the role %s after its renewal", holder, role)
	}
}

// A claim written before claims named a process names none, and the first registration of the
// session holding it writes the registering process in: the adoption the live window bounds at
// one heartbeat. It is still the same claim, taken at the same moment from the same predecessor,
// so nothing but the process and the renewal is written.
func TestTheFirstRenewalOfAClaimThatNamesNoProcessAdoptsItForThatProcess(t *testing.T) {
	const (
		role   = "legion-controller"
		holder = "ses_adopting_holder"
		beat   = 2 * time.Minute
	)
	reg, advance := renewableRoleClaim(t, role, holder, "")
	claimed := storedRoleClaim(t, reg, role)
	if claimed.ClientInstanceID != "" {
		t.Fatalf("the seeded claim names the process %q, want none: this case is the claim an earlier build wrote", claimed.ClientInstanceID)
	}
	advance(beat)

	if err := reg.RenewRoleClaims(context.Background(), holder, roleClaimInstance); err != nil {
		t.Fatalf("renew %s's role claims: %v", holder, err)
	}

	adopted := storedRoleClaim(t, reg, role)
	if adopted.ClientInstanceID != roleClaimInstance {
		t.Fatalf("the adopted claim names the process %q, want %q: the first registration of its holder adopts it", adopted.ClientInstanceID, roleClaimInstance)
	}
	if want := claimed.ClaimedAt + beat.Milliseconds(); adopted.RenewedAt != want {
		t.Fatalf("the adopted claim was renewed at %d, want %d", adopted.RenewedAt, want)
	}
	if adopted.HolderSessionID != holder || adopted.ClaimedAt != claimed.ClaimedAt || adopted.PreviousSessionID != roleClaimPredecessor {
		t.Fatalf("the adopted claim is %+v, want %s's claim of %d continuing %s: adoption writes the process alone",
			adopted, holder, claimed.ClaimedAt, roleClaimPredecessor)
	}
}

// A renewal is the holder's own heartbeat and moves nothing else: not a claim a second process
// that resumed the holder's session id registers under, not a claim naming a process when the
// registration states none - an absent instance names no process, so it matches no claim that
// names one and adopts none either - and not a claim of a session the registration is not for.
func TestARenewalMovesNoClaimTheRegisteringProcessDoesNotHold(t *testing.T) {
	const (
		role   = "legion-controller"
		holder = "ses_held_claim"
		beat   = 2 * time.Minute
	)
	for name, tc := range map[string]struct {
		claimInstance string
		session       string
		instance      string
	}{
		"a second process that resumed the holder's session id": {claimInstance: roleClaimInstance, session: holder, instance: roleClaimSecondInstance},
		"a registration that states no process":                 {claimInstance: roleClaimInstance, session: holder},
		"a registration of a session the claim does not name":   {claimInstance: roleClaimInstance, session: "ses_another_holder", instance: roleClaimInstance},
	} {
		t.Run(name, func(t *testing.T) {
			reg, advance := renewableRoleClaim(t, role, holder, tc.claimInstance)
			claimed := storedRoleClaim(t, reg, role)
			advance(beat)

			if err := reg.RenewRoleClaims(context.Background(), tc.session, tc.instance); err != nil {
				t.Fatalf("a renewal by %s of process %q: %v; want it to pass the claim over, not fail", tc.session, tc.instance, err)
			}

			if stored := storedRoleClaim(t, reg, role); stored != claimed {
				t.Fatalf("the claim is now %+v, want it exactly as it stood, %+v", stored, claimed)
			}
		})
	}
}

// A renewal that loses its conditional write lost it to a claim that landed after the renewal
// read the record: the record is that claimant's now, so the renewal is dropped where it stands.
// It is never read again and written into the claim that won, which would stamp this process's
// heartbeat on a claim it does not hold, and the registration that made it is not failed.
func TestARenewalThatLosesItsCompareAndSwapWritesNothingAndIsNotRetried(t *testing.T) {
	const (
		role   = "legion-controller"
		holder = "ses_raced_holder"
		winner = "ses_winning_claimant"
		beat   = 2 * time.Minute
	)
	reg, advance := renewableRoleClaim(t, role, holder, roleClaimInstance)
	advance(beat)
	racing := &racingUpdateKeyValue{KeyValue: reg.roleKV.Legacy()}
	racing.beforeFirstUpdate = func() {
		// A hard claim writes unconditionally, so it lands under the renewal's read and
		// the renewal's own update meets the bucket's refusal, not one this hook invented.
		winning := RoleClaimRequest{SessionID: winner, MachineID: "example-host", ClientInstanceID: roleClaimSecondInstance, Role: role}
		if _, err := reg.SetRoleWithPrevious(context.Background(), winning); err != nil {
			t.Errorf("the claim that wins the race: %v", err)
		}
	}
	reg.roleKV = bus.NewKeyValue(racing, reg.roleKV.WatchHandle(), reg.roleKV.Streams())

	if err := reg.RenewRoleClaims(context.Background(), holder, roleClaimInstance); err != nil {
		t.Fatalf("a renewal that lost its compare-and-swap: %v; want it dropped, not reported", err)
	}

	if racing.updates != 1 {
		t.Fatalf("the renewal made %d conditional updates, want exactly one: a lost renewal is never retried", racing.updates)
	}
	stored := storedRoleClaim(t, reg, role)
	if stored.HolderSessionID != winner || stored.ClientInstanceID != roleClaimSecondInstance {
		t.Fatalf("the claim is held by %q of process %q, want the winner %q of %q: the renewal wrote into the claim that landed",
			stored.HolderSessionID, stored.ClientInstanceID, winner, roleClaimSecondInstance)
	}
	if stored.RenewedAt != stored.ClaimedAt {
		t.Fatalf("the winning claim was claimed at %d and renewed at %d, want its own single stamp: the renewal moved a claim it does not hold",
			stored.ClaimedAt, stored.RenewedAt)
	}
}

// The hand-back a client built before a release named its process sends: it names the role's
// own lane topic and states no process, so it frees no claim (RoleReleaseAuthority.frees) and
// marks the claim handed back instead. The defect this guards is what followed such a release
// before the mark: the holder went on renewing the claim for as long as its process ran, so the
// role stayed with the very process that gave it up. No registration renews a marked claim,
// whoever sends it, so the renewal stands where the hand-back left it and the role claim reaper
// frees the role at its first pass past the live window.
//
// The client that handed the role back is still running and still registering, so the sweep is
// given an isAlive that answers true for its session throughout: a role handed back comes free
// a live window later, at the sweep that follows, while its old holder lives, which is the
// whole of what the mark buys.
// A row that asked the sweep over a holder the session bucket had lost would pass with the
// reaper reading that liveness and the hand-back doing nothing. The control is the heartbeat
// before the hand-back, which renews this very claim through this very call: without it a case
// that proved nothing - a renewal that never happened at all - would read as this defect fixed.
func TestNoRegistrationRenewsAClaimItsHolderHandedBack(t *testing.T) {
	const (
		role       = "legion-controller"
		holder     = "ses_handing_back"
		liveWindow = time.Minute
		beat       = 20 * time.Second
	)
	reg, advance := renewableRoleClaim(t, role, holder, roleClaimInstance)
	// The handing-back client goes on running, and goes on registering the session id it
	// handed the role back under (the registrations below), so its session is alive to every
	// reader for as long as this case runs.
	alive := func(string) bool { return true }

	// The control: before the hand-back the holder's registration renews the claim.
	advance(beat)
	if err := reg.RenewRoleClaims(context.Background(), holder, roleClaimInstance); err != nil {
		t.Fatalf("renew %s's role claims before the hand-back: %v", holder, err)
	}
	renewed := storedRoleClaim(t, reg, role)
	if renewed.RenewedAt != renewed.ClaimedAt+beat.Milliseconds() {
		t.Fatalf("the claim was renewed at %d, want the registration's own moment %d", renewed.RenewedAt, renewed.ClaimedAt+beat.Milliseconds())
	}

	if _, err := reg.Remove(context.Background(), holder, []string{contracts.RoleTopicPrefix + role}, RoleReleaseAuthority{}); err != nil {
		t.Fatalf("hand %s's role back from a release naming no process: %v", role, err)
	}
	marked := storedRoleClaim(t, reg, role)
	if !marked.HandedBack || marked.HolderSessionID != holder || marked.ClientInstanceID != roleClaimInstance {
		t.Fatalf("the claim after the hand-back is %+v, want %s's claim of process %q marked handed back", marked, holder, roleClaimInstance)
	}

	// Every heartbeat the holder's process sends after it, and every one that states no process.
	for _, since := range []time.Duration{2 * beat, liveWindow, liveWindow + beat} {
		advance(since)
		for _, instance := range []string{roleClaimInstance, ""} {
			if err := reg.RenewRoleClaims(context.Background(), holder, instance); err != nil {
				t.Fatalf("a registration %s after the hand-back by process %q: %v; want it to renew nothing, not fail", since, instance, err)
			}
			if stood := storedRoleClaim(t, reg, role); stood != marked {
				t.Fatalf("the claim %s after the hand-back, registered by process %q, is %+v, want it exactly as the hand-back left it, %+v",
					since, instance, stood, marked)
			}
		}
	}

	// A live window past that last renewal, the claim is nobody's live hold and the sweep frees
	// the role, though the process that handed it back is still there and still registering.
	advance(2 * liveWindow)
	sweep, err := reg.ReapRoleClaims(context.Background(), alive, 0, liveWindow)
	if err != nil || sweep.Reaped != 1 {
		t.Fatalf("the sweep a live window after the hand-back freed %+v, %v; want the one claim nobody answers for", sweep, err)
	}
	assertRoleHolder(t, reg, role, "")
}

// A registration that states no process renews the claims of its own session that name none, and
// leaves them naming none: a claim standing at a deploy is kept by the holder that took it, and
// no claim ages out for want of a registration able to say which process it comes from. It adopts
// nothing, which is the other half of the rule the rows above hold.
func TestARegistrationThatStatesNoProcessRenewsTheClaimsThatNameNone(t *testing.T) {
	const (
		role   = "legion-controller"
		holder = "ses_registering_no_process"
		beat   = 2 * time.Minute
	)
	reg, advance := renewableRoleClaim(t, role, holder, "")
	claimed := storedRoleClaim(t, reg, role)
	advance(beat)

	if err := reg.RenewRoleClaims(context.Background(), holder, ""); err != nil {
		t.Fatalf("renew %s's role claims from a registration stating no process: %v", holder, err)
	}

	renewed := storedRoleClaim(t, reg, role)
	if want := claimed.ClaimedAt + beat.Milliseconds(); renewed.RenewedAt != want {
		t.Fatalf("the claim was renewed at %d, want the registration's own moment %d: a registration stating no process renews the claims that name none",
			renewed.RenewedAt, want)
	}
	if renewed.ClientInstanceID != "" {
		t.Fatalf("the renewed claim names the process %q, want none: a registration that cannot say which process it is adopts no claim", renewed.ClientInstanceID)
	}
	if renewed.HolderSessionID != holder || renewed.ClaimedAt != claimed.ClaimedAt || renewed.PreviousSessionID != roleClaimPredecessor {
		t.Fatalf("the renewed claim is %+v, want %s's claim of %d continuing %s: a renewal writes the renewal alone",
			renewed, holder, claimed.ClaimedAt, roleClaimPredecessor)
	}
}

// The holder that did not hand its role back keeps it however long it runs: every registration of
// the process the claim names renews the claim, and no sweep inside the live window of that
// renewal frees the role. It is the control the hand-back case is read against - a renewal that
// stopped for any other reason would make that case pass for the wrong reason.
func TestAHoldersRegistrationsKeepItsClaimRenewedWhileItRuns(t *testing.T) {
	const (
		role       = "legion-controller"
		holder     = "ses_still_holding"
		liveWindow = time.Minute
		beat       = 20 * time.Second
	)
	reg, advance := renewableRoleClaim(t, role, holder, roleClaimInstance)
	gone := func(string) bool { return false }

	for since := beat; since < 2*liveWindow; since += beat {
		advance(since)
		if err := reg.RenewRoleClaims(context.Background(), holder, roleClaimInstance); err != nil {
			t.Fatalf("renew %s's role claims %s after the claim: %v", holder, since, err)
		}
		if renewed := storedRoleClaim(t, reg, role); renewed.RenewedAt != renewed.ClaimedAt+since.Milliseconds() {
			t.Fatalf("the claim %s after it was taken was renewed at %d, want %d: the holder's own registration renews it",
				since, renewed.RenewedAt, renewed.ClaimedAt+since.Milliseconds())
		}
		sweep, err := reg.ReapRoleClaims(context.Background(), gone, 0, liveWindow)
		if err != nil || sweep.Reaped != 0 {
			t.Fatalf("the sweep %s after the claim freed %+v, %v; want none: the holder answered for it this beat", since, sweep, err)
		}
		assertRoleHolder(t, reg, role, holder)
	}
}
