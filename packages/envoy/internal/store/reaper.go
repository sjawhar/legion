package store

import (
	"context"
	"errors"
	"log/slog"
	"time"

	"github.com/sjawhar/envoy/internal/bus"
)

// Reap deletes the interests of sessions that are gone and whose UpdatedAt is past the grace
// window. It collects the interests past the window from the cache under its read lock, asking
// nothing of any session there, and only then asks live of each, outside every lock. live answers
// from the session's own record (the listener's reads the session's key in the session bucket when
// its cache does not hold it): a session missing from a session copy that is reading its bucket
// again, or has not read it yet, is not gone. live's error is a read that did not answer, and
// keeps the interest for the next pass, as does an interest written again since it was collected.
// A role claim is intentionally not an interest: it remains available for its holder to
// re-register after a listener restart, and the role delivery path drops it if that holder never
// becomes live again.
func (r *Registry) Reap(live func(sessionID string) (bool, error), graceWindow time.Duration) (int, error) {
	now := r.now().UnixMilli()
	graceMs := graceWindow.Milliseconds()

	type candidate struct {
		sessionID string
		updatedAt int64
	}
	r.mu.RLock()
	var candidates []candidate
	for sid, item := range r.cache.All() {
		if now-item.UpdatedAt > graceMs {
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
		err = r.deleteInterest(c.sessionID)
		if errors.Is(err, bus.ErrRefused) {
			r.logger().Warn("reaper skipped an interest it cannot delete", slog.Int("key_bytes", len(c.sessionID)), slog.String("error", err.Error()))
			continue
		}
		if err != nil {
			return 0, err
		}
		reaped++
	}
	return reaped, nil
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
		claim, err := r.RoleClaim(role)
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
		release, err := r.ReleaseExpiredRoleClaim(role, claim.HolderSessionID, sessionTTL)
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
// a delete).
func (r *Registry) StartReaper(ctx context.Context, live func(sessionID string) (bool, error), interval, graceWindow time.Duration) <-chan struct{} {
	return everyInterval(ctx, interval, func() {
		count, err := r.Reap(live, graceWindow)
		if err != nil {
			r.logger().Error("reaper cycle failed", slog.String("error", err.Error()))
			return
		}
		r.logger().Info("reaper cycle", slog.Int("reaped", count))
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
