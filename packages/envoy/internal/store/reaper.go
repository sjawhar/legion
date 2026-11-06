package store

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"time"

	"github.com/nats-io/nats.go"
	"github.com/sjawhar/envoy/internal/bus"
)

const (
	// InterestReapGrace is the grace window the listener passes to StartReaper: a dead
	// session's interest stays at least this long after its row's last write, plus
	// InterestRefreshAge.
	InterestReapGrace = 10 * time.Minute
	// InterestRefreshAge is the most an interest row's UpdatedAt may lag its session's last
	// heartbeat: a writer may skip rewriting an unchanged row younger than this, and must
	// rewrite it once it reaches this age. Reap adds it to the grace window, so a skipped
	// rewrite never shortens the grace a paused session gets after its last heartbeat.
	InterestRefreshAge = 5 * time.Minute
)

// InterestReapAllowance is the age past which Reap deletes a dead session's interest: grace, the
// window the row's session is given after its last write, plus InterestRefreshAge, the most that
// write may lag the session's last heartbeat. A row exactly that old is kept. The session cache
// keeps a departed session's last heartbeat for session.LastSeenMemory (2 hours) unless the
// listener sets another retention (session.WithLastSeenRetention), which outlasts this allowance,
// so every row the reaper can still hold has one.
func InterestReapAllowance(grace time.Duration) time.Duration {
	return grace + InterestRefreshAge
}

// Reap deletes the interests of sessions that are gone and whose UpdatedAt is past the grace
// window plus InterestRefreshAge (InterestReapAllowance); a row exactly that old is kept. It
// collects the interests past the allowance from the cache under its read lock, asking nothing of
// any session there, and only then asks live of each, outside every lock. live answers from the
// session's own record (the listener's reads the session's key in the session bucket when its
// cache does not hold it): a session missing from a session copy that is reading its bucket
// again, or has not read it yet, is not gone. live's error is a read that did not answer, and
// keeps the interest for the next pass, as does an interest written again since it was collected.
// A role claim is intentionally not an interest: it remains available for its holder to
// re-register after a listener restart, and the role delivery path drops it if that holder never
// becomes live again.
//
// A candidate is deleted at exactly the revision the copy holds for it when the delete is sent
// (deleteInterestAt), so a write that lands after the re-read is kept: the delete fails with
// JetStream's wrong last sequence (10071, nats.ErrKeyExists), and the candidate counts as skipped.
func (r *Registry) Reap(live func(sessionID string) (bool, error), graceWindow time.Duration) (int, error) {
	now := r.now().UnixMilli()
	allowanceMs := InterestReapAllowance(graceWindow).Milliseconds()

	type candidate struct {
		sessionID string
		updatedAt int64
	}
	r.mu.RLock()
	var candidates []candidate
	for sid, item := range r.cache.All() {
		if now-item.UpdatedAt > allowanceMs {
			candidates = append(candidates, candidate{sessionID: sid, updatedAt: item.UpdatedAt})
		}
	}
	r.mu.RUnlock()

	reaped := 0
	for _, c := range candidates {
		alive, err := live(c.sessionID)
		if err != nil {
			r.logger().Warn("reaper kept the interest of a session whose record it could not read", slog.String("session_id", c.sessionID), slog.String("error", err.Error()))
			continue
		}
		if alive {
			continue
		}
		if item, ok := r.Cached(c.sessionID); !ok || item.UpdatedAt != c.updatedAt {
			continue // removed, or written again, since it was collected
		}
		deleted, err := r.deleteInterestAt(context.Background(), c.sessionID)
		if errors.Is(err, bus.ErrRefused) {
			r.logger().Warn("reaper skipped an interest it cannot delete", slog.Int("key_bytes", len(c.sessionID)), slog.String("error", err.Error()))
			continue
		}
		if err != nil {
			return 0, err
		}
		if deleted {
			reaped++
		}
	}
	return reaped, nil
}

// deleteInterestAt deletes sessionID's interest only while its latest revision is the one the copy
// holds for it, and reports whether it did, each bucket call waiting no longer than ctx
// (bus.Bounded). A write since then makes JetStream refuse the delete with wrong last sequence
// (10071, which nats.go names nats.ErrKeyExists), and the interest is kept: false and no error. A
// copy that holds no revision for the key, because a watch moved the epoch since its read began
// (kvwatch.Copy.FenceFor), names no revision, which a conditional delete cannot express, so that
// interest is deleted at the revision it reads (deleteInterest). The cache follows the bucket
// afterwards as it does after deleteInterest. A delete ctx abandoned may still land after this
// answer, and that is safe: it deletes only the revision it checked.
func (r *Registry) deleteInterestAt(ctx context.Context, sessionID string) (bool, error) {
	r.mu.RLock()
	kv, epoch := r.watcher.Handle()
	revision := r.cache.FenceFor(epoch, sessionID)
	r.mu.RUnlock()
	if revision == 0 {
		return true, r.deleteInterest(ctx, sessionID)
	}
	_, answered, err := bus.Bounded(ctx, func() (struct{}, error) {
		return struct{}{}, kv.Delete(sessionID, nats.LastRevision(revision))
	})
	if !answered {
		return false, fmt.Errorf("delete %s's interest record at revision %d: %w", sessionID, revision, err)
	}
	if errors.Is(err, nats.ErrKeyExists) {
		return false, nil
	}
	if err != nil {
		return false, err
	}
	r.reconcileDeleted(ctx, kv, epoch, sessionID, revision)
	return true, nil
}

// RoleClaimSweep is what one pass of ReapRoleClaims did: Reaped, the claims it released or
// deleted, and Skipped, the roles a read, a delete or a release that failed left for the next
// pass. The counts travel together because neither reads on its own: a pass that reaped one
// claim is an idle fleet or a bucket refusing most of its reads, and only the pair tells those
// apart. A claim stored under a key NATS refuses on every call is not counted skipped: no pass
// will ever read or delete it. Skipped is otherwise the count of what a later pass can still do,
// short of the stored record this build cannot decode (ErrMalformedRoleClaim), which reads the
// same every pass and so is counted and logged on each of them for as long as it is stored.
type RoleClaimSweep struct {
	Reaped  int
	Skipped int
}

// ReapRoleClaims removes claims whose holders did not re-register before the
// session TTL elapsed. It is intentionally separate from Reap: the interest
// reaper must not tear down a role during a listener restart grace window. A
// claim this build cannot read (bus.ErrRefused: one stored under a key NATS
// refuses, too long or outside its key alphabet) no build can resolve or write
// again, so it is deleted too; one it cannot delete either (a key outside the
// alphabet) is skipped, and the sweep goes on.
//
// The listing (roleKeysUntil), every claim read and every release wait no longer than ctx, which
// is the reaper loop's own context (StartRoleClaimReaper): the listener's shutdown ends that
// context and then waits for this loop to return, up to its shared 10 s shutdown bound, before it
// drains the bus, so a pass that returns inside that bound fails each call it has left on the
// ended context alone, against a bus that is still answering.
//
// One role's failed read, failed delete or failed release costs that one role: it is logged in a
// line of its own, counted in Skipped, and the pass goes on. This reaper is the backstop for the
// claim a deregistration left standing - the one its role claim view had not been delivered when
// the session left, which releaseAllRoleClaims leaves here - and a pass that ends at the first
// transient failure is no backstop at the role count that made the view miss the claim: one
// refused read leaves every role listed after it unswept until the next interval, and a failure
// that lasts leaves them unswept for as long as it does.
//
// Two failures still end the pass. Listing the bucket, because a pass over part of the bucket is
// not the pass over every role its counts would claim it was. And any failure that is not
// bus.ErrRefused once ctx has ended, because every call the pass has left would fail the same
// way: it returns the counts it had reached and ctx's error, and StartRoleClaimReaper writes
// that in one line, where a pass that went on would write a WARN for each of the thousands of
// roles behind this one - what a shutdown, or a bus outage under a deadline, would otherwise cost.
// releaseAllRoleClaimsByScan ends its scan on an ended context for this reason too.
//
// An expired claim costs two bucket calls, the read here and the conditional delete: the release
// is handed the claim this pass read (releaseReadExpiredRoleClaim) rather than reading it again,
// because a pass runs over the whole bucket on every interval.
func (r *Registry) ReapRoleClaims(ctx context.Context, isAlive func(string) bool, sessionTTL time.Duration) (RoleClaimSweep, error) {
	roles, err := r.roleKeysUntil(ctx)
	if err != nil {
		return RoleClaimSweep{}, err
	}
	var sweep RoleClaimSweep
	for _, role := range roles {
		claim, err := r.RoleClaim(ctx, role)
		if errors.Is(err, bus.ErrRefused) {
			switch err := r.roles().Delete(role); {
			case err == nil:
				r.logger().Warn("role reaper deleted a claim it cannot read", slog.Int("key_bytes", len(role)))
				sweep.Reaped++
			case errors.Is(err, bus.ErrRefused):
				r.logger().Warn("role reaper skipped a claim it cannot delete", slog.Int("key_bytes", len(role)), slog.String("error", err.Error()))
			default:
				r.logger().Warn("role reaper skipped a claim it cannot read whose delete failed", slog.Int("key_bytes", len(role)), slog.String("error", err.Error()))
				sweep.Skipped++
			}
			continue
		}
		if err != nil {
			if ended := ctx.Err(); ended != nil {
				return sweep, ended
			}
			r.logger().Warn("role reaper skipped a claim it could not read", slog.String("role", role), slog.String("error", err.Error()))
			sweep.Skipped++
			continue
		}
		if claim.HolderSessionID == "" || isAlive(claim.HolderSessionID) {
			continue
		}
		release, err := r.releaseReadExpiredRoleClaim(ctx, role, claim, sessionTTL)
		if err != nil {
			if ended := ctx.Err(); ended != nil {
				return sweep, ended
			}
			r.logger().Warn("role reaper skipped a claim it could not release", slog.String("role", role), slog.String("holder_session_id", claim.HolderSessionID), slog.String("error", err.Error()))
			sweep.Skipped++
			continue
		}
		if release == ExpiredRoleClaimReleased {
			sweep.Reaped++
		}
	}
	return sweep, nil
}

// StartReaper runs Reap in a background goroutine at the given interval until ctx ends, and returns
// a channel closed once it has returned: a pass in flight when ctx ends finishes first. A pass
// lists the bucket through watchers whose consumers nats.go creates and deletes, so the listener's
// shutdown waits for it before it drains NATS (kvwatch.Watcher.End says what a drain does to such
// a delete). Each pass logs how many interests it reaped and how long it took (elapsed_ms).
func (r *Registry) StartReaper(ctx context.Context, live func(sessionID string) (bool, error), interval, graceWindow time.Duration) <-chan struct{} {
	return everyInterval(ctx, interval, func() {
		started := time.Now()
		count, err := r.Reap(live, graceWindow)
		if err != nil {
			r.logger().Error("reaper cycle failed", slog.String("error", err.Error()))
			return
		}
		r.logger().Info("reaper cycle", slog.Int("reaped", count), slog.Int64("elapsed_ms", time.Since(started).Milliseconds()))
	})
}

// StartRoleClaimReaper expires role holders that fail to re-register after a
// listener restart. It is independent of interest reaping because a role is a
// point-to-point route rather than a general subscription. It runs until ctx ends, and the
// channel it returns is closed once it has returned, as StartReaper's is. Each pass logs what it
// reaped beside what it skipped, since only a reaper whose failures are visible from its own line
// can be read as the backstop it is (RoleClaimSweep).
//
// A pass that skipped anything logs that line at WARN, because a pass goes on past the failures
// that once ended it with an ERROR: a sweep that read nothing it swept - every role failed, the
// backstop doing nothing at all - is otherwise an INFO line every interval and nothing above it
// anywhere.
//
// The pass is given this loop's own ctx, so a pass still running when the listener ends it stops
// at its next failed call rather than writing a WARN for every role left (ReapRoleClaims). Its
// failure line carries reaped and skipped beside the error, because a pass cut short that way
// has already done work, and a line that named only the error would read as a pass that did
// nothing.
//
// A pass that ends on an error logs at ERROR, except where this loop's own ctx has ended under
// it: the listener ends that ctx to shut down, and the pass that fails because the listener is
// stopping is part of an ordered shutdown rather than a failure of the reaper's, so it is logged
// at INFO with the same reaped and skipped. The level turns on this loop's ctx rather than on
// the error's identity because neither direction of that identity holds. A listing the shutdown
// cut short answers the scan's own idle bound when the bucket delivered it nothing
// (kvwatch.ScanIdleTimeout, roleKeysUntil), which is no context error; and the per-call deadline
// inside that scan answers context.DeadlineExceeded against a bus too slow to serve it
// (bus.KeyValue.ReadStreamState) while this loop runs on, which is the outage an operator must
// see. What makes a failed pass an ordered shutdown is that this loop's ctx has ended, so that
// is what the level reads.
func (r *Registry) StartRoleClaimReaper(ctx context.Context, isAlive func(string) bool, interval, sessionTTL time.Duration) <-chan struct{} {
	return everyInterval(ctx, interval, func() {
		sweep, err := r.ReapRoleClaims(ctx, isAlive, sessionTTL)
		if err != nil {
			if ctx.Err() != nil {
				r.logger().Info("role claim reaper cycle ended with its loop",
					slog.String("error", err.Error()),
					slog.Int("reaped", sweep.Reaped),
					slog.Int("skipped", sweep.Skipped),
				)
				return
			}
			r.logger().Error("role claim reaper cycle failed",
				slog.String("error", err.Error()),
				slog.Int("reaped", sweep.Reaped),
				slog.Int("skipped", sweep.Skipped),
			)
			return
		}
		cycle := r.logger().Info
		if sweep.Skipped > 0 {
			cycle = r.logger().Warn
		}
		cycle("role claim reaper cycle", slog.Int("reaped", sweep.Reaped), slog.Int("skipped", sweep.Skipped))
	})
}

// everyInterval runs pass in a background goroutine at each tick of interval until ctx ends, and
// returns a channel closed once the goroutine has returned.
func everyInterval(ctx context.Context, interval time.Duration, pass func()) <-chan struct{} {
	done := make(chan struct{})
	go func() {
		defer close(done)
		ticker := time.NewTicker(interval)
		defer ticker.Stop()
		for {
			select {
			case <-ctx.Done():
				return
			case <-ticker.C:
			}
			// A tick and the end can both be ready, and select picks either.
			if ctx.Err() != nil {
				return
			}
			pass()
		}
	}()
	return done
}
