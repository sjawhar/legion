package sandbox

import (
	"context"
	"fmt"
	"time"

	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
)

// CleanupTree deletes every issue Sandbox of tree, and with each its role Secrets and the issue's
// volume (deleteSandbox says how its foreground delete takes them). The tree's cleanup reservation
// calls it (store.CleanupReservedTree) once every stored claim of the tree has retired, so no launch
// creates a Sandbox of the tree meanwhile. The API's own listing by the tree's label is the census:
// each Sandbox it names is deleted, in any order, and awaited until the API no longer has it, and
// the listing is read again until none is left. Each delete is fenced to the listed object's UID
// and resourceVersion, and one the object changed since (a status write) lists again after a
// recheck interval, so a controller writing status through a foreground delete does not spin the
// census; a Sandbox already gone is gone, so a retry after a failure finishes the rest.
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
	reading, cancel := call(ctx)
	defer cancel()
	list, err := r.sandboxClient().List(reading, metav1.ListOptions{
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
// waits until the API no longer has it, and forgets its launchers' credentials. Every delete is
// foreground: the Sandbox owns its PVC and its role Secrets by owner reference, and foreground
// propagation has garbage collection remove them before the Sandbox is gone, so the Sandbox's
// absence is the API's confirmation that its volume is gone — the restricted daemon identity has no
// PVC verb to confirm it with. A conflict is returned as it is.
func (r *Runtime) deleteSandbox(ctx context.Context, object *unstructured.Unstructured) error {
	name, uid, version := object.GetName(), object.GetUID(), object.GetResourceVersion()
	policy := metav1.DeletePropagationForeground
	options := metav1.DeleteOptions{
		PropagationPolicy: &policy,
		Preconditions:     &metav1.Preconditions{UID: &uid, ResourceVersion: &version},
	}
	deleting, cancel := call(ctx)
	err := r.sandboxClient().Delete(deleting, name, options)
	cancel()
	if err != nil && !apierrors.IsNotFound(err) {
		if apierrors.IsConflict(err) {
			return err
		}
		return fmt.Errorf("delete Sandbox %s: %w", name, err)
	}
	if err := r.awaitSandboxDeleted(ctx, name); err != nil {
		return err
	}
	r.forgetLauncherCredentials(name)
	return nil
}

// awaitSandboxDeleted waits, up to the boot timeout, until the API no longer has the Sandbox name.
func (r *Runtime) awaitSandboxDeleted(ctx context.Context, name string) error {
	deadline := time.NewTimer(r.bootTimeout)
	defer deadline.Stop()
	for {
		reading, cancel := call(ctx)
		_, err := r.sandboxClient().Get(reading, name, metav1.GetOptions{})
		cancel()
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
