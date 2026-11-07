// Which claims a deregistration sweep may release, and which it must leave exactly as they
// stand. The roles it releases come from the role claim view, a copy of the bucket that is at
// best a moment behind it, and every record that copy names carries a holder and a process, so
// three kinds of claim are none of the sweep's to take: one another session holds, one another
// session takes between the scan and the delete, and one another process holds under the very
// session id being deregistered (RoleReleaseAuthority). The cases here are one per kind.
//
// What each turns on is the holder and the revision the bucket ends with, never a duration,
// because those are what taking a claim that was not this sweep's would destroy: an
// unconditional delete would hand the role to nobody, and a release that weighed no process
// would free the roles of a lane whose session id a second process merely resumed. The fixtures
// these cases run over are role_claim_sweep_test.go's, beside the cases about what a sweep costs
// and what it makes of a call that failed.

package store

import (
	"context"
	"sync"
	"testing"
	"time"

	natsgo "github.com/nats-io/nats.go"
	"github.com/sjawhar/envoy/internal/bus"
	"github.com/sjawhar/envoy/internal/contracts"
	"github.com/sjawhar/envoy/internal/kvwatch"
)

// Check v3. The roles the sweep releases come from a copy of the bucket, which is at best a
// moment behind it, so the sweep must never take the copy's word for who holds a role: every
// release it makes is conditional on the very revision it has just read, and a claim that is no
// longer the session's is left alone. Three cases, one per way the copy and the bucket can
// disagree, each with the watcher's delivery of the role gated (gatedRoleClaims) so that what the
// view holds is exactly what this registry's own writes put there and nothing the watcher
// delivered later can mask the disagreement.
//
// Each session's record carries a topic that is not the role's, so the pass over the record
// releases nothing and the claim's fate is the view sweep's alone (removeInterestTopics takes the
// role topic SetRole added back off the record, releasing nothing).
func TestTheDeregistrationSweepLeavesEveryClaimAnotherSessionHolds(t *testing.T) {
	conn, cleanup := connectNATS(t)
	defer cleanup()
	js, err := conn.JetStream()
	if err != nil {
		t.Fatalf("jetstream: %v", err)
	}
	if _, err := js.CreateKeyValue(&natsgo.KeyValueConfig{Bucket: bus.InterestBucket, Replicas: 1, Storage: natsgo.FileStorage}); err != nil {
		t.Fatalf("create the interest bucket: %v", err)
	}
	rawRoles, err := js.CreateKeyValue(&natsgo.KeyValueConfig{Bucket: bus.RoleClaimBucket, Replicas: 1, Storage: natsgo.FileStorage})
	if err != nil {
		t.Fatalf("create the role bucket: %v", err)
	}
	reg, err := Open(conn, WithReplicas(1))
	if err != nil {
		t.Fatalf("Open: %v", err)
	}
	t.Cleanup(reg.StopWatch)
	ready, cancelReady := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancelReady()
	if err := reg.WaitForCacheReady(ready); err != nil {
		t.Fatalf("WaitForCacheReady: %v", err)
	}

	const (
		ownWrite     = "sweep-own-write"
		takenBefore  = "sweep-taken-before"
		takenDuring  = "sweep-taken-during"
		otherHolder  = "ses_sweep_other_holder"
		recordsTopic = "notifications.chat"
	)
	// The watcher holds its delivery of these three roles for good, so no delivery of them ever
	// reaches the view: none of them is in the bucket yet, so the read that makes the view
	// servable does not meet the gate. From here the view's copy of each is whatever this
	// registry's own claim writes put there (setOwnRoleHolder), which is the state a listener is
	// in between a claim's write and the watcher's echo of it.
	gated := map[string]bool{ownWrite: true, takenBefore: true, takenDuring: true}
	gate := make(chan struct{})
	var openOnce sync.Once
	open := func() { openOnce.Do(func() { close(gate) }) }
	t.Cleanup(open)
	reg.roleWatcher.Stop()
	reg.roleWatcher = kvwatch.New("role claims", reg.roles(), gatedRoleClaims{r: reg, hold: func(key string) {
		if gated[key] {
			<-gate
		}
	}})
	reg.roleWatcher.Start()
	waitFor(t, 30*time.Second, func() bool { return servableView(reg) })

	// claimAndDropTheTopic has session claim role through the registry, so the claim reaches the
	// view through the write rather than the watcher, and then takes the role's topic back off
	// session's record, leaving a record that carries one topic that is not a role's. What
	// releases the claim after that can only be the view sweep.
	claimAndDropTheTopic := func(t *testing.T, session, role string) {
		t.Helper()
		if _, err := reg.Upsert(Interest{SessionID: session, MachineID: "m1"}, []string{recordsTopic}); err != nil {
			t.Fatalf("store %s's interest record: %v", session, err)
		}
		if _, err := reg.SetRole(session, "m1", role, false); err != nil {
			t.Fatalf("claim %s for %s: %v", role, session, err)
		}
		if holder := holderOf(reg, role); holder != session {
			t.Fatalf("the view names %q the holder of %s after %s's own claim write, want %s", holder, role, session, session)
		}
		if err := reg.removeInterestTopics(context.Background(), session, []string{contracts.RoleTopicPrefix + role}); err != nil {
			t.Fatalf("take %s's topic off %s's record: %v", role, session, err)
		}
	}

	t.Run("a claim the registry wrote after the view's last read of the bucket is released", func(t *testing.T) {
		const session = "ses_sweep_own_write"
		claimAndDropTheTopic(t, session, ownWrite)

		ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
		defer cancel()
		if _, err := reg.Remove(ctx, session, nil, releasedByItsProcess); err != nil {
			t.Fatalf("Remove(%s): %v", session, err)
		}
		// The watcher has delivered nothing of this role, so a sweep reading the roles from the
		// bucket's last delivered state would have found none to release: the claim is in the
		// view because the write put it there, and that is what the sweep released.
		assertRoleHolder(t, reg, ownWrite, "")
		if holder := holderOf(reg, ownWrite); holder != "" {
			t.Fatalf("the view still names %q the holder of %s after its release", holder, ownWrite)
		}
		assertInterestMissing(t, reg, session)
	})

	t.Run("a claim the view still names the session's that another session has taken is kept", func(t *testing.T) {
		const session = "ses_sweep_taken_before"
		claimAndDropTheTopic(t, session, takenBefore)
		// Another session takes the role by writing its claim to the bucket, as another listener
		// does. The watcher's delivery of it is gated, so the view goes on naming the session
		// below the holder: the disagreement the sweep must not act on.
		revision := putRoleClaim(t, rawRoles, takenBefore, otherHolder)
		if holder := holderOf(reg, takenBefore); holder != session {
			t.Fatalf("the view names %q the holder of %s, want the stale %s: the gate did not hold the delivery and the case tests nothing", holder, takenBefore, session)
		}

		ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
		defer cancel()
		if _, err := reg.Remove(ctx, session, nil, releasedByItsProcess); err != nil {
			t.Fatalf("Remove(%s): %v", session, err)
		}
		// The read the sweep makes before each release names the holder, and it is not this
		// session, so the claim is not this session's to release: the new holder keeps the role
		// at the very revision it wrote.
		assertRoleHolder(t, reg, takenBefore, otherHolder)
		entry, err := rawRoles.Get(takenBefore)
		if err != nil {
			t.Fatalf("read %s's claim after the deregistration: %v", takenBefore, err)
		}
		if entry.Revision() != revision {
			t.Fatalf("%s's claim is at revision %d after the deregistration, want the %d %s wrote", takenBefore, entry.Revision(), revision, otherHolder)
		}
		assertInterestMissing(t, reg, session)
	})

	t.Run("a claim another session takes between the sweep's read and its delete is kept", func(t *testing.T) {
		const session = "ses_sweep_taken_during"
		claimAndDropTheTopic(t, session, takenDuring)
		// The race the conditional delete exists for: the sweep has read the claim and found it
		// the session's, and the new holder's claim lands before the delete reaches the bucket.
		// The delete names the revision the read returned, so it loses, and losing it is not a
		// failure - the claim it did not delete is the new holder's.
		counting := countRoleBucketCalls(t, reg, &sweepRoleKeyValue{beforeFirstDelete: func(key string) {
			if key != takenDuring {
				return
			}
			if _, err := rawRoles.Put(key, []byte(`{"holder_session_id":"`+otherHolder+`"}`)); err != nil {
				t.Errorf("land %s's claim on %s under the sweep's delete: %v", otherHolder, key, err)
			}
		}})

		ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
		defer cancel()
		if _, err := reg.Remove(ctx, session, nil, releasedByItsProcess); err != nil {
			t.Fatalf("Remove(%s) lost its conditional delete to a claim that landed under it: %v, want the loss read as the race it is", session, err)
		}
		if _, deletes := counting.callsOn(takenDuring); deletes != 1 {
			t.Fatalf("the sweep made %d Deletes on %s, want the one it lost", deletes, takenDuring)
		}
		assertRoleHolder(t, reg, takenDuring, otherHolder)
		assertInterestMissing(t, reg, session)
	})
}

// Acceptance criterion A2, on both of the sweeps that now read every claim from one scan. The
// scan carries the revision each record was read at, and the release deletes at that revision
// and no other (nats.LastRevision), so a claim another session takes between the scan and the
// delete is kept: the delete loses its compare-and-swap, which is the answer, not a failure.
// Each case pairs the taken-over claim with one that did not change, which must be released, so
// a sweep that stopped deleting altogether could not pass either of them.
//
// What each turns on is the holder and the revision the bucket ends with, because those are
// what an unconditional delete would destroy: it would take the new holder's claim away and
// leave the role to nobody. Both run over the small fixture, which is all the race needs - the
// claim lands from beforeFirstDelete, under the sweep's own first delete, rather than from any
// timing of the bucket's size.
func TestTheSweepsThatReadFromOneScanKeepAClaimTakenUnderTheirDelete(t *testing.T) {
	const otherHolder = "ses_race_other_holder"

	// landClaimUnderTheFirstDelete returns the hook that writes otherHolder's claim on role when
	// the sweep's first delete reaches the bucket, and the function that reads back the revision
	// it wrote. The revision is guarded because the hook runs on the goroutine bus.Bounded put
	// the delete on and the case reads it from its own.
	landClaimUnderTheFirstDelete := func(t *testing.T, raw natsgo.KeyValue, role string) (func(key string), func() uint64) {
		t.Helper()
		var (
			mu       sync.Mutex
			revision uint64
		)
		hook := func(key string) {
			if key != role {
				t.Errorf("the sweep's first delete fell on %s, want %s, the role the claim lands on", key, role)
				return
			}
			// Written through the bucket's own handle, so the claim the case lands is not
			// counted among the sweep's calls.
			landed, err := raw.Put(key, []byte(`{"holder_session_id":"`+otherHolder+`"}`))
			if err != nil {
				t.Errorf("land %s's claim on %s under the sweep's delete: %v", otherHolder, key, err)
				return
			}
			mu.Lock()
			revision = landed
			mu.Unlock()
		}
		return hook, func() uint64 {
			mu.Lock()
			defer mu.Unlock()
			return revision
		}
	}

	// assertTheNewHoldersClaimStands fails the test unless the bucket holds role for otherHolder
	// at the very revision that holder wrote. Reading both is the point: a delete that named no
	// revision would have deleted the claim, and one that deleted and let the holder write again
	// would leave another revision.
	assertTheNewHoldersClaimStands := func(t *testing.T, raw natsgo.KeyValue, role string, want uint64) {
		t.Helper()
		entry, err := raw.Get(role)
		if err != nil {
			t.Fatalf("read %s's claim after the sweep: %v, want the claim %s took under the delete", role, err, otherHolder)
		}
		claim, err := DecodeRoleClaim(entry.Value())
		if err != nil {
			t.Fatalf("decode %s's claim after the sweep: %v", role, err)
		}
		if claim.HolderSessionID != otherHolder {
			t.Fatalf("%s is held by %q after the sweep, want the %s that took it under the delete", role, claim.HolderSessionID, otherHolder)
		}
		if entry.Revision() != want {
			t.Fatalf("%s's claim is at revision %d after the sweep, want the %d %s wrote", role, entry.Revision(), want, otherHolder)
		}
	}

	t.Run("the role reaper keeps an expired claim another session took under its delete and reaps the one that did not change", func(t *testing.T) {
		const expired = "ses_reaper_race_expired"
		// The taken-over role sorts first, so the sweep's first delete - the one the hook runs
		// before - is the one the new claim lands under.
		taken, unchanged := "reap-race-a-taken-over", "reap-race-b-unchanged"
		reg, rawRoles, _ := smallRoleBucketRegistry(t, expired, []string{taken, unchanged})
		hook, takenRevision := landClaimUnderTheFirstDelete(t, rawRoles, taken)
		counting := countRoleBucketCalls(t, reg, &sweepRoleKeyValue{beforeFirstDelete: hook})

		sweep, err := reg.ReapRoleClaims(context.Background(), func(holder string) bool { return holder != expired }, 0, 0)
		if err != nil {
			t.Fatalf("a reaper pass that lost one conditional delete to a claim landing under it: %v, want the loss read as the race it is", err)
		}
		// One reaped, and the claim the pass did not delete is not among them: the release
		// reported it superseded, which is neither a reap nor a failure to skip.
		if want := (RoleClaimSweep{Reaped: 1}); sweep != want {
			t.Fatalf("the pass = %+v, want %+v: %s reaped and %s left to the session that took it", sweep, want, unchanged, taken)
		}
		if _, deletes := counting.callsOn(taken); deletes != 1 {
			t.Fatalf("the pass made %d Deletes on %s, want the one it lost", deletes, taken)
		}
		assertTheNewHoldersClaimStands(t, rawRoles, taken, takenRevision())
		assertRoleHolder(t, reg, unchanged, "")
	})

	t.Run("the deregistration's fallback keeps a claim another session took under its delete and releases the one that did not change", func(t *testing.T) {
		const session = "ses_sweep_race_held"
		// Both sort before the other sessions' claims, and the taken-over role before the
		// other, so the sweep's first delete is the one the new claim lands under.
		taken, unchanged := "fallback-race-a-taken-over", "fallback-race-b-unchanged"
		reg, rawRoles, _ := smallRoleBucketRegistry(t, session, []string{taken, unchanged})
		hook, takenRevision := landClaimUnderTheFirstDelete(t, rawRoles, taken)
		countRoleBucketCalls(t, reg, &sweepRoleKeyValue{beforeFirstDelete: hook})

		ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
		defer cancel()
		if _, err := reg.Remove(ctx, session, nil, releasedByItsProcess); err != nil {
			t.Fatalf("Remove(%s) lost its conditional delete to a claim that landed under it: %v, want the loss read as the race it is", session, err)
		}
		assertTheNewHoldersClaimStands(t, rawRoles, taken, takenRevision())
		assertRoleHolder(t, reg, unchanged, "")
		assertInterestMissing(t, reg, session)
	})
}

// The seam the forward merge creates on the deregistration path, where the sweep over the roles
// the view names this session the holder of meets the claim that names the process holding each
// of them. The two are one rule: the sweep frees the claims of the process that asked for the
// release and leaves every other claim exactly as it stands. So a second process that resumed a
// live lane's session id frees nothing of that lane's by deregistering the id it shares, while
// the lane's own clean stop frees its roles at once, which is what the deregistration sweep is
// for.
//
// The third claim is the carve-out: one an earlier build wrote names no process at all, so it
// keeps the rule it was written under and the session's own deregistration frees it, whichever
// process asks. Without it every claim standing at the deploy would wait out the role claim
// reaper.
//
// Neither session's record carries a role topic, so the pass over the record releases nothing
// (releaseRoleClaims) and what every claim's fate turns on is the view sweep alone.
func TestTheDeregistrationSweepFreesOnlyTheClaimsTheDeregisteringProcessHolds(t *testing.T) {
	conn, cleanup := connectNATS(t)
	defer cleanup()
	js, err := conn.JetStream()
	if err != nil {
		t.Fatalf("jetstream: %v", err)
	}
	if _, err := js.CreateKeyValue(&natsgo.KeyValueConfig{Bucket: bus.InterestBucket, Replicas: 1, Storage: natsgo.FileStorage}); err != nil {
		t.Fatalf("create the interest bucket: %v", err)
	}
	rawRoles, err := js.CreateKeyValue(&natsgo.KeyValueConfig{Bucket: bus.RoleClaimBucket, Replicas: 1, Storage: natsgo.FileStorage})
	if err != nil {
		t.Fatalf("create the role bucket: %v", err)
	}
	reg, err := Open(conn, WithReplicas(1))
	if err != nil {
		t.Fatalf("Open: %v", err)
	}
	t.Cleanup(reg.StopWatch)
	ready, cancelReady := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancelReady()
	if err := reg.WaitForCacheReady(ready); err != nil {
		t.Fatalf("WaitForCacheReady: %v", err)
	}

	const (
		session  = "ses_sweep_by_process"
		held     = "sweep-process-held"
		unnamed  = "sweep-process-unnamed"
		recorded = "notifications.chat"
		deadline = 30 * time.Second
	)
	// The lane's own claim, written by the process the claim names, and a claim of the same
	// session an earlier build wrote, which names none.
	putRenewedRoleClaim(t, rawRoles, held, session, roleClaimInstance, time.Now())
	putRoleClaim(t, rawRoles, unnamed, session)
	if _, err := reg.Upsert(Interest{SessionID: session, MachineID: "m1"}, []string{recorded}); err != nil {
		t.Fatalf("store %s's interest record: %v", session, err)
	}
	waitForHeld(t, reg, session, held, unnamed)

	resumed, cancelResumed := context.WithTimeout(context.Background(), deadline)
	defer cancelResumed()
	if _, err := reg.Remove(resumed, session, nil, RoleReleaseAuthority{ClientInstanceID: roleClaimSecondInstance}); err != nil {
		t.Fatalf("a second process's deregistration of %s: %v", session, err)
	}
	assertRoleHolder(t, reg, held, session)
	assertRoleHolder(t, reg, unnamed, "")
	assertInterestMissing(t, reg, session)

	// The holder's own process, through the same route the second one used: its claim is freed
	// at the deregistration rather than left to the reaper.
	if _, err := reg.Upsert(Interest{SessionID: session, MachineID: "m1"}, []string{recorded}); err != nil {
		t.Fatalf("register %s's interests again: %v", session, err)
	}
	waitForHeld(t, reg, session, held)
	own, cancelOwn := context.WithTimeout(context.Background(), deadline)
	defer cancelOwn()
	if _, err := reg.Remove(own, session, nil, releasedByItsProcess); err != nil {
		t.Fatalf("the holder's own deregistration of %s: %v", session, err)
	}
	assertRoleHolder(t, reg, held, "")
	assertInterestMissing(t, reg, session)
}
