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

// ReapRoleClaims removes claims whose holders did not re-register before the
// session TTL elapsed. It is intentionally separate from Reap: the interest
// reaper must not tear down a role during a listener restart grace window. A
// claim this build cannot read (bus.ErrRefused: one stored under a key NATS
// refuses, too long or outside its key alphabet) no build can resolve or write
// again, so it is deleted too; one it cannot delete either (a key outside the
// alphabet) is skipped, and the sweep goes on.
func (r *Registry) ReapRoleClaims(isAlive func(string) bool, sessionTTL time.Duration) (int, error) {
	roles, err := r.roleKeys()
	if err != nil {
		return 0, err
	}
	reaped := 0
	for _, role := range roles {
		claim, err := r.RoleClaim(context.Background(), role)
		if errors.Is(err, bus.ErrRefused) {
			switch err := r.roles().Delete(role); {
			case err == nil:
				r.logger().Warn("role reaper deleted a claim it cannot read", slog.Int("key_bytes", len(role)))
				reaped++
			case errors.Is(err, bus.ErrRefused):
				r.logger().Warn("role reaper skipped a claim it cannot delete", slog.Int("key_bytes", len(role)), slog.String("error", err.Error()))
			default:
				return 0, err
			}
			continue
		}
		if err != nil {
			return 0, err
		}
		if claim.HolderSessionID == "" || isAlive(claim.HolderSessionID) {
			continue
		}
		release, err := r.ReleaseExpiredRoleClaim(context.Background(), role, claim.HolderSessionID, sessionTTL)
		if err != nil {
			return 0, err
		}
		if release == ExpiredRoleClaimReleased {
			reaped++
		}
	}
	return reaped, nil
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
// channel it returns is closed once it has returned, as StartReaper's is.
func (r *Registry) StartRoleClaimReaper(ctx context.Context, isAlive func(string) bool, interval, sessionTTL time.Duration) <-chan struct{} {
	return everyInterval(ctx, interval, func() {
		count, err := r.ReapRoleClaims(isAlive, sessionTTL)
		if err != nil {
			r.logger().Error("role claim reaper cycle failed", slog.String("error", err.Error()))
			return
		}
		r.logger().Info("role claim reaper cycle", slog.Int("reaped", count))
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
