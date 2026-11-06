// The sweep of the listener's kept role lanes: the loop that runs every interval and once as the
// listener starts, and the pass it makes, which hands over every lane the stream holds a kept
// copy on and a session claims. It is the backstop behind the two handovers a running listener
// starts for itself, the claim's and the keep's (role_keep.go).

package main

import (
	"context"
	"log/slog"
	"time"
)

// startKeptRoleSweep is the backstop under which no kept lane waits for a claim that will not
// come. A handover runs in a goroutine of this process, so a listener restarted or replaced
// mid-handover ends it, and nothing starts another: a keep happens only while a role has no live
// holder, and a live holder claims no second time. Every interval, and once as the listener
// starts, this hands over every lane left that way (sweepKeptRoleLanes).
//
// It runs under the context the listener's other KV loops run under and returns a channel closed
// once its goroutine has returned, as they do (store.Registry.StartReaper), so the shutdown ends
// it and waits for a pass in flight.
func startKeptRoleSweep(ctx context.Context, cfg listenerDeliveryHandlerConfig, interval time.Duration) <-chan struct{} {
	done := make(chan struct{})
	go func() {
		defer close(done)
		ticker := time.NewTicker(interval)
		defer ticker.Stop()
		for {
			sweepKeptRoleLanes(ctx, cfg)
			select {
			case <-ctx.Done():
				return
			case <-ticker.C:
			}
		}
	}()
	return done
}

// sweepKeptRoleLanes hands over every role the stream holds a kept copy on and a session claims.
// It reads every kept role subject the stream holds a copy on in one call
// (bus.Client.KeptRoleLanes), so a deployment where no role has a kept copy pays one round trip a
// pass and no per-role work at all; a role that has one costs the read of its claim, and a
// handover of its own when that claim names a session.
//
// A lane whose role no session claims is passed over rather than handed over, with nothing logged
// for it: nothing moves such a lane's cursor - the handover it would start resolves no live
// holder and walks nothing - so the role stays on the stream's list of kept lanes for as long as
// it holds those copies, the stream's 72 h and not roleKeepWindow's hour, and a line and a
// handover for it would cost a goroutine and a resolution every pass. The claim is read from the
// registry's watched copy of the role bucket (store.Registry.CachedRoleHolder): a role a session
// claims is in that view and costs no round trip, and an unclaimed one is not - the view drops a
// role as its claim is released - so it costs the one read of the role bucket that miss makes.
// The claim says only that a session holds the role: whether that session is live is the
// handover's own resolution to make.
//
// The lane itself is left to the handover to read, which is the read its first pass opens with
// (readKeptLaneHead): a lane its cursor is finished with ends that pass there, with no holder
// resolved and nothing walked, where a read here would cost every swept lane that read twice.
//
// A role whose handover is already running on this listener is left to it: the sweep takes that
// role's guard or passes the role over, and it never waits for one (roleKeepGuards.tryHold), so
// one slow handover holds up no other role's sweep. The handover the sweep starts runs under the
// guard the sweep took, in a goroutine of its own, so a pass starts the handovers of a hundred
// lanes without walking any of them itself.
func sweepKeptRoleLanes(ctx context.Context, cfg listenerDeliveryHandlerConfig) {
	roles, err := cfg.client.KeptRoleLanes(ctx)
	if err != nil {
		if ctx.Err() == nil {
			cfg.logger.LogEvent(slog.LevelWarn, "listener kept role sweep could not read the kept lanes",
				slog.String("error", err.Error()))
		}
		return
	}
	for _, role := range roles {
		if ctx.Err() != nil {
			return
		}
		holder, err := cfg.registry.CachedRoleHolder(role)
		if err != nil {
			cfg.logger.LogEvent(slog.LevelDebug, "listener kept role sweep could not read a role's claim",
				slog.String("role", role),
				slog.String("error", err.Error()))
			continue
		}
		if holder == "" {
			continue
		}
		release, taken := cfg.roleKeepDrains.tryHold(role)
		if !taken {
			continue
		}
		cfg.logger.LogEvent(slog.LevelDebug, "listener kept role sweep is handing over a claimed role's kept lane",
			slog.String("role", role),
			slog.String("session_id", holder))
		go func() {
			defer release()
			handoverCtx, cancel := context.WithTimeout(ctx, roleKeepHandoverWindow)
			defer cancel()
			reportKeptRoleDrain(cfg, role, handOverKeptRole(handoverCtx, cfg, role, time.Now))
		}()
	}
}
