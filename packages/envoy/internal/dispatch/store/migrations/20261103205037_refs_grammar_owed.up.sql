-- 20261103205037_refs_grammar_owed.up.sql
-- The queue of text owed an index with this build's whole grammar: the bare issue keys, the
-- `#id`s naming a document's question blocks and the short ids a text writes, which a build that
-- predates them indexes without (refs.Mentions). The triggers of the four migrations after this
-- one enqueue every write any build makes, and the seed after them enqueues the text already
-- stored, so text written before this release gets its links and its edges with no edit and no
-- command run.
--
-- A queue of its own rather than a row in 0121's block_grammar_owed: a server of the previous
-- release drains that table, so during a rolling update it would consume the seeded rows, index
-- that text with the grammar it knows and delete them, and this build would never re-index them.
-- It knows nothing of this table, so what is enqueued here waits for a build that reads it.
--
-- `scope` is the one kind that names no source: its id is a document, and draining it re-indexes
-- every source whose text belongs to that document (refs.ScopeSources). A `#id` written before
-- its question settles resolves to nothing at its own write and to the ask once the settlement
-- makes it, which is a write of another row - the ask's, or the document's next version - so the
-- text that cites it is owed its index then.
--
-- Its own migration: it creates a table, an index and three functions no trigger calls yet, so it
-- takes no existing table's lock, and the four trigger migrations after it each hold one table's
-- own (0121-0125's precedent).
create table refs_grammar_owed (
  kind text not null check (kind in ('comment', 'ask', 'message', 'artifact', 'scope')),
  id uuid not null,
  enqueued_at timestamptz not null default now(),
  primary key (kind, id)
);
create index refs_grammar_owed_enqueued on refs_grammar_owed (enqueued_at);

-- The trigger function of the comments, asks and messages triggers: the row's kind is the
-- trigger's argument, and a write that lands on a row already owed moves its enqueued_at, so a
-- drain that read the earlier time leaves the row owed (refs.OwedSources).
create or replace function refs_grammar_owe() returns trigger language plpgsql as $$
begin
  insert into refs_grammar_owed (kind, id, enqueued_at) values (tg_argv[0], new.id, now())
  on conflict (kind, id) do update set enqueued_at = excluded.enqueued_at;
  return null;
end;
$$;

-- A document version owes two rows: the document, whose own text is read with the whole grammar
-- again, and its scope, since the version's question blocks are the map every `#id` written in
-- that document's scope resolves against, and a version can add, rename or drop one.
create or replace function refs_grammar_owe_version() returns trigger language plpgsql as $$
begin
  insert into refs_grammar_owed (kind, id, enqueued_at)
  select owed.kind, new.artifact_id, now() from unnest(array['artifact', 'scope']) as owed(kind)
  on conflict (kind, id) do update set enqueued_at = excluded.enqueued_at;
  return null;
end;
$$;

-- An ask that gains or changes the block it is the question of settles that block: every `#id`
-- naming it in the document's scope resolves from then on, so that document's scope is owed. An
-- ask whose block moves to another document owes the document it left as well, whose scope holds
-- `#id`s that resolved to it and resolve to nothing now.
create or replace function refs_grammar_owe_ask_scope() returns trigger language plpgsql as $$
begin
  if new.block_artifact_id is not null then
    insert into refs_grammar_owed (kind, id, enqueued_at) values ('scope', new.block_artifact_id, now())
    on conflict (kind, id) do update set enqueued_at = excluded.enqueued_at;
  end if;
  if tg_op = 'UPDATE' and old.block_artifact_id is not null
     and old.block_artifact_id is distinct from new.block_artifact_id then
    insert into refs_grammar_owed (kind, id, enqueued_at) values ('scope', old.block_artifact_id, now())
    on conflict (kind, id) do update set enqueued_at = excluded.enqueued_at;
  end if;
  return null;
end;
$$;
