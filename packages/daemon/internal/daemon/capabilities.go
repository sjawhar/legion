// Package daemon: this file holds the deployment's capabilities — what the configuration alone
// states (Deployment) and what a boot or a live session adds to it (supervision.deployment) — and
// the live sessions' capability reports: how they are kept (capabilityReportStore), taken from a
// session's ready (supervision.capabilityReported), copied for the state route
// (supervision.capabilityReports), read from the store at boot (reportsOfClaims), and logged when
// the open set changes (supervision.reportCapabilities).
package daemon

import (
	"context"
	"encoding/json"
	"maps"
	"slices"
	"strings"

	"github.com/sjawhar/legion/daemon/internal/capabilities"
	"github.com/sjawhar/legion/daemon/internal/claim"
	"github.com/sjawhar/legion/daemon/internal/config"
	"github.com/sjawhar/legion/daemon/internal/supervise"
)

// Deployment is the deployment's capabilities as the configuration alone states them
// (capabilities.Deployment): the decisions, the runtime, whether a broker is configured, and the
// roles whose pods reserve no CPU and memory (config.RoleResources.Reserved) — every workflow
// role, and the controller's when the daemon launches it. What boot learns (the broker login, the
// probe, model fallback) is the supervision's to add (supervision.deployment); `legion start
// --check-config` reports from this alone.
func Deployment(cfg config.Config) capabilities.Deployment {
	d := capabilities.Deployment{Decided: cfg.Capabilities.Decided, Runtime: cfg.Runtime.Name}
	k := cfg.Runtime.Kubernetes
	if k == nil {
		return d
	}
	d.AgentSecrets = k.AgentSecrets != nil
	roles := claim.Roles
	if cfg.ControllerLaunch == config.ControllerLaunchDaemon {
		roles = append(slices.Clone(roles), claim.RoleController)
	}
	for _, role := range roles {
		if !k.Resources[role].Reserved() {
			d.RolesWithoutResources = append(d.RolesWithoutResources, role)
		}
	}
	return d
}

// deployment is Deployment with what this boot learned: the broker login's state, whether the
// image passed its probe, the model-fallback mark the probe or the gate read, how many of the
// probe's attempts waited on the pool's capacity before one scheduled, with the scheduler's
// reason, and the live sessions' reports of the live rows (LEGION-663). A session is live when its
// claim is ready, working or idle with a process, and its report is that process's: a report from a
// claim since relaunched, suspended or retired names a session that no longer runs and contributes
// nothing, until the new process reports. The claims are read as their machines last published
// them (supervise.Machine.View), never waiting on a decision in flight, so the tick and the state
// route read the deployment while a relaunch waits on its pods; the sessions are in claim token
// order. It reads memory alone: no store, no context.
func (s *supervision) deployment() capabilities.Deployment {
	d := Deployment(s.cfg)
	if s.plan.secretsLogin != nil {
		d.SecretsLogin = s.plan.secretsLogin.LoginStatus().State
	}
	d.Probed, d.ModelFallback = s.probed, s.imageReport.ModelFallback
	d.ProbeCapacityWaits, d.ProbeCapacityReason = s.imageReport.CapacityWaits, s.imageReport.CapacityReason
	if s.supervisor == nil {
		return d
	}
	s.reportMu.Lock()
	defer s.reportMu.Unlock()
	for _, m := range s.supervisor.supervised() {
		c := m.View().Claim
		report, ok := s.reports[c.Token]
		if !ok || c.Locator == nil || report.Locator.Incarnation != c.Locator.Incarnation {
			continue
		}
		if claimTookRole(c.State) {
			d.Sessions = append(d.Sessions, capabilities.Session{Role: c.Role, Issue: c.Issue, Report: report})
		}
	}
	slices.SortFunc(d.Sessions, func(a, b capabilities.Session) int {
		return strings.Compare(string(a.Report.Claim), string(b.Report.Claim))
	})
	return d
}

// capabilityReportStore is the store's keeping of each claim's latest capability report
// (store.Store.PutCapabilityReport), as capabilityReported persists one.
type capabilityReportStore interface {
	PutCapabilityReport(ctx context.Context, report capabilities.Report) error
}

// capabilityReported takes a session's report of the live rows, which its ready carried
// (api.Options.CapabilityReported): kept as its claim's latest, persisted, and logged — one line
// naming the session and every row it measured, then one warning per row whose check failed, so
// an operator reading the log at the ready sees what this session lacks without waiting for the
// tick's report. A store that refuses the write is logged and the report kept in memory all the
// same: the daemon runs the session and reports what it lacks, so a lost persistence loses a
// report at the next boot, never a claim.
func (s *supervision) capabilityReported(ctx context.Context, c supervise.Claim, report capabilities.Report) {
	s.reportMu.Lock()
	if s.reports == nil {
		s.reports = map[claim.Token]capabilities.Report{}
	}
	s.reports[c.Token] = report
	s.reportMu.Unlock()
	if err := s.reportStore.PutCapabilityReport(ctx, report); err != nil {
		s.log.Error("capabilities: the session's report could not be persisted; it is kept until the daemon stops", "claim", c.Token, "error", err)
	}
	// A slice of strings and booleans always marshals.
	encoded, _ := json.Marshal(report.Rows)
	log := s.log.With("claim", c.Token, "role", c.Role, "issue", c.Issue,
		"locator", report.Locator.Label(), "incarnation", report.Locator.Incarnation)
	log.Info("capabilities: session reported", "elapsedMs", report.ElapsedMs, "rows", string(encoded))
	for _, row := range report.Rows {
		if !row.OK {
			log.Warn(capabilities.State{Name: row.Name, Detail: row.Detail}.OpenLine(), "capability", string(row.Name))
		}
	}
}

// capabilityReports is a copy of every claim's latest report, for the state route
// (source.reports): the projection shows each claim its own, whatever process reported it, with
// the incarnation a reader compares to the claim's locator.
func (s *supervision) capabilityReports() map[claim.Token]capabilities.Report {
	s.reportMu.Lock()
	defer s.reportMu.Unlock()
	return maps.Clone(s.reports)
}

// reportsOfClaims is the stored reports of claims, by claim: the store is shared across projects,
// and another legion's sessions are not this deployment's.
func reportsOfClaims(reports []capabilities.Report, claims []supervise.Claim) map[claim.Token]capabilities.Report {
	tokens := make(map[claim.Token]bool, len(claims))
	for _, c := range claims {
		tokens[c.Token] = true
	}
	own := make(map[claim.Token]capabilities.Report, len(reports))
	for _, report := range reports {
		if tokens[report.Claim] {
			own[report.Claim] = report
		}
	}
	return own
}

// reportCapabilities names the deployment capabilities with no decision, in the table's order, and
// logs the report whenever that set differs from the one last logged: once at boot, and again from
// a controller tick that finds it changed (admit.Admission.ReportCapabilities asks only when the
// tick queues a wake: a controller is registered and no tick notice is pending) — the broker's
// login reaching issued, and a live session's report of a row its check failed
// (capabilityReported), are the changes a running daemon sees — so a gap is logged at boot and
// at the first tick after a change, never on every tick, and never between ticks.
func (s *supervision) reportCapabilities() []string {
	d := s.deployment()
	open := d.Open()
	names := make([]string, len(open))
	for i, name := range open {
		names[i] = string(name)
	}
	s.reportMu.Lock()
	defer s.reportMu.Unlock()
	if !slices.Equal(names, s.reportedGaps) {
		d.Log(s.log)
		s.reportedGaps = names
	}
	return names
}
