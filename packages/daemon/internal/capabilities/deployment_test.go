package capabilities

import (
	"bytes"
	"context"
	"log/slog"
	"slices"
	"strings"
	"testing"

	"github.com/sjawhar/legion/daemon/internal/claim"
)

// satisfied is a kubernetes deployment that satisfies every deployment row, with its image probed.
var satisfied = Deployment{
	Runtime: "kubernetes", Probed: true, AgentSecrets: true, SecretsLogin: "issued", ModelFallback: "on",
}

// states is a report by capability.
func states(report []State) map[Name]State {
	m := map[Name]State{}
	for _, state := range report {
		m[state.Name] = state
	}
	return m
}

// A deployment that satisfies everything, its image probed, renders every row of Table in its
// order: the image rows present, the live and withheld rows as CheckImage renders them (the probe
// pod's log and `legion state` then read alike), the deployment rows present, and nothing open.
func TestReportRendersEveryRowInTableOrder(t *testing.T) {
	report := satisfied.Report()

	if len(report) != len(Table) {
		t.Fatalf("Report rendered %d rows, want one per Table row (%d)", len(report), len(Table))
	}
	probed, err := CheckImage(context.Background(), newStubs(t).image())
	if err != nil {
		t.Fatalf("CheckImage under the stubs: %v", err)
	}
	for i, row := range Table {
		state, line := report[i], probed[i]
		if state.Name != row.Name {
			t.Errorf("row %d is %s, want %s: the table's order", i, state.Name, row.Name)
		}
		want := map[Site]string{SiteImage: StatusPresent, SiteLive: StatusLive, SiteDeployment: StatusPresent, SiteWithheld: StatusWithheld}[row.Site]
		if state.Status != want {
			t.Errorf("%s: status %q, want %q", row.Name, state.Status, want)
		}
		if (row.Site == SiteLive || row.Site == SiteWithheld) && state.Detail != line.Detail {
			t.Errorf("%s: detail %q, want CheckImage's %q", row.Name, state.Detail, line.Detail)
		}
		if row.Site == SiteImage && state.Detail != "checked by the daemon's probe of the worker image, which passed" {
			t.Errorf("%s: detail %q, want the probe named", row.Name, state.Detail)
		}
		if state.Decision != "" || state.ConfigLine != "" {
			t.Errorf("%s: carries decision %q and config line %q, want neither on a row that is not decided or open", row.Name, state.Decision, state.ConfigLine)
		}
	}
	if open := satisfied.Open(); open != nil {
		t.Errorf("Open() = %v, want none", open)
	}
}

// Under tmux no probe runs, so the image rows are unchecked, saying why; a kubernetes daemon whose
// probe has not reported yet says that instead.
func TestImageRowsAreUncheckedUntilAProbeReports(t *testing.T) {
	for name, tc := range map[string]struct {
		deployment Deployment
		detail     string
	}{
		"tmux":                 {Deployment{Runtime: "tmux"}, "the tmux runtime runs the host's tools, which no probe checks"},
		"kubernetes, unprobed": {Deployment{Runtime: "kubernetes"}, "no probe has reported yet"},
	} {
		t.Run(name, func(t *testing.T) {
			for _, state := range tc.deployment.Report() {
				row, _ := Lookup(string(state.Name))
				if row.Site != SiteImage {
					continue
				}
				if state.Status != StatusUnchecked || state.Detail != tc.detail {
					t.Errorf("%s = %s (%s), want unchecked (%s)", state.Name, state.Status, state.Detail, tc.detail)
				}
			}
		})
	}
}

// Each deployment row's measurement: present when the deployment satisfies it, else open with the
// detail and the legion.yaml line that records a decision, or decided with the operator's reason
// when that line is already written. A decision on a satisfied row is moot, so the row stays
// present.
func TestDeploymentRowsArePresentDecidedOrOpen(t *testing.T) {
	decided := map[Name]string{Secrets: "dispatch://LEGION-205 enrolls pods later", ModelFallback: "one model", ResourceLimits: "one tree per node"}
	for _, tc := range []struct {
		name       string
		deployment Deployment
		row        Name
		status     string
		detail     string
	}{
		{"tmux enrolls nothing", Deployment{Runtime: "tmux"}, Secrets, StatusOpen, "the tmux runtime enrolls no process with the secrets broker"},
		{"no broker configured", Deployment{Runtime: "kubernetes"}, Secrets, StatusOpen, "runtime.kubernetes.agent_secrets is not configured"},
		{"a login not yet issued", Deployment{Runtime: "kubernetes", AgentSecrets: true, SecretsLogin: "pending"}, Secrets, StatusOpen, "the agent-secrets login is pending, not issued"},
		{"a login issued", satisfied, Secrets, StatusPresent, "every pod is enrolled with the agent-secrets broker runtime.kubernetes.agent_secrets names, and the daemon's login is issued"},
		{"a decided broker gap", Deployment{Runtime: "kubernetes", Decided: decided}, Secrets, StatusDecided, "runtime.kubernetes.agent_secrets is not configured"},
		{"a decision on a satisfied row is moot", Deployment{Runtime: "kubernetes", Probed: true, AgentSecrets: true, SecretsLogin: "issued", Decided: decided}, Secrets, StatusPresent, "every pod is enrolled with the agent-secrets broker runtime.kubernetes.agent_secrets names, and the daemon's login is issued"},

		{"fallback on", satisfied, ModelFallback, StatusPresent, "retry.modelFallback is true under the pod's Oh My Pi settings (read by the probe)"},
		{"fallback off", Deployment{Runtime: "kubernetes", ModelFallback: "off"}, ModelFallback, StatusOpen, "retry.modelFallback is false under the pod's Oh My Pi settings (read by the probe)"},
		{"fallback unread", Deployment{Runtime: "kubernetes"}, ModelFallback, StatusOpen, "not read: no probe has reported retry.modelFallback"},
		{"fallback off on the host", Deployment{Runtime: "tmux", ModelFallback: "off"}, ModelFallback, StatusOpen, "retry.modelFallback is false under the host's Oh My Pi settings (read by the plugin gate)"},
		{"fallback unread on the host", Deployment{Runtime: "tmux"}, ModelFallback, StatusOpen, "not read: the plugin gate could not read retry.modelFallback"},
		{"a decided fallback gap", Deployment{Runtime: "kubernetes", ModelFallback: "off", Decided: decided}, ModelFallback, StatusDecided, "retry.modelFallback is false under the pod's Oh My Pi settings (read by the probe)"},

		{"tmux sets no limits", Deployment{Runtime: "tmux"}, ResourceLimits, StatusOpen, "the tmux runtime sets no requests or limits on a pane"},
		{"every role reserved", satisfied, ResourceLimits, StatusPresent, "every role has CPU and memory requests and limits under runtime.kubernetes.resources"},
		{"roles without a reservation", Deployment{Runtime: "kubernetes", RolesWithoutResources: []claim.Role{claim.RoleTester, claim.RoleController}}, ResourceLimits, StatusOpen, "roles without CPU and memory requests and limits under runtime.kubernetes.resources: tester, controller"},
		{"a decided limits gap", Deployment{Runtime: "tmux", Decided: decided}, ResourceLimits, StatusDecided, "the tmux runtime sets no requests or limits on a pane"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			got := states(tc.deployment.Report())[tc.row]
			if got.Status != tc.status || got.Detail != tc.detail {
				t.Fatalf("%s = %s (%s), want %s (%s)", tc.row, got.Status, got.Detail, tc.status, tc.detail)
			}
			wantDecision, wantLine := "", ""
			switch tc.status {
			case StatusDecided:
				wantDecision = decided[tc.row]
			case StatusOpen:
				wantLine = `capabilities.decided.` + string(tc.row) + `: "<reason>"`
			}
			if got.Decision != wantDecision || got.ConfigLine != wantLine {
				t.Fatalf("%s carries decision %q and config line %q, want %q and %q", tc.row, got.Decision, got.ConfigLine, wantDecision, wantLine)
			}
		})
	}
}

// Open names the open deployment rows in Table order, and Log writes one warning per open row —
// the gap and the line that records a decision, as attributes too — and nothing for the rest.
func TestOpenAndLogNameTheGapsAlone(t *testing.T) {
	d := Deployment{Runtime: "kubernetes", Probed: true, ModelFallback: "on", Decided: map[Name]string{Secrets: "dispatch://LEGION-205 enrolls pods later"},
		RolesWithoutResources: []claim.Role{claim.RoleTester}}

	if got, want := d.Open(), []Name{ResourceLimits}; !slices.Equal(got, want) {
		t.Errorf("Open() = %v, want %v", got, want)
	}
	var logged bytes.Buffer
	d.Log(slog.New(slog.NewTextHandler(&logged, nil)))
	lines := strings.Split(strings.TrimSpace(logged.String()), "\n")
	if len(lines) != 1 {
		t.Fatalf("Log wrote %d lines, want one per open row (1):\n%s", len(lines), logged.String())
	}
	for _, want := range []string{
		`level=WARN`,
		`msg="capability resource-limits is open: roles without CPU and memory requests and limits under runtime.kubernetes.resources: tester; to record a decision, add to legion.yaml: capabilities.decided.resource-limits: \"<reason>\""`,
		`capability=resource-limits`,
		`detail="roles without CPU and memory requests and limits under runtime.kubernetes.resources: tester"`,
		`configLine="capabilities.decided.resource-limits: \"<reason>\""`,
	} {
		if !strings.Contains(lines[0], want) {
			t.Errorf("the warning lacks %s:\n%s", want, lines[0])
		}
	}

	three := Deployment{Runtime: "tmux"}
	if got, want := three.Open(), []Name{Secrets, ModelFallback, ResourceLimits}; !slices.Equal(got, want) {
		t.Errorf("Open() under an undecided tmux deployment = %v, want %v in Table order", got, want)
	}
}

// Every deployment row of Table has a measurement: a row added to the table without one would
// read as open with no detail.
func TestEveryDeploymentRowIsMeasured(t *testing.T) {
	for _, name := range Decidable() {
		if _, detail := (Deployment{Runtime: "kubernetes"}).measure(name); detail == "" || strings.HasPrefix(detail, "no measurement") {
			t.Errorf("deployment row %s has no measurement (%q)", name, detail)
		}
	}
}
