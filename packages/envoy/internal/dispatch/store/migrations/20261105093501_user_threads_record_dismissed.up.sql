-- 20261105093501_user_threads_record_dismissed.up.sql
--
-- INFRA-450: a new write by somebody else brings a dismissed thread back, unread, and the
-- person's own reply does not - a reply they send from the Dismissed view leaves the thread
-- there. This replaces user_threads_record (0095) with the same function plus two assignments:
-- the statement that moves the other participants' replied_at also clears their dismissed_at and
-- increments their version. One statement, so a thread can never be left dismissed behind a
-- write by somebody else, which is what lets every read spell "dismissed" as `dismissed_at is
-- not null`. The writer's own row is the insert below, which touches neither column, so their
-- own reply leaves their own mark standing and their own version where it stood, which is what
-- keeps a reply they send from the Dismissed view there.
--
-- Both assignments are unconditional, and the increment has to be: every row this statement
-- reaches is one somebody else is writing in (login is distinct from v_writer), so every one of
-- them has gained a write the person may not have drawn, whatever that write's time or id. Two
-- writes have neither a new time nor a new id - an answer changed through 0096, which passes the
-- ask's own id as p_write, so last_reply_id is already that id, and a write stamped older than
-- the row's replied_at, which moves neither column since both take the newer of the two - and a
-- version read off either column would stand still across them and let a dismissal that never
-- drew the write bury it.
--
-- A write is stamped at its transaction's start, before anybody else can see it: p_at is the
-- write's own time - now(), which is transaction_timestamp(), for a message, the moment the
-- insert ran for a comment (0065) and the answer's own time for an answer - and every one of
-- those is fixed before the writing transaction commits. This is the one statement of that
-- fact, which the Inbox's dismissal is built on: a dismiss that began later - and that
-- therefore cannot have seen this write - may carry a mark newer than it, so no comparison of
-- the two times can tell a person who had seen this write from one who could not have. The
-- clear below compares the mark with nothing for that reason, and the dismissal's own two
-- checks (inboxDismissMarkThreads and inboxDismissCheckAgentRoots, api/inbox_dismiss.go) point
-- here rather than saying it again.
--
-- Nor is there anything for the clear to protect: a trigger runs once, inside the writing
-- transaction, so every mark this statement can reach was written before this write committed,
-- by a dismiss that had not seen it. A dismiss that had seen it is turned away by the version
-- instead - its update writes only rows whose version is still the one the list answered - and
-- the increment below moves that version under the row lock that dismissal holds.
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
        dismissed_at = null,
        version = version + 1
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
