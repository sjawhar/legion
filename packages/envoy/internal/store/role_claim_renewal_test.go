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
	if _, err := reg.SetRoleWithPrevious(context.Background(), holder, "example-host", roleClaimInstance, role, "", false, nil); err != nil {
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
	if _, err := reg.SetRoleWithPrevious(context.Background(), holder, "example-host", roleClaimInstance, role, "", false, nil); err != nil {
		t.Fatalf("claim %s for %s: %v", role, holder, err)
	}
	return reg, func(since time.Duration) { elapsed.Store(int64(since)) }
}

// The incident this guards: a second process resumed the holder's session id and deregistered it,
// so the holder's session record is gone while the holder itself goes on answering for its claim.
// The claim's own renewal is inside the live window, so neither the release nor a sweep frees it,
// and the release says which window kept it.
func TestAClaimRenewedInsideTheLiveWindowIsKeptThoughItsHoldersSessionRecordIsGone(t *testing.T) {
	const (
		role       = "legion-controller"
		holder     = "ses_renewing_holder"
		liveWindow = time.Minute
	)
	reg, advance := renewedRoleClaim(t, role, holder)
	advance(liveWindow - time.Second)
	gone := func(string) bool { return false }

	// The release is handed the claim its caller read, as the reaper's pass hands it the claim
	// it read to find the holder gone (ReapRoleClaims).
	claim, err := reg.RoleClaim(context.Background(), role)
	if err != nil {
		t.Fatalf("read %s's claim to release: %v", role, err)
	}
	release, err := reg.releaseReadExpiredRoleClaim(context.Background(), role, claim, 0, liveWindow)
	if err != nil || release != ExpiredRoleClaimRenewed {
		t.Fatalf("release a claim renewed inside the live window = %v, %v; want it kept as renewed", release, err)
	}
	assertRoleHolder(t, reg, role, holder)

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
		release, err := reg.releaseReadExpiredRoleClaim(context.Background(), role, claim, 0, liveWindow)
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

// RoleClaimRenewedWithin is the one test of a claim's renewal, and the release, the sweep and
// POST /v1/roles/set weighing a claim held under the claimant's own session id all read a claim
// through it, so it is asked here as a caller outside this package asks it: against a clock the
// caller names. A claim an earlier build wrote carries no renewal and is weighed at the moment
// it was claimed, so it is renewed while that moment is inside the window and stale once it is
// older; a window of zero holds nothing renewed, and neither does a record carrying no time at
// all.
func TestRoleClaimRenewedWithinWeighsEveryClaimAgainstTheWindowItIsGiven(t *testing.T) {
	now := time.Date(2026, 11, 6, 9, 30, 0, 0, time.UTC)
	const liveWindow = time.Minute
	at := func(age time.Duration) int64 { return now.Add(-age).UnixMilli() }
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
			if got := RoleClaimRenewedWithin(tc.claim, now, tc.liveWindow); got != tc.want {
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
	if _, err := reg.SetRoleWithPrevious(ctx, holder, "example-host", instance, role, roleClaimPredecessor, true, nil); err != nil {
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
// that resumed the holder's session id registers under, not one the holder's session registers
// without saying which process it is - an absent instance names no process, so it matches no
// claim and adopts none either - and not a claim of a session the registration is not for.
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
		"a second process that resumed the holder's session id":             {claimInstance: roleClaimInstance, session: holder, instance: roleClaimSecondInstance},
		"a registration that states no process":                             {claimInstance: roleClaimInstance, session: holder},
		"a registration that states no process, on a claim that names none": {session: holder},
		"a registration of a session the claim does not name":               {claimInstance: roleClaimInstance, session: "ses_another_holder", instance: roleClaimInstance},
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
		if _, err := reg.SetRoleWithPrevious(context.Background(), winner, "example-host", roleClaimSecondInstance, role, "", false, nil); err != nil {
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
