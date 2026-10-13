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
// volume (deleteSandbox says how its foreground delete takes them), except a Sandbox whose home is
// another tree (Store.IssueSandboxHome: a child whose hand-over is pending, or one carried into a
// tree of its own that is in it or can still be admitted into it): that one is handed over to its
// home, relabelled and left Suspended with its volume, its role Secrets and the sessions on it
// (handOverSandbox), so the new tree's first launch finds it its own whenever that launch comes. The
// tree's cleanup reservation calls it (store.CleanupReservedTree) once every stored claim of the tree
// has retired, or runs nothing and belongs to an issue that moved, so no launch creates a Sandbox of
// the tree meanwhile. The API's own listing by the tree's label is the census: each Sandbox it names
// is deleted or handed over, in any order, and awaited, and the listing is read again until none is
// left. Each write is fenced to the listed object's UID and resourceVersion, and one the object
// changed since lists again after a recheck interval: a delete's precondition the API refuses with
// 409, and the hand-over's failed patch `test` with 409 or 422 (a status write, or the new tree's
// launch relabelling it first), so a controller writing status through a foreground delete does not
// spin the census; a Sandbox already gone is gone, so a retry after a failure finishes the rest.
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
			home, err := r.store.IssueSandboxHome(ctx, r.project, object.GetLabels()[labelIssue])
			if err != nil {
				return fmt.Errorf("cleanup tree %s: %w", tree, err)
			}
			var changed bool
			if home != "" && home != tree {
				err = r.handOverSandbox(ctx, object, tree, home)
				changed = apierrors.IsConflict(err) || apierrors.IsInvalid(err)
			} else {
				err = r.deleteSandbox(ctx, object)
				changed = apierrors.IsConflict(err)
			}
			if changed {
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

// handOverSandbox gives object, a Sandbox of tree whose issue now belongs to home, to home instead of
// deleting it: under the pod's launch turn, so a launch of the issue's new tree is never cut in on, it
// relabels the Sandbox for home and suspends it in one patch fenced to the uid and resourceVersion the
// census listed, as the new tree's launch itself would (ensureSandbox). A Sandbox no longer of tree
// once the turn is taken (the new tree's launch relabelled it first) is left as it is. A fence the
// Sandbox changed since is the API's refusal (a failed test operation), which the census answers by
// listing again. The volume, the role Secrets and the sessions on the volume are kept.
func (r *Runtime) handOverSandbox(ctx context.Context, object *unstructured.Unstructured, tree, home string) error {
	name := object.GetName()
	release, err := r.lockPod(ctx, name)
	if err != nil {
		return err
	}
	defer release()
	reading, cancel := call(ctx)
	current, err := r.sandboxClient().Get(reading, name, metav1.GetOptions{})
	cancel()
	if apierrors.IsNotFound(err) || err == nil && (current.GetUID() != object.GetUID() || current.GetLabels()[labelTree] != labelValue(tree)) {
		return nil
	}
	if err != nil {
		return fmt.Errorf("cleanup tree %s: read Sandbox %s: %w", tree, name, err)
	}
	s, err := decodeSandbox(object)
	if err != nil {
		return err
	}
	if _, err := r.patch(ctx, s,
		jsonPatchOp{Op: "test", Path: "/metadata/resourceVersion", Value: object.GetResourceVersion()},
		jsonPatchOp{Op: "add", Path: labelPatchPath(labelTree), Value: labelValue(home)},
		jsonPatchOp{Op: "add", Path: "/spec/operatingMode", Value: modeSuspended},
	); err != nil {
		if apierrors.IsConflict(err) || apierrors.IsInvalid(err) {
			return err
		}
		return fmt.Errorf("cleanup tree %s: hand Sandbox %s over to tree %s: %w", tree, name, home, err)
	}
	r.log.Info("sandbox runtime: handed a carried issue's sandbox over to its new tree, keeping its volume",
		"sandbox", name, "uid", object.GetUID(), "tree", tree, "to", home)
	return nil
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
