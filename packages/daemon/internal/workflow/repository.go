package workflow

import (
	"context"
	"errors"
	"fmt"
	"strings"

	"github.com/jackc/pgx/v5"

	"github.com/sjawhar/legion/daemon/internal/claim"
	"github.com/sjawhar/legion/daemon/internal/intake"
	"github.com/sjawhar/legion/daemon/internal/record"
)

// Repositories is what the daemon holds in memory of the repositories of its project, which the
// workflow reads, without I/O, when it first records an issue: whether Legion serves the
// repository an issue resolves to, and if so as GitHub names it and at which default branch. The
// daemon reads both before the fact that records the issue is applied (its PrepareRepository), so
// an answer it has not read yet is an error, and the fact is applied again once it has.
type Repositories interface {
	ResolveRepository(resolved string) (Resolution, error)
}

// Resolution is the repository an issue is recorded on: Repo and DefaultBranch when Legion serves
// it, with GitHubRepo, GitHub's spelling of Repo that Envoy's event subjects carry, or Refusal, why
// it does not. LeftOut is a mapped repository Legion leaves out for a GitHub App not installed on
// it, which Refusal names.
type Resolution struct {
	Repo          string
	DefaultBranch string
	GitHubRepo    string
	Refusal       string
	LeftOut       bool
}

// resolvesToNone is the refusal of an issue whose repository Dispatch resolves to none: neither it
// nor an ancestor names one, and its project has no default.
const resolvesToNone = "its repository resolves to none"

// ResolveRepository is the repository an issue resolving to resolved is recorded on, or why it is
// not recorded. Admission records roots and orphans with it, as the engine records children.
func (e *Engine) ResolveRepository(resolved string) (Resolution, error) {
	if resolved == "" {
		return Resolution{Refusal: resolvesToNone}, nil
	}
	if e.cfg.Repositories == nil {
		return Resolution{}, errors.New("the workflow holds no repository mapping to record an issue's repository from")
	}
	return e.cfg.Repositories.ResolveRepository(resolved)
}

// SettleRetainedRepository is fact with a retained event's missing repository
// (intake.DispatchIssue.ResolvedRepoMissing) settled from the issue's record, recorded: an issue's
// repository is its record's, so the event is applied with it, keeping its own actor and sequence.
// A fact the consumer's preparation did not fill for an issue not yet recorded is refused, so the
// event is retried: nothing records an issue on a repository nobody read.
func SettleRetainedRepository(fact intake.DispatchIssue, recorded *record.Issue) (intake.DispatchIssue, error) {
	if !fact.ResolvedRepoMissing {
		return fact, nil
	}
	if recorded == nil {
		return fact, fmt.Errorf("the event %d of %s predates Dispatch's resolved_repo and %s is not recorded: it is applied once its repository is read from Dispatch",
			fact.Seq, fact.Key, fact.Key)
	}
	fact.ResolvedRepo, fact.ResolvedRepoMissing = recorded.Repo, false
	return fact, nil
}

// setRepositoryHint is the end of every note that asks for an issue's repository.
const setRepositoryHint = "the issue header's Repository picker, or dispatch issue-update --repo owner/name"

// RefuseRecording says on the Dispatch issue key why Legion did not record it: its repository,
// resolved, resolves to none or to one Legion does not serve (refused). It says it once per
// resolution (record.RepoNote), so a repeated event of the issue says nothing again, and reports
// whether it said it now. A child's refusal is also told to its tree's architect (refuseChild).
func (e *Engine) RefuseRecording(ctx context.Context, tx pgx.Tx, key, resolved string, refused Resolution) (bool, error) {
	note := "Legion did not admit this issue: " + refused.Refusal + ". Set its repository (" + setRepositoryHint + ") and move it to todo again."
	if refused.LeftOut {
		note = "Legion did not admit this issue: " + refused.Refusal + ". Install it on " + resolved + ", or set another repository (" + setRepositoryHint + "), and move the issue to todo again."
	}
	return e.noteOnce(ctx, tx, key, resolved, note)
}

// NoteRepositoryKept says on a recorded issue that the repository Dispatch now resolves it to,
// resolved, is not applied: the record keeps the one it was recorded with. It says it once per
// resolution, and nothing when resolved is the recorded repository.
func (e *Engine) NoteRepositoryKept(ctx context.Context, tx pgx.Tx, issue record.Issue, resolved string) error {
	if strings.EqualFold(resolved, issue.Repo) {
		return nil
	}
	changed := resolved
	if changed == "" {
		changed = "none"
	}
	note := fmt.Sprintf("Legion keeps %s as this issue's repository; the change to %s in Dispatch is not applied — an issue's repository is fixed when Legion records it. Create a new issue for %s instead.", issue.Repo, changed, changed)
	_, err := e.noteOnce(ctx, tx, issue.Key, resolved, note)
	return err
}

// noteOnce posts note on the Dispatch issue key unless the issue's last repository note was the
// same note at the same resolution, and reports whether it posted.
func (e *Engine) noteOnce(ctx context.Context, tx pgx.Tx, key, resolved, note string) (bool, error) {
	last, err := e.store.RepoNote(ctx, tx, key)
	if err != nil {
		return false, err
	}
	if last != nil && last.ResolvedRepo == resolved && last.Note == note {
		return false, nil
	}
	if err := e.enqueue(ctx, tx, key, record.MessagePost{Body: note}); err != nil {
		return false, err
	}
	return true, e.store.PutRepoNote(ctx, tx, record.RepoNote{Issue: key, ResolvedRepo: resolved, Note: note, PostedAt: e.now()})
}

// refuseChild says on a child, and to its tree's architect, why the child was not recorded under
// the tree: its repository resolves to none or to one Legion does not serve.
func (e *Engine) refuseChild(ctx context.Context, tx pgx.Tx, root record.Issue, key, resolved string, refused Resolution) error {
	noted, err := e.RefuseRecording(ctx, tx, key, resolved, refused)
	if err != nil || !noted {
		return err
	}
	e.logOnCommit(ctx, "workflow: child not recorded; its repository is not one Legion serves", "issue", key, "tree", root.Key, "resolved", resolved, "reason", refused.Refusal)
	return e.notice(ctx, tx, root.Key, record.Notice{Kind: "child-not-recorded", Role: claim.RoleArchitect,
		Reason: fmt.Sprintf("%s was not recorded under your tree: %s; the daemon said so on the issue. Set its repository to one of the project's (dispatch issue-update --repo owner/name), then release it again.", key, refused.Refusal)})
}
