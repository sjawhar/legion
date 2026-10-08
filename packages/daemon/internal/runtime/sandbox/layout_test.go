package sandbox

import (
	"context"
	"fmt"
	"strings"
	"testing"

	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"

	"github.com/sjawhar/legion/daemon/internal/claim"
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

// The census passes both pods this runtime builds, an issue pod's six launchers and the project
// controller's one, and holds every other Sandbox to the launchers of the kind its labels name
// (podKindOf): the controller Sandbox a daemon before issue pods made (role=controller, one worker
// container), a pod missing a launcher or running one more, and a Sandbox whose labels name no kind
// are each refused, by name, before the daemon adopts, suspends or deletes any of them.
func TestTheBootCensusHoldsEverySandboxToTheLaunchersItsLabelsName(t *testing.T) {
	g := newRig(t, nil)
	g.spawn(controllerSpec(t))
	g.spawn(rootSpec(t))
	if err := rejectLegacyIssueSandboxes(g.ctx, g.dyn.Resource(sandboxGVR).Namespace(testNamespace), testProject); err != nil {
		t.Fatalf("census of a current controller pod and issue pod = %v, want none", err)
	}

	workflow := make([]string, 0, len(claim.Roles))
	for _, role := range claim.Roles {
		workflow = append(workflow, string(role))
	}
	issue := claimLabels(claim.RoleArchitect)
	controller := map[string]string{labelProject: testProject, labelRole: string(claim.RoleController)}
	for label, tc := range map[string]struct {
		name       string
		labels     map[string]string
		containers []string
		want       string
	}{
		"the controller pod of a daemon before issue pods": {"legion-legion-controller", controller, []string{"worker"}, "not exactly the launchers its labels name"},
		"a controller pod running the workflow launchers":  {"legion-legion-controller", controller, workflow, "not exactly the launchers its labels name"},
		"an issue pod missing a launcher":                  {"legion-legion-legion-208", issue, workflow[1:], "not exactly the launchers its labels name"},
		"an issue pod running a container more":            {"legion-legion-legion-208", issue, append(append([]string{}, workflow...), string(claim.RoleController)), "not exactly the launchers its labels name"},
		"a controller-labelled pod of a tree": {"legion-legion-other", map[string]string{labelProject: testProject, labelRole: string(claim.RoleController), labelTree: testTree},
			[]string{string(claim.RoleController)}, "name neither an issue nor the project controller"},
		"labels naming no role list": {"legion-legion-other", map[string]string{labelProject: testProject}, []string{string(claim.RoleController)}, "name neither an issue nor the project controller"},
	} {
		t.Run(label, func(t *testing.T) {
			sandboxes := newDynamic(t, sandboxRunning(t, tc.name, tc.labels, tc.containers...)).Resource(sandboxGVR).Namespace(testNamespace)
			err := rejectLegacyIssueSandboxes(context.Background(), sandboxes, testProject)
			if err == nil || !strings.Contains(err.Error(), tc.name) || !strings.Contains(err.Error(), tc.want) {
				t.Fatalf("census = %v, want a refusal naming %s: %q", err, tc.name, tc.want)
			}
		})
	}
}

// A namespace of a deployment before issue pods holds a per-claim Sandbox for every running role.
// The census's one refusal names every Sandbox it refuses, each with its reason, and how many, and
// none of the pods this runtime builds, so the operator clears them all before the next boot.
func TestTheBootCensusNamesEveryRefusedSandboxAndHowMany(t *testing.T) {
	g := newRig(t, nil)
	g.spawn(rootSpec(t))
	built := SandboxName(rootToken)
	legacy := map[string]string{
		"legion-legion-legion-208-tester":   "not exactly the launchers its labels name",
		"legion-legion-legion-208-reviewer": "not exactly the launchers its labels name",
		"legion-legion-controller":          "not exactly the launchers its labels name",
		"legion-legion-other":               "name neither an issue nor the project controller",
	}
	for name, labels := range map[string]map[string]string{
		"legion-legion-legion-208-tester":   claimLabels(claim.RoleTester),
		"legion-legion-legion-208-reviewer": claimLabels(claim.RoleReviewer),
		"legion-legion-controller":          {labelProject: testProject, labelRole: string(claim.RoleController)},
		"legion-legion-other":               {labelProject: testProject},
	} {
		if err := g.dyn.Tracker().Create(sandboxGVR, sandboxRunning(t, name, labels, "worker"), testNamespace); err != nil {
			t.Fatal(err)
		}
	}
	err := rejectLegacyIssueSandboxes(g.ctx, g.dyn.Resource(sandboxGVR).Namespace(testNamespace), testProject)
	if err == nil {
		t.Fatal("census = nil, want a refusal naming every legacy Sandbox")
	}
	if want := fmt.Sprintf("%d of project %s's Sandboxes are not pods this runtime builds", len(legacy), testProject); !strings.Contains(err.Error(), want) {
		t.Errorf("census = %q, want it to say %q", err, want)
	}
	for name, reason := range legacy {
		if !strings.Contains(err.Error(), "legacy issue Sandbox "+name) {
			t.Errorf("census = %q, want it to name %s", err, name)
		}
		_, after, _ := strings.Cut(err.Error(), "legacy issue Sandbox "+name)
		if next, _, _ := strings.Cut(after, "; legacy issue Sandbox "); !strings.Contains(next, reason) {
			t.Errorf("census names %s with %q, want %q", name, next, reason)
		}
	}
	if strings.Contains(err.Error(), "legacy issue Sandbox "+built+" ") || strings.Contains(err.Error(), "legacy issue Sandbox "+built+":") {
		t.Errorf("census = %q, which names the issue pod this runtime built, %s", err, built)
	}
}

// sandboxRunning is a Sandbox as the API would hold it, whose pod runs containers by these names.
func sandboxRunning(t *testing.T, name string, labels map[string]string, containers ...string) *unstructured.Unstructured {
	t.Helper()
	u := sandboxObject(t, name, "uid-"+name, modeRunning, labels)
	list := make([]any, 0, len(containers))
	for _, container := range containers {
		list = append(list, map[string]any{"name": container, "image": testImage})
	}
	if err := unstructured.SetNestedSlice(u.Object, list, "spec", "podTemplate", "spec", "containers"); err != nil {
		t.Fatal(err)
	}
	return u
}
