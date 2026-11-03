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
//
// The bundle names the pod its work came off (runtime.WorkspaceBundle.PodUID), which the row
// keeps as source_pod_uid and the take refuses by: that pod's own workspace is where the work
// is, so a role relaunched in it must be answered nothing and must leave the attempt for the pod
// that replaces it. A save with no such pod is refused rather than stored, since the row could
// not then tell the released pod from its replacement.
func (s *Store) PutWorkspaceBundle(ctx context.Context, issue string, release int64, b runtime.WorkspaceBundle) error {
	if issue == "" {
		return errors.New("a workspace bundle needs its issue")
	}
	if release <= 0 {
		return fmt.Errorf("the workspace bundle of %s needs the release that saved it", issue)
	}
	if b.PodUID == "" {
		return fmt.Errorf("the workspace bundle of %s needs the uid of the pod its work came off", issue)
	}
	tips, bookmarks, err := bundleJSON(b)
	if err != nil {
		return fmt.Errorf("save the workspace bundle of %s: %w", issue, err)
	}
	_, err = s.pool.Exec(ctx, `insert into workspace_bundles (issue, release_id, source_pod_uid, bundle, size_bytes, at, head, tips, bookmarks)
		values ($1, $2, $3, $4, $5, $6, $7, $8, $9)
		on conflict (issue) do update set release_id = excluded.release_id, source_pod_uid = excluded.source_pod_uid,
			bundle = excluded.bundle, size_bytes = excluded.size_bytes, at = excluded.at, head = excluded.head,
			tips = excluded.tips, bookmarks = excluded.bookmarks,
			attempt_pod_uid = null, attempt_at = null, attempt_outcome = null`,
		issue, release, b.PodUID, b.Bundle, b.Size(), b.At, b.Head, tips, bookmarks)
	if err != nil {
		return fmt.Errorf("save the workspace bundle of %s: %w", issue, err)
	}
	return nil
}

// TakeWorkspaceBundleForRestore answers the bundle issue's last release saved for the pod of
// podUID, created at podCreated, whose launcher it is about to be sent to, and records in the
// same statement what that ask did to the bundle. It answers runtime.WorkspaceBundleNone, having
// recorded nothing, when the issue has no bundle — one never released, one whose restore the
// launcher confirmed, one whose release a wake cancelled — when the row already carries an
// attempt, and when the pod asking is the pod the work came off (source_pod_uid).
//
// One attempt per bundle is the rule, and this statement is where it is kept. The launcher's own
// record of the restore it ran lives in the process that ran it: a roles container that restarts
// on the pod's own disk comes back with that record gone and the workspace as the roles left it,
// hours of work past the save, and a bundle sent again there would `jj edit` back to the saved
// head and move each bookmark `--allow-backwards` over all of it. The attempt is written before
// any slice leaves the daemon, so a send that fails halfway, a restore that fails and a restore
// whose report never arrived all leave the same row: one no pod is sent again.
//
// Whose attempt it is, the row's two pod columns decide, and they are the other half of the same
// rule. Every start of every role takes the bundle for its pod (sandbox's launchers.start), and
// a release stops the roles of the pod it is releasing: a role relaunched in that pod, between
// the save and the teardown that deletes the pod, is such a start. source_pod_uid is that pod,
// as the save recorded it (PutWorkspaceBundle): its workspace is the one the work was taken
// from, so it is answered nothing and the row is left as it was, and the attempt is still there
// for the pod that replaces it. Every other pod's ask takes the attempt, and the first of them
// is the only pod this bundle is ever offered to.
//
// What that pod is answered is podCreated's rule. A pod created after the save is the pod the
// teardown's launch made, and it is sent the work (runtime.WorkspaceBundleSend). A pod that is
// not is refused (runtime.WorkspaceBundleRefusedOlder) and the refusal closes the bundle: such a
// pod's workspace holds what its own fetch provisioned and whatever its roles have done since,
// and the pod that comes after it holds that work's descendants, so a bundle kept open would be
// applied over work pushed after the save. The bytes stay for a person to recover.
//
// The comparison rests on two instants from two clocks: podCreated is the pod's creationTimestamp
// as the Kubernetes API server stamped it, and the row's `at` is when the shim took the bundle,
// by the clock of the node the released pod ran on (runtime.WorkspaceBundle.At). It cannot tell
// them apart more finely than the coarser of the two: a creationTimestamp has one-second
// resolution, so the save is read to its own second and a pod created inside that second is
// taken as the newer one. Nor can it see clock skew: a node running ahead of the API server
// stamps the save in the replacement pod's future, and that pod is refused. Refusing costs the
// restore and loses no work, which is the direction to fail in — the opposite, a restore applied
// over work done since, is the harm — and the refusal is recorded so that it costs that restore
// once rather than moving the issue's bookmarks back in the pod after it.
//
// The row stays. A bundle nothing restored holds the issue's pod (workflow.IdleHoldBundleUnrestored)
// and keeps the work in the database until the issue's own work is over (outbox's
// dropReleasedWork), which is what the launcher's notice tells the roles.
func (s *Store) TakeWorkspaceBundleForRestore(ctx context.Context, issue, podUID string, podCreated time.Time) (runtime.WorkspaceBundle, runtime.WorkspaceBundleTake, error) {
	if podUID == "" {
		return runtime.WorkspaceBundle{}, runtime.WorkspaceBundleNone, fmt.Errorf("the restore attempt of the workspace bundle of %s needs the pod it is sent to", issue)
	}
	if podCreated.IsZero() {
		return runtime.WorkspaceBundle{}, runtime.WorkspaceBundleNone, fmt.Errorf("the restore attempt of the workspace bundle of %s needs when the pod %s it is sent to was created", issue, podUID)
	}
	var b runtime.WorkspaceBundle
	var outcome string
	var tips, bookmarks []byte
	// The body is read back only when it is being sent: a refusal records the attempt and
	// answers the row's facts alone, and a bundle runs to tens of megabytes.
	err := s.pool.QueryRow(ctx, `update workspace_bundles
		set attempt_pod_uid = $2, attempt_at = now(),
			attempt_outcome = case when date_trunc('second', at) <= $3 then 'sent' else 'refused_older' end
		where issue = $1 and attempt_pod_uid is null and source_pod_uid <> $2
		returning attempt_outcome, case when attempt_outcome = 'sent' then bundle end,
			at, head, tips, bookmarks, release_id, source_pod_uid`,
		issue, podUID, podCreated).Scan(&outcome, &b.Bundle, &b.At, &b.Head, &tips, &bookmarks, &b.Release, &b.PodUID)
	if errors.Is(err, pgx.ErrNoRows) {
		return runtime.WorkspaceBundle{}, runtime.WorkspaceBundleNone, nil
	}
	if err != nil {
		return runtime.WorkspaceBundle{}, runtime.WorkspaceBundleNone, fmt.Errorf("take the workspace bundle of %s for the pod %s to restore: %w", issue, podUID, err)
	}
	if err := json.Unmarshal(tips, &b.Tips); err != nil {
		return runtime.WorkspaceBundle{}, runtime.WorkspaceBundleNone, fmt.Errorf("read the workspace bundle of %s: its tips: %w", issue, err)
	}
	if err := json.Unmarshal(bookmarks, &b.Bookmarks); err != nil {
		return runtime.WorkspaceBundle{}, runtime.WorkspaceBundleNone, fmt.Errorf("read the workspace bundle of %s: its bookmarks: %w", issue, err)
	}
	// The outcome the statement wrote is the answer: `sent` for the body above, `refused_older`
	// for the pod it refused, whose attempt is spent all the same.
	if outcome != "sent" {
		return b, runtime.WorkspaceBundleRefusedOlder, nil
	}
	return b, runtime.WorkspaceBundleSend, nil
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
// failed is such a row, and so is one whose attempt was spent refusing a pod no newer than the
// save (attempt_outcome, TakeWorkspaceBundleForRestore); each holds the issue's pod until the
// issue's work is over, and the row's own columns say which it was. The work is in the database
// alone, and a release of that pod would take the disk the roles have worked on since.
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
