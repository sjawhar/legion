package sandbox

import (
	"strings"
	"testing"

	k8sruntime "k8s.io/apimachinery/pkg/runtime"
)

func TestLegacyIssueSandboxObjectsAreRefusedBeforeLaunchersRegister(t *testing.T) {
	legacy := sandboxObject(t, "legion-legion-legion-208-tester", "uid-legacy", modeRunning, claimLabels(workerSpec(t).Role))
	g := newRig(t, []k8sruntime.Object{legacy})
	err := g.r.rejectLegacyIssueSandboxes(g.ctx)
	if err == nil || !strings.Contains(err.Error(), "legacy issue Sandbox") || !strings.Contains(err.Error(), legacy.GetName()) {
		t.Fatalf("legacy object census = %v", err)
	}
}
