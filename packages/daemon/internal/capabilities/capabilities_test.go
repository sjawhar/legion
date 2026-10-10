package capabilities

import (
	"reflect"
	"slices"
	"strings"
	"testing"
	"unicode/utf8"

	"github.com/sjawhar/legion/daemon/internal/claim"
)

// every Name constant, so the table is held to the declared names and nothing else.
var names = []Name{
	Subagents, EvalJS, EvalPython, Browser, LSP, CodeGraph, WebSearch, Skills, MCP, RepositoryExtensions,
	DispatchEnvoyTools, GitHub, Secrets, ModelFallback, Toolchain, ResourceLimits, Network, OperatorSetup,
	ProductionIdentities,
}

// The table is the one declared list: every Name constant appears in it exactly once, and no row
// names anything else.
func TestTableNamesEveryCapabilityOnce(t *testing.T) {
	seen := map[Name]int{}
	for _, row := range Table {
		seen[row.Name]++
		if !slices.Contains(names, row.Name) {
			t.Errorf("Table names %q, which no Name constant declares", row.Name)
		}
		if row.Summary == "" {
			t.Errorf("row %s has no summary", row.Name)
		}
	}
	for _, name := range names {
		if seen[name] != 1 {
			t.Errorf("Table names %s %d times, want once", name, seen[name])
		}
	}
	if len(Table) != len(names) {
		t.Errorf("Table has %d rows, want the %d names", len(Table), len(names))
	}
}

// Every row is checked at one of the four sites.
func TestEveryRowHasASite(t *testing.T) {
	for _, row := range Table {
		if !slices.Contains([]Site{SiteImage, SiteLive, SiteDeployment, SiteWithheld}, row.Site) {
			t.Errorf("row %s is checked at %q, which is no site", row.Name, row.Site)
		}
	}
}

// The deployment rows are what a legion.yaml decided line may name, in the table's order.
func TestDecidableIsTheDeploymentRowsInTableOrder(t *testing.T) {
	if got, want := Decidable(), []Name{Secrets, ModelFallback, ResourceLimits}; !slices.Equal(got, want) {
		t.Errorf("Decidable() = %v, want %v", got, want)
	}
}

// The live rows are what each session measures at its start, in the table's order.
func TestLiveIsTheLiveRowsInTableOrder(t *testing.T) {
	if got, want := Live(), []Name{Subagents, WebSearch, MCP, RepositoryExtensions, DispatchEnvoyTools, GitHub}; !slices.Equal(got, want) {
		t.Errorf("Live() = %v, want %v", got, want)
	}
}

// Every live row cites the Legion issue whose check proves it: the unchecked row's detail names it
// (Deployment.Report), so a row with no ruling would print an empty parenthesis.
func TestEveryLiveRowHasARuling(t *testing.T) {
	for _, row := range Table {
		if row.Site != SiteLive {
			continue
		}
		if !strings.HasPrefix(row.Ruling, "dispatch://LEGION-") {
			t.Errorf("live row %s cites %q, want the dispatch://LEGION-<n> whose check proves it", row.Name, row.Ruling)
		}
	}
}

// A withheld row cites the Legion ruling that withholds it, and only Legion's own keys: the root
// AGENTS.md keeps the deployment repository's project key out of the tree.
func TestWithheldRowsCiteALegionRuling(t *testing.T) {
	for _, row := range Table {
		if row.Site != SiteWithheld {
			continue
		}
		if !strings.HasPrefix(row.Ruling, "dispatch://LEGION-") {
			t.Errorf("withheld row %s cites %q, want a dispatch://LEGION-<n> ruling", row.Name, row.Ruling)
		}
	}
	for _, row := range Table {
		for _, cited := range strings.Split(row.Ruling, ",") {
			if cited = strings.TrimSpace(cited); cited != "" && !strings.HasPrefix(cited, "dispatch://LEGION-") {
				t.Errorf("row %s cites %q, which is not a LEGION issue", row.Name, cited)
			}
		}
	}
}

// No row awaits a pod launch. The field exists for an image row whose tooling the image carries but
// a pod's agent cannot use yet, and CodeGraph was the one such row — a pod's agent was launched
// `--no-extensions` and never loaded the profile's plugin — until LEGION-629 turned extension
// discovery on in the pod's launch, so the row reads present like every other image row. Were a
// row to await again, the sentence is an image row's alone: a live, deployment or withheld row has
// nothing in the image to await a launch for.
func TestNoRowAwaitsAPodLaunch(t *testing.T) {
	for _, row := range Table {
		if row.Awaits != "" {
			t.Errorf("row %s awaits %q, want no row to await a pod launch", row.Name, row.Awaits)
		}
	}
}

// unreported is a normalised report as Normalize fills one out from a wire that sent nothing:
// every live row in Table order, not OK, saying the session did not report it — with the given
// rows in their names' place.
func unreported(sent ...Row) []Row {
	rows := make([]Row, 0, len(Live()))
	for _, name := range Live() {
		row := Row{Name: name, Detail: "not reported by this session"}
		for _, candidate := range sent {
			if candidate.Name == name {
				row = candidate
			}
		}
		rows = append(rows, row)
	}
	return rows
}

// Normalize turns what a session sent into the rows the daemon keeps: a name Table has no live
// row for is dropped and returned, a detail is one line (newlines, tabs and control characters
// out, whitespace runs collapsed) cut on a rune boundary when longer than MaxReportDetail, a
// failing row with no detail left reads "no detail reported" while a passing row's empty detail
// stays empty, a live row the wire lacks is filled in as not reported, the rows come out in Table
// order whatever order the wire sent them, and a name sent twice keeps the first.
func TestNormalizeKeepsTheLiveRowsInTableOrder(t *testing.T) {
	long := strings.Repeat("€", 400)
	for _, testCase := range []struct {
		name        string
		rows        []claim.CapabilityRow
		wantKept    []Row
		wantDropped []string
	}{
		{
			name:     "nothing sent",
			wantKept: unreported(),
		},
		{
			name: "drops an unknown name",
			rows: []claim.CapabilityRow{
				{Name: "subagents", OK: true, Detail: "4 agents"},
				{Name: "teleport", OK: false, Detail: "no such row"},
				{Name: "eval-js", OK: true, Detail: "an image row, not a live one"},
			},
			wantKept:    unreported(Row{Name: Subagents, OK: true, Detail: "4 agents"}),
			wantDropped: []string{"teleport", "eval-js"},
		},
		{
			name:     "cuts a long detail on a rune boundary",
			rows:     []claim.CapabilityRow{{Name: "web-search", OK: false, Detail: long}},
			wantKept: unreported(Row{Name: WebSearch, Detail: strings.Repeat("€", 341)}),
		},
		{
			name:     "keeps a detail of exactly the limit, cleaned before the cut",
			rows:     []claim.CapabilityRow{{Name: "mcp", OK: true, Detail: "\n " + strings.Repeat("a", MaxReportDetail) + "\t"}},
			wantKept: unreported(Row{Name: MCP, OK: true, Detail: strings.Repeat("a", MaxReportDetail)}),
		},
		{
			name:     "floors a failing row's empty detail",
			rows:     []claim.CapabilityRow{{Name: "github", OK: false, Detail: ""}},
			wantKept: unreported(Row{Name: GitHub, Detail: "no detail reported"}),
		},
		{
			name:     "floors a failing row's detail that is only whitespace",
			rows:     []claim.CapabilityRow{{Name: "github", OK: false, Detail: "  \n\t "}},
			wantKept: unreported(Row{Name: GitHub, Detail: "no detail reported"}),
		},
		{
			name:     "keeps a passing row's empty detail empty",
			rows:     []claim.CapabilityRow{{Name: "github", OK: true, Detail: ""}},
			wantKept: unreported(Row{Name: GitHub, OK: true, Detail: ""}),
		},
		{
			name:     "puts a detail on one line with its control characters out",
			rows:     []claim.CapabilityRow{{Name: "mcp", OK: false, Detail: "not connected: evil\nserver\x1b[31m (boom)"}},
			wantKept: unreported(Row{Name: MCP, Detail: "not connected: evil server [31m (boom)"}),
		},
		{
			name: "orders the rows as Table does",
			rows: []claim.CapabilityRow{
				{Name: "github", OK: true, Detail: "gh api user: octocat"},
				{Name: "dispatch-envoy-tools", OK: true, Detail: "dispatch read answered"},
				{Name: "repository-extensions", OK: true, Detail: "2 extensions"},
				{Name: "mcp", OK: true, Detail: "no server configured"},
				{Name: "web-search", OK: true, Detail: "web_search active"},
				{Name: "subagents", OK: true, Detail: "4 agents"},
			},
			wantKept: []Row{
				{Name: Subagents, OK: true, Detail: "4 agents"},
				{Name: WebSearch, OK: true, Detail: "web_search active"},
				{Name: MCP, OK: true, Detail: "no server configured"},
				{Name: RepositoryExtensions, OK: true, Detail: "2 extensions"},
				{Name: DispatchEnvoyTools, OK: true, Detail: "dispatch read answered"},
				{Name: GitHub, OK: true, Detail: "gh api user: octocat"},
			},
		},
		{
			name: "keeps the first of a name sent twice",
			rows: []claim.CapabilityRow{
				{Name: "mcp", OK: true, Detail: "first"},
				{Name: "mcp", OK: false, Detail: "second"},
			},
			wantKept: unreported(Row{Name: MCP, OK: true, Detail: "first"}),
		},
	} {
		t.Run(testCase.name, func(t *testing.T) {
			kept, dropped := Normalize(testCase.rows)

			if !reflect.DeepEqual(kept, testCase.wantKept) {
				t.Errorf("kept = %+v\nwant   %+v", kept, testCase.wantKept)
			}
			if !slices.Equal(dropped, testCase.wantDropped) {
				t.Errorf("dropped = %q, want %q", dropped, testCase.wantDropped)
			}
			for _, row := range kept {
				if len(row.Detail) > MaxReportDetail {
					t.Errorf("%s: detail is %d bytes, want at most %d", row.Name, len(row.Detail), MaxReportDetail)
				}
				if !utf8.ValidString(row.Detail) {
					t.Errorf("%s: detail %q is not valid UTF-8: the cut split a rune", row.Name, row.Detail)
				}
			}
		})
	}
}
