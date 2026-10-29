package docs

import (
	"context"
	"fmt"
	"log/slog"
	"time"

	"github.com/sjawhar/envoy/internal/dispatch/duty"
	"github.com/sjawhar/envoy/internal/dispatch/store"
)

// SettlementResumeInterval is how often the server arms the settlements documents still owe that no
// settlement committed.
const SettlementResumeInterval = time.Minute

// settlementResumeAge is how long a document's pending-settlement row stands before the resumption
// arms its settlement. Every update refreshes the row and arms a settlement in the process that
// wrote it, which runs two seconds later, so a row this old was left by one that did not commit: a
// shutdown cut it short, or a failure dropped it, perhaps in another process. On a rolling deploy
// that process stops after the new one has started, which is why this runs on an interval and not
// only at start.
const settlementResumeAge = time.Minute

// settlementResumptionLease names the lease each resumption pass runs under (duty.Exclusive). A
// settlement reads what it settles from the store under the document's lock, whichever process
// runs it, so one process arming the owed settlements is the whole of the work; every process
// arming them each period would run each owed document's settlement once per process.
const settlementResumptionLease = "settlement-resumption"

// RunSettlementResumption arms the settlement of every document owing one past
// settlementResumeAge, at start and every SettlementResumeInterval, until ctx ends. Each pass runs
// under settlementResumptionLease for the period SettlementResumeInterval (duty.RunLeased), so one
// of the envoy-dispatch processes serving the database runs it per period, in that process. A
// document nobody opens therefore settles too: its ask blocks reach the open asks without anyone
// loading it.
func (s *Service) RunSettlementResumption(ctx context.Context, leases duty.Leases) {
	duty.RunLeased(ctx, leases, settlementResumptionLease, SettlementResumeInterval, duty.TickAtStart, func(ctx context.Context) {
		if err := s.resumeOwedSettlements(ctx, settlementResumeAge); err != nil && ctx.Err() == nil {
			slog.Error("dispatch: resume owed document settlements", "error", err)
		}
	})
}

// resumeOwedSettlements arms the settlement of each document whose pending-settlement row is at
// least age old. A closed issue's documents are skipped: a closed issue's rooms settle nothing until
// it reopens, so arming one would only fail.
func (s *Service) resumeOwedSettlements(ctx context.Context, age time.Duration) error {
	ctx = store.WithTransactionTracking(ctx)
	rows, err := s.store.Pool.Query(ctx, `
		select p.artifact_id::text
		from doc_settlements_pending p
		join artifacts a on a.id = p.artifact_id
		left join issues i on i.key = a.issue_key
		where p.marked_at <= now() - make_interval(secs => $1) and i.closed_at is null
		order by p.marked_at
	`, age.Seconds())
	if err != nil {
		return fmt.Errorf("list owed document settlements: %w", err)
	}
	// Drained before any is armed: an open cursor holds its connection, and a settlement takes one.
	var rooms []string
	for rows.Next() {
		var room string
		if err := rows.Scan(&room); err != nil {
			rows.Close()
			return fmt.Errorf("scan owed document settlement: %w", err)
		}
		rooms = append(rooms, room)
	}
	rows.Close()
	if err := rows.Err(); err != nil {
		return fmt.Errorf("list owed document settlements: %w", err)
	}
	for _, room := range rooms {
		s.scheduleSettle(room)
	}
	if len(rooms) > 0 {
		s.hooks.countSettlementResumption(rooms, time.Now())
		slog.Info("dispatch: resumed owed document settlements", "documents", len(rooms))
	}
	return nil
}
