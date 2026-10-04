package sandbox

import (
	"context"
	"fmt"

	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"

	"github.com/sjawhar/legion/daemon/internal/runtime"
)

var _ runtime.IssueSuspender = (*Runtime)(nil)

// SuspendIssue is the durable close effect, not a per-role stop. The issue launch lock orders
// authorize (the store's check of the close against the issue's starts, stops and stored roles)
// and the mode change with physical starts: a later re-admission starts only after this patch and
// resumes the Sandbox. No database transaction spans a Kubernetes call.
func (r *Runtime) SuspendIssue(ctx context.Context, issue, tree string, authorize func(context.Context) (bool, error)) error {
	name, err := issueSandboxName(r.project, issue)
	if err != nil {
		return fmt.Errorf("suspend issue %s: %w", issue, err)
	}
	release, err := r.lockIssue(ctx, issue)
	if err != nil {
		return err
	}
	defer release()
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
	_, err = r.awaitPodGone(ctx, s)
	return err
}
