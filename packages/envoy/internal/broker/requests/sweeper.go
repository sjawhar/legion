package requests

import (
	"context"
	"log/slog"
	"time"

	"github.com/sjawhar/envoy/internal/broker/enroll"
	"github.com/sjawhar/envoy/internal/broker/machine"
)

// Sweeper is the one thing that moves state no human decides (the shared broker contract,
// dispatch://AGENTC-393/artifact/plan-overview-md): every tick it ends the enrollments whose lease
// lapsed (revoking their grants and cancelling their pending requests), expires overdue pending
// agent_secret requests (waking each one's owner) and overdue pending machine logins — all read
// fresh from Postgres, never from memory, so a restart resumes exactly where the rows are.
type Sweeper struct {
	Enrollments   *enroll.Service
	Machine       *Machine
	MachineLogins *machine.Service
	Interval      time.Duration
	Wake          func(ctx context.Context, enrollmentID, requestID, state string)
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

// Tick runs one sweep: lapsed enrollments, then agent_secret requests, then machine logins. Each
// sweep's own failure is logged and does not stop the others — they share nothing but the ticker.
// Lapsed enrollments go first, so a request whose session is gone is cancelled with it rather
// than expired and its owner woken.
func (s *Sweeper) Tick(ctx context.Context) {
	ended, err := s.Enrollments.EndLapsed(ctx)
	for _, e := range ended {
		slog.Info("broker sweeper: ended an enrollment whose lease lapsed", "enrollment_id", e.ID, "kind", e.Kind,
			"runtime_id", e.RuntimeID, "slot", e.Slot, "lease_expired_at", e.LeaseExpires,
			"grants_revoked", e.GrantsRevoked, "requests_cancelled", e.RequestsCancelled)
	}
	if err != nil {
		slog.Warn("broker sweeper: end lapsed enrollments", "error", err)
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
}
