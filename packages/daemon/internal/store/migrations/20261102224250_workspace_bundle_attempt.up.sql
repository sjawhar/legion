-- 20261102224250_workspace_bundle_attempt.up.sql — a saved workspace bundle gets one restore
-- attempt, and the row records it (INFRA-945).
--
-- attempt_pod_uid is the uid of the pod whose launcher the bundle was sent to, and attempt_at when
-- the daemon sent it. Both are written before the first slice leaves the daemon
-- (store.TakeWorkspaceBundleForRestore), and a row that carries them is never sent again: not to
-- that pod when its roles container restarts and its launcher comes back holding no record of the
-- restore it already ran, not to a pod that replaces it, and not to a third pod after an eviction.
-- Without the record every start of every role re-sent the bundle, and a second apply ran
-- `jj bookmark set --allow-backwards` and `jj edit` over however many hours of work the roles had
-- done in that pod since.
--
-- The record is no delete: a restore that failed keeps its work here, the launcher's notice names
-- the bundle and the release that saved it, and the issue's pod stays held
-- (workflow.IdleHoldBundleUnrestored) until the issue's work is over. A restore the launcher
-- confirms deletes the row (store.DeleteRestoredWorkspaceBundle), and the issue's next release
-- writes a row of its own with no attempt on it, so the pair is cleared by that save's upsert.
--
-- The columns are nullable and nothing is backfilled: the table itself is added by the migrations
-- of this same change, so no deployment reaching this one holds a row.
alter table workspace_bundles
  add column attempt_pod_uid text,
  add column attempt_at      timestamptz,
  add constraint workspace_bundles_attempt_whole check ((attempt_pod_uid is null) = (attempt_at is null));
