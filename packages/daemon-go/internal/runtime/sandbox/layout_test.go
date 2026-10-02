package sandbox

import (
	"strings"
	"testing"
)

// The boot census passes a project whose only Sandbox is an issue pod this runtime launched, and
// refuses one that still holds a per-claim Sandbox of the layout before issue pods, naming it.
func TestTheBootCensusRefusesLegacyIssueSandboxesOnly(t *testing.T) {
	g := newRig(t, nil)
	g.spawn(rootSpec(t))
	sandboxes := g.dyn.Resource(sandboxGVR).Namespace(testNamespace)
	if err := rejectLegacyIssueSandboxes(g.ctx, sandboxes, testProject); err != nil {
		t.Fatalf("census of a current issue pod = %v, want none", err)
	}
	legacy := sandboxObject(t, "legion-legion-legion-208-tester", "uid-legacy", modeRunning, claimLabels(workerSpec(t).Role))
	if err := g.dyn.Tracker().Create(sandboxGVR, legacy, testNamespace); err != nil {
		t.Fatal(err)
	}
	err := rejectLegacyIssueSandboxes(g.ctx, sandboxes, testProject)
	if err == nil || !strings.Contains(err.Error(), "legacy issue Sandbox") || !strings.Contains(err.Error(), legacy.GetName()) {
		t.Fatalf("legacy object census = %v", err)
	}
}
