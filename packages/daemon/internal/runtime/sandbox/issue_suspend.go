package sandbox

import (
	"context"
	"fmt"
	"time"

	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"

	"github.com/sjawhar/legion/daemon/internal/runtime"
)

var _ runtime.IssueSuspender = (*Runtime)(nil)

// SuspendIssue is the durable close effect, not a per-role stop. Its pod's launch turn (lockPod, by
// the issue's Sandbox name) orders authorize (the store's check of the close against the issue's
// starts, stops and stored roles) and the mode change with physical starts: a later re-admission
// starts only after this patch and resumes the Sandbox. No database transaction spans a Kubernetes
// call. With release — a child's close as done, whose stops retired every claim of the issue and
// dropped their sessions — the Sandbox is deleted once its pod is gone (releaseIssue), the volume it
// owns with it, so the issue's next run, if a person sets it todo again, starts fresh; the parent's
// Sandbox is another issue's and is never touched.
func (r *Runtime) SuspendIssue(ctx context.Context, issue, tree string, release bool, authorize func(context.Context) (bool, error)) error {
	name, err := issueSandboxName(r.project, issue)
	if err != nil {
		return fmt.Errorf("suspend issue %s: %w", issue, err)
	}
	endTurn, err := r.lockPod(ctx, name)
	if err != nil {
		return err
	}
	defer endTurn()
	act, err := authorize(ctx)
	if err != nil || !act {
		return err
	}
	reading, cancel := call(ctx)
	object, err := r.sandboxClient().Get(reading, name, metav1.GetOptions{})
	cancel()
	if apierrors.IsNotFound(err) {
		return nil
	}
	if err != nil {
		return fmt.Errorf("suspend issue %s: read Sandbox: %w", issue, err)
	}
	s, err := decodeSandbox(object)
	if err != nil {
		return err
	}
	if s.Labels[labelProject] != r.project || s.Labels[labelIssue] != labelValue(issue) || s.Labels[labelTree] != labelValue(tree) {
		return fmt.Errorf("suspend issue %s: Sandbox %s does not belong to this issue and tree", issue, s.Name)
	}
	if s.mode() != modeSuspended {
		if err := r.setMode(ctx, s, modeSuspended); err != nil {
			return fmt.Errorf("suspend issue %s: %w", issue, err)
		}
	}
	if _, err := r.awaitPodGone(ctx, s); err != nil {
		return err
	}
	if !release {
		return nil
	}
	return r.releaseIssue(ctx, issue, name)
}

// releaseIssue deletes the issue's Sandbox name, Suspended and without a pod, with foreground
// propagation, so garbage collection removes the volume claim and the role Secrets it owns before
// the Sandbox is gone, and returns once the API no longer has it. Each delete is fenced to the
// object as read (deleteSandbox); one the controller wrote since (a status write) is read and
// deleted again after a recheck interval. A Sandbox already gone is gone: a close retried after a
// failure, or after the daemon restarted mid-delete, finishes. The caller holds the pod's launch
// turn.
func (r *Runtime) releaseIssue(ctx context.Context, issue, name string) error {
	for {
		reading, cancel := call(ctx)
		object, err := r.sandboxClient().Get(reading, name, metav1.GetOptions{})
		cancel()
		if apierrors.IsNotFound(err) {
			return nil
		}
		if err != nil {
			return fmt.Errorf("release issue %s: read Sandbox: %w", issue, err)
		}
		err = r.deleteSandbox(ctx, object)
		if apierrors.IsConflict(err) {
			select {
			case <-ctx.Done():
				return ctx.Err()
			case <-time.After(recheckInterval):
			}
			continue
		}
		if err != nil {
			return fmt.Errorf("release issue %s: %w", issue, err)
		}
		r.log.Info("sandbox runtime: released the closed issue's Sandbox and the volume it owned", "issue", issue, "sandbox", name, "uid", object.GetUID())
		return nil
	}
}
