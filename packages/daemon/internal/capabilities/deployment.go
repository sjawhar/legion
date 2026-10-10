package capabilities

import (
	"fmt"
	"log/slog"
	"strings"
	"time"

	"github.com/sjawhar/legion/daemon/internal/bootprobe"
	"github.com/sjawhar/legion/daemon/internal/claim"
)

// Deployment is what the daemon knows of its deployment's capabilities: what its configuration
// decides and sets, what boot learned of the image, the broker and the pod's Oh My Pi, and what
// its live sessions reported of the live rows. Report renders every row of Table from it. A gap is
// reported, never refused (LEGION-578, "The check"): a daemon that will not run its pods would
// itself keep workers from working.
type Deployment struct {
	// Decided is legion.yaml's capabilities.decided: the deployment rows the operator has decided,
	// each with the reason, which the report shows in the gap's place.
	Decided map[Name]string
	// Runtime is the daemon's runtime, "tmux" or "kubernetes".
	Runtime string
	// Probed is whether the worker image passed the daemon's probe (kubernetes), whose log carries
	// every image row's evidence; the image rows then read present, or installed where the row
	// awaits a pod launch that loads what the image carries (Capability.Awaits).
	Probed bool
	// AgentSecrets is whether runtime.kubernetes.agent_secrets is configured.
	AgentSecrets bool
	// SecretsLogin is the broker login's state (agentsecrets.LoginState.State: "none", "pending",
	// "issued", "denied" or "expired"); "" with no broker.
	SecretsLogin string
	// RolesWithoutResources are the roles whose pods reserve no CPU and memory: those lacking CPU
	// and memory requests and limits under runtime.kubernetes.resources, among claim.Roles and,
	// under controller: daemon, claim.RoleController.
	RolesWithoutResources []claim.Role
	// ModelFallback is whether the pod's Oh My Pi falls back to another model,
	// bootprobe.ModelFallbackOn or bootprobe.ModelFallbackOff as the probe's OK line reports it
	// (bootprobe.ImageReport) or the plugin gate read it under tmux, and "" when nothing has read
	// it.
	ModelFallback string
	// Sessions is every session the daemon judges live — its claim ready, working or idle, and its
	// report's incarnation its claim's current Locator's — each with its report of the live rows
	// (LEGION-663). The daemon filters; Report renders a live row from them.
	Sessions []Session
}

// The statuses a State carries: an image row is present or installed (as a Line is) or unchecked,
// a withheld row withheld, a deployment row present, decided or open, and a live row present, open
// or unchecked by what the live sessions reported.
const (
	StatusPresent   = present
	StatusInstalled = installed
	StatusUnchecked = "unchecked"
	StatusWithheld  = withheld
	StatusDecided   = "decided"
	StatusOpen      = "open"
)

// State is one row of the deployment's report, as `legion state` carries it (api.CapabilityState).
type State struct {
	Name   Name
	Status string
	// Detail is the row's evidence: what the probe checked, what the live sessions reported, the
	// ruling, or the deployment's measurement — whatever the status.
	Detail string
	// Decision is the operator's reason on a decided row, "" otherwise.
	Decision string
	// ConfigLine is, on an open row, the legion.yaml line that records a decision:
	// `capabilities.decided.<name>: "<reason>"`; "" otherwise.
	ConfigLine string
}

// OpenLine is the sentence an open row is logged with (Log) and `legion start --check-config`
// prints: the gap and, on a deployment row, the line that records a decision. A live row has no
// such line: a session's failing check is a fact to fix, not a gap an operator decides.
func (s State) OpenLine() string {
	if s.ConfigLine == "" {
		return fmt.Sprintf("capability %s is open: %s", s.Name, s.Detail)
	}
	return fmt.Sprintf("capability %s is open: %s; to record a decision, add to legion.yaml: %s", s.Name, s.Detail, s.ConfigLine)
}

// Report renders every row of Table, in its order. An image row is present once the image passed
// the probe — installed where the row awaits a pod launch that loads what the image carries, the
// sentence that says so after the probe's — else unchecked; a live row is open when a live
// session's check of it failed, present when none failed and one proved it, and unchecked when no
// session has reported; a withheld row carries its ruling (as CheckImage renders it); a deployment
// row is present when the deployment satisfies it, decided when legion.yaml records a decision on
// it, and open otherwise, with the line that records one.
func (d Deployment) Report() []State {
	states := make([]State, 0, len(Table))
	for _, row := range Table {
		state := State{Name: row.Name}
		switch row.Site {
		case SiteImage:
			state.Status, state.Detail = d.image(row)
		case SiteLive:
			state = d.live(row)
		case SiteWithheld:
			state.Status, state.Detail = StatusWithheld, withheldDetail(row)
		case SiteDeployment:
			state = d.deployment(row.Name)
		}
		states = append(states, state)
	}
	return states
}

// Open is the rows whose status is open, in Table order: the deployment gaps with no decision, and
// the live rows a live session's check failed.
func (d Deployment) Open() []Name {
	var open []Name
	for _, state := range d.openStates() {
		open = append(open, state.Name)
	}
	return open
}

// OpenFromConfiguration is the open rows the configuration alone decides, in Table order, which
// `legion start --check-config` prints: resource-limits, and secrets where no broker is configured.
// The rest is boot's to measure — a configured broker's login, and model fallback, which the probe
// or the plugin gate reads — so the check says nothing of them.
func (d Deployment) OpenFromConfiguration() []State {
	var open []State
	for _, state := range d.openStates() {
		if state.Name == ResourceLimits || (state.Name == Secrets && !d.AgentSecrets) {
			open = append(open, state)
		}
	}
	return open
}

// Log writes one warning per open row: the gap and, on a deployment row, the legion.yaml line that
// records a decision; a live row's configLine is "".
func (d Deployment) Log(log *slog.Logger) {
	for _, state := range d.openStates() {
		log.Warn(state.OpenLine(), "capability", string(state.Name), "detail", state.Detail, "configLine", state.ConfigLine)
	}
}

// openStates is the open rows of Report, in Table order: a deployment row with no decision, or a
// live row a live session's check failed; no image or withheld row is ever open.
func (d Deployment) openStates() []State {
	var open []State
	for _, state := range d.Report() {
		if state.Status == StatusOpen {
			open = append(open, state)
		}
	}
	return open
}

// image is one image row's status: present once the probe passed, which checked each — installed
// where the row awaits a pod launch that loads what the probe proved the image carries, since the
// report names what a worker lacks and no pod's agent has that tool yet; unchecked where nothing
// has checked — the tmux runtime runs the host's tools, which no probe checks, and a kubernetes
// daemon's probe may not have reported yet.
func (d Deployment) image(row Capability) (string, string) {
	switch {
	case d.Probed && row.Awaits != "":
		return StatusInstalled, "the image carries it (checked by the daemon's probe of the worker image, which passed); " + row.Awaits
	case d.Probed:
		return StatusPresent, "checked by the daemon's probe of the worker image, which passed"
	case d.Runtime == "tmux":
		return StatusUnchecked, "the tmux runtime runs the host's tools, which no probe checks"
	default:
		return StatusUnchecked, "no probe has reported yet"
	}
}

// live is one live row's state from the live sessions' reports. Open when any session's check of
// it failed: each failing session, in Sessions order, as its label and the fact its check found,
// the first three then how many more. Present when none failed and at least one proved it: how
// many did, and the latest measurement — the greatest MeasuredAt, the first in Sessions order on
// a tie — with its fact. Unchecked when no live session has reported: the ruling whose check
// proves the row, and the summary.
func (d Deployment) live(row Capability) State {
	state := State{Name: row.Name}
	var named []string
	failed, proved, latest := 0, 0, -1
	for i, session := range d.Sessions {
		measured, ok := session.row(row.Name)
		switch {
		case !ok:
			continue
		case !measured.OK:
			// The first three failing sessions are named; the rest are counted, so a swarm's worth
			// of sessions never fills the line.
			if failed++; failed <= maxFailingNamed {
				named = append(named, session.label()+": "+measured.Detail)
			}
			continue
		}
		proved++
		if latest < 0 || session.Report.MeasuredAt.After(d.Sessions[latest].Report.MeasuredAt) {
			latest = i
		}
	}
	switch {
	case failed > 0:
		state.Status, state.Detail = StatusOpen, strings.Join(named, "; ")
		if more := failed - len(named); more > 0 {
			state.Detail += fmt.Sprintf(" +%d more", more)
		}
	case proved > 0:
		session := d.Sessions[latest]
		measured, _ := session.row(row.Name)
		state.Status = StatusPresent
		state.Detail = fmt.Sprintf("proved by %d live session(s); latest %s measured %s: %s",
			proved, session.label(), session.Report.MeasuredAt.Format(time.RFC3339), measured.Detail)
	default:
		state.Status, state.Detail = StatusUnchecked, "no session has reported yet ("+row.Ruling+"): "+row.Summary
	}
	return state
}

// maxFailingNamed is how many failing sessions an open live row's detail names before it counts
// the rest.
const maxFailingNamed = 3

// row is s's measurement of name, and whether its report carries one.
func (s Session) row(name Name) (Row, bool) {
	for _, row := range s.Report.Rows {
		if row.Name == name {
			return row, true
		}
	}
	return Row{}, false
}

// label is how a live row's detail names s: its process (runtime.Locator.Label), then its claim's
// role and issue.
func (s Session) label() string {
	return fmt.Sprintf("%s (%s, %s)", s.Report.Locator.Label(), s.Role, s.Issue)
}

// withheldDetail is a withheld row's detail as CheckImage renders it: the ruling, the summary.
func withheldDetail(row Capability) string { return row.Ruling + ": " + row.Summary }

// deployment is one deployment row: its measurement, and the status that follows. A decision on a
// row the deployment satisfies is moot — the measurement stands, and the row reads present.
func (d Deployment) deployment(name Name) State {
	satisfied, detail := d.measure(name)
	state := State{Name: name, Detail: detail}
	switch {
	case satisfied:
		state.Status = StatusPresent
	case d.Decided[name] != "":
		state.Status, state.Decision = StatusDecided, d.Decided[name]
	default:
		state.Status, state.ConfigLine = StatusOpen, configLine(name)
	}
	return state
}

// configLine is the legion.yaml line that records a decision on name.
func configLine(name Name) string {
	return fmt.Sprintf("capabilities.decided.%s: \"<reason>\"", name)
}

// measure is one deployment row's measurement: whether the deployment satisfies it, and the detail
// that says how.
func (d Deployment) measure(name Name) (bool, string) {
	tmux := d.Runtime == "tmux"
	switch name {
	case Secrets:
		switch {
		case tmux:
			return false, "the tmux runtime enrolls no process with the secrets broker"
		case !d.AgentSecrets:
			return false, "runtime.kubernetes.agent_secrets is not configured"
		case d.SecretsLogin == "issued":
			return true, "every pod is enrolled with the agent-secrets broker runtime.kubernetes.agent_secrets names, and the daemon's login is issued"
		default:
			return false, fmt.Sprintf("the agent-secrets login is %s, not issued", d.SecretsLogin)
		}
	case ModelFallback:
		where, reader := "the pod's Oh My Pi settings (read by the probe)", "no probe has reported"
		if tmux {
			where, reader = "the host's Oh My Pi settings (read by the plugin gate)", "the plugin gate could not read"
		}
		switch d.ModelFallback {
		case bootprobe.ModelFallbackOn:
			return true, "retry.modelFallback is true under " + where
		case bootprobe.ModelFallbackOff:
			return false, "retry.modelFallback is false under " + where
		default:
			return false, "not read: " + reader + " retry.modelFallback"
		}
	case ResourceLimits:
		switch {
		case tmux:
			return false, "the tmux runtime sets no requests or limits on a pane"
		case len(d.RolesWithoutResources) == 0:
			return true, "every role has CPU and memory requests and limits under runtime.kubernetes.resources"
		default:
			roles := make([]string, len(d.RolesWithoutResources))
			for i, role := range d.RolesWithoutResources {
				roles[i] = string(role)
			}
			return false, "roles without CPU and memory requests and limits under runtime.kubernetes.resources: " + strings.Join(roles, ", ")
		}
	}
	// Table names no other deployment row; TestEveryDeploymentRowIsMeasured holds it to this switch.
	return false, "no measurement"
}
