-- 20261101214041_artifact_grants.up.sql
-- Which data files a page file may read (dispatch://INFRA-682, "Data grants"). A `view` block
-- names one HTML page file and one to eight data files, and Dispatch sends the page only the
-- files a person granted it. A grant belongs to the page file, not to the block: any author, an
-- agent included, writes a block, so a list in the block would let an agent choose its own data,
-- while a grant is written only through routes no token may call (403 HUMAN_ONLY).
--
-- One row per grant:
--   id                the grant's own id, which DELETE /api/v1/artifacts/{id}/grants/{grant_id}
--                     names, so two people changing the list at once lose no change.
--   page_artifact_id  the page file the grant is read for. It covers every version of that file,
--                     so a new upload of the page's code keeps its grants.
--   data_artifact_id  the file the page may read, stored as the id the `dispatch://` reference
--                     resolved to, so a later file of the same name is not covered. It may be the
--                     page file itself, which costs no code and refuses nothing a person means.
--   version           the one version the grant covers, or null for every version of that file,
--                     the newest included. A person who wants a fixed record grants one version.
--   granted_by        the person who granted it. Only a person may, so every grantor is a row of
--                     people (0068); a token gets 403 and writes nothing.
--   created_at        when it was granted. The panel on the page file's view shows both.
--
-- Both files are artifacts, and a deleted artifact takes its grants with it, as the block graph's
-- rows do (0121). artifact_grants_pair holds one grant per (page, data, version), with nulls not
-- distinct, so two unpinned grants of one pair cannot both exist and a repeated POST of one pair
-- is the grant that is already there rather than a second row. artifact_grants_page serves the
-- one read the panel and every block make, this page file's grants oldest first, without a sort.
--
-- A new table: nothing existing is read or rewritten, and the release before this one neither
-- reads nor writes it, so it rolls beside this one (cmd/dispatch/README.md, "Database
-- migrations"), serving no block and no panel while it runs. The census answers 0, and rolling
-- back past it is an image rollback alone.
create table artifact_grants (
    id uuid primary key default gen_random_uuid(),
    page_artifact_id uuid not null references artifacts (id) on delete cascade,
    data_artifact_id uuid not null references artifacts (id) on delete cascade,
    version integer check (version is null or version >= 1),
    granted_by text not null references people (email),
    created_at timestamptz not null default now(),
    constraint artifact_grants_pair unique nulls not distinct (page_artifact_id, data_artifact_id, version)
);
create index artifact_grants_page on artifact_grants (page_artifact_id, created_at, id);
