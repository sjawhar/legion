-- 0014_relaunching_claim_serving_generation.up.sql — the run a relaunching claim is serving.
--
-- 0012 gave the issue's run to every claim whose process was live, and read launching and
-- shim_connected as processes that are being replaced. They are not: a worker that died mid-task
-- leaves its claim in one of them while the relaunch resumes that session, and the relaunch is
-- given no task, because the claim already holds the one it was working. An upgrade landing in
-- that window left the worker serving no run, so its next completion was refused and its phase
-- stalled — the state 0012 exists to prevent. A claim on an issue no workflow records still keeps
-- zero, and so does one that is failed or retired, which is replaced rather than resumed.
update claims c
set serving_generation = i.generation
from issues i
where i.key = c.issue
  and i.generation > 0
  and c.serving_generation = 0
  and c.state in ('launching', 'shim_connected');
