package capabilities

import (
	"slices"
	"strings"
	"testing"
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
