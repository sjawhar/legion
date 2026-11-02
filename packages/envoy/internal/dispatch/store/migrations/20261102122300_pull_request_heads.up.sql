-- 20261102122300_pull_request_heads.up.sql
--
-- The current head of every pull request Dispatch has seen an event for, so a recorded check
-- result can say whether it ran at the commit the work stands at. This is not
-- delivery_pull_requests, which keeps only the merged pull requests of the delivery population
-- and holds no head commit at all: this table holds open ones too, by any author and in any
-- repository, and keeps one row per pull request rather than one per merge.
--   repo, number       the pull request, as the rest of Dispatch names one ("owner/repo" and n).
--   head_sha           the commit its branch stands at; the envelope's head_sha, which on a
--                      merge ("closed" with merged true) is the final head of the merged work.
--   head_ref, base_ref the branch names.
--   state              'open', 'closed' (closed unmerged) or 'merged'.
--   issue_key          the issue Dispatch's pull request attribution picks for it
--                      (delivery/attribution.go), null when none is named.
--   attribution_source which of the attribution's five sources named that issue, or 'none'.
--   title, url         what the event or the App read said, for a reader.
--   event_at           the pull request's updated_at as the event reported it; an older event
--                      arriving after a newer one leaves the row as it is.
--   checked_at         when the reconcile last read the pull request through the App; the
--                      reconcile re-reads an open row older than five minutes.
--
-- A new table: nothing existing is read, locked or rewritten, and the release before this one
-- neither reads nor writes it, so it rolls beside this one (cmd/dispatch/README.md, "Database
-- migrations"). The census answers 0, and rolling back past it is an image rollback alone.
create table pull_request_heads (
    repo text not null,
    number integer not null,
    head_sha text not null,
    head_ref text not null default '',
    base_ref text not null default '',
    state text not null check (state in ('open', 'closed', 'merged')),
    issue_key text references issues (key) on delete set null,
    attribution_source text not null default 'none',
    title text not null default '',
    url text not null default '',
    event_at timestamptz,
    checked_at timestamptz,
    primary key (repo, number)
);

-- The currency read: is this commit the head of a pull request attributed to this issue?
create index pull_request_heads_issue_head on pull_request_heads (issue_key, head_sha) where issue_key is not null;
-- The reconcile's batch: the open rows read longest ago first.
create index pull_request_heads_open_stale on pull_request_heads (checked_at) where state = 'open';
