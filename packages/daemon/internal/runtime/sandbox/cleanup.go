package sandbox

import (
	"context"
	"fmt"
	"time"

	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
)

// CleanupTree deletes every issue Sandbox of tree, and with each the role Secrets it owns
// (deleteSandbox says how its foreground delete takes them). It keeps nothing for a child the
// tree carried into a tree of its own: an issue pod's disk is the pod's own, which went with that
// pod, so there is no disk to hand over and the child's next launch creates a Sandbox of its own
// (ensureSandbox). The tree's cleanup reservation calls it (store.CleanupReservedTree) once every
// stored claim of the tree has retired, or runs nothing and belongs to an issue that moved, so no
// launch creates a Sandbox of the tree meanwhile. The API's own listing by the tree's label is the
// census: each Sandbox it names is deleted, in any order, and awaited, and the listing is read
// again until none is left. Each delete is fenced to the listed object's UID and resourceVersion,
// and one the object changed since — the precondition the API refuses with 409, as a controller
// writing status through a foreground delete does — lists again after a recheck interval rather
// than spinning the census; a Sandbox the new tree's launch relabelled meanwhile fails the same
// fence and the next listing no longer names it, so it is left to that tree. A Sandbox already
// gone is gone, so a retry after a failure finishes the rest.
func (r *Runtime) CleanupTree(ctx context.Context, tree string) error {
	for {
		sandboxes, err := r.treeSandboxes(ctx, tree)
		if err != nil {
			return err
		}
		if len(sandboxes) == 0 {
			return nil
		}
		for _, object := range sandboxes {
			err := r.deleteSandbox(ctx, object)
			if apierrors.IsConflict(err) {
				select {
				case <-ctx.Done():
					return ctx.Err()
				case <-time.After(recheckInterval):
				}
				break
			}
			if err != nil {
				return err
			}
		}
	}
}

// treeSandboxes lists tree's issue Sandboxes from the API, the image probe's left out.
func (r *Runtime) treeSandboxes(ctx context.Context, tree string) ([]*unstructured.Unstructured, error) {
	list, err := r.sandboxClient().List(ctx, metav1.ListOptions{
		LabelSelector: labelProject + "=" + r.project + "," + labelTree + "=" + labelValue(tree),
	})
	if err != nil {
		return nil, fmt.Errorf("cleanup tree %s: list its Sandboxes: %w", tree, err)
	}
	var sandboxes []*unstructured.Unstructured
	for i := range list.Items {
		if object := &list.Items[i]; object.GetLabels()[labelProbe] == "" {
			sandboxes = append(sandboxes, object)
		}
	}
	return sandboxes, nil
}

// deleteSandbox deletes object with foreground propagation, fenced to its UID and resourceVersion,
// forgets its launchers' credentials once the API has accepted the delete, and waits until the API
// no longer has it. Every delete is foreground: the Sandbox owns its role Secrets by owner
// reference — and an issue Sandbox of the disk shape before the pod-owned disk also the PVC its
// claim template made — and foreground propagation has garbage collection remove them before the
// Sandbox is gone, so the Sandbox's absence is the API's confirmation that what it owned is gone —
// the restricted daemon identity has no PVC verb to confirm it with. An issue's disk of this
// runtime's shape is not among them: it is its pod's own, and goes when the delete takes the pod.
// A conflict is returned as it is.
func (r *Runtime) deleteSandbox(ctx context.Context, object *unstructured.Unstructured) error {
	name, uid, version := object.GetName(), object.GetUID(), object.GetResourceVersion()
	policy := metav1.DeletePropagationForeground
	options := metav1.DeleteOptions{
		PropagationPolicy: &policy,
		Preconditions:     &metav1.Preconditions{UID: &uid, ResourceVersion: &version},
	}
	err := r.sandboxClient().Delete(ctx, name, options)
	if err != nil && !apierrors.IsNotFound(err) {
		if apierrors.IsConflict(err) {
			return err
		}
		return fmt.Errorf("delete Sandbox %s: %w", name, err)
	}
	r.forgetLauncherCredentials(name)
	return r.awaitSandboxDeleted(ctx, name)
}

// deleteStoredSandbox deletes the Sandbox name as the API holds it now, in the foreground, and
// returns once the API no longer has it; one already gone is gone. Each delete is fenced to the
// object as read (deleteSandbox), so one the Sandbox controller wrote in between — a status write —
// is refused as a conflict, read again and deleted again after a recheck interval. Both callers
// hold the pod's launch turn, so nothing of this daemon recreates the Sandbox under the loop: a
// closed issue's suspension (SuspendIssue) and a launch replacing an issue Sandbox of the disk
// shape before the pod-owned disk (replaceSupersededSandbox). Each logs the delete it wanted at
// its own call site.
func (r *Runtime) deleteStoredSandbox(ctx context.Context, name string) error {
	for {
		object, err := r.sandboxClient().Get(ctx, name, metav1.GetOptions{})
		if apierrors.IsNotFound(err) {
			return nil
		}
		if err != nil {
			return fmt.Errorf("read sandbox %s: %w", name, err)
		}
		if err := r.deleteSandbox(ctx, object); !apierrors.IsConflict(err) {
			return err
		}
		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-time.After(recheckInterval):
		}
	}
}

// awaitSandboxDeleted waits, up to the boot timeout, until the API no longer has the Sandbox name.
func (r *Runtime) awaitSandboxDeleted(ctx context.Context, name string) error {
	deadline := time.NewTimer(r.bootTimeout)
	defer deadline.Stop()
	for {
		_, err := r.sandboxClient().Get(ctx, name, metav1.GetOptions{})
		if apierrors.IsNotFound(err) {
			return nil
		}
		if err != nil {
			return fmt.Errorf("confirm Sandbox %s deletion: %w", name, err)
		}
		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-deadline.C:
			return fmt.Errorf("confirm Sandbox %s deletion: timed out after %s", name, r.bootTimeout)
		case <-time.After(recheckInterval):
		}
	}
}
