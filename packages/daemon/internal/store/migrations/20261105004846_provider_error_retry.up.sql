-- 20261105004846_provider_error_retry.up.sql — what the daemon keeps of a turn that ended in a
-- provider error (INFRA-956 R2).
--
-- Such a turn used to retire its task: the claim went idle with its work owed and only a store
-- edit gave it back. The task is kept now, taken back unconfirmed and sent to the same session
-- again after a wait that grows to ten minutes, with no attempt limit. Two facts outlive the
-- process that learned them, so they are columns rather than memory: how far up the ladder this
-- task already is, and the sentence the re-sent prompt carries.
--
-- pending_task_deliveries.provider_error_retries is how many times this task has been sent again
-- after a provider error, from 0. It is what the wait is a function of
-- (supervise's promptRetryDelay: 30s, 60s, 120s, and never more than 10 minutes), so a daemon
-- restarted in the middle of a ladder goes on climbing it rather than starting again at 30
-- seconds. It is a fact of the task rather than of the claim, so a new task starts at 0, and the
-- column goes with the delivery when the delivery is retired.
alter table pending_task_deliveries add column provider_error_retries integer not null default 0;

-- pending_task_deliveries.provider_error_note is the sentence the re-sent prompt carries in front
-- of the task, as interrupted carries supervise's interruptedTask: what ended the previous turn,
-- and that the daemon is sending the task again. It is built from structured fields alone — the
-- HTTP status the provider answered, else nothing but "a provider error" — because a
-- refusal's own explanation is itself refused when it is sent back
-- (dispatch://INFRA-956/artifact/cause-evidence-md), so no provider text ever reaches a prompt.
-- '' is a task no provider error has taken back.
alter table pending_task_deliveries add column provider_error_note text not null default '';

-- claims.provider_error_run is how many turns of this claim ended in a provider error in a row. A
-- turn that ends with no error puts it back to 0. At 6 the project's controller is told once
-- (record's ProviderErrorsNotice), and not again until a clean turn resets the run: there is no
-- new hold and no attempt limit, because a chain's bottom refusing deterministically costs one
-- request every ten minutes, and a limit here would fail every claim at once in a provider
-- outage.
alter table claims add column provider_error_run integer not null default 0;
