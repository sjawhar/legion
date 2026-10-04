package sandbox

import (
	"context"
	"errors"
	"strings"
	"testing"

	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	k8sruntime "k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/runtime/schema"
	k8stesting "k8s.io/client-go/testing"
)

const childIssue = "LEGION-209"

// cleanupRig is a tree of two issues, LEGION-208 and its child LEGION-209, as the API holds them:
// the root's Sandbox and the tree PVC it owns, the child's Sandbox, and Sandboxes the tree's
// cleanup must leave alone, another tree's and the image probe's. Every Sandbox delete's options
// are recorded, and the root's foreground delete removes the PVC as garbage collection would.
type cleanupRig struct {
	*rig
	root, child, other, probe string
	rootPVC                   string
	sandboxDeletes            map[string]metav1.DeleteOptions
	pvcDeletes                map[string]metav1.DeleteOptions
}

func newCleanupRig(t *testing.T) *cleanupRig {
	t.Helper()
	c := &cleanupRig{
		root: SandboxName(rootToken), child: SandboxName(childToken), other: "legion-legion-legion-300", probe: "legion-probe-cleanup",
		rootPVC:        TreeClaimName(rootToken),
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
	rootOwner := metav1.OwnerReference{
		APIVersion: sandboxGVR.GroupVersion().String(), Kind: "Sandbox", Name: c.root, UID: "uid-sandbox-root",
		Controller: new(true), BlockOwnerDeletion: new(true),
	}
	c.rig = newRig(t, []k8sruntime.Object{
		rootSandbox, childSandbox, otherSandbox, probeSandbox,
		&corev1.PersistentVolumeClaim{ObjectMeta: metav1.ObjectMeta{
			Name: c.rootPVC, Namespace: testNamespace, UID: "uid-pvc-root", ResourceVersion: "pvc-rv-root", OwnerReferences: []metav1.OwnerReference{rootOwner},
		}},
	}, withoutController())
	c.dyn.PrependReactor("delete", "sandboxes", func(action k8stesting.Action) (bool, k8sruntime.Object, error) {
		delete := action.(k8stesting.DeleteAction)
		options := delete.GetDeleteOptions()
		c.sandboxDeletes[delete.GetName()] = options
		if delete.GetName() == c.root && options.PropagationPolicy != nil && *options.PropagationPolicy == metav1.DeletePropagationForeground {
			if err := c.kube.Tracker().Delete(corev1.SchemeGroupVersion.WithResource("persistentvolumeclaims"), testNamespace, c.rootPVC); err != nil {
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

func (c *cleanupRig) deletes() []string {
	var out []string
	for _, a := range c.writes() {
		if a.verb == "delete" {
			out = append(out, a.resource+" "+a.name)
		}
	}
	return out
}

// Children first, root last: the child's Sandbox is deleted and gone before the root's is
// requested, and the root's request uses foreground propagation, so garbage collection removes the
// blocking tree PVC before the root is absent; the restricted runtime makes no PVC request itself.
// Each delete is fenced to the listed object's UID and resourceVersion, so a stale cleanup cannot
// delete a replacement. Another tree's Sandbox and the image probe's are left alone.
func TestATreeCleanupDeletesItsChildrenThenItsRootByForegroundGarbageCollection(t *testing.T) {
	c := newCleanupRig(t)
	if err := c.r.CleanupTree(c.ctx, testTree); err != nil {
		t.Fatalf("cleanup: %v", err)
	}
	want := []string{"sandboxes " + c.child, "sandboxes " + c.root}
	if got := c.deletes(); strings.Join(got, ", ") != strings.Join(want, ", ") {
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
	}
	if root := c.sandboxDeletes[c.root]; root.PropagationPolicy == nil || *root.PropagationPolicy != metav1.DeletePropagationForeground {
		t.Fatalf("root Sandbox delete propagation = %+v, want foreground", root.PropagationPolicy)
	}
	if child := c.sandboxDeletes[c.child]; child.PropagationPolicy != nil {
		t.Fatalf("child Sandbox delete propagation = %v, want the default", *child.PropagationPolicy)
	}
	if len(c.pvcDeletes) != 0 {
		t.Fatalf("the restricted runtime sent PVC delete requests: %+v", c.pvcDeletes)
	}
	if _, err := c.kube.CoreV1().PersistentVolumeClaims(testNamespace).Get(context.Background(), c.rootPVC, metav1.GetOptions{}); !apierrors.IsNotFound(err) {
		t.Fatalf("the tree PVC after the cleanup: %v, want NotFound", err)
	}
	for _, kept := range []string{c.other, c.probe} {
		if c.sandbox(kept) == nil {
			t.Fatalf("the cleanup of %s deleted Sandbox %s", testTree, kept)
		}
	}
}

// The API's listing is read again after the children it named are gone: a child Sandbox it did not
// name the first time is deleted too, before the root.
func TestATreeCleanupListsAgainUntilNoChildRemains(t *testing.T) {
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
	if got := c.deletes(); strings.Join(got, ", ") != strings.Join(want, ", ") {
		t.Fatalf("deletes = %v, want %v", got, want)
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

// A failed root delete fails the cleanup, leaving the root and its volume, and the same cleanup
// run again finishes it: the children already gone are not asked for again.
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
	if c.sandbox(c.root) == nil || c.sandbox(c.child) != nil {
		t.Fatalf("after the failure: root %+v, child %+v; want the root kept and the child gone", c.sandbox(c.root), c.sandbox(c.child))
	}
	c.clearActions()
	if err := c.r.CleanupTree(c.ctx, testTree); err != nil {
		t.Fatalf("retried cleanup: %v", err)
	}
	if got, want := c.deletes(), []string{"sandboxes " + c.root}; strings.Join(got, ", ") != strings.Join(want, ", ") {
		t.Fatalf("retry deletes = %v, want %v", got, want)
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
