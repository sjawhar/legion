-- 0120_refs_owed.up.sql
-- INFRA-189: the reference sources whose stored text the reference index has not read by this
-- release's rules. A bare issue key and a `#<question id>` become references, so every text stored
-- before this release, and every text a server of the release before it writes while a rolling
-- update runs, owes a re-index. Old and new servers write one database while a roll lasts, and only
-- the database sees both, so triggers fill this queue (0121-0124, one table per migration as
-- 0073-0077 are) and 0125 seeds it with every source already stored.
--
-- One row per source, by the kind refs.from_kind names and its id as refs.from_id holds it (text):
-- artifact (a document, re-indexed from its latest version), ask (its question), comment (its
-- body), message (an issue message's body), and scope (a document whose question map may have
-- changed, so every source in its scope citing a `#id` is re-indexed; its id is the document's).
-- refs.ReplaceCounted deletes its own source's row in the write's transaction, so a write this
-- release indexes is not owed; a release before it never deletes, so its writes stay owed until
-- docs.Service.RunReferenceReindex re-indexes them. The triggers re-date a row they find
-- (enqueued_at), which also takes its row lock for the writer's transaction: the drain claims a row
-- with `for update skip locked` and re-reads the source only after deleting it, so a write it has
-- not seen is never dropped with the row.
--
-- This migration creates only the new table, which no statement reads yet.
create table refs_owed (
  kind text not null check (kind in ('artifact', 'ask', 'comment', 'message', 'scope')),
  id text not null,
  enqueued_at timestamptz not null default now(),
  primary key (kind, id)
);

-- refs_owed_enqueue is the one write path onto refs_owed besides the seed: each trigger calls it.
-- It never fails the write that fires it; a row already owed is re-dated, which locks it until the
-- writer commits.
create function refs_owed_enqueue(p_kind text, p_id text) returns void language sql as $$
  insert into refs_owed (kind, id) values (p_kind, p_id)
  on conflict (kind, id) do update set enqueued_at = excluded.enqueued_at
$$;
