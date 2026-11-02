// The wake reports: the listener's one channel to the Legion daemons, on bus.WakeTopic. A message
// that reached nobody because the issue pod its recipient runs in was released is reported there,
// and so is the listener itself, at start and on a ticker, so a daemon can tell "the listener
// sends no reports" from "the listener has nothing to report".

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

// wakeReportBudget bounds one report's publish. A report is a hint, never a step of the request
// that produced it: the role send has already answered its caller 404 and the agent delivery has
// already been deferred or given up, so a bus that does not answer costs this long and one log
// line, and the next message to the same role or session reports again.
const wakeReportBudget = 2 * time.Second

// wakePublisher is all a report needs of the bus: one publish. A test substitutes its own.
type wakePublisher interface {
	PublishWithin(ctx context.Context, item contracts.Envelope) error
}

// wakeReporter publishes the listener's wake reports. It is nil on a listener built without a bus
// client — every test rig that wires no publisher — and every method of it tolerates that, so no
// path has to ask whether reports are on.
type wakeReporter struct {
	client    wakePublisher
	machineID string
	logger    *logging.Logger
	// now is the clock the reports are stamped by; a test replaces it.
	now func() time.Time
}

func newWakeReporter(client wakePublisher, machineID string, logger *logging.Logger) *wakeReporter {
	if client == nil {
		return nil
	}
	return &wakeReporter{client: client, machineID: machineID, logger: logger, now: time.Now}
}

// roleUnheld reports a message published to a role topic that has no live holder. The role token
// is what the daemon matches against its own released claims, and the envelope's event id is what
// makes the wake one wake however often the sender's publish is retried under the same envelope.
func (r *wakeReporter) roleUnheld(role string, item contracts.Envelope, reason string) {
	r.report(bus.WakeReport{Kind: bus.WakeKindRoleMessage, Role: role, ReportID: item.EventID, Reason: reason})
}

// sessionUnplaceable reports an agent delivery this listener could place nowhere. Every
// redelivery of one message reports the same id, so the pod is woken once however many attempts
// the retry window pays for.
func (r *wakeReporter) sessionUnplaceable(sessionID string, item contracts.Envelope, reason string) {
	r.report(bus.WakeReport{Kind: bus.WakeKindAgentMessage, Session: sessionID, ReportID: item.EventID, Reason: reason})
}

// run says the listener is listening, now and every bus.ListenerReadyInterval until ctx ends. The
// first report goes before the ticker's first tick: a daemon restarted beside the listener must
// not wait out an interval with release off.
func (r *wakeReporter) run(ctx context.Context) {
	if r == nil {
		return
	}
	r.report(bus.WakeReport{Kind: bus.WakeKindListenerReady})
	ticker := time.NewTicker(bus.ListenerReadyInterval)
	defer ticker.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
			r.report(bus.WakeReport{Kind: bus.WakeKindListenerReady})
		}
	}
}

// report publishes one report. It fills the listener's machine id and its own clock, in epoch
// milliseconds, and publishes under a budget of its own rather than the caller's: the caller has
// answered already, and a report must not outlive the request that noticed it was needed.
func (r *wakeReporter) report(report bus.WakeReport) {
	if r == nil || r.client == nil {
		return
	}
	report.Listener, report.At = r.machineID, r.now().UnixMilli()
	payload, err := json.Marshal(report)
	if err != nil {
		r.log(slog.LevelWarn, "listener wake report not encoded", report, err)
		return
	}
	item := contracts.Envelope{
		EventID:        id.New(),
		Source:         "envoy",
		SourceEventID:  id.New(),
		Topic:          bus.WakeTopic,
		DedupeKey:      "legion-wake." + id.New(),
		IssuedAt:       report.At,
		PayloadSummary: wakeReportSummary(report),
		Payload:        string(payload),
		TraceID:        id.New(),
	}
	if err := item.Validate(); err != nil {
		r.log(slog.LevelWarn, "listener wake report not valid", report, err)
		return
	}
	ctx, cancel := context.WithTimeout(context.Background(), wakeReportBudget)
	defer cancel()
	if err := r.client.PublishWithin(ctx, item); err != nil {
		r.log(slog.LevelWarn, "listener wake report not published", report, err)
		return
	}
	r.log(slog.LevelInfo, "listener wake report published", report, nil)
}

// wakeReportSummary is the one line an operator reading the stream sees.
func wakeReportSummary(report bus.WakeReport) string {
	switch report.Kind {
	case bus.WakeKindRoleMessage:
		return "wake " + report.Role
	case bus.WakeKindAgentMessage:
		return "wake session " + report.Session
	}
	return "listener ready"
}

func (r *wakeReporter) log(level slog.Level, line string, report bus.WakeReport, err error) {
	if r.logger == nil {
		return
	}
	attrs := []slog.Attr{slog.String("kind", report.Kind), slog.String("role", report.Role),
		slog.String("session", report.Session), slog.String("report_id", report.ReportID)}
	if err != nil {
		attrs = append(attrs, slog.String("error", err.Error()))
	}
	r.logger.LogEvent(level, line, attrs...)
}
