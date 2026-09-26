-- 0011_delivery_generation.up.sql — the issue generation a pending task belongs to.
--
-- A start delivers its run's task to whatever process the claim already has: a live worker is not
-- relaunched, so the pane's own environment names the run it was launched for, which may be an
-- earlier one. The completion a worker reports belongs to the task it was given, so the task
-- carries the generation and the daemon attributes the completion to it. Zero is an operator's
-- own task, which belongs to no run.
alter table pending_task_deliveries add column generation bigint not null default 0;

-- A daemon upgraded in place has workers holding tasks delivered before this column existed, and
-- a completion attributed to no run is refused: those phases would stall until each worker
-- happened to be handed a new task. Such a task was delivered by a start of the run its issue was
-- on, and a newer start would have replaced the row, so the issue's recorded generation is that
-- run. A task of an issue no workflow records — an operator's own claim (LEGION-272) — belongs to
-- no run and stays at zero. An operator's task sent to a workflow claim is not distinguishable
-- from that claim's own task here: it takes the issue's run too, which is the run that claim is
-- working, so the worker's next completion is attributed to the run it is in either way.
update pending_task_deliveries d
set generation = i.generation
from claims c
join issues i on i.key = c.issue
where d.claim_token = c.token
  and i.generation > 0;
