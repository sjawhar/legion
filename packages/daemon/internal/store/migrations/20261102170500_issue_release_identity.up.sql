-- 20261102170500_issue_release_identity.up.sql — a release has an id of its own, and its mark
-- names the claims it suspended (INFRA-945).
--
-- id is the release's identity, which outlives the rows it queued: the bundle the save stores
-- carries it (workspace_bundles.release_id), so the restore report of the pod that replaces the
-- released one names the release whose work it put back and deletes no newer release's bundle.
-- The mark is one row per issue and is deleted when the release ends, so the identity is the
-- sequence's and not the key's.
--
-- suspended is the roles whose claims this release's own role stops suspended as `released`,
-- appended by each stop once its claim is suspended (store.ReleaseSuspendedClaim). It is what
-- makes "the claims this release suspended" a fact rather than a guess, and three decisions read
-- it:
--
--   * a cancel resumes those claims and no others. A release that finishes leaves its claims
--     `released` until a wake brings them back, so "every released claim of the issue" is the
--     set an earlier release left behind as much as this one's, and resuming it put roles an
--     earlier release released back into a pod that has no work for them;
--   * the teardown records those roles as the release's (pod_releases.roles), so one release's
--     record does not count another's claims;
--   * a claim in it that is no longer suspended as `released` is the pod being used again
--     (store.releaseOvertaken): a wake's rule resume moves no start row, so it is the only trace
--     a resume between the release's stops and its teardown leaves.
--
-- A mark written before these columns existed names neither. The table holds one row per release
-- in flight and is empty between releases, so the marks are cleared rather than guessed at: each
-- of their rows then finishes without acting, and the next idle tick decides the pod again.
delete from issue_releases;
alter table issue_releases
  add column id bigint generated always as identity unique,
  add column suspended text[] not null default '{}';
