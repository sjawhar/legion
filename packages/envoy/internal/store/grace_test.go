package store

import (
	"context"
	"sync/atomic"
	"testing"
	"time"
)

// gracePod is one listener pod's interest registry for a grace test: opened over its own NATS
// connection, with its own start set by startedAgo (the registry's clock reads that far back while
// Open runs, and the real time after it), its clock moved on by advance, and its service start read
// from since, as a pod reads its delivery lease's service_since.
type gracePod struct {
	*Registry
	elapsed *atomic.Int64
}

func openGracePod(t *testing.T, startedAgo time.Duration, since func() time.Time) gracePod {
	t.Helper()
	conn, cleanup := connectNATS(t)
	t.Cleanup(cleanup)
	elapsed := &atomic.Int64{}
	elapsed.Store(int64(-startedAgo))
	clock := func() time.Time { return time.Now().Add(time.Duration(elapsed.Load())) }
	reg, err := Open(conn, WithReplicas(1), WithClock(clock), WithServiceSince(since))
	if err != nil {
		t.Fatalf("open the registry: %v", err)
	}
	t.Cleanup(reg.StopWatch)
	elapsed.Store(0)
	return gracePod{Registry: reg, elapsed: elapsed}
}

func (p gracePod) advance(d time.Duration) { p.elapsed.Add(int64(d)) }

// release asks the pod to release role's claim, whose holder is gone, and names the outcome.
func (p gracePod) release(t *testing.T, role, holder string) string {
	t.Helper()
	release, err := p.ReleaseExpiredRoleClaim(context.Background(), role, holder, graceTestTTL)
	if err != nil {
		t.Fatalf("release %s's expired claim: %v", role, err)
	}
	return map[ExpiredRoleClaimRelease]string{
		ExpiredRoleClaimReleased:   "released",
		ExpiredRoleClaimMissing:    "missing",
		ExpiredRoleClaimSuperseded: "superseded",
		ExpiredRoleClaimRetained:   "retained",
	}[release]
}

func (p gracePod) claim(t *testing.T, role, holder string) {
	t.Helper()
	if _, err := p.SetRole(holder, "box-a", role, false); err != nil {
		t.Fatalf("claim %s for %s: %v", role, holder, err)
	}
}

const graceTestTTL = 5 * time.Minute

// A role claim whose holder is gone keeps its grace by the machine's service start, which every pod
// reads from one lease record, so a pod that has run for a long time and one that started a moment
// ago answer one claim alike. Before, each pod counted the grace from its own start: the long-running
// pod released a claim the bucket held from before a gap, and the new pod kept a claim made long
// after the machine's service began. With no record to read (a gap, or a listener without the
// bucket) a pod counts from its own start, as it did.
func TestEveryPodGivesARoleClaimTheGraceOfTheMachinesServiceStart(t *testing.T) {
	t.Run("a claim made after the service start has no grace on any pod", func(t *testing.T) {
		serviceSince := time.Now().Add(-10 * time.Minute)
		since := func() time.Time { return serviceSince }
		a := openGracePod(t, 10*time.Minute, since)
		a.claim(t, "role-a", "ses_gone_a")
		a.claim(t, "role-b", "ses_gone_b")
		b := openGracePod(t, 0, since)

		if release := a.release(t, "role-a", "ses_gone_a"); release != "released" {
			t.Fatalf("pod a, running since the service start: release = %v, want released", release)
		}
		if release := b.release(t, "role-b", "ses_gone_b"); release != "released" {
			t.Fatalf("pod b, started after the claim: release = %v, want released as pod a did: the claim was made after the machine's service start", release)
		}
	})

	t.Run("a claim from before the service start keeps its grace on every pod for one TTL", func(t *testing.T) {
		var serviceSince atomic.Pointer[time.Time]
		since := func() time.Time {
			if at := serviceSince.Load(); at != nil {
				return *at
			}
			return time.Time{}
		}
		a := openGracePod(t, 10*time.Minute, since)
		a.claim(t, "role", "ses_gone")
		// The machine's pods start serving again after a gap: the lease record's service_since is
		// the server's time of a write after the claim.
		started := time.Now()
		serviceSince.Store(&started)
		b := openGracePod(t, 0, since)

		if release := a.release(t, "role", "ses_gone"); release != "retained" {
			t.Fatalf("pod a, whose own start is long before the claim: release = %v, want retained: the claim is from before the service start", release)
		}
		if release := b.release(t, "role", "ses_gone"); release != "retained" {
			t.Fatalf("pod b: release = %v, want retained as on pod a", release)
		}
		a.advance(graceTestTTL)
		if release := a.release(t, "role", "ses_gone"); release != "released" {
			t.Fatalf("pod a one TTL after the service start: release = %v, want released", release)
		}
	})

	t.Run("with no lease record each pod counts from its own start", func(t *testing.T) {
		none := func() time.Time { return time.Time{} }
		a := openGracePod(t, 10*time.Minute, none)
		a.claim(t, "role-a", "ses_gone_a")
		a.claim(t, "role-b", "ses_gone_b")
		b := openGracePod(t, 0, none)

		if release := a.release(t, "role-a", "ses_gone_a"); release != "released" {
			t.Fatalf("pod a, started before the claim: release = %v, want released", release)
		}
		if release := b.release(t, "role-b", "ses_gone_b"); release != "retained" {
			t.Fatalf("pod b, started after the claim: release = %v, want retained for one TTL after its start", release)
		}
		b.advance(graceTestTTL)
		if release := b.release(t, "role-b", "ses_gone_b"); release != "released" {
			t.Fatalf("pod b one TTL after its start: release = %v, want released", release)
		}
	})
}
