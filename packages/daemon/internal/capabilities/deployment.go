package capabilities

import (
	"fmt"
	"log/slog"
	"strings"

	"github.com/sjawhar/legion/daemon/internal/bootprobe"
	"github.com/sjawhar/legion/daemon/internal/claim"
)

// Deployment is what the daemon knows of its deployment's capabilities: what its configuration
// decides and sets, and what boot learned of the image, the broker and the pod's Oh My Pi. Report
// renders every row of Table from it. A gap is reported, never refused (LEGION-578, "The check"):
// a daemon that will not run its pods would itself keep workers from working.
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
}

// The statuses a State carries: an image row is present or installed (as a Line is) or unchecked,
// a live row live, a withheld row withheld, and a deployment row present, decided or open.
const (
	StatusPresent   = present
	StatusInstalled = installed
	StatusUnchecked = "unchecked"
	StatusLive      = live
	StatusWithheld  = withheld
	StatusDecided   = "decided"
	StatusOpen      = "open"
)

// State is one row of the deployment's report, as `legion state` carries it (api.CapabilityState).
type State struct {
	Name   Name
	Status string
	// Detail is the row's evidence: what the probe checked, the live check, the ruling, or the
	// deployment's measurement — whatever the status.
	Detail string
	// Decision is the operator's reason on a decided row, "" otherwise.
	Decision string
	// ConfigLine is, on an open row, the legion.yaml line that records a decision:
	// `capabilities.decided.<name>: "<reason>"`; "" otherwise.
	ConfigLine string
}

// OpenLine is the sentence an open row is logged with (Log) and `legion start --check-config`
// prints: the gap, and the line that records a decision on it.
func (s State) OpenLine() string {
	return fmt.Sprintf("capability %s is open: %s; to record a decision, add to legion.yaml: %s", s.Name, s.Detail, s.ConfigLine)
}

// Report renders every row of Table, in its order. An image row is present once the image passed
// the probe — installed where the row awaits a pod launch that loads what the image carries, the
// sentence that says so after the probe's — else unchecked; a live row is live, naming its check;
// a withheld row carries its ruling (both as CheckImage renders them); a deployment row is present
// when the deployment satisfies it, decided when legion.yaml records a decision on it, and open
// otherwise, with the line that records one.
func (d Deployment) Report() []State {
	states := make([]State, 0, len(Table))
	for _, row := range Table {
		state := State{Name: row.Name}
		switch row.Site {
		case SiteImage:
			state.Status, state.Detail = d.image(row)
		case SiteLive:
			state.Status, state.Detail = StatusLive, liveDetail(row)
		case SiteWithheld:
			state.Status, state.Detail = StatusWithheld, withheldDetail(row)
		case SiteDeployment:
			state = d.deployment(row.Name)
		}
		states = append(states, state)
	}
	return states
}

// Open is the deployment rows whose status is open, in Table order: the gaps with no decision.
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

// Log writes one warning per open row: the gap and the legion.yaml line that records a decision.
func (d Deployment) Log(log *slog.Logger) {
	for _, state := range d.openStates() {
		log.Warn(state.OpenLine(), "capability", string(state.Name), "detail", state.Detail, "configLine", state.ConfigLine)
	}
}

// openStates is the open rows, in Table order: only a deployment row is ever open, and Decidable
// names those in Table's order.
func (d Deployment) openStates() []State {
	var open []State
	for _, name := range Decidable() {
		if state := d.deployment(name); state.Status == StatusOpen {
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

// liveDetail is a live row's detail as CheckImage renders it: the check still to run, its issue,
// the summary. Nothing in the daemon runs a live check yet, so the row says the check is pending
// rather than passed.
func liveDetail(row Capability) string {
	check := "to be proved by a live check against a running pod"
	if row.Ruling != "" {
		check += " (" + row.Ruling + ")"
	}
	return check + ": " + row.Summary
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
