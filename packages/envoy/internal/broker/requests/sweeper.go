package requests

import (
	"context"
	"log/slog"
	"time"

	"github.com/sjawhar/envoy/internal/broker/enroll"
	"github.com/sjawhar/envoy/internal/broker/machine"
	"github.com/sjawhar/envoy/internal/broker/oauthstore"
)

// Sweeper is the one thing that ends what no human decides: every tick it ends the enrollments
// whose lease lapsed (revoking their grants and cancelling their pending requests), ends the
// dormant enrollments whose carry window passed, expires overdue
// pending agent_secret requests (waking each one's owner) and overdue pending machine logins,
// deletes spent OAuth state (oauthstore.Store.Sweep), and deletes expired browser sessions — all
// read fresh from Postgres, never from memory, so a restart resumes exactly where the rows are. A
// lease that ran out while no broker was serving is not lapsed (enroll.Service.EndLapsed), so the
// first sweep after an outage never ends it for that: the broker's serving clock
// (enroll.ServingClock) gives it the time back instead.
type Sweeper struct {
	Enrollments   *enroll.Service
	Machine       *Machine
	MachineLogins *machine.Service
	OAuth         *oauthstore.Store
	Interval      time.Duration
	// SessionCarry is BROKER_SESSION_CARRY_SECONDS: how long a dormant enrollment's grants and
	// pending requests wait for the next enrollment of its session name to take them before this
	// sweep revokes them (enroll.Service.EndCarried). 0 ends every dormant enrollment the next
	// tick finds, which is the broker's behaviour before the carry.
	SessionCarry time.Duration
	Wake         func(ctx context.Context, enrollmentID, requestID, state string)
}

func (s *Sweeper) Run(ctx context.Context) {
	ticker := time.NewTicker(s.Interval)
	defer ticker.Stop()
	for {
		s.Tick(ctx)
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
		}
	}
}

// Tick runs one sweep: lapsed enrollments, then the dormant enrollments whose carry window
// passed, then agent_secret requests, then machine logins, then
// spent OAuth state, then expired browser sessions. Each
// sweep's own failure is logged and does not stop the others — they share nothing but the ticker.
// Lapsed enrollments go first, so a request whose session is gone is cancelled with it rather
// than expired and its owner woken; the carry sweep follows them, so a lapse that leaves an
// enrollment dormant with the carry turned off (SessionCarry 0) is finished in the same tick.
func (s *Sweeper) Tick(ctx context.Context) {
	ended, err := s.Enrollments.EndLapsed(ctx)
	for _, e := range ended {
		slog.Info("broker sweeper: ended an enrollment whose lease lapsed", "enrollment_id", e.ID, "kind", e.Kind,
			"runtime_id", e.RuntimeID, "slot", e.Slot, "lease_expired_at", e.LeaseExpires,
			"grants_revoked", e.GrantsRevoked, "requests_cancelled", e.RequestsCancelled, "left_dormant", e.Dormant)
	}
	if err != nil {
		slog.Warn("broker sweeper: end lapsed enrollments", "error", err)
	}
	carried, err := s.Enrollments.EndCarried(ctx, s.SessionCarry)
	for _, e := range carried {
		slog.Info("broker sweeper: ended a dormant enrollment whose carry window passed", "enrollment_id", e.ID,
			"kind", e.Kind, "runtime_id", e.RuntimeID, "slot", e.Slot, "session_id", e.SessionID,
			"dormant_since", e.DormantSince, "grants_revoked", e.GrantsRevoked, "requests_cancelled", e.RequestsCancelled)
	}
	if err != nil {
		slog.Warn("broker sweeper: end dormant enrollments", "error", err)
	}
	now := time.Now()
	expired, err := s.Machine.expirePending(ctx, now)
	if err != nil {
		slog.Warn("broker sweeper: expire pending requests", "error", err)
	}
	for _, r := range expired {
		if s.Wake != nil {
			s.Wake(ctx, r.EnrollmentID, r.ID, "expired")
		}
	}
	if err := s.MachineLogins.ExpirePending(ctx, now); err != nil {
		slog.Warn("broker sweeper: expire pending machine logins", "error", err)
	}
	if err := s.OAuth.Sweep(ctx); err != nil {
		slog.Warn("broker sweeper: delete spent OAuth state", "error", err)
	}
	if deleted, err := s.Machine.Store.DeleteExpiredBrowserSessions(ctx, now); err != nil {
		slog.Warn("broker sweeper: delete expired browser sessions", "error", err)
	} else if deleted > 0 {
		slog.Info("broker sweeper: deleted expired browser sessions", "count", deleted)
	}
}
