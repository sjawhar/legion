// Wake reports for Legion's released issue pods: the two wakes only this listener sees, and the
// liveness signal that says its silence means "nothing to report" rather than "nothing reporting".

package main

import (
	"context"
	"encoding/json"
	"log/slog"
	"time"

	"github.com/sjawhar/envoy/internal/bus"
	"github.com/sjawhar/envoy/internal/contracts"
	"github.com/sjawhar/envoy/internal/id"
	"github.com/sjawhar/envoy/internal/logging"
)

// The kinds of wake report, as the daemon's decoder reads them
// (packages/daemon/internal/intake/decode.go, decodeListenerWake).
const (
	// wakeKindRole is a message published to a role topic with no live holder. The Legion daemon
	// resumes the released claim of that role token, if one of its own is released: a message
	// sent with keep: false keeps nothing and is answered 404 all the same, and the wake is what
	// makes the sender's next attempt reach a live session.
	wakeKindRole = "role_message"
	// wakeKindAgent is a message sent to the agent subject of a session nothing can place: an
	// Envoy direct message, or a person's answer, comment or message a Dispatch claim routes to
	// that session.
	wakeKindAgent = "agent_message"
	// wakeKindReady is this listener saying it is alive and reporting, published at its start and
	// every listenerReadyInterval after it. The daemon releases no pod unless it has read one
	// within its own window, because without a positive signal "the listener sends no reports"
	// and "the listener has nothing to report" are the same silence.
	wakeKindReady = "listener_ready"
)

// listenerReadyInterval is how often a running listener republishes its liveness report. The
// daemon's window is three times this, so one report lost to a reconnect costs nothing.
const listenerReadyInterval = 5 * time.Minute

// A wake report is retried until the bus acknowledges it. Losing one is not "one wake late": a
// released architect gets no backstop at all (the daemon arms one for a phase worker only), a
// phase worker whose phase is closed gets none either, and the daemon's stranded report passes
// over a released claim no wake ever touched — so a report dropped during a NATS reconnect
// leaves a tree stalled with nothing saying so. R3 asks for the report to reach the daemon
// across a daemon restart too, which the stream's own retention gives once the publish is
// stored, and a publish that never happened defeats in the same way.
//
// wakeReportAttempt bounds one publish, wakeReportRetry is the first wait between two of them,
// doubling to wakeReportRetryCap, and wakeReportBudget bounds the whole report: past it the
// listener has been unable to reach the stream for minutes, which its own liveness report then
// says, and holding the goroutine longer buys nothing. No caller waits any of it out — the two
// reports a 404 raises are published off that 404's path (reportRoleWake, reportAgentWake).
const (
	wakeReportAttempt  = 2 * time.Second
	wakeReportRetry    = 250 * time.Millisecond
	wakeReportRetryCap = 15 * time.Second
	wakeReportBudget   = 5 * time.Minute
)

// wakeReport is the wire shape of a report on bus.LegionWakeTopic. At is this listener's own
// clock in epoch milliseconds, matching contracts.Envelope's KeptAt, because the stream's store
// time is the NATS server's clock and the daemon measures a wake against its own.
type wakeReport struct {
	Kind    string `json:"kind"`
	Role    string `json:"role,omitempty"`
	Session string `json:"session,omitempty"`
	At      int64  `json:"at"`
}

// publishWakeReport publishes one report on bus.LegionWakeTopic, retrying until the stream
// acknowledges it, the whole report's budget runs out, or ctx ends with the listener. Nothing
// waits for it: the 404 it was raised beside is answered whatever the bus does, and is answered
// without waiting for it (reportRoleWake).
//
// Every attempt publishes the one envelope built below, under the one event id and dedupe key,
// as the stream keeps no duplicate window for these: an attempt the stream stored and failed to
// acknowledge is reported twice, and a wake the daemon has taken already is taken no second time
// (its mark's key is the report's event id, which every attempt of one report carries, and the
// role and agent reports of one message carry the same name whichever attempt the daemon reads).
func publishWakeReport(lifetime context.Context, client *bus.Client, logger *logging.Logger, report wakeReport) {
	if client == nil {
		return
	}
	report.At = contracts.NowMillis()
	payload, err := json.Marshal(report)
	if err != nil {
		logger.Warn("legion wake report not encoded", slog.String("kind", report.Kind), slog.String("error", err.Error()))
		return
	}
	eventID := id.New()
	summary := "legion wake: " + report.Kind
	if report.Role != "" {
		summary += " " + report.Role
	}
	if report.Session != "" {
		summary += " " + report.Session
	}
	// Every field contracts.Envelope.Validate requires is set here, SourceEventID included: a
	// report that fails Validate is dropped before it is published (below), and a dropped
	// listener_ready leaves every Legion daemon's release gate shut.
	item := contracts.Envelope{
		EventID:        eventID,
		Source:         "envoy",
		SourceEventID:  eventID,
		Topic:          bus.LegionWakeTopic,
		DedupeKey:      "legion.wake." + eventID,
		IssuedAt:       report.At,
		PayloadSummary: summary,
		Payload:        string(payload),
		TraceID:        id.New(),
	}
	if err := item.Validate(); err != nil {
		logger.Warn("legion wake report not valid", slog.String("kind", report.Kind), slog.String("error", err.Error()))
		return
	}
	ctx, cancel := context.WithTimeout(lifetime, wakeReportBudget)
	defer cancel()
	for attempt, wait := 1, wakeReportRetry; ; attempt++ {
		if err := publishWithinBudget(ctx, client, item); err == nil {
			logger.Info("legion wake report published", slog.String("kind", report.Kind),
				slog.String("role", report.Role), slog.String("session", report.Session), slog.Int("attempts", attempt))
			return
		} else if ctx.Err() != nil {
			logger.Warn("legion wake report not published", slog.String("kind", report.Kind),
				slog.String("role", report.Role), slog.String("session", report.Session),
				slog.Int("attempts", attempt), slog.String("error", err.Error()))
			return
		} else {
			logger.Warn("legion wake report not published; retrying until the bus takes it",
				slog.String("kind", report.Kind), slog.String("role", report.Role),
				slog.String("session", report.Session), slog.Int("attempts", attempt), slog.String("error", err.Error()))
		}
		select {
		case <-ctx.Done():
			return
		case <-time.After(wait):
		}
		if wait *= 2; wait > wakeReportRetryCap {
			wait = wakeReportRetryCap
		}
	}
}

// publishWithinBudget makes one publish attempt, bounded by wakeReportAttempt and by what is
// left of the report's whole budget.
func publishWithinBudget(budget context.Context, client *bus.Client, item contracts.Envelope) error {
	ctx, cancel := context.WithTimeout(budget, wakeReportAttempt)
	defer cancel()
	return client.PublishWithin(ctx, item)
}

// wakeReportsInFlight is how many wake reports may be retrying at once. One report holds its
// goroutine for as long as wakeReportBudget, and during a bus outage every role send with no
// holder and every message nothing can place raises one, so an unbounded reporter turns a
// minutes-long outage into a goroutine and a report's worth of memory per such request. Past
// the cap a report is dropped and said so: the listener has not reached the stream for minutes,
// which its own liveness report already tells every daemon, and a daemon that reads no fresh
// report releases no pod — so there is no released pod left for the dropped report to wake.
const wakeReportsInFlight = 64

// wakeReporter publishes wake reports off the paths that raise them: under the listener's own
// lifetime, so the shutdown ends every retry still running, and no more than
// wakeReportsInFlight at a time.
type wakeReporter struct {
	lifetime context.Context
	client   *bus.Client
	slots    chan struct{}
}

func newWakeReporter(lifetime context.Context, client *bus.Client) *wakeReporter {
	return &wakeReporter{lifetime: lifetime, client: client, slots: make(chan struct{}, wakeReportsInFlight)}
}

// report publishes one report off the caller's path. The caller is writing a 404 the report
// owes nothing, and every role send with no holder raises one — including the common case where
// no daemon holds a released claim of that role at all — so publishing it inline would put the
// report's retries in front of each of those 404s.
func (r *wakeReporter) report(logger *logging.Logger, report wakeReport) {
	if r == nil || r.client == nil {
		return
	}
	select {
	case r.slots <- struct{}{}:
	default:
		logger.Warn("legion wake report dropped: too many are already retrying",
			slog.String("kind", report.Kind), slog.String("role", report.Role),
			slog.String("session", report.Session), slog.Int("inFlight", wakeReportsInFlight))
		return
	}
	go func() {
		defer func() { <-r.slots }()
		publishWakeReport(r.lifetime, r.client, logger, report)
	}()
}

// reportRoleWake reports that a message was published to role and no live session held it. The
// role is the claim token a Legion role topic is named by, which is what the daemon matches its
// own released claims on.
func reportRoleWake(d *listenerDeps, logger *logging.Logger, role string) {
	if d == nil || role == "" {
		return
	}
	d.wakes.report(logger, wakeReport{Kind: wakeKindRole, Role: role})
}

// reportAgentWake reports that a message was sent to the agent subject of sessionID and nothing
// could place it.
func reportAgentWake(wakes *wakeReporter, logger *logging.Logger, sessionID string) {
	if sessionID == "" {
		return
	}
	wakes.report(logger, wakeReport{Kind: wakeKindAgent, Session: sessionID})
}

// startListenerReadyReports publishes this listener's liveness report at once and every
// listenerReadyInterval until ctx ends. It is the whole of the daemon's R7 gate: a daemon that
// has read no report within its window releases nothing, so a listener that is down, partitioned
// from the stream, or denied the publish by its own grant holds release off rather than letting
// pods be released into a silence nothing would wake them out of.
func startListenerReadyReports(ctx context.Context, client *bus.Client, logger *logging.Logger, interval time.Duration) {
	if interval <= 0 {
		interval = listenerReadyInterval
	}
	go func() {
		publishWakeReport(ctx, client, logger, wakeReport{Kind: wakeKindReady})
		ticker := time.NewTicker(interval)
		defer ticker.Stop()
		for {
			select {
			case <-ctx.Done():
				return
			case <-ticker.C:
				publishWakeReport(ctx, client, logger, wakeReport{Kind: wakeKindReady})
			}
		}
	}()
}
