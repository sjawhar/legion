-- 20261105093501_user_threads_record_dismissed.up.sql
--
-- INFRA-450: a new write by somebody else brings a dismissed thread back, unread, and the person's
-- own reply does not - a reply they send from the Dismissed view leaves the thread there. This
-- replaces user_threads_record (0095) with the same function plus one assignment: the statement
-- that moves the other participants' replied_at also clears their dismissed_at. One statement, so
-- a thread can never be left dismissed behind a write by somebody else, which is what lets every
-- read spell "dismissed" as `dismissed_at is not null`. The writer's own row is the insert below,
-- which never touches dismissed_at, so their own reply leaves their own mark standing, which is
-- what keeps a reply they send from the Dismissed view there.
--
-- The clear is unconditional: it compares the mark with nothing. p_at is the writing
-- transaction's own stamp (now(), which is transaction_timestamp()), taken before the write
-- became visible to anybody, so a dismiss that began later - and that therefore cannot have seen
-- this write - may carry a mark newer than it, and no comparison of the two can tell a person who
-- had seen this reply from one who could not have. Nor is there anything for one to protect: a
-- trigger runs once, inside the writing transaction, so every mark this statement can reach was
-- written before this write committed, by a dismiss that had not seen it. A dismiss that had seen
-- it is turned away by the version it carries instead - its update writes only rows whose
-- last_reply_id is still the one the person saw (inboxDismissMarkThreads, api/inbox_dismiss.go),
-- and the statement below moves last_reply_id with replied_at, under the row lock that dismissal
-- holds.
--
-- Its own migration, as each trigger of 0095-0097 is: `create or replace function` takes ACCESS
-- EXCLUSIVE on the function's own catalogue row and no lock at all on user_threads, comments, asks
-- or messages, so the window is one catalogue write and the triggers that call it pick up the new
-- body on their next statement. No row is read or rewritten and the census answers 0.
--
-- Measured on this pod's test database: user_threads held 24 rows after the api suite's Inbox
-- seeds, the largest any one test's database held while
-- `go test ./internal/dispatch/api/ -run 'InboxRead|Replies|InboxRows'` ran (sampled every 0.3 s).
-- The window does not grow with that count: a function replacement reads no row of any size of
-- table.
create or replace function user_threads_record(
  p_thread uuid, p_kind text, p_author_kind text, p_author_id text, p_at timestamptz,
  p_write uuid, p_reply boolean, p_issue text, p_artifact uuid
) returns void language plpgsql as $$
declare
  v_writer text := case when p_author_kind = 'user' then user_threads_login(p_author_id) end;
  v_project text;
begin
  if p_issue is not null then
    select project_key into v_project from issues where key = p_issue;
  elsif p_artifact is not null then
    select project_key into v_project from artifacts where id = p_artifact;
  end if;
  if v_project is null then
    return;
  end if;
  if p_reply then
    update user_threads
    set replied_at = greatest(replied_at, p_at),
        last_reply_id = case when replied_at is null or p_at >= replied_at then p_write else last_reply_id end,
        dismissed_at = null
    where thread_id = p_thread and login is distinct from v_writer;
  end if;
  if v_writer is not null then
    insert into user_threads (login, thread_id, kind, project_key, issue_key, artifact_id, read_through)
    values (v_writer, p_thread, p_kind, v_project, p_issue,
            case when p_issue is null then p_artifact end, p_at)
    on conflict (login, thread_id) do update
      set read_through = greatest(user_threads.read_through, excluded.read_through);
  end if;
end;
$$;
