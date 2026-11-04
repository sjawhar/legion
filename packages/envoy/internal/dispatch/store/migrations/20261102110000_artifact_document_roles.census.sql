-- The backfill gives one role to each issue document slugged verification-md or plan-md, and the
-- new partial unique index holds an issue to one document per role. Each role comes from one
-- slug, so a row the index would refuse is a second document of one issue under one of those
-- slugs; artifacts_ref_key_key (0009) makes <issue key>/<slug> unique, so there is none. The two
-- checks admit every stored row, whose role starts null. The census cannot name the column,
-- which does not exist yet when it is taken.
select count(*) - count(distinct (issue_key, slug))
from artifacts
where kind = 'doc' and issue_key is not null and slug in ('verification-md', 'plan-md');
