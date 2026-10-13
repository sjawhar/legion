package sandbox

import (
	"context"
	"errors"
	"net/http"
	"slices"
	"strings"
	"sync"
	"testing"

	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	k8sruntime "k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/runtime/schema"
	"k8s.io/apimachinery/pkg/types"
	k8stesting "k8s.io/client-go/testing"
)

const childIssue = "LEGION-209"

// cleanupRig is a tree of two issues, LEGION-208 and its child LEGION-209, as the API holds them:
// each issue's Sandbox and the PVC it owns, and Sandboxes the tree's cleanup must leave alone,
// another tree's and the image probe's. Every Sandbox delete's options are recorded, and a
// Sandbox's foreground delete removes the PVC it owns as garbage collection would.
type cleanupRig struct {
	*rig
	root, child, other, probe string
	// pvcs are the volume claims the tree's issue Sandboxes own, by Sandbox name.
	pvcs           map[string]string
	sandboxDeletes map[string]metav1.DeleteOptions
	pvcDeletes     map[string]metav1.DeleteOptions
}

func newCleanupRig(t *testing.T) *cleanupRig {
	t.Helper()
	c := &cleanupRig{
		root: SandboxName(rootToken), child: SandboxName(childToken), other: "legion-legion-legion-300", probe: "legion-probe-cleanup",
		pvcs:           map[string]string{SandboxName(rootToken): IssueClaimName(rootToken), SandboxName(childToken): IssueClaimName(childToken)},
		sandboxDeletes: map[string]metav1.DeleteOptions{}, pvcDeletes: map[string]metav1.DeleteOptions{},
	}
	rootSandbox := sandboxObject(t, c.root, "uid-sandbox-root", modeRunning, claimLabels(rootSpec(t).Role))
	rootSandbox.SetResourceVersion("sandbox-rv-root")
	childSandbox := sandboxObject(t, c.child, "uid-sandbox-child", modeRunning,
		map[string]string{labelProject: testProject, labelTree: testTree, labelIssue: childIssue})
	childSandbox.SetResourceVersion("sandbox-rv-child")
	otherSandbox := sandboxObject(t, c.other, "uid-sandbox-other", modeRunning,
		map[string]string{labelProject: testProject, labelTree: "LEGION-300", labelIssue: "LEGION-300"})
	probeSandbox := sandboxObject(t, c.probe, "uid-sandbox-probe", modeRunning,
		map[string]string{labelProject: testProject, labelTree: testTree, labelIssue: testTree, labelProbe: "true"})
	objects := []k8sruntime.Object{rootSandbox, childSandbox, otherSandbox, probeSandbox}
	for sandbox, uid := range map[string]types.UID{c.root: "uid-sandbox-root", c.child: "uid-sandbox-child"} {
		objects = append(objects, &corev1.PersistentVolumeClaim{ObjectMeta: metav1.ObjectMeta{
			Name: c.pvcs[sandbox], Namespace: testNamespace, UID: "uid-pvc-" + uid, ResourceVersion: "pvc-rv-" + sandbox,
			OwnerReferences: []metav1.OwnerReference{{
				APIVersion: sandboxGVR.GroupVersion().String(), Kind: "Sandbox", Name: sandbox, UID: uid,
				Controller: new(true), BlockOwnerDeletion: new(true),
			}},
		}})
	}
	c.rig = newRig(t, objects, withoutController())
	c.dyn.PrependReactor("delete", "sandboxes", func(action k8stesting.Action) (bool, k8sruntime.Object, error) {
		delete := action.(k8stesting.DeleteAction)
		options := delete.GetDeleteOptions()
		c.sandboxDeletes[delete.GetName()] = options
		pvc, owns := c.pvcs[delete.GetName()]
		if owns && options.PropagationPolicy != nil && *options.PropagationPolicy == metav1.DeletePropagationForeground {
			if err := c.kube.Tracker().Delete(corev1.SchemeGroupVersion.WithResource("persistentvolumeclaims"), testNamespace, pvc); err != nil && !apierrors.IsNotFound(err) {
				return true, nil, err
			}
		}
		return false, nil, nil
	})
	c.kube.PrependReactor("delete", "persistentvolumeclaims", func(action k8stesting.Action) (bool, k8sruntime.Object, error) {
		delete := action.(k8stesting.DeleteAction)
		c.pvcDeletes[delete.GetName()] = delete.GetDeleteOptions()
		return false, nil, nil
	})
	c.clearActions()
	return c
}

// deletes are the Sandbox deletes the runtime requested, sorted: the census deletes them in the
// order the API listed them, which the test does not fix.
func (c *cleanupRig) deletes() []string {
	var out []string
	for _, a := range c.writes() {
		if a.verb == "delete" {
			out = append(out, a.resource+" "+a.name)
		}
	}
	slices.Sort(out)
	return out
}

// pvc is the tracker's copy of the claim Sandbox name owns, or nil once garbage collection removed it.
func (c *cleanupRig) pvc(sandbox string) *corev1.PersistentVolumeClaim {
	pvc, err := c.kube.CoreV1().PersistentVolumeClaims(testNamespace).Get(context.Background(), c.pvcs[sandbox], metav1.GetOptions{})
	if err != nil {
		return nil
	}
	return pvc
}

// Every issue Sandbox of the tree is deleted with foreground propagation and awaited, so garbage
// collection removes the PVC each one owns before the Sandbox is absent; the restricted runtime
// makes no PVC request itself. Each delete is fenced to the listed object's UID and resourceVersion,
// so a stale cleanup cannot delete a replacement. Another tree's Sandbox and the image probe's are
// left alone.
func TestATreeCleanupForegroundDeletesEveryIssueSandboxAndItsVolume(t *testing.T) {
	c := newCleanupRig(t)
	if err := c.r.CleanupTree(c.ctx, testTree); err != nil {
		t.Fatalf("cleanup: %v", err)
	}
	want := []string{"sandboxes " + c.child, "sandboxes " + c.root}
	slices.Sort(want)
	if got := c.deletes(); !slices.Equal(got, want) {
		t.Fatalf("deletes = %v, want %v", got, want)
	}
	for name, want := range map[string]struct{ uid, version string }{
		c.child: {"uid-sandbox-child", "sandbox-rv-child"},
		c.root:  {"uid-sandbox-root", "sandbox-rv-root"},
	} {
		got := c.sandboxDeletes[name]
		if got.Preconditions == nil || got.Preconditions.UID == nil || got.Preconditions.ResourceVersion == nil ||
			string(*got.Preconditions.UID) != want.uid || *got.Preconditions.ResourceVersion != want.version {
			t.Fatalf("Sandbox %s delete options = %+v, want UID %s and resourceVersion %s", name, got, want.uid, want.version)
		}
		if got.PropagationPolicy == nil || *got.PropagationPolicy != metav1.DeletePropagationForeground {
			t.Fatalf("Sandbox %s delete propagation = %v, want foreground", name, got.PropagationPolicy)
		}
		if c.sandbox(name) != nil || c.pvc(name) != nil {
			t.Fatalf("after the cleanup Sandbox %s is %+v and its PVC %+v, want both gone", name, c.sandbox(name), c.pvc(name))
		}
	}
	if len(c.pvcDeletes) != 0 {
		t.Fatalf("the restricted runtime sent PVC delete requests: %+v", c.pvcDeletes)
	}
	for _, kept := range []string{c.other, c.probe} {
		if c.sandbox(kept) == nil {
			t.Fatalf("the cleanup of %s deleted Sandbox %s", testTree, kept)
		}
	}
}

// The API's listing is read again after the Sandboxes it named are gone: an issue Sandbox it did
// not name the first time is deleted too.
func TestATreeCleanupListsAgainUntilNoSandboxRemains(t *testing.T) {
	c := newCleanupRig(t)
	late := "legion-legion-legion-210"
	created := false
	c.dyn.PrependReactor("delete", "sandboxes", func(action k8stesting.Action) (bool, k8sruntime.Object, error) {
		if action.(k8stesting.DeleteAction).GetName() == c.child && !created {
			created = true
			object := sandboxObject(t, late, "uid-sandbox-late", modeRunning,
				map[string]string{labelProject: testProject, labelTree: testTree, labelIssue: "LEGION-210"})
			if err := c.dyn.Tracker().Create(sandboxGVR, object, testNamespace); err != nil {
				return true, nil, err
			}
		}
		return false, nil, nil
	})
	if err := c.r.CleanupTree(c.ctx, testTree); err != nil {
		t.Fatalf("cleanup: %v", err)
	}
	want := []string{"sandboxes " + c.child, "sandboxes " + late, "sandboxes " + c.root}
	slices.Sort(want)
	if got := c.deletes(); !slices.Equal(got, want) {
		t.Fatalf("deletes = %v, want %v", got, want)
	}
	if policy := c.sandboxDeletes[late].PropagationPolicy; policy == nil || *policy != metav1.DeletePropagationForeground {
		t.Fatalf("the late Sandbox's delete propagation = %v, want foreground", policy)
	}
}

// A delete the object's change since the listing refused (a status write moved its
// resourceVersion) is no failure: the listing is read again and the delete fenced to what it now
// names.
func TestATreeCleanupListsAgainAfterAConflict(t *testing.T) {
	c := newCleanupRig(t)
	conflicted := false
	c.dyn.PrependReactor("delete", "sandboxes", func(action k8stesting.Action) (bool, k8sruntime.Object, error) {
		if action.(k8stesting.DeleteAction).GetName() == c.root && !conflicted {
			conflicted = true
			return true, nil, apierrors.NewConflict(schema.GroupResource{Group: sandboxGVR.Group, Resource: sandboxGVR.Resource}, c.root, errors.New("the object has been modified"))
		}
		return false, nil, nil
	})
	if err := c.r.CleanupTree(c.ctx, testTree); err != nil {
		t.Fatalf("cleanup after a conflict: %v", err)
	}
	if !conflicted || c.sandbox(c.root) != nil {
		t.Fatalf("conflicted %t, root Sandbox %+v; want the conflict met and the root deleted after it", conflicted, c.sandbox(c.root))
	}
}

// A failed delete fails the cleanup, leaving that Sandbox and its volume, and the same cleanup run
// again finishes it: the Sandboxes already gone are not asked for again.
func TestATreeCleanupThatFailedFinishesOnItsRetry(t *testing.T) {
	c := newCleanupRig(t)
	failed := false
	c.dyn.PrependReactor("delete", "sandboxes", func(action k8stesting.Action) (bool, k8sruntime.Object, error) {
		if action.(k8stesting.DeleteAction).GetName() == c.root && !failed {
			failed = true
			return true, nil, errors.New("transient Sandbox API failure")
		}
		return false, nil, nil
	})
	if err := c.r.CleanupTree(c.ctx, testTree); err == nil || !strings.Contains(err.Error(), "transient Sandbox API failure") {
		t.Fatalf("first cleanup = %v, want its transient delete failure", err)
	}
	if c.sandbox(c.root) == nil || c.pvc(c.root) == nil {
		t.Fatalf("after the failure: root %+v, its PVC %+v; want both kept", c.sandbox(c.root), c.pvc(c.root))
	}
	var left []string
	for _, name := range []string{c.child, c.root} {
		if c.sandbox(name) != nil {
			left = append(left, "sandboxes "+name)
		}
	}
	slices.Sort(left)
	c.clearActions()
	if err := c.r.CleanupTree(c.ctx, testTree); err != nil {
		t.Fatalf("retried cleanup: %v", err)
	}
	if got := c.deletes(); !slices.Equal(got, left) {
		t.Fatalf("retry deletes = %v, want exactly the Sandboxes the failure left, %v", got, left)
	}
	for _, name := range []string{c.child, c.root} {
		if c.sandbox(name) != nil || c.pvc(name) != nil {
			t.Fatalf("after the retry Sandbox %s is %+v and its PVC %+v, want both gone", name, c.sandbox(name), c.pvc(name))
		}
	}
}

// A tree the API lists no Sandbox of, as one closed before its first launch, is released at once:
// nothing is written.
func TestATreeCleanupOfATreeWithNoSandboxWritesNothing(t *testing.T) {
	g := newRig(t, nil, withoutController())
	g.clearActions()
	if err := g.r.CleanupTree(g.ctx, testTree); err != nil {
		t.Fatalf("cleanup of a tree with no Sandbox: %v", err)
	}
	if writes := g.writes(); len(writes) != 0 {
		t.Fatalf("a tree with no Sandbox wrote %+v", writes)
	}
}

// A Sandbox of the tree whose home is another tree — a child whose hand-over is pending, or one
// carried into a tree of its own that is in it or waits for admission into it — is handed over, not
// deleted: one patch, fenced to the uid and resourceVersion the census listed, relabels it for the
// issue's tree and suspends it, and the Sandbox keeps its uid and the PVC it owns, so the new tree's
// launch finds it its own. The rest of the tree is deleted as before. The orphan sweep then keeps the
// handed-over Sandbox while its home is the tree it is labelled with, whether that tree is open or
// not yet, and deletes it once the child belongs nowhere: it left the line before admission, or its
// tree closed.
func TestATreeCleanupHandsACarriedIssuesSandboxOverToItsNewTree(t *testing.T) {
	for _, tc := range []struct {
		name   string
		opened bool
	}{
		{"carried into a tree admission opened", true},
		{"waiting for admission, or its hand-over still pending", false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			c := newCleanupRig(t)
			c.store.homes[childIssue] = childIssue
			if !tc.opened {
				c.store.close(childIssue) // no lifecycle of the child's tree: TreeLive reads false
			}
			if err := c.r.CleanupTree(c.ctx, testTree); err != nil {
				t.Fatalf("cleanup: %v", err)
			}
			if got, want := c.deletes(), []string{"sandboxes " + c.root}; !slices.Equal(got, want) {
				t.Fatalf("deletes = %v, want %v: the carried child's Sandbox is handed over, not deleted", got, want)
			}
			s := c.sandbox(c.child)
			if s == nil || s.UID != "uid-sandbox-child" || s.Labels[labelTree] != labelValue(childIssue) || s.mode() != modeSuspended {
				t.Fatalf("the carried child's Sandbox after the cleanup = %+v, want uid-sandbox-child relabelled for %s and Suspended", s, childIssue)
			}
			if pvc := c.pvc(c.child); pvc == nil || pvc.UID != "uid-pvc-uid-sandbox-child" {
				t.Fatalf("the carried child's PVC after the cleanup = %+v, want the one it owned", pvc)
			}
			var handOvers []string
			for _, w := range c.writes() {
				if w.resource == "sandboxes" && w.verb == "patch" && w.name == c.child {
					handOvers = append(handOvers, w.patch)
				}
			}
			if len(handOvers) != 1 || !strings.Contains(handOvers[0], `{"op":"test","path":"/metadata/uid","value":"uid-sandbox-child"}`) ||
				!strings.Contains(handOvers[0], `{"op":"test","path":"/metadata/resourceVersion","value":"sandbox-rv-child"}`) {
				t.Fatalf("the hand-over patches = %v, want one fenced to the listed uid and resourceVersion", handOvers)
			}
			if c.sandbox(c.root) != nil || c.pvc(c.root) != nil {
				t.Fatalf("the root's Sandbox %+v or its PVC survived the cleanup", c.sandbox(c.root))
			}

			// The sweep, past every grace, once the old tree's cleanup has confirmed, on the informer's
			// copy of the handed-over Sandbox.
			c.store.close(testTree)
			c.eventually("the runtime's store to hold the handed-over Sandbox under "+childIssue, func() bool {
				object, ok, _ := c.r.sandboxes.GetStore().GetByKey(testNamespace + "/" + c.child)
				return ok && object.(*unstructured.Unstructured).GetLabels()[labelTree] == labelValue(childIssue)
			})
			if err := c.r.ReconcileOrphans(c.ctx, nil, 0); err != nil {
				t.Fatal(err)
			}
			if s := c.sandbox(c.child); s == nil || s.UID != "uid-sandbox-child" {
				t.Fatalf("the orphan sweep deleted the handed-over Sandbox of %s, whose home is the tree it is labelled with", childIssue)
			}
			// The child then belongs nowhere: it left the line before admission, or its tree closed.
			delete(c.store.homes, childIssue)
			c.store.close(childIssue)
			if err := c.r.ReconcileOrphans(c.ctx, nil, 0); err != nil {
				t.Fatal(err)
			}
			if c.sandbox(c.child) != nil {
				t.Fatalf("the handed-over Sandbox of %s survived the sweep once it belonged to no tree", childIssue)
			}
		})
	}
}

// A hand-over the API refuses because the Sandbox was written since the census listed it — a status
// write, its tree label unchanged — is answered as the real API answers a failed JSON patch `test`,
// 422 Invalid: the census lists again, and the second hand-over, fenced to the new resourceVersion,
// lands. The cleanup does not fail.
func TestATreeCleanupListsAgainWhenItsHandOverPatchIsRefusedAsStale(t *testing.T) {
	c := newCleanupRig(t)
	c.store.homes[childIssue] = childIssue
	var mu sync.Mutex
	var refused []string // the reactor answers ahead of the rig's recorder, so it keeps its own
	c.dyn.PrependReactor("patch", "sandboxes", func(action k8stesting.Action) (bool, k8sruntime.Object, error) {
		mu.Lock()
		defer mu.Unlock()
		if action.(k8stesting.PatchAction).GetName() != c.child || len(refused) > 0 {
			return false, nil, nil
		}
		refused = append(refused, string(action.(k8stesting.PatchAction).GetPatch()))
		object, err := c.dyn.Tracker().Get(sandboxGVR, testNamespace, c.child)
		if err != nil {
			return true, nil, err
		}
		u := object.(*unstructured.Unstructured)
		u.SetResourceVersion("sandbox-rv-child-status")
		if err := c.dyn.Tracker().Update(sandboxGVR, u, testNamespace); err != nil {
			return true, nil, err
		}
		return true, nil, apierrors.NewGenericServerResponse(http.StatusUnprocessableEntity, "patch", sandboxGVR.GroupResource(), c.child,
			"the server rejected our request due to an error in our request", 0, false)
	})
	if err := c.r.CleanupTree(c.ctx, testTree); err != nil {
		t.Fatalf("cleanup after a stale hand-over patch: %v", err)
	}
	mu.Lock()
	defer mu.Unlock()
	if len(refused) != 1 || !strings.Contains(refused[0], `{"op":"test","path":"/metadata/resourceVersion","value":"sandbox-rv-child"}`) {
		t.Fatalf("the refused hand-over patches = %v, want one fenced to the listed resourceVersion", refused)
	}
	var patches []string
	for _, w := range c.writes() {
		if w.resource == "sandboxes" && w.verb == "patch" && w.name == c.child {
			patches = append(patches, w.patch)
		}
	}
	if len(patches) != 1 || !strings.Contains(patches[0], `{"op":"test","path":"/metadata/resourceVersion","value":"sandbox-rv-child-status"}`) {
		t.Fatalf("the hand-over patches after the refusal = %v, want one fenced to the resourceVersion the census listed again", patches)
	}
	if s := c.sandbox(c.child); s == nil || s.UID != "uid-sandbox-child" || s.Labels[labelTree] != labelValue(childIssue) || s.mode() != modeSuspended {
		t.Fatalf("the carried child's Sandbox after the cleanup = %+v, want it handed over", s)
	}
}

// A carried issue's Sandbox the new tree's launch relabelled after the census listed it is not
// written again: under the pod's turn the cleanup reads it, no longer of the tree, and leaves it.
func TestATreeCleanupLeavesASandboxTheNewTreeRelabelledFirst(t *testing.T) {
	c := newCleanupRig(t)
	c.store.homes[childIssue] = childIssue
	relabelled := false
	c.dyn.PrependReactor("get", "sandboxes", func(action k8stesting.Action) (bool, k8sruntime.Object, error) {
		if action.(k8stesting.GetAction).GetName() != c.child || relabelled {
			return false, nil, nil
		}
		relabelled = true
		object, err := c.dyn.Tracker().Get(sandboxGVR, testNamespace, c.child)
		if err != nil {
			return true, nil, err
		}
		u := object.(*unstructured.Unstructured)
		labels := u.GetLabels()
		labels[labelTree] = labelValue(childIssue)
		u.SetLabels(labels)
		u.SetResourceVersion("sandbox-rv-child-relabelled")
		if err := c.dyn.Tracker().Update(sandboxGVR, u, testNamespace); err != nil {
			return true, nil, err
		}
		return true, u, nil
	})
	if err := c.r.CleanupTree(c.ctx, testTree); err != nil {
		t.Fatalf("cleanup: %v", err)
	}
	for _, w := range c.writes() {
		if w.resource == "sandboxes" && w.name == c.child {
			t.Fatalf("the cleanup wrote %s %s to the Sandbox the new tree relabelled first, want nothing", w.verb, w.patch)
		}
	}
	if s := c.sandbox(c.child); s == nil || s.UID != "uid-sandbox-child" || s.Labels[labelTree] != labelValue(childIssue) {
		t.Fatalf("the relabelled Sandbox after the cleanup = %+v, want it as the new tree's launch left it", s)
	}
}
