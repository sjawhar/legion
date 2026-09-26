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

// launcherReconciler is launcher.Service's own Reconcile method, following the same
// small-seam-not-concrete-type precedent as askReader above: the requests package must not import
// launcher (it would cycle back through dispatch/enroll), so it names just the one method it
// calls.
type launcherReconciler interface {
	Reconcile(ctx context.Context) error
}

// Poller is the one thing that moves pending requests: every interval it reads each pending row
// from Postgres (never from memory, so a restart resumes exactly where the rows are), asks
// Dispatch for the ask's authoritative state, and applies it. Envoy is only told afterwards.
// Launcher, when set, is also reconciled each tick (see RunOnce) — a nil Launcher is a no-op so
// that existing Poller literals with no Launcher field keep working unmodified.
type Poller struct {
	Machine  *Machine
	Dispatch askReader
	Interval time.Duration
	Wake     func(ctx context.Context, enrollmentID, requestID, state string)
	Launcher launcherReconciler
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
	if p.Launcher != nil {
		// Reconcile owns its own errors per pending row (it logs and skips a row it can't read
		// from Dispatch); a failure here means something broader went wrong (e.g. Postgres is
		// unreachable), which is worth a warning but shouldn't stop this tick's own pending-request
		// reconciliation below — the two flows share nothing but the ticker.
		if err := p.Launcher.Reconcile(ctx); err != nil {
			slog.Warn("broker poller: launcher reconcile", "error", err)
		}
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
