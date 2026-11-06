-- 20261103205042_refs_grammar_seed.up.sql
-- Owes an index with the whole grammar to every text already stored: each comment, each ask, each
-- issue message and each document with a markdown version - the sources rebuild-refs reads
-- (refs.RebuildAll) - and each such document's scope, since a `#id` any of those texts wrote
-- before its question settled resolves now and was indexed by no build that stored it.
-- It enqueues every row rather than the ones whose text looks like a citation: no SQL predicate
-- finds a bare issue key, a `#id` or a short id, which is why the four triggers before it enqueue
-- unconditionally too. The drain reads each text once and writes only what moved.
-- Those triggers are live by now, so a write that lands while this runs is owed by its trigger,
-- and a row a trigger already wrote keeps its enqueued_at.
-- Reads comments, asks, messages, artifacts and artifact_versions (ACCESS SHARE) and writes only
-- refs_grammar_owed (0126's precedent).
insert into refs_grammar_owed (kind, id)
select 'comment', id from comments
union all
select 'ask', id from asks
union all
select 'message', id from messages where issue_key is not null
union all
select owed.kind, a.id from artifacts a, unnest(array['artifact', 'scope']) as owed(kind)
where exists (select 1 from artifact_versions v where v.artifact_id = a.id and v.markdown is not null)
on conflict (kind, id) do nothing;
