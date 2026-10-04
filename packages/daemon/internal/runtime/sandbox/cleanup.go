package sandbox

import (
	"context"
	"fmt"
	"time"

	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
)

// CleanupTree deletes every issue Sandbox of tree, and with them their role Secrets and, with the
// root, the tree volume. The tree's cleanup reservation calls it (store.CleanupReservedTree) once
// every stored claim of the tree has retired, so no launch creates a Sandbox of the tree
// meanwhile. The API's own listing by the tree's label is the census: child issues' Sandboxes go
// first, each awaited until the API no longer has it, and the listing is read again until none is
// left; only then the root's, with foreground propagation, since the root Sandbox owns the tree PVC
// every child pod mounts and garbage collection deletes the volume with it. The restricted daemon
// identity has no PVC verb, so the root Sandbox's absence after a foreground delete is the API's
// confirmation that the volume is gone. Each delete is fenced to the listed object's UID and
// resourceVersion, and one the object changed since (a status write) lists again; a Sandbox already
// gone is gone, so a retry after a failure finishes the rest.
func (r *Runtime) CleanupTree(ctx context.Context, tree string) error {
	for {
		root, children, err := r.treeSandboxes(ctx, tree)
		if err != nil {
			return err
		}
		next := children
		if len(children) == 0 {
			if root == nil {
				return nil
			}
			next = []*unstructured.Unstructured{root}
		}
		for _, object := range next {
			if err := r.deleteSandbox(ctx, object, object == root); apierrors.IsConflict(err) {
				break
			} else if err != nil {
				return err
			}
		}
	}
}

// treeSandboxes lists tree's issue Sandboxes from the API, apart into its root issue's and its
// child issues'.
func (r *Runtime) treeSandboxes(ctx context.Context, tree string) (*unstructured.Unstructured, []*unstructured.Unstructured, error) {
	reading, cancel := call(ctx)
	defer cancel()
	list, err := r.sandboxClient().List(reading, metav1.ListOptions{
		LabelSelector: labelProject + "=" + r.project + "," + labelTree + "=" + labelValue(tree),
	})
	if err != nil {
		return nil, nil, fmt.Errorf("cleanup tree %s: list its Sandboxes: %w", tree, err)
	}
	var root *unstructured.Unstructured
	var children []*unstructured.Unstructured
	for i := range list.Items {
		object := &list.Items[i]
		if object.GetLabels()[labelProbe] != "" {
			continue
		}
		if object.GetLabels()[labelIssue] == labelValue(tree) {
			root = object
		} else {
			children = append(children, object)
		}
	}
	return root, children, nil
}

// deleteSandbox deletes object, fenced to its UID and resourceVersion, waits until the API no
// longer has it, and forgets its launchers' credentials. A conflict is returned as it is.
func (r *Runtime) deleteSandbox(ctx context.Context, object *unstructured.Unstructured, foreground bool) error {
	name, uid, version := object.GetName(), object.GetUID(), object.GetResourceVersion()
	options := metav1.DeleteOptions{Preconditions: &metav1.Preconditions{UID: &uid, ResourceVersion: &version}}
	if foreground {
		policy := metav1.DeletePropagationForeground
		options.PropagationPolicy = &policy
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
