package sandbox

import (
	"context"
	"errors"
	"fmt"

	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"

	"github.com/sjawhar/legion/daemon/internal/runtime"
)

var _ runtime.IssueSuspender = (*Runtime)(nil)

// SuspendIssue is the durable close effect, not a per-role stop. The issue launch lock orders
// the store fence and mode change with physical starts: a later re-admission starts only after
// this patch and resumes the Sandbox. No database transaction spans a Kubernetes call.
func (r *Runtime) SuspendIssue(ctx context.Context, key runtime.IssueResourceKey) error {
	release, err := r.lockIssue(ctx, key.Issue)
	if err != nil {
		return err
	}
	defer release()
	if r.resourceStore == nil {
		return errors.New("suspend issue: no durable issue resource store")
	}
	resource, act, err := r.resourceStore.IssueSuspension(ctx, r.project, key)
	if err != nil || !act {
		return err
	}
	reading, cancel := call(ctx)
	object, err := r.sandboxClient().Get(reading, resource.Sandbox, metav1.GetOptions{})
	cancel()
	if apierrors.IsNotFound(err) {
		return nil
	}
	if err != nil {
		return fmt.Errorf("suspend issue %s: read Sandbox: %w", key.Issue, err)
	}
	s, err := decodeSandbox(object)
	if err != nil {
		return err
	}
	if s.Labels[labelProject] != r.project || s.Labels[labelIssue] != labelValue(key.Issue) || s.Labels[labelTree] != labelValue(key.Tree) {
		return fmt.Errorf("suspend issue %s: Sandbox %s does not belong to this issue and tree", key.Issue, s.Name)
	}
	if s.mode() != modeSuspended {
		if err := r.setMode(ctx, s, modeSuspended); err != nil {
			return fmt.Errorf("suspend issue %s: %w", key.Issue, err)
		}
	}
	_, err = r.awaitPodGone(ctx, s)
	return err
}
