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

	release, err := reg.ReleaseExpiredRoleClaim(context.Background(), role, holder, 0, liveWindow)
	if err != nil || release != ExpiredRoleClaimRenewed {
		t.Fatalf("release a claim renewed inside the live window = %v, %v; want it kept as renewed", release, err)
	}
	assertRoleHolder(t, reg, role, holder)

	reaped, err := reg.ReapRoleClaims(gone, 0, liveWindow)
	if err != nil || reaped != 0 {
		t.Fatalf("the sweep freed %d claims, %v; want none: the holder still renews its claim", reaped, err)
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
		release, err := reg.ReleaseExpiredRoleClaim(context.Background(), role, holder, 0, liveWindow)
		if err != nil || release != ExpiredRoleClaimReleased {
			t.Fatalf("release a claim renewed one live window ago = %v, %v; want it released", release, err)
		}
		assertRoleHolder(t, reg, role, "")
	})
	t.Run("the sweep frees it", func(t *testing.T) {
		const holder = "ses_lapsed_sweep"
		reg, advance := renewedRoleClaim(t, role, holder)
		advance(liveWindow + time.Second)
		reaped, err := reg.ReapRoleClaims(func(string) bool { return false }, 0, liveWindow)
		if err != nil || reaped != 1 {
			t.Fatalf("the sweep freed %d claims, %v; want the one whose holder stopped answering", reaped, err)
		}
		assertRoleHolder(t, reg, role, "")
	})
}
