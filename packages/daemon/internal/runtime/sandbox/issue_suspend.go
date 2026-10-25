package sandbox

import (
	"context"
	"fmt"

	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"

	"github.com/sjawhar/legion/daemon/internal/runtime"
)

var _ runtime.IssueSuspender = (*Runtime)(nil)

// SuspendIssue is the durable close effect, not a per-role stop. Its pod's launch turn (lockPod, by
// the issue's Sandbox name) orders authorize (the store's check of the close against the issue's
// starts, stops and stored roles) and the mode change with physical starts: a later re-admission
// starts only after this patch, and makes a Sandbox of its own rather than resuming this one. No
// database transaction spans a Kubernetes call. Once the pod is gone the Sandbox is deleted
// (deleteStoredSandbox), with the six role Secrets it owns — each holding a launcher token and a
// rendered `gh` credential bundle — so a close leaves the issue nothing: its disk was the pod's own and went with it, and the issue's
// next launch, if a person sets it todo again, makes the Sandbox afresh (ensureSandbox). The
// parent's Sandbox is another issue's and is never touched.
func (r *Runtime) SuspendIssue(ctx context.Context, issue, tree string, authorize func(context.Context) (bool, error)) error {
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
	object, err := r.sandboxClient().Get(ctx, name, metav1.GetOptions{})
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
	if err := r.deleteStoredSandbox(ctx, name); err != nil {
		return fmt.Errorf("suspend issue %s: %w", issue, err)
	}
	r.log.Info("sandbox runtime: deleted the closed issue's Sandbox and the role Secrets it owned", "issue", issue, "sandbox", name, "uid", s.UID)
	return nil
}
