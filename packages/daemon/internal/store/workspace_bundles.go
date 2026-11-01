package store

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"

	"github.com/jackc/pgx/v5"

	"github.com/sjawhar/legion/daemon/internal/runtime"
)

// PutWorkspaceBundle saves issue's workspace bundle, replacing whatever the issue's last release
// left: one issue has one bundle, the newest release's, and a release that ran again after a wake
// cancelled the first must not leave the older work behind to be restored instead.
func (s *Store) PutWorkspaceBundle(ctx context.Context, issue string, b runtime.WorkspaceBundle) error {
	if issue == "" {
		return errors.New("a workspace bundle needs its issue")
	}
	tips, bookmarks, err := bundleJSON(b)
	if err != nil {
		return fmt.Errorf("save the workspace bundle of %s: %w", issue, err)
	}
	_, err = s.pool.Exec(ctx, `insert into workspace_bundles (issue, bundle, size_bytes, at, head, tips, bookmarks)
		values ($1, $2, $3, $4, $5, $6, $7)
		on conflict (issue) do update set bundle = excluded.bundle, size_bytes = excluded.size_bytes,
			at = excluded.at, head = excluded.head, tips = excluded.tips, bookmarks = excluded.bookmarks`,
		issue, b.Bundle, b.Size(), b.At, b.Head, tips, bookmarks)
	if err != nil {
		return fmt.Errorf("save the workspace bundle of %s: %w", issue, err)
	}
	return nil
}

// WorkspaceBundle is the bundle issue's last release saved, and false when the issue has none:
// one never released, one whose restore was confirmed, or one whose release a wake cancelled.
func (s *Store) WorkspaceBundle(ctx context.Context, issue string) (runtime.WorkspaceBundle, bool, error) {
	var b runtime.WorkspaceBundle
	var tips, bookmarks []byte
	err := s.pool.QueryRow(ctx, `select bundle, at, head, tips, bookmarks from workspace_bundles where issue = $1`,
		issue).Scan(&b.Bundle, &b.At, &b.Head, &tips, &bookmarks)
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

// DeleteWorkspaceBundle drops issue's bundle: the launcher confirmed the restore, a wake cancelled
// the release before the pod went, or the issue's work is over. Deleting one the table does not
// hold is success, so a retried confirmation finishes.
func (s *Store) DeleteWorkspaceBundle(ctx context.Context, issue string) error {
	if _, err := s.pool.Exec(ctx, `delete from workspace_bundles where issue = $1`, issue); err != nil {
		return fmt.Errorf("delete the workspace bundle of %s: %w", issue, err)
	}
	return nil
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
