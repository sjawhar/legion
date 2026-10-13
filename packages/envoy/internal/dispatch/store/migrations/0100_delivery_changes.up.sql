-- 0100: a log of delivery fact changes, one row per changed row, so Dispatch can tell each open
-- Delivery or Pipeline page what changed (LEGION-690). A row is written only when a row's content
-- changed: the columns every reconcile pass rewrites are ignored, so a pass that finds nothing new
-- logs nothing. Each row carries the time that decides which windows it can touch and, for a run,
-- its kind. Each trigger takes one transaction advisory lock before it inserts, so seq is allocated
-- in commit order, and notifies 'delivery_changes', which Postgres delivers at commit. The lock is
-- global across delivery-table writers and lasts until the writer's transaction ends, so concurrent
-- intake, job intake and reconcile writes take turns. The hub needs that order: a reader that has
-- seen seq N must never later receive an earlier-committed N-1. Almost every writer is one
-- statement, holding the lock from its trigger to its commit; the longest holders are a run's jobs
-- write and a session-group write (a few statements). A run's jobs write (UpsertRunJobs) writes the
-- run row first, whose trigger takes this lock only when the write clears an unfetchable mark, then
-- every job in one statement, whose trigger takes it at the first job row it changes; it holds the
-- lock from whichever takes it first to its commit. A writer that also writes a delivery_runs row
-- takes that row's lock before this one, as a one-statement run write does (row lock, then this
-- lock in its trigger), so the two never wait on each other in a cycle. This migration locks no
-- existing table: 0101-0106 each add one table's trigger, so each migration holds one table's lock
-- alone, for milliseconds (0073's reason).
create table delivery_changes (
  seq      bigserial primary key,
  at       timestamptz not null default clock_timestamp(),
  kind     text not null check (kind in ('pull_request', 'run', 'run_job', 'session_group', 'settings')),
  run_kind text,
  repo     text,
  ref      text,
  t_old    timestamptz,
  t_new    timestamptz
);
create index delivery_changes_at on delivery_changes (at);

create function delivery_change_log(p_kind text, p_run_kind text, p_repo text, p_ref text, p_old timestamptz, p_new timestamptz)
returns void language plpgsql as $$
begin
  perform pg_advisory_xact_lock(6900, 0);
  insert into delivery_changes (kind, run_kind, repo, ref, t_old, t_new)
    values (p_kind, p_run_kind, p_repo, p_ref, p_old, p_new);
  perform pg_notify('delivery_changes', '');
end $$;

create function delivery_pull_requests_changed() returns trigger language plpgsql as $$
declare
  ignored constant text[] := array['updated_at', 'attribution_checked_at'];
begin
  if tg_op = 'INSERT' then
    perform delivery_change_log('pull_request', null, new.repo, new.number::text, null, new.merged_at);
  elsif tg_op = 'DELETE' then
    perform delivery_change_log('pull_request', null, old.repo, old.number::text, old.merged_at, null);
  elsif (to_jsonb(old) - ignored) is distinct from (to_jsonb(new) - ignored) then
    perform delivery_change_log('pull_request', null, new.repo, new.number::text, old.merged_at, new.merged_at);
  end if;
  return null;
end $$;

-- A run touches a window when its head commit or its start is at or after the window's start
-- (ListRuns filters head_commit_at, the timeline's and measures' runs started_at), so it logs the
-- later of the two.
create function delivery_runs_changed() returns trigger language plpgsql as $$
declare
  ignored constant text[] := array['jobs_complete'];
begin
  if tg_op = 'INSERT' then
    perform delivery_change_log('run', new.kind, new.repo, new.run_id::text, null, greatest(new.head_commit_at, new.started_at));
  elsif tg_op = 'DELETE' then
    perform delivery_change_log('run', old.kind, old.repo, old.run_id::text, greatest(old.head_commit_at, old.started_at), null);
  elsif (to_jsonb(old) - ignored) is distinct from (to_jsonb(new) - ignored) then
    perform delivery_change_log('run', new.kind, new.repo, new.run_id::text,
      greatest(old.head_commit_at, old.started_at), greatest(new.head_commit_at, new.started_at));
  end if;
  return null;
end $$;

create function delivery_run_jobs_changed() returns trigger language plpgsql as $$
declare
  r record;
  v_kind text;
  v_t timestamptz;
begin
  if tg_op = 'DELETE' then r := old; else r := new; end if;
  if tg_op = 'UPDATE' and to_jsonb(old) is not distinct from to_jsonb(new) then
    return null;
  end if;
  select kind, greatest(head_commit_at, started_at) into v_kind, v_t
    from delivery_runs where repo = r.repo and run_id = r.run_id;
  perform delivery_change_log('run_job', v_kind, r.repo, r.run_id::text, v_t, v_t);
  return null;
end $$;

create function delivery_session_groups_changed() returns trigger language plpgsql as $$
begin
  if tg_op = 'UPDATE' and to_jsonb(old) - 'updated_at' is not distinct from to_jsonb(new) - 'updated_at' then
    return null;
  end if;
  perform delivery_change_log('session_group', null, null, null, null, null);
  return null;
end $$;

-- Freshness columns change on every GitHub event and every reconcile pass; they log no change.
-- They notify 'delivery_freshness' with their values, so the hub can tell a page when the verdict
-- its freshness row shows changes, without reading the row. left(…, 1000) keeps the payload under
-- Postgres's 8,000-byte NOTIFY limit, which pg_notify enforces by raising an error that would fail
-- the settings update: JSON writes a character as at most six bytes, so the error is at most 6,000.
create function delivery_settings_changed() returns trigger language plpgsql as $$
declare
  freshness constant text[] := array['last_event_at', 'last_reconcile_at', 'last_error', 'updated_at'];
begin
  if tg_op <> 'UPDATE' or (to_jsonb(old) - freshness) is distinct from (to_jsonb(new) - freshness) then
    perform delivery_change_log('settings', null, null, null, null, null);
  end if;
  if tg_op <> 'DELETE' then
    perform pg_notify('delivery_freshness', json_build_object(
      'last_event_at', new.last_event_at, 'last_reconcile_at', new.last_reconcile_at,
      'last_error', left(new.last_error, 1000))::text);
  end if;
  return null;
end $$;
