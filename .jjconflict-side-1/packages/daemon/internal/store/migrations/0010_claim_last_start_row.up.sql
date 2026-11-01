-- 0010_claim_last_start_row.up.sql — the newest start the outbox ran against a claim.
--
-- A stop and the start that replaces it are two outbox rows of one role, and the stop is retried
-- until the runtime takes it. Nothing ordered them against each other: a stop retried after its
-- start had run suspended the run that start began, and the task went with it, leaving the phase
-- with nobody in it. The claim records the row id of the newest start executed against it, so a
-- stop older than that is finished as superseded however late it runs and whatever the retry
-- timing is. Zero is a claim no start has run for.
alter table claims add column last_start_row bigint not null default 0;
