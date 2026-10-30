// Ordinary, always-visible unit tests for the Stage 4a update-round check's pure logic that
// live_pod_update_pure_test.go defines with no e2e build tag.
package sandbox

import (
	"encoding/json"
	"slices"
	"strings"
	"testing"
	"time"

	corev1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/api/equality"
	"k8s.io/apimachinery/pkg/api/resource"

	"github.com/sjawhar/legion/daemon/internal/claim"
	"github.com/sjawhar/legion/daemon/internal/workspace"
)

// Both workers of pod-update's issue push that issue's branch onto the run's repository, which
// every Stage 4a run shares, so two runs at once must push different branches, and neither may
// push cache-b's. The key is a claim token's issue, the same key every time the run asks for it,
// and of the shape a later run's sweep takes a killed run's leftover by.
func TestEachRunsPodUpdateIssueNamesABranchOfItsOwn(t *testing.T) {
	runs := []string{"s4a-20261019144300-4108", "s4a-20261019144300-4109", "s4a-20261019145627-1308"}
	branches := map[string]string{}
	for _, project := range runs {
		issue := podUpdateRunIssue(project)
		if !claim.IsIssueKey(issue) {
			t.Fatalf("podUpdateRunIssue(%q) = %q, not an issue key", project, issue)
		}
		if _, err := claim.NewToken("s4a20261019144300", issue, claim.RoleArchitect); err != nil {
			t.Fatalf("podUpdateRunIssue(%q) = %q, which a claim token refuses: %v", project, issue, err)
		}
		if again := podUpdateRunIssue(project); again != issue {
			t.Fatalf("podUpdateRunIssue(%q) is %q, then %q", project, issue, again)
		}
		if cached := cacheRunIssue(project); cached == issue {
			t.Fatalf("run %s gives pod-update and cache-b the one issue %q, so they push one branch", project, issue)
		}
		branch := workspace.Bookmark(issue)
		if other, taken := branches[branch]; taken {
			t.Fatalf("runs %s and %s both push %s", other, project, branch)
		}
		branches[branch] = project
		// A run killed past its test timeout leaves this branch behind, and the next run's
		// cache-lost-volume sweep is what deletes it: that sweep takes the branch only at the
		// harness's own commit and only once it is older than any run lives.
		old := time.Now().Add(-staleRunBranchAge - time.Minute)
		left := []runBranch{{name: branch, message: runCommitMessage(branch), committed: old}}
		if stale := staleRunBranches(left, time.Now()); !slices.Equal(stale, []string{branch}) {
			t.Fatalf("a killed run's %s is swept as %q, want the branch itself: its shape is not the sweep's", branch, stale)
		}
	}
}

// The live check moves the shape a Sandbox records and then holds the runtime's own answer to one
// reason, so what movedPodShape writes and what podShapeDrift says of it must agree: the moved
// shape is the recorded one with its disk alone moved, and the pod's one drift reason is that
// disk's, in the words the check compares. A disk spelled otherwise moves the same way, since the
// amount, not the spelling, is what drift compares.
func TestTheMovedShapeDriftsByItsDiskAlone(t *testing.T) {
	image := "example.test/legion/worker@sha256:" + strings.Repeat("a", 64)
	for name, disk := range map[string]string{"the run's own disk": "20Gi", "the same amount spelled otherwise": "20480Mi"} {
		t.Run(name, func(t *testing.T) {
			record := podShapeRecord{
				Image: image,
				Resources: corev1.ResourceRequirements{
					Requests: corev1.ResourceList{corev1.ResourceCPU: resource.MustParse("2"), corev1.ResourceMemory: resource.MustParse("4Gi")},
					Limits:   corev1.ResourceList{corev1.ResourceCPU: resource.MustParse("2"), corev1.ResourceMemory: resource.MustParse("4Gi")},
				},
				Volumes: []corev1.Volume{{Name: "route", VolumeSource: corev1.VolumeSource{EmptyDir: &corev1.EmptyDirVolumeSource{}}}},
				Mounts:  []corev1.VolumeMount{{Name: "route", MountPath: "/var/run/legion/route"}},
				Disk:    disk,
			}
			encoded, err := json.Marshal(record)
			if err != nil {
				t.Fatal(err)
			}
			held := string(encoded)
			moved, reason, err := movedPodShape(held)
			if err != nil {
				t.Fatal(err)
			}
			var after podShapeRecord
			if err := json.Unmarshal([]byte(moved), &after); err != nil {
				t.Fatalf("the moved shape %q cannot be read: %v", moved, err)
			}
			if after.Disk == record.Disk {
				t.Fatalf("the moved shape holds the recorded disk %s", after.Disk)
			}
			after.Disk = record.Disk
			if !equality.Semantic.DeepEqual(after, record) {
				t.Fatalf("the moved shape moved more than the disk: %+v, want %+v", after, record)
			}
			s := &sandbox{Spec: sandboxSpec{PodTemplate: podTemplate{
				Metadata: podMetadata{Annotations: map[string]string{podShapeAnnotation: moved}},
			}}}
			if drifted := podShapeDrift(s, held); !slices.Equal(drifted, []string{reason}) {
				t.Fatalf("podShapeDrift of the moved shape = %q, want the one reason the check holds it to, %q", drifted, reason)
			}
		})
	}
}

// A shape no launch of this release wrote is not something the check moves on: it says so instead
// of patching a Sandbox with a shape of its own invention.
func TestAnUnreadableShapeIsNotMoved(t *testing.T) {
	for name, held := range map[string]string{
		"no shape at all":    "",
		"not a shape":        "{",
		"no disk in it":      `{"image":"example.test/legion/worker@sha256:a","resources":{},"disk":""}`,
		"an unreadable disk": `{"image":"example.test/legion/worker@sha256:a","resources":{},"disk":"twenty"}`,
	} {
		t.Run(name, func(t *testing.T) {
			if moved, reason, err := movedPodShape(held); err == nil {
				t.Fatalf("movedPodShape(%q) = %q, %q, nil; want a refusal", held, moved, reason)
			}
		})
	}
}

// pod-update-fleet drifts the pods of two issues with one change, and both issues' workers push
// their own issue's branch onto the run's repository, which every Stage 4a run shares. The two
// keys must therefore be two, neither of them cache-b's, and each of the shape a later run's sweep
// takes a killed run's leftover by.
func TestTheFleetRoundsTwoIssuesNameBranchesOfTheirOwn(t *testing.T) {
	runs := []string{"s4a-20261030101500-4108", "s4a-20261030101500-4109", "s4a-20261030112233-1308"}
	branches := map[string]string{}
	for _, project := range runs {
		keys := map[string]string{
			"pod-update":       podUpdateRunIssue(project),
			"pod-update-fleet": podUpdateFleetRunIssue(project),
			"cache-b":          cacheRunIssue(project),
		}
		if keys["pod-update-fleet"] == keys["pod-update"] || keys["pod-update-fleet"] == keys["cache-b"] {
			t.Fatalf("run %s gives %v one issue key, so two checkpoints push one branch", project, keys)
		}
		if again := podUpdateFleetRunIssue(project); again != keys["pod-update-fleet"] {
			t.Fatalf("podUpdateFleetRunIssue(%q) is %q, then %q", project, keys["pod-update-fleet"], again)
		}
		for name, issue := range keys {
			if !claim.IsIssueKey(issue) {
				t.Fatalf("%s's issue in run %s is %q, not an issue key", name, project, issue)
			}
			if _, err := claim.NewToken("s4a20261030101500", issue, claim.RoleArchitect); err != nil {
				t.Fatalf("%s's issue %q is one a claim token refuses: %v", name, issue, err)
			}
			branch := workspace.Bookmark(issue)
			if other, taken := branches[branch]; taken {
				t.Fatalf("%s of run %s and %s both push %s", name, project, other, branch)
			}
			branches[branch] = name + " of run " + project
			// A run killed past its test timeout leaves this branch behind, and the next run's
			// cache-lost-volume sweep is what deletes it.
			old := time.Now().Add(-staleRunBranchAge - time.Minute)
			left := []runBranch{{name: branch, message: runCommitMessage(branch), committed: old}}
			if stale := staleRunBranches(left, time.Now()); !slices.Equal(stale, []string{branch}) {
				t.Fatalf("a killed run's %s is swept as %q, want the branch itself: its shape is not the sweep's", branch, stale)
			}
		}
	}
}

// The fleet check's premise: every issue pod of one runtime is built from the one configuration,
// so the one moved shape the operator writes onto each Sandbox drifts every one of them, by the
// same one reason the check holds each worker's PodDrift to. A pod built from another
// configuration is another fleet, and the check says so rather than patching a shape of its own
// invention over it.
func TestOneMovedShapeDriftsEveryPodBuiltFromTheHeldOne(t *testing.T) {
	image := "example.test/legion/worker@sha256:" + strings.Repeat("b", 64)
	record := podShapeRecord{
		Image: image,
		Resources: corev1.ResourceRequirements{
			Requests: corev1.ResourceList{corev1.ResourceCPU: resource.MustParse("2"), corev1.ResourceMemory: resource.MustParse("4Gi")},
			Limits:   corev1.ResourceList{corev1.ResourceCPU: resource.MustParse("2"), corev1.ResourceMemory: resource.MustParse("4Gi")},
		},
		Volumes: []corev1.Volume{{Name: "route", VolumeSource: corev1.VolumeSource{EmptyDir: &corev1.EmptyDirVolumeSource{}}}},
		Mounts:  []corev1.VolumeMount{{Name: "route", MountPath: "/var/run/legion/route"}},
		Disk:    "20Gi",
	}
	encoded, err := json.Marshal(record)
	if err != nil {
		t.Fatal(err)
	}
	held := string(encoded)
	moved, reason, err := movedPodShape(held)
	if err != nil {
		t.Fatal(err)
	}
	// Two issues, each a Sandbox of its own, each built from the one held configuration and each
	// carrying the one moved shape the operator's single change writes.
	for _, issue := range []string{"S4A-101", "S4A-102"} {
		s := &sandbox{Spec: sandboxSpec{PodTemplate: podTemplate{
			Metadata: podMetadata{Annotations: map[string]string{podShapeAnnotation: moved}},
		}}}
		if drifted := podShapeDrift(s, held); !slices.Equal(drifted, []string{reason}) {
			t.Fatalf("%s's pod drifts by %q after the one change, want the one reason %q every pod of the fleet reads", issue, drifted, reason)
		}
	}
	// A pod of another configuration is no part of this fleet: the same moved shape leaves it
	// drifting by more than that one reason, which is what the check refuses before it patches.
	other := record
	other.Image = "example.test/legion/worker@sha256:" + strings.Repeat("c", 64)
	otherEncoded, err := json.Marshal(other)
	if err != nil {
		t.Fatal(err)
	}
	s := &sandbox{Spec: sandboxSpec{PodTemplate: podTemplate{
		Metadata: podMetadata{Annotations: map[string]string{podShapeAnnotation: moved}},
	}}}
	if drifted := podShapeDrift(s, string(otherEncoded)); slices.Equal(drifted, []string{reason}) {
		t.Fatalf("a pod built from another configuration reads the one fleet reason %q, so the check could not tell the two apart", reason)
	}
}
