package store

import (
	"context"
	"errors"
	"fmt"

	"github.com/jackc/pgx/v5"

	"github.com/sjawhar/legion/daemon/internal/phase"
	"github.com/sjawhar/legion/daemon/internal/treelifecycle"
)

// IssueReleased is one idle release of an issue's pod as its issue_release outbox row names it:
// the run it released (the issue's generation and its tree root's) and the row, written after the
// release's per-role stops. It is the release's counterpart to IssueClose, and differs in what it
// authorizes: a close acts on an issue the workflow has taken out, a release on an issue that is
// still in it and comes back at the next wake for one of its roles.
type IssueReleased struct {
	Issue, Tree                     string
	IssueGeneration, TreeGeneration uint64
	Row                             int64
}

// ReleaseAct is what one row of an idle release may do, which the store decides and the row's
// executor carries out.
type ReleaseAct int

const (
	// ReleaseOver: the row's own release is over. Either no mark stands — it finished, or a row
	// of it cancelled it and undid what it had done — or the mark that stands names another
	// release's rows and not this one. Such a row does nothing and undoes nothing: the pod it
	// would act on is not the pod it was queued for.
	ReleaseOver ReleaseAct = iota
	// ReleaseCancel: the release is cancelled here, and the mark is gone. The caller resumes
	// every claim the release suspended and deletes the bundle it stored, so the pod is left as
	// a wake would have left it.
	ReleaseCancel
	// ReleaseActs: the row acts.
	ReleaseActs
)

// String is the act's name in a log line.
func (a ReleaseAct) String() string {
	switch a {
	case ReleaseOver:
		return "over"
	case ReleaseCancel:
		return "cancel"
	case ReleaseActs:
		return "acts"
	}
	return "unknown"
}

// releaseMark is the mark one release in flight left, as every row of that release reads it.
type releaseMark struct {
	first, boundary, last, started int64
}

// holds is whether row is one of the rows this mark's release queued. Every row of a release was
// written in the mark's own transaction, so its id lies between the release's first row and its
// last; a row outside that range belongs to a release this mark is not.
func (m releaseMark) holds(row int64) bool { return row >= m.first && row <= m.last }

// readReleaseMark reads the mark of issue's release and places row in it: the act is ReleaseOver
// for a row no mark holds, and otherwise the mark the row belongs to.
func readReleaseMark(ctx context.Context, tx pgx.Tx, project, issue string, row int64) (releaseMark, ReleaseAct, error) {
	var mark releaseMark
	err := tx.QueryRow(ctx, `select first_row, boundary_row, last_row, started_row from issue_releases
		where project = $1 and issue = $2`, project, issue).Scan(&mark.first, &mark.boundary, &mark.last, &mark.started)
	if errors.Is(err, pgx.ErrNoRows) {
		return releaseMark{}, ReleaseOver, nil
	}
	if err != nil {
		return releaseMark{}, ReleaseOver, err
	}
	if !mark.holds(row) {
		return releaseMark{}, ReleaseOver, nil
	}
	return mark, ReleaseActs, nil
}

// releaseOvertaken is whether the pod went on working after its release was marked, which is what
// cancels the release: a claim of the issue started or resumed since the newest start the mark
// was taken at, or a live claim in a turn, starting, or whose launch is not yet known — the
// states the idle rule refuses to release a pod in (workflow's idleStateReleasable). A claim the
// release itself suspended is none of those, so the release's own stops never read as work.
func releaseOvertaken(ctx context.Context, tx pgx.Tx, project, issue string, mark releaseMark) (bool, error) {
	var overtaken bool
	err := tx.QueryRow(ctx, `select exists (
		select 1 from claims where project = $1 and issue = $2
		and (last_start_row > $3 or state not in ('ready', 'idle', 'suspended', 'failed', 'retired')))`,
		project, issue, mark.started).Scan(&overtaken)
	return overtaken, err
}

// IssueReleaseStop decides what one role stop of a release may do, and is the one place the
// release's boundary is read. The boundary is the row that stops the first role process of the
// pod — the release's first `release` supervise row, recorded as issue_releases.boundary_row
// when the release was marked.
//
// Two things cancel the release here, and both leave the pod exactly as it stands with its roles
// going on in it:
//
//   - the pod went on working after the mark (releaseOvertaken), read at every role stop of the
//     release and not at the boundary alone. The release's rows run on the issue's lifecycle
//     lane, which runs them by next_at and then by id, so a start of one role can run ahead of
//     another role's stop; a stop that suspended its claim after such a start would strand that
//     role's task and the teardown would take its pod;
//
//   - at the boundary, where no role process of the pod has been stopped yet, a task, notice,
//     message or start of the issue addressed to a role of the pod that the release did not
//     queue itself. One below the release's own rows was queued by a transaction already in
//     flight when the mark was written; one above them was queued once the mark stood, while
//     every role still ran. Either way the pod is the one that must answer it, so the release
//     goes rather than the pod.
//
// A row no mark holds — a release already cancelled or finished, or a row of an earlier release
// still on the lane while a newer one runs — is ReleaseOver: it does nothing and cancels
// nothing, so a stale row can neither suspend a claim of the live release nor undo it.
func (s *Store) IssueReleaseStop(ctx context.Context, project, issue string, row int64) (ReleaseAct, error) {
	if project == "" || issue == "" || row <= 0 {
		return ReleaseOver, errors.New("a release's role stop requires its project, its issue and its outbox row")
	}
	act := ReleaseOver
	err := s.Tx(ctx, func(tx pgx.Tx) error {
		mark, placed, err := readReleaseMark(ctx, tx, project, issue, row)
		if err != nil || placed == ReleaseOver {
			act = ReleaseOver
			return err
		}
		overtaken, err := releaseOvertaken(ctx, tx, project, issue, mark)
		if err != nil {
			return err
		}
		if !overtaken && row == mark.boundary {
			if err := tx.QueryRow(ctx, `select exists (
				select 1 from outbox where issue = $1 and kind = any($2) and (id < $3 or id > $4))`,
				issue, idleWaitingKinds, mark.first, mark.last).Scan(&overtaken); err != nil {
				return err
			}
		}
		if overtaken {
			act = ReleaseCancel
			return deleteIssueRelease(ctx, tx, project, issue)
		}
		act = ReleaseActs
		return nil
	})
	if err != nil {
		return ReleaseOver, fmt.Errorf("authorize the role stop of %s's release: %w", issue, err)
	}
	return act, nil
}

// IssueReleaseSave decides what the release's save of the workspace may do: it acts for a row the
// release's mark holds, and is ReleaseOver for every other, which is a save of a release that is
// over. The save reads no boundary — it stops nothing, so there is nothing yet to cancel — but it
// does write the issue's one stored bundle, and a save of a finished release must not replace the
// bundle the live one stored.
func (s *Store) IssueReleaseSave(ctx context.Context, project, issue string, row int64) (ReleaseAct, error) {
	if project == "" || issue == "" || row <= 0 {
		return ReleaseOver, errors.New("a release's save requires its project, its issue and its outbox row")
	}
	act := ReleaseOver
	err := s.Tx(ctx, func(tx pgx.Tx) error {
		var err error
		_, act, err = readReleaseMark(ctx, tx, project, issue, row)
		return err
	})
	if err != nil {
		return ReleaseOver, fmt.Errorf("authorize the save of %s's release: %w", issue, err)
	}
	return act, nil
}

// IssueRelease decides whether an idle release may now stop its issue's Sandbox, in one short
// transaction under the shared serializer. It is ReleaseOver for a row the release's mark does
// not hold, which is a teardown of a release that is over: one a role stop cancelled, having
// already resumed what it suspended, and one that finished, whose pod is gone and whose saved
// bundle the issue's next pod is owed.
//
// It cancels the release — the pod stays, the claims it suspended are resumed and the bundle it
// stored goes — when the run it released has moved on, the issue has left the workflow or its
// tree lingers (each of which is a close's business, not a release's), when the tree's cleanup is
// reserved, which deletes the Sandbox this release would, or when the pod went on working after
// the mark: a claim started or resumed since, or one holding a process now. It waits while one of
// the release's own role stops is unfinished or a stored claim of the issue may still hold a
// process. outOfWorkflow is the workflow's rule for a status that takes an issue out of it
// (record.OutOfWorkflow), which the store does not restate; the rest of the gate is the one
// every other path that acts on a released pod reads (workflow.TreeLive), down to the issue's
// own linger, which this read missed.
//
// This is IssueSuspension's shape with its authorization inverted: a close acts only once its
// issue has left the workflow or its tree lingers, and a release acts only while the issue is
// still in the workflow and its tree is live. The two order against each other because each
// waits on the other's role stops: both read the same `suspend`, `issue_close` and `release` ops.
func (s *Store) IssueRelease(ctx context.Context, project string, release IssueReleased, outOfWorkflow func(status string) bool) (ReleaseAct, error) {
	if release.Issue == "" || release.Tree == "" || release.IssueGeneration == 0 || release.TreeGeneration == 0 || release.Row <= 0 {
		return ReleaseOver, errors.New("issue release requires issue, tree, both generations and its outbox row")
	}
	act := ReleaseOver
	err := s.Tx(ctx, func(tx pgx.Tx) error {
		if err := treelifecycle.Serialize(ctx, tx); err != nil {
			return err
		}
		mark, placed, err := readReleaseMark(ctx, tx, project, release.Issue, release.Row)
		if err != nil || placed == ReleaseOver {
			act = ReleaseOver
			return err
		}
		act = ReleaseCancel
		var issueGeneration, rootGeneration int64
		var issuePhase, status, rootPhase, rootStatus string
		var lingering, rootLingering bool
		err = tx.QueryRow(ctx, `select i.generation, i.phase, i.status, i.linger_until is not null,
			root.generation, root.phase, root.status, root.linger_until is not null
			from issues i join issues root on root.key = i.tree where i.key = $1 and i.tree = $2`,
			release.Issue, release.Tree).Scan(&issueGeneration, &issuePhase, &status, &lingering,
			&rootGeneration, &rootPhase, &rootStatus, &rootLingering)
		if errors.Is(err, pgx.ErrNoRows) {
			return nil
		}
		if err != nil {
			return err
		}
		live := !outOfWorkflow(status) && phase.Phase(issuePhase) != phase.Done && !lingering &&
			!outOfWorkflow(rootStatus) && phase.Phase(rootPhase) != phase.Done && !rootLingering
		if uint64(issueGeneration) != release.IssueGeneration || uint64(rootGeneration) != release.TreeGeneration || !live {
			return nil
		}
		// A role started or resumed since the mark, or a start queued above the release's own
		// rows, is the pod being used again: the teardown would take a pod with a live role in
		// it, and the stop that would have caught it has already run. This is the check
		// IssueSuspension makes against a start that supersedes a close.
		overtaken, err := releaseOvertaken(ctx, tx, project, release.Issue, mark)
		if err != nil {
			return err
		}
		if !overtaken {
			if err := tx.QueryRow(ctx, `select exists (
				select 1 from outbox where issue = $1 and id > $2 and kind = 'supervise' and payload->>'op' = 'start')`,
				release.Issue, mark.last).Scan(&overtaken); err != nil {
				return err
			}
		}
		if overtaken {
			return nil
		}
		may, err := issueStopFences(ctx, tx, project, release.Issue, release.Tree, "release", release.Row, release.IssueGeneration)
		if err != nil {
			return err
		}
		if may {
			act = ReleaseActs
		}
		return nil
	})
	if err != nil {
		return ReleaseOver, fmt.Errorf("authorize issue release: %w", err)
	}
	return act, nil
}
