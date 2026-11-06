-- 20261102122200_check_results.up.sql
--
-- One row per result recorded against a `check` block of an issue's Verification document
-- (dispatch result). A result proves one check ran, so the row names the check, the words it
-- proved and the commit it ran at:
--   issue_key          the issue the result was recorded on, which may be a descendant of the
--                      issue whose Verification document holds the check.
--   check_issue_key    the issue whose Verification document holds the check, and the issue the
--                      result.recorded event is appended on.
--   check_artifact_id  that Verification document.
--   check_block_id     the check block's id within it.
--   check_version      document_blocks.changed_version of that block when the result was
--                      recorded: the document version whose words the result proves.
--   check_sha256       document_blocks.text_sha256 of that block at the same moment. A read
--                      compares it with the block's digest now: a check whose words changed
--                      since makes every result recorded against the older words older too.
--   role               who recorded it (implementer, tester, reviewer): at most 32 characters
--                      of [a-z-], the grammar the pull-request body line prints.
--   surface, run       where a person reaches the change and the command that proved it.
--   observed           what the command printed.
--   commit_sha         the 40-hex commit the check ran at.
--   negative_control   the deliberately broken input and the refusal seen.
--   failed, skipped, excluded
--                      how many cases the run failed, skipped and excluded; a pass counting any
--                      of them contradicts itself and is refused by the route.
--   verdict            'pass' or 'fail'.
--   screenshots        dispatch:// references to image documents of the recording issue or of
--                      the check's issue.
--   session, actor     the session that recorded it ('' for a person) and the actor as stored
--                      everywhere else.
--   event_seq          the seq of the result.recorded event on the check's issue, so a reader
--                      can find the event a row was written by.
-- Nothing updates a row: a result is a record of one run, and another run is another row.
--
-- A new table: nothing existing is read, locked or rewritten, and the release before this one
-- neither reads nor writes it, so it rolls beside this one (cmd/dispatch/README.md, "Database
-- migrations"). The census answers 0, and rolling back past it is an image rollback alone.
create table check_results (
    id uuid primary key default gen_random_uuid(),
    issue_key text not null references issues (key) on delete cascade,
    check_issue_key text not null references issues (key) on delete cascade,
    check_artifact_id uuid not null references artifacts (id) on delete cascade,
    check_block_id text not null,
    check_version integer not null,
    check_sha256 text not null,
    role text not null check (role ~ '^[a-z-]{1,32}$'),
    surface text not null default '',
    run text not null default '',
    observed text not null default '',
    commit_sha text not null check (commit_sha ~ '^[0-9a-f]{40}$'),
    negative_control text not null default '',
    failed integer not null default 0 check (failed >= 0),
    skipped integer not null default 0 check (skipped >= 0),
    excluded integer not null default 0 check (excluded >= 0),
    verdict text not null check (verdict in ('pass', 'fail')),
    screenshots text[] not null default '{}',
    session text not null default '',
    actor jsonb not null,
    created_at timestamptz not null default now(),
    event_seq bigint,
    check (verdict <> 'pass' or (failed = 0 and skipped = 0 and excluded = 0))
);

-- The Verification tab's read: every result of one check, newest first.
create index check_results_check on check_results (check_artifact_id, check_block_id, created_at desc);
-- Every result an issue recorded, for the issue's own reads.
create index check_results_issue on check_results (issue_key);
