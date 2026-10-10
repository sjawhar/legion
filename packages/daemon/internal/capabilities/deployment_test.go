package capabilities

import (
	"bytes"
	"context"
	"fmt"
	"log/slog"
	"reflect"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/sjawhar/legion/daemon/internal/bootprobe"
	"github.com/sjawhar/legion/daemon/internal/claim"
	"github.com/sjawhar/legion/daemon/internal/runtime"
)

// satisfied is a kubernetes deployment that satisfies every deployment row, with its image probed.
var satisfied = Deployment{
	Runtime: "kubernetes", Probed: true, AgentSecrets: true, SecretsLogin: "issued", ModelFallback: bootprobe.ModelFallbackOn,
}

// states is a report by capability.
func states(report []State) map[Name]State {
	m := map[Name]State{}
	for _, state := range report {
		m[state.Name] = state
	}
	return m
}

// A deployment that satisfies everything, its image probed, with no session reporting yet,
// renders every row of Table in its order: the image rows present, the withheld rows as CheckImage
// renders them (the probe pod's log and `legion state` then read alike), the live rows unchecked
// with the ruling whose check proves each, the deployment rows present, and nothing open.
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
		want := map[Site]string{SiteImage: StatusPresent, SiteLive: StatusUnchecked, SiteDeployment: StatusPresent, SiteWithheld: StatusWithheld}[row.Site]
		if row.Awaits != "" {
			want = StatusInstalled
		}
		if state.Status != want {
			t.Errorf("%s: status %q, want %q", row.Name, state.Status, want)
		}
		if row.Site == SiteWithheld && state.Detail != line.Detail {
			t.Errorf("%s: detail %q, want CheckImage's %q", row.Name, state.Detail, line.Detail)
		}
		if row.Site == SiteLive && state.Detail != "no session has reported yet ("+row.Ruling+"): "+row.Summary {
			t.Errorf("%s: detail %q, want no session reported, the ruling, the summary", row.Name, state.Detail)
		}
		if row.Site == SiteImage && row.Awaits == "" && state.Detail != "checked by the daemon's probe of the worker image, which passed" {
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

// The codegraph row, once the probe passed, is present like every other image row: the probe proved
// the image carries the CLI and the plugin, and a pod's launch loads that plugin now that
// LEGION-629 turned extension discovery on. Before a probe reports, it is unchecked like every
// other image row.
func TestTheCodeGraphRowIsPresentOnceProbedAndUncheckedBefore(t *testing.T) {
	probed := states(satisfied.Report())[CodeGraph]
	want := State{Name: CodeGraph, Status: StatusPresent, Detail: "checked by the daemon's probe of the worker image, which passed"}
	if !reflect.DeepEqual(probed, want) {
		t.Errorf("probed codegraph row = %+v, want %+v", probed, want)
	}

	unprobed := states(Deployment{Runtime: "kubernetes"}.Report())[CodeGraph]
	if unprobed.Status != StatusUnchecked || unprobed.Detail != "no probe has reported yet" {
		t.Errorf("unprobed codegraph row = %+v, want unchecked, no probe having reported", unprobed)
	}
}

// measured is when the fixture sessions measured their rows; later is a later measurement.
var (
	measured = time.Date(2026, 10, 10, 2, 18, 59, 0, time.UTC)
	later    = measured.Add(90 * time.Second)
)

// rows is a normalised report with every live row proved, its detail "<name> proved", except the
// failing names, each not OK with the fact given.
func rows(failing map[Name]string) []Row {
	report := make([]Row, 0, len(Live()))
	for _, name := range Live() {
		if fact, ok := failing[name]; ok {
			report = append(report, Row{Name: name, Detail: fact})
			continue
		}
		report = append(report, Row{Name: name, OK: true, Detail: string(name) + " proved"})
	}
	return report
}

// podSession is a live session in an issue's pod — the role's container of the pod
// legion-omp-<issue> — which measured at measuredAt and whose report fails the given rows.
func podSession(issue string, role claim.Role, measuredAt time.Time, failing map[Name]string) Session {
	token := claim.Token(fmt.Sprintf("legion-omp-%s-%s", issue, role))
	return Session{Role: role, Issue: issue, Report: Report{
		Claim: token, Generation: 2,
		Locator: runtime.Locator{
			Runtime: runtime.RuntimeSandbox, Claim: token, Incarnation: runtime.SandboxIncarnation("3f2b1c7e", 2),
			Sandbox: &runtime.SandboxLocator{
				Namespace: "legion", Name: "legion-omp-" + strings.ToLower(issue), PodUID: "3f2b1c7e",
				Container: string(role), Generation: 2,
			},
		},
		MeasuredAt: measuredAt, ReportedAt: measuredAt.Add(time.Second), ElapsedMs: 1200,
		Rows: rows(failing),
	}}
}

// paneSession is a live session in a tmux pane, @3:%41, which measured at measuredAt and whose
// report fails the given rows.
func paneSession(issue string, role claim.Role, measuredAt time.Time, failing map[Name]string) Session {
	token := claim.Token(fmt.Sprintf("legion-omp-%s-%s", issue, role))
	return Session{Role: role, Issue: issue, Report: Report{
		Claim: token, Generation: 1,
		Locator: runtime.Locator{
			Runtime: runtime.RuntimeTmux, Claim: token, Incarnation: "31847:918273",
			Tmux: &runtime.TmuxLocator{Window: "@3", Pane: "%41"},
		},
		MeasuredAt: measuredAt, ReportedAt: measuredAt.Add(time.Second), ElapsedMs: 800,
		Rows: rows(failing),
	}}
}

// With no session reporting, every live row is unchecked, naming the issue whose check proves it
// and the row's summary: each detail is pinned whole, so a reworded summary or a moved ruling is
// read here.
func TestLiveRowsAreUncheckedUntilASessionReports(t *testing.T) {
	report := states(satisfied.Report())
	for name, want := range map[Name]string{
		Subagents:            "no session has reported yet (dispatch://LEGION-663): dispatches task subagents, each on the model its role configures",
		WebSearch:            "no session has reported yet (dispatch://LEGION-663): searches the web through the web-search tool",
		MCP:                  "no session has reported yet (dispatch://LEGION-663): reaches the MCP servers the deployment configures",
		RepositoryExtensions: "no session has reported yet (dispatch://LEGION-629): loads the Oh My Pi extensions the repository it works carries",
		DispatchEnvoyTools:   "no session has reported yet (dispatch://LEGION-663): reaches Dispatch through the `dispatch` command and Envoy through the pi-envoy tools",
		GitHub:               "no session has reported yet (dispatch://LEGION-631): reads and writes GitHub through its plain `gh` and `git` on its role's App token file",
	} {
		if got := report[name]; got.Status != StatusUnchecked || got.Detail != want {
			t.Errorf("%s = %s (%s), want unchecked (%s)", name, got.Status, got.Detail, want)
		}
	}
}

// A live row renders what the live sessions reported. Present when every session proved it, with
// the count and the latest measurement — the greatest MeasuredAt, named by its pod and container,
// role and issue, in RFC 3339 — and its fact; open when any session's check failed, naming each
// failing session as its pod or pane, the first three then how many more, and never a config
// line, since no legion.yaml line decides a session's check. One failing row leaves the others
// present.
func TestLiveRowsRenderWhatTheSessionsReported(t *testing.T) {
	noSearch := map[Name]string{WebSearch: "web_search is not among the active tools"}
	for _, testCase := range []struct {
		name     string
		sessions []Session
		want     map[Name]State
	}{
		{
			name:     "present with one session",
			sessions: []Session{podSession("LEGION-663", claim.RoleImplementer, measured, nil)},
			want: map[Name]State{
				Subagents: {Name: Subagents, Status: StatusPresent, Detail: "proved by 1 live session(s); latest pod legion-omp-legion-663/implementer (implementer, LEGION-663) measured 2026-10-10T02:18:59Z: subagents proved"},
				GitHub:    {Name: GitHub, Status: StatusPresent, Detail: "proved by 1 live session(s); latest pod legion-omp-legion-663/implementer (implementer, LEGION-663) measured 2026-10-10T02:18:59Z: github proved"},
			},
		},
		{
			name: "present with several sessions names the latest",
			sessions: []Session{
				podSession("LEGION-663", claim.RoleImplementer, measured, nil),
				podSession("LEGION-664", claim.RoleTester, later, nil),
				podSession("LEGION-665", claim.RoleArchitect, measured, nil),
			},
			want: map[Name]State{
				MCP: {Name: MCP, Status: StatusPresent, Detail: "proved by 3 live session(s); latest pod legion-omp-legion-664/tester (tester, LEGION-664) measured 2026-10-10T02:20:29Z: mcp proved"},
			},
		},
		{
			name:     "open with one failing session",
			sessions: []Session{podSession("LEGION-663", claim.RoleImplementer, measured, noSearch)},
			want: map[Name]State{
				WebSearch: {Name: WebSearch, Status: StatusOpen, Detail: "pod legion-omp-legion-663/implementer (implementer, LEGION-663): web_search is not among the active tools"},
			},
		},
		{
			name:     "open with a tmux pane",
			sessions: []Session{paneSession("LEGION-663", claim.RoleArchitect, measured, map[Name]string{GitHub: "gh api user: exit 1: gh: not logged in"})},
			want: map[Name]State{
				GitHub: {Name: GitHub, Status: StatusOpen, Detail: "pane @3:%41 (architect, LEGION-663): gh api user: exit 1: gh: not logged in"},
			},
		},
		{
			name: "open with five failing sessions names three",
			sessions: []Session{
				podSession("LEGION-663", claim.RoleImplementer, measured, noSearch),
				podSession("LEGION-664", claim.RoleTester, measured, noSearch),
				podSession("LEGION-665", claim.RoleArchitect, measured, noSearch),
				podSession("LEGION-666", claim.RolePlanner, measured, noSearch),
				podSession("LEGION-667", claim.RoleReviewer, measured, noSearch),
			},
			want: map[Name]State{
				WebSearch: {Name: WebSearch, Status: StatusOpen, Detail: "pod legion-omp-legion-663/implementer (implementer, LEGION-663): web_search is not among the active tools; " +
					"pod legion-omp-legion-664/tester (tester, LEGION-664): web_search is not among the active tools; " +
					"pod legion-omp-legion-665/architect (architect, LEGION-665): web_search is not among the active tools +2 more"},
			},
		},
		{
			name: "one failing session opens its row alone",
			sessions: []Session{
				podSession("LEGION-663", claim.RoleImplementer, measured, nil),
				podSession("LEGION-664", claim.RoleTester, later, map[Name]string{Subagents: "task agents are disabled"}),
			},
			want: map[Name]State{
				Subagents:            {Name: Subagents, Status: StatusOpen, Detail: "pod legion-omp-legion-664/tester (tester, LEGION-664): task agents are disabled"},
				WebSearch:            {Name: WebSearch, Status: StatusPresent, Detail: "proved by 2 live session(s); latest pod legion-omp-legion-664/tester (tester, LEGION-664) measured 2026-10-10T02:20:29Z: web-search proved"},
				MCP:                  {Name: MCP, Status: StatusPresent, Detail: "proved by 2 live session(s); latest pod legion-omp-legion-664/tester (tester, LEGION-664) measured 2026-10-10T02:20:29Z: mcp proved"},
				RepositoryExtensions: {Name: RepositoryExtensions, Status: StatusPresent, Detail: "proved by 2 live session(s); latest pod legion-omp-legion-664/tester (tester, LEGION-664) measured 2026-10-10T02:20:29Z: repository-extensions proved"},
				DispatchEnvoyTools:   {Name: DispatchEnvoyTools, Status: StatusPresent, Detail: "proved by 2 live session(s); latest pod legion-omp-legion-664/tester (tester, LEGION-664) measured 2026-10-10T02:20:29Z: dispatch-envoy-tools proved"},
				GitHub:               {Name: GitHub, Status: StatusPresent, Detail: "proved by 2 live session(s); latest pod legion-omp-legion-664/tester (tester, LEGION-664) measured 2026-10-10T02:20:29Z: github proved"},
			},
		},
	} {
		t.Run(testCase.name, func(t *testing.T) {
			d := satisfied
			d.Sessions = testCase.sessions

			report := states(d.Report())

			for name, want := range testCase.want {
				if got := report[name]; !reflect.DeepEqual(got, want) {
					t.Errorf("%s = %+v\nwant %+v", name, got, want)
				}
			}
			for _, name := range Live() {
				if got := report[name]; got.Status == StatusUnchecked {
					t.Errorf("%s is unchecked, want a status the %d sessions' reports decide", name, len(testCase.sessions))
				}
			}
		})
	}
}

// Open names every open row in Table order, the live rows a session's check failed among the
// deployment gaps; OpenFromConfiguration never names a live row, since no legion.yaml line
// decides one, so `legion start --check-config` says nothing of them.
func TestOpenNamesLiveRowsInTableOrder(t *testing.T) {
	d := Deployment{Runtime: "tmux", Decided: map[Name]string{ModelFallback: "the host's own", ResourceLimits: "a pane has none"},
		Sessions: []Session{paneSession("LEGION-663", claim.RoleArchitect, measured, map[Name]string{Subagents: "task agents are disabled", GitHub: "gh: not logged in"})}}

	if got, want := d.Open(), []Name{Subagents, GitHub, Secrets}; !slices.Equal(got, want) {
		t.Errorf("Open() = %v, want %v in Table order", got, want)
	}
	fromConfiguration := d.OpenFromConfiguration()
	if len(fromConfiguration) != 1 || fromConfiguration[0].Name != Secrets {
		t.Errorf("OpenFromConfiguration() = %+v, want the secrets row alone", fromConfiguration)
	}
}

// An open row's line names the gap and, where a legion.yaml line records a decision on it, that
// line; a live row, which no line decides, ends at the gap.
func TestOpenLineEndsAtTheGapWithoutAConfigLine(t *testing.T) {
	for _, testCase := range []struct {
		name  string
		state State
		want  string
	}{
		{
			name:  "a deployment row",
			state: State{Name: ResourceLimits, Status: StatusOpen, Detail: "the tmux runtime sets no requests or limits on a pane", ConfigLine: configLine(ResourceLimits)},
			want:  `capability resource-limits is open: the tmux runtime sets no requests or limits on a pane; to record a decision, add to legion.yaml: capabilities.decided.resource-limits: "<reason>"`,
		},
		{
			name:  "a live row",
			state: State{Name: Subagents, Status: StatusOpen, Detail: "pane @3:%41 (architect, LEGION-663): task agents are disabled"},
			want:  "capability subagents is open: pane @3:%41 (architect, LEGION-663): task agents are disabled",
		},
	} {
		t.Run(testCase.name, func(t *testing.T) {
			if got := testCase.state.OpenLine(); got != testCase.want {
				t.Errorf("OpenLine() = %q, want %q", got, testCase.want)
			}
		})
	}
}

// Log warns of an open live row as it does of a deployment gap — the sentence, the capability and
// the detail as attributes — with an empty configLine, since no legion.yaml line decides it.
func TestLogWarnsOfAnOpenLiveRow(t *testing.T) {
	d := satisfied
	d.Sessions = []Session{podSession("LEGION-663", claim.RoleImplementer, measured, map[Name]string{WebSearch: "web_search is not among the active tools"})}

	var logged bytes.Buffer
	d.Log(slog.New(slog.NewTextHandler(&logged, nil)))

	lines := strings.Split(strings.TrimSpace(logged.String()), "\n")
	if len(lines) != 1 {
		t.Fatalf("Log wrote %d lines, want one per open row (1):\n%s", len(lines), logged.String())
	}
	for _, want := range []string{
		`level=WARN`,
		`msg="capability web-search is open: pod legion-omp-legion-663/implementer (implementer, LEGION-663): web_search is not among the active tools"`,
		`capability=web-search`,
		`detail="pod legion-omp-legion-663/implementer (implementer, LEGION-663): web_search is not among the active tools"`,
		`configLine=""`,
	} {
		if !strings.Contains(lines[0], want) {
			t.Errorf("the warning lacks %s:\n%s", want, lines[0])
		}
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
			report := tc.deployment.Report()
			for i, row := range Table {
				if row.Site != SiteImage {
					continue
				}
				if state := report[i]; state.Status != StatusUnchecked || state.Detail != tc.detail {
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
		{"fallback off", Deployment{Runtime: "kubernetes", ModelFallback: bootprobe.ModelFallbackOff}, ModelFallback, StatusOpen, "retry.modelFallback is false under the pod's Oh My Pi settings (read by the probe)"},
		{"fallback unread", Deployment{Runtime: "kubernetes"}, ModelFallback, StatusOpen, "not read: no probe has reported retry.modelFallback"},
		{"fallback off on the host", Deployment{Runtime: "tmux", ModelFallback: bootprobe.ModelFallbackOff}, ModelFallback, StatusOpen, "retry.modelFallback is false under the host's Oh My Pi settings (read by the plugin gate)"},
		{"fallback unread on the host", Deployment{Runtime: "tmux"}, ModelFallback, StatusOpen, "not read: the plugin gate could not read retry.modelFallback"},
		{"a decided fallback gap", Deployment{Runtime: "kubernetes", ModelFallback: bootprobe.ModelFallbackOff, Decided: decided}, ModelFallback, StatusDecided, "retry.modelFallback is false under the pod's Oh My Pi settings (read by the probe)"},

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
	d := Deployment{Runtime: "kubernetes", Probed: true, ModelFallback: bootprobe.ModelFallbackOn, Decided: map[Name]string{Secrets: "dispatch://LEGION-205 enrolls pods later"},
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

// OpenFromConfiguration is what `legion start --check-config` prints: the open rows the file alone
// decides, each as Report renders it — secrets where no broker is configured, resource-limits where
// a role lacks a reservation — and never a configured broker's secrets row, whose login boot
// measures, nor model fallback, which the probe or the plugin gate reads.
func TestOpenFromConfigurationNamesTheGapsTheFileAloneDecides(t *testing.T) {
	lacking := []claim.Role{claim.RoleTester}
	for _, tc := range []struct {
		name       string
		deployment Deployment
		want       []Name
	}{
		{"no broker, a role lacking", Deployment{Runtime: "kubernetes", RolesWithoutResources: lacking}, []Name{Secrets, ResourceLimits}},
		{"no broker, every role reserved", Deployment{Runtime: "kubernetes"}, []Name{Secrets}},
		{"a broker whose login is not issued, a role lacking", Deployment{Runtime: "kubernetes", AgentSecrets: true, RolesWithoutResources: lacking}, []Name{ResourceLimits}},
		{"a broker whose login is not issued, every role reserved", Deployment{Runtime: "kubernetes", AgentSecrets: true}, nil},
		{"fallback off alone", Deployment{Runtime: "kubernetes", AgentSecrets: true, ModelFallback: bootprobe.ModelFallbackOff}, nil},
		{"tmux", Deployment{Runtime: "tmux"}, []Name{Secrets, ResourceLimits}},
		{"tmux, both decided", Deployment{Runtime: "tmux", Decided: map[Name]string{Secrets: "later", ResourceLimits: "one tree per node"}}, nil},
	} {
		t.Run(tc.name, func(t *testing.T) {
			got := tc.deployment.OpenFromConfiguration()
			var names []Name
			for _, state := range got {
				names = append(names, state.Name)
			}
			if !slices.Equal(names, tc.want) {
				t.Fatalf("OpenFromConfiguration() names %v, want %v", names, tc.want)
			}
			reported := states(tc.deployment.Report())
			for _, state := range got {
				if want := reported[state.Name]; !reflect.DeepEqual(state, want) {
					t.Errorf("%s = %+v, want Report's %+v", state.Name, state, want)
				}
			}
		})
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
