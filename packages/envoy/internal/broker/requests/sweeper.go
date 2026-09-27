package requests

import (
	"context"
	"log/slog"
	"time"

	"github.com/sjawhar/envoy/internal/broker/approvers"
	"github.com/sjawhar/envoy/internal/broker/machine"
)

// Sweeper is the one thing that moves pending state now that every decision comes from a
// WebAuthn assertion rather than a Dispatch ask (AGENTC-393 v9): every tick it expires overdue
// pending agent_secret requests (waking each one's owner, the wake seam poller.go used to own),
// overdue pending machine logins, and abandoned WebAuthn registration/endorsement ceremonies —
// all read fresh from Postgres, never from memory, so a restart resumes exactly where the rows
// are.
type Sweeper struct {
	Machine       *Machine
	MachineLogins *machine.Service
	// Approvers, when set, also sweeps webauthn_ceremonies past their own expires_at: a caller
	// who opened a registration or endorsement ceremony and never finished it would otherwise
	// leave that row forever, since only Finish* consumes one and only on completion.
	Approvers *approvers.Service
	Interval  time.Duration
	Wake      func(ctx context.Context, enrollmentID, requestID, state string)
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

// Tick runs one sweep: agent_secret requests, machine logins, then ceremonies. Each sweep's own
// failure is logged and does not stop the others — they share nothing but the ticker.
func (s *Sweeper) Tick(ctx context.Context) {
	now := time.Now()
	expired, err := s.Machine.expirePending(ctx, now)
	if err != nil {
		slog.Warn("broker sweeper: expire pending requests", "error", err)
	}
	for _, r := range expired {
		if s.Wake != nil {
			s.Wake(ctx, r.enrollmentID, r.id, "expired")
		}
	}
	if err := s.MachineLogins.ExpirePending(ctx, now); err != nil {
		slog.Warn("broker sweeper: expire pending machine logins", "error", err)
	}
	if s.Approvers != nil {
		if _, err := s.Approvers.SweepExpiredCeremonies(ctx); err != nil {
			slog.Warn("broker sweeper: sweep expired webauthn ceremonies", "error", err)
		}
	}
}
