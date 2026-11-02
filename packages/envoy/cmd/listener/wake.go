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

// wakeReportBound is how long a wake report's publish may take. A report that does not go out in
// it costs only this wake: the daemon's backstop still brings the pod back. No caller waits it
// out — the two reports a 404 raises are published off that 404's path (reportRoleWake,
// reportAgentWake) — so it bounds the report's own goroutine and nothing else.
const wakeReportBound = 2 * time.Second

// wakeReport is the wire shape of a report on bus.LegionWakeTopic. At is this listener's own
// clock in epoch milliseconds, matching contracts.Envelope's KeptAt, because the stream's store
// time is the NATS server's clock and the daemon measures a wake against its own.
type wakeReport struct {
	Kind    string `json:"kind"`
	Role    string `json:"role,omitempty"`
	Session string `json:"session,omitempty"`
	At      int64  `json:"at"`
}

// publishWakeReport publishes one report on bus.LegionWakeTopic. It never fails anything: a
// report the bus does not take is logged and nothing else, since the 404 it was raised beside is
// answered whatever the bus does, and is answered without waiting for it (reportRoleWake).
func publishWakeReport(client *bus.Client, logger *logging.Logger, report wakeReport) {
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
	ctx, cancel := context.WithTimeout(context.Background(), wakeReportBound)
	defer cancel()
	if err := client.PublishWithin(ctx, item); err != nil {
		logger.Warn("legion wake report not published", slog.String("kind", report.Kind),
			slog.String("role", report.Role), slog.String("session", report.Session), slog.String("error", err.Error()))
		return
	}
	logger.Info("legion wake report published", slog.String("kind", report.Kind),
		slog.String("role", report.Role), slog.String("session", report.Session))
}

// reportRoleWake reports that a message was published to role and no live session held it. The
// role is the claim token a Legion role topic is named by, which is what the daemon matches its
// own released claims on.
//
// The report is published off the caller's path. The caller is writing a 404 the report owes
// nothing, and every role send with no holder raises one — including the common case where no
// daemon holds a released claim of that role at all — so publishing it inline would put
// wakeReportBound in front of each of those 404s.
func reportRoleWake(d *listenerDeps, logger *logging.Logger, role string) {
	if d == nil || role == "" {
		return
	}
	go publishWakeReport(d.client, logger, wakeReport{Kind: wakeKindRole, Role: role})
}

// reportAgentWake reports that a message was sent to the agent subject of sessionID and nothing
// could place it. It is published off the caller's path for the reason reportRoleWake is.
func reportAgentWake(client *bus.Client, logger *logging.Logger, sessionID string) {
	if sessionID == "" {
		return
	}
	go publishWakeReport(client, logger, wakeReport{Kind: wakeKindAgent, Session: sessionID})
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
		publishWakeReport(client, logger, wakeReport{Kind: wakeKindReady})
		ticker := time.NewTicker(interval)
		defer ticker.Stop()
		for {
			select {
			case <-ctx.Done():
				return
			case <-ticker.C:
				publishWakeReport(client, logger, wakeReport{Kind: wakeKindReady})
			}
		}
	}()
}
