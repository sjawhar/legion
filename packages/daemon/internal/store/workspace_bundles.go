package store

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"time"

	"github.com/jackc/pgx/v5"

	"github.com/sjawhar/legion/daemon/internal/runtime"
)

// PutWorkspaceBundle saves issue's workspace bundle as the work of release, replacing whatever the
// issue's last release left: one issue has one bundle, the newest release's, and a release that
// ran again after a wake cancelled the first must not leave the older work behind to be restored
// instead. release is the id of the idle release making the save (issue_releases.id), which the
// restore of the issue's next pod carries to that pod's launcher and back, so the report that
// confirms a restore names the release whose work it put back (DeleteRestoredWorkspaceBundle).
// The save clears the restore attempt the row carried with the rest of it: the work of this
// release is another bundle, and it gets an attempt of its own (TakeWorkspaceBundleForRestore).
func (s *Store) PutWorkspaceBundle(ctx context.Context, issue string, release int64, b runtime.WorkspaceBundle) error {
	if issue == "" {
		return errors.New("a workspace bundle needs its issue")
	}
	if release <= 0 {
		return fmt.Errorf("the workspace bundle of %s needs the release that saved it", issue)
	}
	tips, bookmarks, err := bundleJSON(b)
	if err != nil {
		return fmt.Errorf("save the workspace bundle of %s: %w", issue, err)
	}
	_, err = s.pool.Exec(ctx, `insert into workspace_bundles (issue, release_id, bundle, size_bytes, at, head, tips, bookmarks)
		values ($1, $2, $3, $4, $5, $6, $7, $8)
		on conflict (issue) do update set release_id = excluded.release_id, bundle = excluded.bundle,
			size_bytes = excluded.size_bytes, at = excluded.at, head = excluded.head,
			tips = excluded.tips, bookmarks = excluded.bookmarks,
			attempt_pod_uid = null, attempt_at = null`,
		issue, release, b.Bundle, b.Size(), b.At, b.Head, tips, bookmarks)
	if err != nil {
		return fmt.Errorf("save the workspace bundle of %s: %w", issue, err)
	}
	return nil
}

// TakeWorkspaceBundleForRestore answers the bundle issue's last release saved and records, in the
// same statement, that it has had its one restore attempt, against the pod of podUID, created at
// podCreated, whose launcher it is about to be sent to. It answers false when the issue has no
// bundle — one never released, one whose restore the launcher confirmed, one whose release a wake
// cancelled — when the row already carries an attempt, and when the pod is not newer than the
// save.
//
// One attempt per bundle is the rule, and this statement is where it is kept. The launcher's own
// record of the restore it ran lives in the process that ran it: a roles container that restarts
// on the pod's own disk comes back with that record gone and the workspace as the roles left it,
// hours of work past the save, and a bundle sent again there would `jj edit` back to the saved
// head and move each bookmark `--allow-backwards` over all of it. The attempt is written before
// any slice leaves the daemon, so a send that fails halfway, a restore that fails and a restore
// whose report never arrived all leave the same row: one no pod is sent again.
//
// Which pod may spend that attempt is podCreated's own rule, and it is the other half of the
// same one. Every start of every role takes the bundle for its pod (sandbox's launchers.start),
// and a release stops the roles of the pod it is releasing: a role relaunched in that pod,
// between the save and the teardown that deletes the pod, is such a start. That pod's workspace
// is the one the work was taken from, so it needs nothing put back, and letting it spend the
// attempt would leave the pod that replaces it — the one with an empty workspace — sent nothing,
// while the row stayed and held the issue's pod (workflow.IdleHoldBundleUnrestored). A pod
// created before the save is that released pod, or one older still; only a pod created after it
// can be the replacement.
//
// The comparison rests on two instants from two clocks: podCreated is the pod's creationTimestamp
// as the Kubernetes API server stamped it, and the row's `at` is when the shim took the bundle,
// by the clock of the node the released pod ran on (runtime.WorkspaceBundle.At). It cannot tell
// them apart more finely than the coarser of the two: a creationTimestamp has one-second
// resolution, so the save is read to its own second and a pod created inside that second is
// taken as the newer one. Nor can it see clock skew. A badly skewed node stamps a save in the
// replacement pod's future and the restore is refused — which keeps the bundle, holds the issue's
// pod and loses no work, and is the direction to fail in; the opposite, a restore spent on the
// pod being released, is the work never reaching the pod that needs it.
//
// The row stays. A bundle nothing restored holds the issue's pod (workflow.IdleHoldBundleUnrestored)
// and keeps the work in the database until the issue's own work is over (outbox's
// dropReleasedWork), which is what the launcher's notice tells the roles.
func (s *Store) TakeWorkspaceBundleForRestore(ctx context.Context, issue, podUID string, podCreated time.Time) (runtime.WorkspaceBundle, bool, error) {
	if podUID == "" {
		return runtime.WorkspaceBundle{}, false, fmt.Errorf("the restore attempt of the workspace bundle of %s needs the pod it is sent to", issue)
	}
	if podCreated.IsZero() {
		return runtime.WorkspaceBundle{}, false, fmt.Errorf("the restore attempt of the workspace bundle of %s needs when the pod %s it is sent to was created", issue, podUID)
	}
	var b runtime.WorkspaceBundle
	var tips, bookmarks []byte
	err := s.pool.QueryRow(ctx, `update workspace_bundles set attempt_pod_uid = $2, attempt_at = now()
		where issue = $1 and attempt_pod_uid is null and date_trunc('second', at) <= $3
		returning bundle, at, head, tips, bookmarks, release_id`,
		issue, podUID, podCreated).Scan(&b.Bundle, &b.At, &b.Head, &tips, &bookmarks, &b.Release)
	if errors.Is(err, pgx.ErrNoRows) {
		return runtime.WorkspaceBundle{}, false, nil
	}
	if err != nil {
		return runtime.WorkspaceBundle{}, false, fmt.Errorf("take the workspace bundle of %s for the pod %s to restore: %w", issue, podUID, err)
	}
	if err := json.Unmarshal(tips, &b.Tips); err != nil {
		return runtime.WorkspaceBundle{}, false, fmt.Errorf("read the workspace bundle of %s: its tips: %w", issue, err)
	}
	if err := json.Unmarshal(bookmarks, &b.Bookmarks); err != nil {
		return runtime.WorkspaceBundle{}, false, fmt.Errorf("read the workspace bundle of %s: its bookmarks: %w", issue, err)
	}
	return b, true, nil
}

// WorkspaceBundleSize is the size in bytes of the bundle issue's last release saved, and false
// when the issue has none. The release's teardown wants the size alone, and a bundle runs to
// tens of megabytes: reading the row's size_bytes rather than its bundle keeps those bytes out
// of the pool and out of the daemon's memory.
func (s *Store) WorkspaceBundleSize(ctx context.Context, issue string) (int, bool, error) {
	var size int
	err := s.pool.QueryRow(ctx, `select size_bytes from workspace_bundles where issue = $1`, issue).Scan(&size)
	if errors.Is(err, pgx.ErrNoRows) {
		return 0, false, nil
	}
	if err != nil {
		return 0, false, fmt.Errorf("read the size of the workspace bundle of %s: %w", issue, err)
	}
	return size, true, nil
}

// DeleteWorkspaceBundle drops issue's bundle whatever release saved it: a wake cancelled the
// release before the pod went, the release saved a workspace with nothing unpushed, or the
// issue's work is over. Deleting one the table does not hold is success, so a retried delete
// finishes.
func (s *Store) DeleteWorkspaceBundle(ctx context.Context, issue string) error {
	if _, err := s.pool.Exec(ctx, `delete from workspace_bundles where issue = $1`, issue); err != nil {
		return fmt.Errorf("delete the workspace bundle of %s: %w", issue, err)
	}
	return nil
}

// DeleteRestoredWorkspaceBundle drops the bundle of release, which a pod's launcher has reported
// restored. It is the restore's own delete and names the release because a launcher holds its
// report until some connection carries it (internal/launcher's bundleRestore.owed): a report can
// reach the daemon after the issue's next release has saved a bundle of its own, and deleting the
// issue's row on it would take work no pod holds any more. The row goes only while it is still
// the one that release saved; a report of any other release deletes nothing, and its work stays
// for the issue's next pod. Deleting a row the table no longer holds is success, so a
// confirmation the daemon read twice finishes.
func (s *Store) DeleteRestoredWorkspaceBundle(ctx context.Context, issue string, release int64) error {
	if _, err := s.pool.Exec(ctx, `delete from workspace_bundles where issue = $1 and release_id = $2`, issue, release); err != nil {
		return fmt.Errorf("delete the restored workspace bundle of %s saved by release %d: %w", issue, release, err)
	}
	return nil
}

// workspaceBundleUnrestored is whether issue has a stored bundle whose restore is not recorded,
// which is the idle rule's reason to release no pod of the issue (store.readIdleView,
// workflow.IdleHoldBundleUnrestored). The row's own presence is that record: a restore the pod's
// launcher confirmed deletes it (DeleteRestoredWorkspaceBundle), and it does so before any role of
// the pod starts, so a row still here when the idle rule reads it is work no pod has put back —
// the release that would overwrite it is the release the hold stops. A bundle whose one attempt
// failed is such a row, and it holds the issue's pod until the issue's work is over: the work is
// in the database alone, and a release of that pod would take the disk the roles have worked on
// since.
func workspaceBundleUnrestored(ctx context.Context, tx pgx.Tx, issue string) (bool, error) {
	var unrestored bool
	if err := tx.QueryRow(ctx, `select exists (select 1 from workspace_bundles where issue = $1)`, issue).Scan(&unrestored); err != nil {
		return false, fmt.Errorf("read whether the workspace bundle of %s is unrestored: %w", issue, err)
	}
	return unrestored, nil
}

// bundleJSON renders the two members the row keeps as jsonb. Each is written even when empty — a
// bundle with no local bookmark is `{}`, not null — so every row reads back the same way.
func bundleJSON(b runtime.WorkspaceBundle) (tips, bookmarks []byte, err error) {
	if b.Tips == nil {
		b.Tips = []string{}
	}
	if b.Bookmarks == nil {
		b.Bookmarks = map[string]string{}
	}
	if tips, err = json.Marshal(b.Tips); err != nil {
		return nil, nil, fmt.Errorf("its tips: %w", err)
	}
	if bookmarks, err = json.Marshal(b.Bookmarks); err != nil {
		return nil, nil, fmt.Errorf("its bookmarks: %w", err)
	}
	return tips, bookmarks, nil
}
