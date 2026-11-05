-- 20261103214558_workspace_bundle_pod_binding.up.sql — a saved workspace bundle names the pod its
-- work came off, and its one restore attempt is bound to the first other pod that asks for it,
-- whether that pod is sent the work or refused (INFRA-945).
--
-- source_pod_uid is the uid of the pod the release took the work off: the pod the save's own
-- BundleWorkspace asked, as the claim's locator recorded it (sandbox's Runtime.BundleWorkspace,
-- loc.Sandbox.PodUID). A release stops the roles of the pod it is releasing, so a role relaunched
-- in that pod between the save and the teardown asks for the bundle like any other start; that
-- pod's workspace is the one the work was taken from, so it is answered nothing and the row is
-- left untouched, which keeps the attempt for the pod that replaces it. It is the pod's identity
-- and not its clock that says so, so a node whose clock runs ahead of the API server's cannot
-- make the released pod look like the replacement, nor the replacement like the released pod.
--
-- attempt_outcome is what the one attempt did, beside the attempt_pod_uid and attempt_at that
-- record whose it was: `sent`, the body left the daemon for that pod, or `refused_older`, that
-- pod was not created after the save and nothing was sent. Either closes the bundle to every
-- later pod, which is the whole of the rule: the first pod that is not the source to ask is the
-- only pod a bundle is ever offered to.
--
-- A refusal that closed nothing is the sequence this column stops. A pod that is not the source
-- and not newer than the save is either a pod the release did not replace, or the replacement
-- itself under a skewed node clock, and the roles of such a pod go on working and pushing. Before
-- this column its refusal recorded nothing, so the bundle stayed open and the next pod of the
-- issue — an eviction's, an update round's, a park and unpark's — took it and ran
-- `jj bookmark set --allow-backwards` and `jj edit` over the branch that newer work was pushed to.
-- The bytes stay for a person to recover (store.WorkspaceBundleSize, the operator's procedure in
-- docs/kubernetes.md) and the row goes on holding the issue's pod
-- (workflow.IdleHoldBundleUnrestored) until the issue's work is over.
--
-- A row written before these columns names no source pod, so its released pod could spend the
-- attempt its replacement needs. The rows are deleted rather than given a pod that never asked:
-- the table holds one row per issue only between a release and the restore of the pod that
-- replaces it, the issue's next release saves again, and the pods of this change's own
-- deployments are the only ones that could hold such a row.
delete from workspace_bundles;
alter table workspace_bundles
  add column source_pod_uid  text not null check (source_pod_uid <> ''),
  add column attempt_outcome text,
  add constraint workspace_bundles_attempt_outcome
    check (case when attempt_pod_uid is null then attempt_outcome is null
                else attempt_outcome in ('sent', 'refused_older') end);
