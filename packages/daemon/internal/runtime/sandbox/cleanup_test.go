package sandbox

import (
	"context"
	"errors"
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
// each issue's Sandbox, the pod it owns and the disk that pod owns, its own generic ephemeral
// volume (issuePod.diskSource), and Sandboxes the tree's cleanup must leave alone, another tree's
// and the image probe's. Every Sandbox delete's options are recorded, and a Sandbox's foreground
// delete removes its pod, and with the pod the pod's disk, as garbage collection would.
type cleanupRig struct {
	*rig
	root, child, other, probe string
	// disks are the claims of the tree's issue pods' own disks (issueDiskName), by Sandbox name.
	disks          map[string]string
	sandboxDeletes map[string]metav1.DeleteOptions
	pvcDeletes     map[string]metav1.DeleteOptions
}

func newCleanupRig(t *testing.T) *cleanupRig {
	t.Helper()
	c := &cleanupRig{
		root: SandboxName(rootToken), child: SandboxName(childToken), other: "legion-legion-legion-300", probe: "legion-probe-cleanup",
		disks:          map[string]string{SandboxName(rootToken): issueDiskName(rootToken), SandboxName(childToken): issueDiskName(childToken)},
		sandboxDeletes: map[string]metav1.DeleteOptions{}, pvcDeletes: map[string]metav1.DeleteOptions{},
	}
	rootLabels := claimLabels(rootSpec(t).Role)
	childLabels := map[string]string{labelProject: testProject, labelTree: testTree, labelIssue: childIssue}
	rootSandbox := sandboxObject(t, c.root, "uid-sandbox-root", modeRunning, rootLabels)
	rootSandbox.SetResourceVersion("sandbox-rv-root")
	childSandbox := sandboxObject(t, c.child, "uid-sandbox-child", modeRunning, childLabels)
	childSandbox.SetResourceVersion("sandbox-rv-child")
	otherSandbox := sandboxObject(t, c.other, "uid-sandbox-other", modeRunning,
		map[string]string{labelProject: testProject, labelTree: "LEGION-300", labelIssue: "LEGION-300"})
	probeSandbox := sandboxObject(t, c.probe, "uid-sandbox-probe", modeRunning,
		map[string]string{labelProject: testProject, labelTree: testTree, labelIssue: testTree, labelProbe: "true"})
	objects := []k8sruntime.Object{rootSandbox, childSandbox, otherSandbox, probeSandbox}
	for sandbox, issue := range map[string]struct {
		uid    types.UID
		labels map[string]string
	}{c.root: {"uid-sandbox-root", rootLabels}, c.child: {"uid-sandbox-child", childLabels}} {
		pod := podObject(sandbox, "uid-pod-"+sandbox, string(issue.uid), issue.labels, runningStatus())
		objects = append(objects, pod, &corev1.PersistentVolumeClaim{ObjectMeta: metav1.ObjectMeta{
			Name: c.disks[sandbox], Namespace: testNamespace, UID: "uid-pvc-" + issue.uid, ResourceVersion: "pvc-rv-" + sandbox,
			OwnerReferences: []metav1.OwnerReference{{
				APIVersion: "v1", Kind: "Pod", Name: pod.Name, UID: pod.UID, Controller: new(true), BlockOwnerDeletion: new(true),
			}},
		}})
	}
	c.rig = newRig(t, objects, withoutController())
	c.dyn.PrependReactor("delete", "sandboxes", func(action k8stesting.Action) (bool, k8sruntime.Object, error) {
		delete := action.(k8stesting.DeleteAction)
		options := delete.GetDeleteOptions()
		c.sandboxDeletes[delete.GetName()] = options
		disk, owns := c.disks[delete.GetName()]
		if owns && options.PropagationPolicy != nil && *options.PropagationPolicy == metav1.DeletePropagationForeground {
			if err := c.kube.Tracker().Delete(podsGVR, testNamespace, delete.GetName()); err != nil && !apierrors.IsNotFound(err) {
				return true, nil, err
			}
			if err := c.kube.Tracker().Delete(corev1.SchemeGroupVersion.WithResource("persistentvolumeclaims"), testNamespace, disk); err != nil && !apierrors.IsNotFound(err) {
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

// disk is the tracker's copy of the claim of the disk the pod of Sandbox name owns, or nil once
// garbage collection removed it with that pod.
func (c *cleanupRig) disk(sandbox string) *corev1.PersistentVolumeClaim {
	pvc, err := c.kube.CoreV1().PersistentVolumeClaims(testNamespace).Get(context.Background(), c.disks[sandbox], metav1.GetOptions{})
	if err != nil {
		return nil
	}
	return pvc
}

// Every issue Sandbox of the tree is deleted with foreground propagation and awaited, so garbage
// collection removes the pod each one owns, and with it the disk that pod owns, before the
// Sandbox is absent; the restricted runtime makes no PVC request itself. Each delete is fenced to
// the listed object's UID and resourceVersion, so a stale cleanup cannot delete a replacement.
// Another tree's Sandbox and the image probe's are left alone.
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
		if c.sandbox(name) != nil || c.disk(name) != nil {
			t.Fatalf("after the cleanup Sandbox %s is %+v and its pod's disk %+v, want both gone", name, c.sandbox(name), c.disk(name))
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

// A failed delete fails the cleanup, leaving that Sandbox and its pod's disk, and the same cleanup
// run again finishes it: the Sandboxes already gone are not asked for again.
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
	if c.sandbox(c.root) == nil || c.disk(c.root) == nil {
		t.Fatalf("after the failure: root %+v, its pod's disk %+v; want both kept", c.sandbox(c.root), c.disk(c.root))
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
		if c.sandbox(name) != nil || c.disk(name) != nil {
			t.Fatalf("after the retry Sandbox %s is %+v and its pod's disk %+v, want both gone", name, c.sandbox(name), c.disk(name))
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

// A Sandbox of the tree whose issue has moved to a tree of its own — a child whose hand-over is
// pending, or one carried into a tree that is open or can still be admitted — is deleted with the
// rest of the tree: an issue pod's disk is the pod's own, so the carried child has nothing to
// carry and its next launch creates a Sandbox of its own. The orphan sweep then has nothing of
// the old tree left to judge.
func TestATreeCleanupDeletesACarriedIssuesSandboxToo(t *testing.T) {
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
			want := []string{"sandboxes " + c.child, "sandboxes " + c.root}
			slices.Sort(want)
			if got := c.deletes(); !slices.Equal(got, want) {
				t.Fatalf("deletes = %v, want %v: the carried child's Sandbox goes with the tree's", got, want)
			}
			for _, w := range c.writes() {
				if w.resource == "sandboxes" && w.verb == "patch" {
					t.Fatalf("the cleanup patched Sandbox %s (%s), want it deleted rather than handed over", w.name, w.patch)
				}
			}
			for _, name := range []string{c.child, c.root} {
				if c.sandbox(name) != nil || c.disk(name) != nil {
					t.Fatalf("after the cleanup Sandbox %s is %+v and its pod's disk %+v, want both gone", name, c.sandbox(name), c.disk(name))
				}
			}
		})
	}
}

// The sweep keeps an issue Sandbox whose issue belongs to the tree it is labelled with, though
// that tree is not live: a child carried into a tree of its own, whose launch created this
// Sandbox and may already run a pod in it, while the child waits for the admission that opens its
// tree (Store.IssueSandboxHome). Once the child belongs nowhere — it left the line before
// admission, or its tree closed — the Sandbox is deleted, as every other orphan is.
func TestTheSweepKeepsTheSandboxOfAnIssueWhoseHomeIsTheTreeItIsLabelledWith(t *testing.T) {
	name := "legion-legion-legion-209"
	g := newRig(t, []k8sruntime.Object{sandboxObject(t, name, "uid-sandbox-child", modeSuspended,
		map[string]string{labelProject: testProject, labelTree: childIssue, labelIssue: childIssue})}, withoutController())
	g.store.close(childIssue) // admission has not opened the child's tree: TreeLive reads false
	g.store.homes[childIssue] = childIssue
	if err := g.r.ReconcileOrphans(g.ctx, nil, 0); err != nil {
		t.Fatal(err)
	}
	if s := g.sandbox(name); s == nil || s.UID != "uid-sandbox-child" {
		t.Fatalf("the sweep deleted the Sandbox of %s, whose home is the tree it is labelled with", childIssue)
	}
	delete(g.store.homes, childIssue)
	if err := g.r.ReconcileOrphans(g.ctx, nil, 0); err != nil {
		t.Fatal(err)
	}
	if s := g.sandbox(name); s != nil {
		t.Fatalf("the Sandbox of %s = %+v, want it swept once the issue belonged to no tree", childIssue, s)
	}
}

// A carried issue's Sandbox the new tree's launch relabelled after the census listed it is left to
// that tree: the delete is fenced to the uid and resourceVersion the census read, which the API
// refuses with 409 once the launch has written it, and the listing read again no longer names it.
func TestATreeCleanupLeavesASandboxTheNewTreeRelabelledFirst(t *testing.T) {
	c := newCleanupRig(t)
	var mu sync.Mutex
	var childDeletes int // the reactor answers ahead of the rig's recorder, so it keeps its own count
	c.dyn.PrependReactor("delete", "sandboxes", func(action k8stesting.Action) (bool, k8sruntime.Object, error) {
		mu.Lock()
		defer mu.Unlock()
		if action.(k8stesting.DeleteAction).GetName() != c.child {
			return false, nil, nil
		}
		childDeletes++
		if childDeletes > 1 {
			return false, nil, nil
		}
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
		return true, nil, apierrors.NewConflict(schema.GroupResource{Group: sandboxGVR.Group, Resource: sandboxGVR.Resource}, c.child,
			errors.New("the object has been modified"))
	})
	if err := c.r.CleanupTree(c.ctx, testTree); err != nil {
		t.Fatalf("cleanup: %v", err)
	}
	mu.Lock()
	defer mu.Unlock()
	if childDeletes != 1 {
		t.Fatalf("deletes of the relabelled Sandbox = %d, want the one its new tree's write refused", childDeletes)
	}
	if s := c.sandbox(c.child); s == nil || s.UID != "uid-sandbox-child" || s.Labels[labelTree] != labelValue(childIssue) {
		t.Fatalf("the relabelled Sandbox after the cleanup = %+v, want it as the new tree's launch left it", s)
	}
	if c.sandbox(c.root) != nil {
		t.Fatalf("the root's Sandbox = %+v, want the rest of the tree deleted", c.sandbox(c.root))
	}
}
