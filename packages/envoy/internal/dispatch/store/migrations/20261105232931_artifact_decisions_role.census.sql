-- 20261105232931 replaces artifacts_role_check with one that admits a third role and writes no
-- row. The count is the stored rows the new check would refuse. It reads artifacts.role, which
-- 20261102110000_artifact_document_roles creates and which ships in an earlier release, so the
-- column is there when a deployment takes this census, before any migration of this release
-- applies. The check being replaced admits only two of the three roles the new one admits, so no
-- stored row is refused, and a non-zero answer would mean a role reached the column that no
-- release writes.
select count(*)
from artifacts
where role is not null and role not in ('verification', 'plan', 'decisions');
