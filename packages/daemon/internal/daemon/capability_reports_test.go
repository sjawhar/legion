package daemon

import (
	"bytes"
	"context"
	"errors"
	"log/slog"
	"reflect"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/sjawhar/legion/daemon/internal/capabilities"
	"github.com/sjawhar/legion/daemon/internal/claim"
	"github.com/sjawhar/legion/daemon/internal/config"
	"github.com/sjawhar/legion/daemon/internal/promptrefs"
	"github.com/sjawhar/legion/daemon/internal/runtime"
	"github.com/sjawhar/legion/daemon/internal/supervise"
)

// reportingClaim is a claim of this project in state, on a tmux pane at incarnation "4242:1".
func reportingClaim(token claim.Token, role claim.Role, state supervise.ClaimState) supervise.Claim {
	return supervise.Claim{
		Token: token, Project: "legion", Tree: "LEGION-208", Issue: "LEGION-209", Role: role, Generation: 2,
		State: state,
		Locator: &runtime.Locator{
			Runtime: runtime.RuntimeTmux, Claim: token, Incarnation: "4242:1",
			Tmux: &runtime.TmuxLocator{Window: "@1", Pane: "%1"},
		},
	}
}

// sessionReport is a session's report of every live row from c's process at incarnation, every
// check passing but github's.
func sessionReport(c supervise.Claim, incarnation string) capabilities.Report {
	locator := *c.Locator
	locator.Incarnation = incarnation
	rows := make([]capabilities.Row, 0, len(capabilities.Live()))
	for _, name := range capabilities.Live() {
		row := capabilities.Row{Name: name, OK: true, Detail: string(name) + " checked"}
		if name == capabilities.GitHub {
			row.OK, row.Detail = false, "gh api user: HTTP 401"
		}
		rows = append(rows, row)
	}
	return capabilities.Report{
		Claim: c.Token, Generation: c.Generation, Locator: locator,
		MeasuredAt: time.Date(2026, 10, 10, 2, 18, 59, 0, time.UTC), ReportedAt: time.Date(2026, 10, 10, 2, 19, 0, 0, time.UTC),
		ElapsedMs: 1200, Rows: rows,
	}
}

// supervisorOf is a supervisor over machines for claims, as restore would build them, with nothing
// fed to them: deployment reads each machine's published claim.
func supervisorOf(t *testing.T, claims ...supervise.Claim) *supervisor {
	t.Helper()
	deps := supervise.Deps{
		Limits:   supervise.Limits{LaunchFailures: 2, PromptFailures: 2, PromptRetires: 2},
		Timeouts: supervise.Timeouts{Boot: time.Second, RegistrationIntervals: 2, RPC: time.Second, Probe: time.Second, Stop: time.Second},
	}
	all := make([]*supervise.Machine, 0, len(claims))
	for _, c := range claims {
		m, err := supervise.NewMachine(context.Background(), deps, c)
		if err != nil {
			t.Fatalf("build the machine of %s: %v", c.Token, err)
		}
		all = append(all, m)
	}
	sup := &supervisor{}
	sup.all.Store(&all)
	return sup
}

// capabilityState is the deployment report's row for name.
func capabilityState(t *testing.T, d capabilities.Deployment, name capabilities.Name) capabilities.State {
	t.Helper()
	for _, state := range d.Report() {
		if state.Name == name {
			return state
		}
	}
	t.Fatalf("the report has no %s row", name)
	return capabilities.State{}
}

// A session is live in the deployment when its claim is ready, working or idle with a process and
// its report is that process's: the report fills Sessions, in claim token order, and the rows it
// proved read present. A report from another incarnation — the claim relaunched since, and the new
// process not yet reported — a report of a suspended claim, and a claim with no process contribute
// nothing: the row stays unchecked, as a session that no longer runs proves nothing of the one that
// does.
func TestDeploymentRendersTheReportsOfLiveSessionsAlone(t *testing.T) {
	ready := reportingClaim("legion-LEGION-209-implementer", claim.RoleImplementer, supervise.StateReady)
	working := reportingClaim("legion-LEGION-209-architect", claim.RoleArchitect, supervise.StateWorking)
	idle := reportingClaim("legion-LEGION-209-tester", claim.RoleTester, supervise.StateIdle)
	relaunched := reportingClaim("legion-LEGION-209-planner", claim.RolePlanner, supervise.StateReady)
	suspended := reportingClaim("legion-LEGION-209-reviewer", claim.RoleReviewer, supervise.StateSuspended)
	processless := reportingClaim("legion-LEGION-209-merger", claim.RoleMerger, supervise.StateReady)
	// The ready of a claim whose machine held no process reported a locator naming the claim alone.
	bare := sessionReport(processless, "")
	bare.Locator = runtime.Locator{Claim: processless.Token}
	processless.Locator = nil
	unreported := reportingClaim("legion-LEGION-210-implementer", claim.RoleImplementer, supervise.StateReady)
	for _, tc := range []struct {
		name    string
		claims  []supervise.Claim
		reports map[claim.Token]capabilities.Report
		want    []capabilities.Session
		github  string
	}{
		{
			name:   "live claims with their process's report",
			claims: []supervise.Claim{working, idle, ready, unreported},
			reports: map[claim.Token]capabilities.Report{
				ready.Token:   sessionReport(ready, "4242:1"),
				working.Token: sessionReport(working, "4242:1"),
				idle.Token:    sessionReport(idle, "4242:1"),
			},
			want: []capabilities.Session{
				{Role: claim.RoleArchitect, Issue: "LEGION-209", Report: sessionReport(working, "4242:1")},
				{Role: claim.RoleImplementer, Issue: "LEGION-209", Report: sessionReport(ready, "4242:1")},
				{Role: claim.RoleTester, Issue: "LEGION-209", Report: sessionReport(idle, "4242:1")},
			},
			github: capabilities.StatusOpen,
		},
		{
			name:    "a report from the incarnation the claim relaunched from",
			claims:  []supervise.Claim{relaunched},
			reports: map[claim.Token]capabilities.Report{relaunched.Token: sessionReport(relaunched, "4242:0")},
			github:  capabilities.StatusUnchecked,
		},
		{
			name:    "a report of a suspended claim",
			claims:  []supervise.Claim{suspended},
			reports: map[claim.Token]capabilities.Report{suspended.Token: sessionReport(suspended, "4242:1")},
			github:  capabilities.StatusUnchecked,
		},
		{
			name:    "a report of a claim whose machine holds no process",
			claims:  []supervise.Claim{processless},
			reports: map[claim.Token]capabilities.Report{processless.Token: bare},
			github:  capabilities.StatusUnchecked,
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			s := &supervision{
				cfg:        config.Config{Runtime: config.Runtime{Name: "tmux"}},
				log:        quietLogger(),
				supervisor: supervisorOf(t, tc.claims...),
				reports:    tc.reports,
			}

			d := s.deployment()

			if !reflect.DeepEqual(d.Sessions, tc.want) {
				t.Fatalf("deployment sessions =\n%+v\nwant\n%+v", d.Sessions, tc.want)
			}
			if got := capabilityState(t, d, capabilities.GitHub).Status; got != tc.github {
				t.Errorf("the github row reads %s, want %s", got, tc.github)
			}
			subagents := capabilityState(t, d, capabilities.Subagents)
			if len(tc.want) > 0 && subagents.Status != capabilities.StatusPresent {
				t.Errorf("the subagents row reads %s (%s), want present", subagents.Status, subagents.Detail)
			}
			if len(tc.want) == 0 && subagents.Status != capabilities.StatusUnchecked {
				t.Errorf("the subagents row reads %s (%s), want unchecked", subagents.Status, subagents.Detail)
			}
		})
	}
}

// A supervision with no supervisor yet — `legion start --check-config`'s, and a bare test's —
// reports the deployment from what it has.
func TestDeploymentWithoutASupervisorRendersNoSession(t *testing.T) {
	s := &supervision{cfg: config.Config{Runtime: config.Runtime{Name: "tmux"}}, log: quietLogger()}
	if d := s.deployment(); d.Sessions != nil {
		t.Fatalf("deployment sessions = %+v, want none", d.Sessions)
	}
}

// fakeReportStore is the store's keeping of capability reports, with its refusal scripted.
type fakeReportStore struct {
	put []capabilities.Report
	err error
}

func (s *fakeReportStore) PutCapabilityReport(_ context.Context, report capabilities.Report) error {
	s.put = append(s.put, report)
	return s.err
}

// A session's report is kept as its claim's latest, persisted, and logged at the ready: one line
// naming the session, its process, how long the measuring took and every row as JSON, then one
// warning per failing row naming the capability — so the log says what this session lacks before
// the tick's report does. A store that refuses the write is logged at error and the report kept all
// the same: the daemon runs the session and reports what it lacks.
func TestCapabilityReportedKeepsPersistsAndLogsTheReport(t *testing.T) {
	c := reportingClaim("legion-LEGION-209-implementer", claim.RoleImplementer, supervise.StateReady)
	report := sessionReport(c, "4242:1")
	for _, tc := range []struct {
		name     string
		storeErr error
		errors   int
	}{
		{name: "the store keeps it"},
		{name: "the store refuses it", storeErr: errors.New("connection refused"), errors: 1},
	} {
		t.Run(tc.name, func(t *testing.T) {
			var logged bytes.Buffer
			store := &fakeReportStore{err: tc.storeErr}
			s := &supervision{
				cfg:         config.Config{Runtime: config.Runtime{Name: "tmux"}},
				log:         slog.New(slog.NewTextHandler(&logged, nil)),
				supervisor:  supervisorOf(t, c),
				reportStore: store,
			}

			s.capabilityReported(context.Background(), c, report)

			if !reflect.DeepEqual(store.put, []capabilities.Report{report}) {
				t.Errorf("the store was given %+v, want the report once", store.put)
			}
			if got := s.capabilityReports(); !reflect.DeepEqual(got, map[claim.Token]capabilities.Report{c.Token: report}) {
				t.Errorf("the reports kept = %+v, want the claim's", got)
			}
			if got := s.deployment().Sessions; len(got) != 1 || !reflect.DeepEqual(got[0].Report, report) {
				t.Errorf("deployment sessions = %+v, want the session with its report", got)
			}
			lines := strings.Split(strings.TrimSpace(logged.String()), "\n")
			var info, warn, errored []string
			for _, line := range lines {
				switch {
				case strings.Contains(line, "level=INFO"):
					info = append(info, line)
				case strings.Contains(line, "level=WARN"):
					warn = append(warn, line)
				case strings.Contains(line, "level=ERROR"):
					errored = append(errored, line)
				}
			}
			if len(info) != 1 || len(warn) != 1 || len(errored) != tc.errors {
				t.Fatalf("logged %d info, %d warn and %d error lines, want 1, 1 and %d:\n%s", len(info), len(warn), len(errored), tc.errors, logged.String())
			}
			for _, want := range []string{
				`msg="capabilities: session reported"`,
				"claim=legion-LEGION-209-implementer", "role=implementer", "issue=LEGION-209",
				`locator="pane @1:%1"`, "incarnation=4242:1", "elapsedMs=1200",
				`rows="[{\"name\":\"subagents\",\"ok\":true,\"detail\":\"subagents checked\"},` +
					`{\"name\":\"web-search\",\"ok\":true,\"detail\":\"web-search checked\"},` +
					`{\"name\":\"mcp\",\"ok\":true,\"detail\":\"mcp checked\"},` +
					`{\"name\":\"repository-extensions\",\"ok\":true,\"detail\":\"repository-extensions checked\"},` +
					`{\"name\":\"dispatch-envoy-tools\",\"ok\":true,\"detail\":\"dispatch-envoy-tools checked\"},` +
					`{\"name\":\"github\",\"ok\":false,\"detail\":\"gh api user: HTTP 401\"}]"`,
			} {
				if !strings.Contains(info[0], want) {
					t.Errorf("the info line lacks %s:\n%s", want, info[0])
				}
			}
			for _, want := range []string{
				`msg="capability github is open: gh api user: HTTP 401"`,
				"claim=legion-LEGION-209-implementer", "role=implementer", "issue=LEGION-209",
				`locator="pane @1:%1"`, "incarnation=4242:1", "capability=github",
			} {
				if !strings.Contains(warn[0], want) {
					t.Errorf("the warning lacks %s:\n%s", want, warn[0])
				}
			}
			if tc.errors == 1 {
				for _, want := range []string{"claim=legion-LEGION-209-implementer", `error="connection refused"`} {
					if !strings.Contains(errored[0], want) {
						t.Errorf("the error line lacks %s:\n%s", want, errored[0])
					}
				}
			}
		})
	}
}

// The store is shared across projects: boot keeps the reports of this project's claims alone.
func TestReportsOfClaimsKeepsThisProjectsAlone(t *testing.T) {
	own := reportingClaim("legion-LEGION-209-implementer", claim.RoleImplementer, supervise.StateReady)
	other := reportingClaim("widgets-WIDGETS-7-implementer", claim.RoleImplementer, supervise.StateReady)
	other.Project = "widgets"
	reports := []capabilities.Report{sessionReport(other, "4242:1"), sessionReport(own, "4242:1")}

	got := reportsOfClaims(reports, []supervise.Claim{own})

	if want := map[claim.Token]capabilities.Report{own.Token: sessionReport(own, "4242:1")}; !reflect.DeepEqual(got, want) {
		t.Fatalf("reportsOfClaims = %+v, want %+v", got, want)
	}
}

// The task agents the registration answers are sorted, and none when the prompts dispatch none
// (the API's register writes an empty list for a nil slice).
func TestPromptAgentsAreSorted(t *testing.T) {
	names := promptrefs.New()
	names.Text("roles/core/architect.md", []byte(`task(agent="plan-gap-analyst") then task(agent="deep-worker")`))
	names.Text("roles/core/reviewer.md", []byte(`task(agent="oracle") and skill://legion-worker`))
	if got, want := promptAgents(names), []string{"deep-worker", "oracle", "plan-gap-analyst"}; !slices.Equal(got, want) {
		t.Errorf("promptAgents = %v, want %v", got, want)
	}
	if got := promptAgents(promptrefs.New()); len(got) != 0 {
		t.Errorf("promptAgents with none = %v, want none", got)
	}
	if got := promptAgents(promptrefs.Names{}); len(got) != 0 {
		t.Errorf("promptAgents of the zero Names = %v, want none", got)
	}
}
