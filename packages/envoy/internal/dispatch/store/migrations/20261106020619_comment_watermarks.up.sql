-- 20261106020619_comment_watermarks.up.sql
--
-- A comment list answers under a validator read before its body is built (INFRA-635). Until now
-- that validator was the hash of the answer's own bytes, so the handler had to load the comments,
-- their mentions and their delivery attempts, shape them and encode them - display trees and all -
-- before it could know the key its kept answer sits under, which is the whole of what a warm read
-- costs. The issue's activity feed is already answered the other way round: it reads
-- `issues.last_seq`, builds its ETag from that, and writes the kept bytes out having built
-- nothing. A comment list cannot stand on `last_seq`, because a delivery claim and the resolution
-- recorded before a send write `comment_deliveries` and append no event at all. So it gets a
-- counter of its own, one per list, raised by every write the list shows.
--
-- One row per comment-list owner, keyed the way a comment is keyed (`comments_one_owner`): an
-- issue's list and an unlinked document's list are the two lists the API answers, so the two
-- owner columns are the two a comment carries, exactly one of them filled. `seq` only ever rises,
-- and what it counts is writes rather than rows: a reader compares it with the one it was given
-- and never reads a meaning into the distance between them.
--
-- The table carries no foreign key to `issues` or `artifacts` on purpose. The triggers below are
-- deferred to commit, so a transaction that deletes an owner's comments and then the owner itself
-- would raise the counter for a row that no longer exists, and a foreign key would refuse that
-- commit; a counter left behind for a deleted owner costs one row and is read by nobody, and a key
-- taken again later keeps rising from where it stood, which hands no new issue the validator an
-- old one gave.
create table comment_watermarks (
  issue_key text,
  artifact_id uuid,
  seq bigint not null,
  constraint comment_watermarks_one_owner check ((issue_key is null) <> (artifact_id is null))
);
-- One unique index per owner column: each is what the upsert below infers its conflict from, and
-- together they are what holds a list to one counter row.
create unique index comment_watermarks_issue_key on comment_watermarks (issue_key) where issue_key is not null;
create unique index comment_watermarks_artifact_id on comment_watermarks (artifact_id) where artifact_id is not null;

-- comment_watermark_bump raises one owner's counter, creating its row the first time. The
-- increment is `seq + 1` inside the upsert rather than a value the caller read first: an
-- `on conflict do update` waits on the conflicting row and then reads the version that commit
-- left, so two transactions committing at once each add one and neither loses the other's bump.
-- A call naming no owner at all does nothing, which is the shape a row that carries neither
-- column could take; `comments_one_owner` means no such comment exists.
create function comment_watermark_bump(bump_issue_key text, bump_artifact_id uuid) returns void
language plpgsql as $$
begin
  if bump_issue_key is not null then
    insert into comment_watermarks (issue_key, seq) values (bump_issue_key, 1)
      on conflict (issue_key) where issue_key is not null
      do update set seq = comment_watermarks.seq + 1;
  elsif bump_artifact_id is not null then
    insert into comment_watermarks (artifact_id, seq) values (bump_artifact_id, 1)
      on conflict (artifact_id) where artifact_id is not null
      do update set seq = comment_watermarks.seq + 1;
  end if;
end $$;

-- comments_watermark raises the counter of every list the written row belongs to: the owner it
-- had and the owner it has, which are the same owner for every write the API makes and two only
-- for a row moved between lists, where both lists have changed.
create function comments_watermark() returns trigger language plpgsql as $$
begin
  if tg_op <> 'INSERT' then
    perform comment_watermark_bump(old.issue_key, old.artifact_id);
  end if;
  if tg_op <> 'DELETE' then
    perform comment_watermark_bump(new.issue_key, new.artifact_id);
  end if;
  return null;
end $$;

-- comment_side_table_watermark raises the counter of the list holding the comment a mention or a
-- delivery attempt belongs to. It reads that comment rather than carrying the owner on the side
-- table, so the side tables keep the shape every reader and writer of them already has. A comment
-- deleted in the same transaction leaves nothing to read, and the loop then bumps nothing: that
-- comment's own delete has already bumped the list it was on.
create function comment_side_table_watermark() returns trigger language plpgsql as $$
declare
  written uuid[] := array[]::uuid[];
  written_comment uuid;
  list record;
begin
  if tg_op <> 'INSERT' then
    written := written || old.comment_id;
  end if;
  if tg_op <> 'DELETE' then
    written := written || new.comment_id;
  end if;
  foreach written_comment in array written loop
    for list in select issue_key, artifact_id from comments where id = written_comment loop
      perform comment_watermark_bump(list.issue_key, list.artifact_id);
    end loop;
  end loop;
  return null;
end $$;

-- Three constraint triggers, one per table a comment list reads, each `after insert or update or
-- delete` with no column list: every write of those rows moves the validator whatever column it
-- wrote, so a writer added later needs no change here and a test or a migration that writes the
-- rows straight through SQL moves it too. `deferrable initially deferred` is what makes them
-- usable at all: an immediate trigger would take the counter row where the write happens and hold
-- it until that transaction commits, so one long-running comment transaction would serialise every
-- comment write of its issue for its whole life. Deferred, the row is taken for the length of a
-- commit, and the bump is still inside the writing transaction, so a reader sees the new rows and
-- the new counter together or neither.
create constraint trigger comments_watermark after insert or update or delete on comments
  deferrable initially deferred for each row execute function comments_watermark();
create constraint trigger comment_mentions_watermark after insert or update or delete on comment_mentions
  deferrable initially deferred for each row execute function comment_side_table_watermark();
create constraint trigger comment_deliveries_watermark after insert or update or delete on comment_deliveries
  deferrable initially deferred for each row execute function comment_side_table_watermark();
