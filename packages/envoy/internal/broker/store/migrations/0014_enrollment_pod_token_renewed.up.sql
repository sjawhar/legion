-- packages/envoy/internal/broker/store/migrations/0014_enrollment_pod_token_renewed.up.sql
-- When a pod enrollment's renewal first carried its pod's projected token (enroll.Service.Renew).
-- Every pod row starts null, whether this migration found it or enroll.Service.Create inserted it;
-- the first renewal with an accepted token sets it, and it is never cleared. Null for session
-- enrollments. Additive and nullable, safe on a fresh or already-migrated database.
--
-- The ratchet is keyed on the pod, not the row: once any pod enrollment naming a runtime_id (the
-- pod UID its token proves) has this column set, live or ended, Renew refuses a token-less renewal
-- from every enrollment naming that runtime_id, whichever launcher credential made it. A pod UID
-- names one pod for ever, so a mark refuses nothing of another pod. The partial index holds exactly
-- the rows that lookup reads, marked pod rows, so it costs one index probe and grows only with the
-- role generations of pods on the current worker image.
alter table enrollments add column if not exists pod_token_renewed_at timestamptz;
create index if not exists enrollments_pod_token_renewed
    on enrollments (runtime_id)
    where kind = 'pod' and pod_token_renewed_at is not null;
