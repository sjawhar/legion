package requests

import (
	"context"
	"log/slog"
	"time"

	"github.com/sjawhar/envoy/internal/broker/machine"
)

// Sweeper is the one thing that moves pending state no human decides (the shared broker contract,
// dispatch://AGENTC-393/artifact/plan-overview-md): every tick it expires overdue pending
// agent_secret requests (waking each one's owner) and overdue pending machine logins — all read
// fresh from Postgres, never from memory, so a restart resumes exactly where the rows are.
type Sweeper struct {
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

// Tick runs one sweep: agent_secret requests, then machine logins. Each sweep's own failure is
// logged and does not stop the other — they share nothing but the ticker.
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
}
