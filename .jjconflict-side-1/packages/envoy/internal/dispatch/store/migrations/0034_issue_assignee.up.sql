-- Every issue names the human who answers its asks: the lowercase GitHub login of someone on
-- the sign-in allowlist, or null for unassigned. Filters compare with plain equality, so the
-- API canonicalises (trim + lower) on every write. The partial index serves the inbox's
-- "mine" / "unassigned" partitions, which only ever look at open issues.
alter table issues add column assignee text;
create index issues_assignee on issues (assignee) where closed_at is null;
