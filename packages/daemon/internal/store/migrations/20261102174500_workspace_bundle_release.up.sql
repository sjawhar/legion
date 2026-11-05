-- 20261102174500_workspace_bundle_release.up.sql — a saved workspace bundle names the release that
-- saved it (INFRA-945).
--
-- release_id is issue_releases.id of the idle release whose save wrote the row. The restore of the
-- issue's next pod carries it to that pod's launcher in every launcher_bundle slice and reads it
-- back out of the launcher_restored that confirms the restore, and the confirmation deletes the row
-- only while release_id is still the release that report names (store.DeleteRestoredWorkspaceBundle).
-- A launcher holds its report until some connection carries it, so one can reach the daemon after
-- the issue's next release has saved a bundle of its own; without this column that report deleted
-- the issue's one row, and the teardown of that newer release had already taken the disk the work
-- came off, so the newer bundle's work was lost.
--
-- The id outlives its mark: issue_releases holds one row per release in flight and is cleared when
-- the release finishes, while the bundle stays until a pod restores it, so this is no foreign key.
--
-- A row written before this column named no release, and no report can name one, so it would be
-- kept for ever and hold the issue's pod (workflow.IdleHoldBundleUnrestored). The rows are deleted
-- rather than given a release that never existed; the table holds one row per issue only between a
-- release and the restore of the pod that replaces it, and the issue's next release saves again.
delete from workspace_bundles;
alter table workspace_bundles
  add column release_id bigint not null check (release_id > 0);
