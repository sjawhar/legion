-- packages/envoy/internal/broker/store/migrations/0024_drop_vm_enrollment_kind.up.sql
-- A machine no longer enrolls itself as a kind of its own. It presents its Google identity token
-- to the RFC 8693 token exchange, which mints the launcher credential it then enrols a session
-- under, so an enrollment is a session or a pod and nothing else. This retires every vm
-- enrollment a running broker still holds and takes vm out of the kind the table admits, so a
-- kind no build writes can never be written again: the vocabulary is closed here and not in Go
-- alone, because the column is what every reader of an enrollment branches on.
--
-- A live vm enrollment is ended exactly as a revoke ends one (enroll.endEnrollment): its row is
-- marked revoked, every live grant under it is revoked, and every request still pending under it
-- is cancelled with its audit row and its record's cancelled event, so no approver's pending list
-- keeps showing a request that can now grant nothing. The actor is the broker, which is what did
-- it. The whole of it is one statement, so the rows and their audit trail land together.
--
-- Every vm row, ended here or ended earlier, reads as a session from now on, which is what the
-- narrowed check needs and what the row now is: a machine's enrollment today is a session under
-- the launcher credential its exchange minted. Migration 0012 rewrote box and host the same way.
--
-- The deploy order is safe either way round. A build before this one writes a vm enrollment only
-- at POST /v1/enrollments of kind vm, which this release also removes, and the narrowed check
-- refuses such a row rather than letting it through; a build after this one writes none. The
-- re-added constraint validates the rows the table holds under the runner's one transaction, and
-- every row left is session or pod, which the new check admits.
with live as (
  select id from enrollments where kind = 'vm' and revoked_at is null
), retired as (
  update enrollments set kind = 'session', revoked_at = coalesce(revoked_at, now()) where kind = 'vm'
), grants_revoked as (
  update grants set revoked_at = now(), revoked_by = 'broker'
  where enrollment_id in (select id from live) and revoked_at is null
), requests_cancelled as (
  update requests
  set state = 'cancelled', decided_at = now(), decided_by = 'broker',
      decision_detail = 'the requesting enrollment ended: the broker no longer enrols a VM'
  where state = 'pending' and enrollment_id in (select id from live)
  returning id, enrollment_id, record_id
), requests_audited as (
  insert into audit (kind, enrollment_id, request_id, actor, detail)
  select 'request.cancelled', enrollment_id, id, 'broker',
         jsonb_build_object('reason', 'the requesting enrollment ended: the broker no longer enrols a VM')
  from requests_cancelled
), requests_recorded as (
  insert into credential_request_events (record_id, event, actor, detail)
  select record_id, 'cancelled', 'broker', 'the requesting enrollment ended: the broker no longer enrols a VM'
  from requests_cancelled where record_id is not null
)
insert into audit (kind, enrollment_id, actor, detail)
select 'enrollment.revoked', id, 'broker', jsonb_build_object('reason', 'the broker no longer enrols a VM')
from live;

alter table enrollments drop constraint enrollments_kind_check;
alter table enrollments add constraint enrollments_kind_check check (kind in ('session', 'pod'));
