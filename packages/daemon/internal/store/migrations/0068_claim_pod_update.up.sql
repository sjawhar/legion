-- 0068_claim_pod_update.up.sql — the update round each claim is in (INFRA-484), which lets a pod
-- whose configuration has drifted be replaced only after the work on it is pushed, rather than the
-- moment the drift is found.
--
-- claims.update_round is the round's identity and its kind in one string: 'pod:<pod uid>' for the
-- drift round of the pod the claim runs on, shared by every claim of that pod, and
-- 'plugin:<daemon contract>' for the per-claim round of a claim whose plugin is behind this
-- daemon. '' means no round is open, which every claim has before its first round.
-- claims.update_reasons is what the round is for, as the daemon found it, for the task's text and
-- the operator's reading.
-- claims.update_opened_at is the round's own clock: when the round was opened on this claim, which
-- is what its notice is measured from. claims.update_tasked_at is when the task was last sent, so
-- a re-task never postpones the round's notice. claims.update_signalled_at is when the worker
-- answered ready_for_update, and null until it has.
-- claims.update_head is the commit the worker reported as the head of its issue branch at origin
-- when it signalled, '' when it reported none (a planner before its first push). It is the
-- worker's word, which the daemon records and verifies nothing of.
-- claims.update_told_at is when the operator's side was last told about this round, so the same
-- round is not reported again every sweep.
alter table claims add column update_round text not null default '';
alter table claims add column update_reasons text not null default '';
alter table claims add column update_opened_at timestamptz;
alter table claims add column update_tasked_at timestamptz;
alter table claims add column update_signalled_at timestamptz;
alter table claims add column update_head text not null default '';
alter table claims add column update_told_at timestamptz;
