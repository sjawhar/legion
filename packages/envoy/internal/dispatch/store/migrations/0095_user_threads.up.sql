-- 0095_user_threads.up.sql
-- LEGION-687: the Replies page. One row per person per thread they wrote in (an ask's thread, a
-- comment thread by its root, or an issue message thread by its root), holding how far they have
-- read it (read_through) and the newest write anyone else made there (replied_at, last_reply_id).
-- A row is unread while someone else wrote after the person's read mark; the stored flag and its
-- partial index let the page and its count read only a person's unread rows.
--
-- Triggers keep the rows, so no read scans comments: a write by anyone else (a comment, an issue
-- message, an answer) moves every other participant's replied_at, and a person's own write moves
-- their read_through. Only an insert or an answer counts (an edit, a resolution or an anchor
-- refresh changes nothing), authorship is the author's kind and id (a session writing under a
-- person's own token is still a session), and a write whose owner has no project (an agent
-- conversation's file, which no route comments on) makes no row, since it has no page to open.
--
-- This migration creates the table and the comment trigger, and defines the login normalizer and
-- the message trigger's function, which 0097 attaches; 0096 adds the asks trigger, 0098 seeds the
-- rows from history and 0099 catches up the writes that committed while 0098 ran.
-- Each trigger is its own migration so its transaction holds that table's lock alone, for
-- milliseconds (0075's precedent); this one locks comments alone.
create table user_threads (
  login         text not null,
  thread_id     uuid not null,
  kind          text not null check (kind in ('ask', 'comment', 'message')),
  project_key   text not null,
  issue_key     text,
  artifact_id   uuid,
  read_through  timestamptz not null,
  replied_at    timestamptz,
  last_reply_id uuid,
  unread        boolean generated always as (replied_at is not null and replied_at > read_through) stored,
  primary key (login, thread_id)
);

create index user_threads_unread on user_threads (login, replied_at desc, thread_id desc) where unread;
create index user_threads_thread on user_threads (thread_id);

-- user_threads_login is the login a person's write keys its row on: the author id lowercased with
-- the ASCII white space around it trimmed, as the API's canonicalLogin keys the read routes
-- (strings.ToLower(strings.TrimSpace(...))); the two agree on every ASCII id. A person's author id
-- is their identity's login, which sign-in has already lowercased and trimmed in Go
-- (identity.Person, HeaderIdentity, the dev sign-in), so no other id reaches a row.
create function user_threads_login(p_author_id text) returns text
  language sql immutable as $$ select lower(btrim(p_author_id, E' \t\n\x0b\f\r')) $$;

-- user_threads_record applies one write to thread p_thread. p_reply says whether the other
-- participants should see the write (a comment, a message or an answer), as opposed to asking,
-- which starts the thread and only moves its asker's own read mark. p_write names the write the
-- others' rows point at: the comment or message, or the ask itself for its answer. p_issue and
-- p_artifact name the thread's owner, whose project the row records and the mark route locks.
create function user_threads_record(
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
        last_reply_id = case when replied_at is null or p_at >= replied_at then p_write else last_reply_id end
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

-- Both comment write paths store every reply against its thread's head (threadHeadOf), so the
-- thread is the ask, else the root a reply names, else the comment itself.
create function user_threads_comment() returns trigger language plpgsql as $$
begin
  perform user_threads_record(
    coalesce(new.ask_id, new.reply_to, new.id),
    case when new.ask_id is not null then 'ask' else 'comment' end,
    new.author->>'kind', new.author->>'id', new.created_at, new.id, true,
    new.issue_key, new.artifact_id
  );
  return null;
end;
$$;

-- An issue message's thread is its root, found up in_reply_to (seven deep at most in production;
-- the walk stops at 64). An issue-less message is a direct conversation, the Agents page's.
create function user_threads_message() returns trigger language plpgsql as $$
declare
  v_root uuid := new.id;
  v_parent uuid := new.in_reply_to;
  v_depth integer := 0;
begin
  if new.issue_key is null then
    return null;
  end if;
  while v_parent is not null and v_depth < 64 loop
    v_root := v_parent;
    select in_reply_to into v_parent from messages where id = v_root;
    v_depth := v_depth + 1;
  end loop;
  perform user_threads_record(
    v_root, 'message', new.author->>'kind', new.author->>'id', new.created_at, new.id, true,
    new.issue_key, null
  );
  return null;
end;
$$;

create trigger user_threads_comment after insert on comments
  for each row execute function user_threads_comment();
