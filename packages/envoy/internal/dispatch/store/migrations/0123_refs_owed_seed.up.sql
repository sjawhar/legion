-- 0123_refs_owed_seed.up.sql
-- INFRA-189: every reference source stored before this release is owed a re-index (0118), so the
-- reference index reads a bare issue key and a `#id` in it with no command run: each document with
-- a version holding markdown, each ask, each comment and each issue message - the sources
-- refs.RebuildAll reads. 0119-0122's triggers are live by now, so a write that lands while this
-- runs is owed either way, and a row a trigger wrote is kept as it is. About 4,600 rows on
-- production (763 documents, 929 asks, 2,916 comments and messages, counted 2026-10-24).
-- Reads artifacts, artifact_versions, asks, comments and messages (ACCESS SHARE) and writes only
-- refs_owed.
insert into refs_owed (kind, id)
select 'artifact', a.id::text from artifacts a
where exists (select 1 from artifact_versions v where v.artifact_id = a.id and v.markdown is not null)
union all
select 'ask', id::text from asks
union all
select 'comment', id::text from comments
union all
select 'message', id::text from messages where issue_key is not null
on conflict (kind, id) do nothing;
