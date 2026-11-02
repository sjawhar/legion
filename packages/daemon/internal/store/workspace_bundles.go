package store

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"

	"github.com/jackc/pgx/v5"

	"github.com/sjawhar/legion/daemon/internal/runtime"
)

// PutWorkspaceBundle saves issue's workspace bundle as the work of release, replacing whatever the
// issue's last release left: one issue has one bundle, the newest release's, and a release that
// ran again after a wake cancelled the first must not leave the older work behind to be restored
// instead. release is the id of the idle release making the save (issue_releases.id), which the
// restore of the issue's next pod carries to that pod's launcher and back, so the report that
// confirms a restore names the release whose work it put back
// (DeleteRestoredWorkspaceBundle).
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
			tips = excluded.tips, bookmarks = excluded.bookmarks`,
		issue, release, b.Bundle, b.Size(), b.At, b.Head, tips, bookmarks)
	if err != nil {
		return fmt.Errorf("save the workspace bundle of %s: %w", issue, err)
	}
	return nil
}

// WorkspaceBundle is the bundle issue's last release saved, with the id of that release, and false
// when the issue has none: one never released, one whose restore was confirmed, or one whose
// release a wake cancelled.
func (s *Store) WorkspaceBundle(ctx context.Context, issue string) (runtime.WorkspaceBundle, bool, error) {
	var b runtime.WorkspaceBundle
	var tips, bookmarks []byte
	err := s.pool.QueryRow(ctx, `select bundle, at, head, tips, bookmarks, release_id from workspace_bundles where issue = $1`,
		issue).Scan(&b.Bundle, &b.At, &b.Head, &tips, &bookmarks, &b.Release)
	if errors.Is(err, pgx.ErrNoRows) {
		return runtime.WorkspaceBundle{}, false, nil
	}
	if err != nil {
		return runtime.WorkspaceBundle{}, false, fmt.Errorf("read the workspace bundle of %s: %w", issue, err)
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
// the release that would overwrite it is the release the hold stops.
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
