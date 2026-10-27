-- 0121_related_checks.up.sql
-- INFRA-568: the related-item check that runs on every issue create, ask and new decision block
-- searches by meaning first and by the project's rarest words, and says what it did. These tables
-- record each check, so how often it completed, how long it took and why it was not made can be
-- counted, and hold the word statistics it picks its keywords from.
--
-- One row per check of an issue, an ask or a decision block added by a document edit, written by
-- the check itself (api's related check) under its own short context once it has answered, so a
-- check that ran out of time is recorded too. The no-write related route checks without recording.
create table related_checks (
  id uuid primary key default gen_random_uuid(),
  created_at timestamptz not null default now(),
  project_key text not null references projects(key) on delete cascade,
  source_kind text not null check (source_kind in ('issue', 'ask', 'decision_block')),
  -- Where the source lives: its issue, or for an ask or a decision block on an unlinked project
  -- document, that document.
  source_issue_key text references issues(key) on delete cascade,
  source_artifact_id uuid references artifacts(id) on delete cascade,
  check (num_nonnulls(source_issue_key, source_artifact_id) = 1),
  source_ask_id uuid references asks(id) on delete cascade,
  check ((source_kind = 'ask') = (source_ask_id is not null)),
  -- The decision block's id within its document.
  source_block_id text,
  check ((source_kind = 'decision_block') = (source_block_id is not null)),
  actor_kind text not null,
  actor_id text not null,
  -- found: it listed at least one related item; none: it searched and found nothing; not_made: it
  -- did not search to the end, and reason says why.
  outcome text not null check (outcome in ('found', 'none', 'not_made')),
  -- How the check searched: by meaning and by keywords, or by keywords alone. A check not made
  -- did neither to the end, so it has no search.
  search text check (search in ('meaning_and_keywords', 'keywords_only')),
  check ((outcome = 'not_made') = (search is null)),
  -- Why a check was not made (blank_text, timeout, search_failed, too_many_blocks,
  -- already_exists, project_unresolved), or why a check that was made searched by keywords alone
  -- (no_embedder, meaning_timeout, embed_failed, meaning_failed, keywords_only_requested). Null
  -- for a check that searched by meaning and keywords.
  reason text check (reason in (
    'blank_text', 'timeout', 'search_failed', 'too_many_blocks', 'already_exists', 'project_unresolved',
    'no_embedder', 'meaning_timeout', 'embed_failed', 'meaning_failed', 'keywords_only_requested'
  )),
  -- The lexemes the keyword search used, in the order it chose them.
  terms text[] not null default '{}',
  -- Milliseconds: the keyword search's own time with its term selection, the meaning search's
  -- (embedding and query) when it ran, and the whole check's.
  keyword_ms integer,
  meaning_ms integer,
  total_ms integer not null,
  related_count integer not null default 0
);

-- The records route reads one project's checks over a span of time.
create index related_checks_project_created on related_checks (project_key, created_at);

-- How many of a project's searchable items hold each lexeme (the check's df), and how many
-- searchable items the project has (its N), as of refreshed_at. The term-statistics refresh
-- (api's related terms, under the search-term-stats lease, at boot and every 10 minutes) rebuilds a
-- project's rows whole; the check only reads them. A lexeme written since the last refresh has no
-- row, and the check probes it live.
create table search_term_stats (
  project_key text not null references projects(key) on delete cascade,
  lexeme text not null,
  ndoc integer not null,
  primary key (project_key, lexeme)
);

create table search_term_totals (
  project_key text primary key references projects(key) on delete cascade,
  items integer not null,
  refreshed_at timestamptz not null
);
