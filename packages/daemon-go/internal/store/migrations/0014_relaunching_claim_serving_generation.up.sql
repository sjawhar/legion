-- 0014_relaunching_claim_serving_generation.up.sql — the run a relaunching claim is serving.
--
-- 0012 gave the issue's run to every claim whose process was live, and read launching,
-- shim_connected and launch_uncertain as processes being replaced. They are not: a worker that
-- died mid-task leaves its claim in one of them while the relaunch resumes the session it
-- recorded, and the relaunch is given no task, because the claim already holds the one it was
-- working. An upgrade landing in that window left the worker serving no run, so its next
-- completion was refused and its phase stalled — the state 0012 exists to prevent.
--
-- Only a claim resuming a recorded session is one: a first launch has no session, has worked no
-- run, and is attributed by the task it is given (Claim.ServingRun's third source), so it keeps
-- zero. A claim on an issue no workflow records keeps zero too, and so does one that is failed or
-- retired, which is replaced rather than resumed.
--
-- Its limit: it keys on the state a claim is in now, which is the population 0012 skipped only
-- when both run in one boot — on a database brought up to 0011 or earlier. On a database already
-- past 0012, a claim 0012 skipped has usually finished relaunching and serves no run from
-- registered, ready or idle, where this does not reach, because from there a legitimate zero (a
-- relaunch whose task was refused unread) cannot be told apart from one 0012 missed. No Go daemon
-- database has lived past 0012 before this migration, so both run together everywhere it applies;
-- a database that did would need its serving runs set by hand.
update claims c
set serving_generation = i.generation
from issues i
where i.key = c.issue
  and i.generation > 0
  and c.serving_generation = 0
  and c.session <> ''
  and c.state in ('launching', 'shim_connected', 'launch_uncertain');
