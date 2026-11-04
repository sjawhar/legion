-- 20261102110000_artifact_document_roles.up.sql
--
-- The role an issue's document plays, beside is_primary, which keeps naming the spec. A document
-- an issue holds under one of two reserved names is the issue's Verification document or its
-- Plan, and the tabs that show each find it by this column rather than by reading names back:
--   role  'verification' for the document created as verification.md, 'plan' for the one created
--         as plan.md, null for every other document. The upload route sets it from the slug the
--         name gives when it creates the row (api/artifacts.go) and never changes it, as
--         is_primary never changes: a second upload of the same name is that document's next
--         version, under the row this one already made.
-- Its two checks hold the column to what that writer can produce: one of the two roles, and only
-- on a markdown document of an issue (a project document and a conversation's file have no
-- issue to play a role for). The partial unique index holds an issue to one document per role.
-- Both are sound on every stored row, which starts null, and on the backfill below:
-- artifacts_ref_key_key (0009) makes <issue key>/<slug> unique, so an issue holds at most one
-- verification-md and at most one plan-md document.
--
-- The backfill gives the role to the issue documents already stored under those slugs, so the
-- Plan tab works on every issue that already has a plan rather than only on plans written after
-- this deploy. It reads the slug, not the name, because the slug is what the upload route holds
-- unique within an issue and what it derives the role from.
--
-- The column is nullable with no default: the release before this one neither reads nor writes
-- it and goes on writing documents with no role, which this schema admits, so it rolls beside
-- this one (cmd/dispatch/README.md, "Database migrations"). The add and the backfill each take
-- artifacts' ACCESS EXCLUSIVE lock in this transaction, and the census answers 0.
alter table artifacts
    add column role text,
    add constraint artifacts_role_check check (role is null or role in ('verification', 'plan')),
    add constraint artifacts_role_is_an_issue_document check (role is null or (kind = 'doc' and issue_key is not null));

update artifacts
set role = case slug when 'verification-md' then 'verification' else 'plan' end
where kind = 'doc' and issue_key is not null and slug in ('verification-md', 'plan-md');

create unique index artifacts_issue_role on artifacts (issue_key, role) where role is not null;
