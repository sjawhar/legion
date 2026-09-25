-- 0012_claim_serving_generation.up.sql — the run a claim is serving.
--
-- A completion is reported inside a turn, and the daemon attributes it to the run of the task the
-- worker is working. The pending delivery alone cannot answer it: a worker told to wait is woken
-- by a notice, which starts a turn of its own, and the delivery whose turn ended was already
-- retired — so a tester waiting on CI, an implementer waiting on a review, or a merger waiting on
-- a person reports from a turn no delivery backs. The claim keeps the generation of the last
-- delivery it confirmed, which is the run it is serving until another delivery replaces it. Zero
-- is a claim that has confirmed none.
alter table claims add column serving_generation bigint not null default 0;

-- A daemon upgraded in place has workers between turns: no delivery to read, and a claim that
-- serves no run, so the next completion each of them reports is refused and its phase stalls. A
-- claim whose worker is registered and working the issue's run is serving that run — the workflow
-- starts one role's worker per phase, and the issue's recorded generation is the run it is on —
-- so each such claim takes it. A claim on an issue no workflow records keeps zero, as does one
-- whose process is gone: launching, failed and retired claims are relaunched or replaced, and the
-- task the new process is given carries its own run.
update claims c
set serving_generation = i.generation
from issues i
where i.key = c.issue
  and i.generation > 0
  and c.state in ('registered', 'ready', 'working', 'idle', 'suspended');
