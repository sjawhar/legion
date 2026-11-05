-- 20261102153000_issue_release_rows.up.sql — a release's mark names the rows that are its own
-- (INFRA-945).
--
-- first_row and last_row are the outbox ids of the release's first row, its `issue_bundle` save,
-- and of its last, the `issue_release` teardown, written with the mark in the transaction that
-- queued them. Every row of a release reads itself against that range before it acts
-- (store.IssueReleaseStop, store.IssueRelease): a row whose id falls outside it belongs to a
-- release this mark is not — one cancelled whose rows are still on the lane — and it does
-- nothing and cancels nothing, where before it could act under, and cancel, a later release's
-- mark.
--
-- The range is also what makes the boundary's cancel readable. Every row the release queued is
-- inside it, so a row of the issue addressed to one of its roles that is not — below it,
-- because its transaction took an id before the mark's and committed after, or above it,
-- because it was queued once the mark stood — is a delivery the pod must stay for, and it
-- cancels the release while the first role process is still running.
--
-- A mark written before this column existed names no range, and nothing can place its rows. The
-- table holds one row per release in flight and is empty between releases, so the marks are
-- cleared rather than guessed at: each of their rows then finishes without acting, and the next
-- idle tick decides the pod again.
delete from issue_releases;
alter table issue_releases
  add column first_row bigint not null check (first_row > 0),
  add column last_row bigint not null check (last_row > 0),
  add constraint issue_releases_row_range check (first_row <= boundary_row and boundary_row <= last_row);
