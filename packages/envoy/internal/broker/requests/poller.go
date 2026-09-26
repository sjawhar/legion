// packages/envoy/internal/broker/requests/poller.go
package requests

import (
	"context"
	"log/slog"
	"time"

	"github.com/sjawhar/envoy/internal/broker/dispatch"
)

type askReader interface {
	GetAsk(ctx context.Context, id string) (dispatch.Ask, error)
}

// Poller is the one thing that moves pending requests: every interval it reads each pending row
// from Postgres (never from memory, so a restart resumes exactly where the rows are), asks
// Dispatch for the ask's authoritative state, and applies it. Envoy is only told afterwards.
type Poller struct {
	Machine  *Machine
	Dispatch askReader
	Interval time.Duration
	Wake     func(ctx context.Context, enrollmentID, requestID, state string)
}

func (p *Poller) Run(ctx context.Context) {
	ticker := time.NewTicker(p.Interval)
	defer ticker.Stop()
	for {
		if err := p.RunOnce(ctx); err != nil {
			slog.Warn("broker poller", "error", err)
		}
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
		}
	}
}

func (p *Poller) RunOnce(ctx context.Context) error {
	if _, err := p.Machine.ExpirePending(ctx, time.Now()); err != nil {
		return err
	}
	rows, err := p.Machine.Store.Pool.Query(ctx, `select id, enrollment_id, ask_id from requests where state='pending' and ask_id is not null`)
	if err != nil {
		return err
	}
	type pending struct{ id, enrollment, ask string }
	var all []pending
	for rows.Next() {
		var x pending
		if err := rows.Scan(&x.id, &x.enrollment, &x.ask); err != nil {
			rows.Close()
			return err
		}
		all = append(all, x)
	}
	rows.Close()
	for _, x := range all {
		ask, err := p.Dispatch.GetAsk(ctx, x.ask)
		if err != nil {
			slog.Warn("broker poller: read ask", "request", x.id, "error", err)
			continue
		}
		changed, err := p.Machine.ApplyAnswer(ctx, x.id, ask)
		if err != nil {
			slog.Warn("broker poller: apply answer", "request", x.id, "error", err)
			continue
		}
		if changed && p.Wake != nil {
			r, err := p.Machine.Get(ctx, x.id)
			if err == nil {
				p.Wake(ctx, x.enrollment, x.id, r.State) // the waker reads requests.session_id first, then the enrollment's
			}
		}
	}
	return nil
}
