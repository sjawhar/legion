-- 0070_write_suggestions.up.sql
-- LEGION-550: filing an issue or an ask returns its likely duplicates and matching past
-- decisions without blocking the write, and Dispatch records what the agent did next so the
-- suggestion's accuracy can be counted later.
--
-- One row per suggested item (up to suggestionRelatedCount "related" rows plus at most one
-- "decision" row) offered on one write. outcome starts 'ignored' and is advanced in place by
-- the outcome sweep (api.RunSuggestionOutcomeSweep): to 'acted_on' when a later write cites the
-- suggested item from the source (the issue, or the project document an ask sits on), or writes
-- directly to the suggested issue, by the same actor; to 'overridden' when the source instead
-- gets further activity from that actor with neither signal. A row that never sees either stays
-- 'ignored', which also carries "nobody has reacted yet" for a suggestion too recent to judge.
--
-- Both signals are actor-and-timing heuristics, not a check that the later activity is actually
-- about the suggestion: 'overridden' fires on any later write to the source by that actor, so a
-- correct suggestion the agent simply never read scores the same as one it read and rejected
-- (undercounts true positives); 'acted_on' by direct update fires on any later write to the
-- suggested issue by that actor, with no topical check, so unrelated same-actor activity there
-- can inflate it (overcounts true positives). This table is not yet the input to LEGION-386's
-- 90%-right threshold on its own; whoever computes that threshold should read outcome_detail and
-- weigh these two known error directions, not treat the ratio as exact.
create table write_suggestions (
  id uuid primary key default gen_random_uuid(),
  created_at timestamptz not null default now(),
  source_kind text not null check (source_kind in ('issue', 'ask')),
  -- Where the source lives: its issue, or for an ask on an unlinked project document, that document.
  source_issue_key text references issues(key) on delete cascade,
  source_artifact_id uuid references artifacts(id) on delete cascade,
  check (num_nonnulls(source_issue_key, source_artifact_id) = 1),
  source_ask_id uuid references asks(id) on delete cascade,
  check ((source_kind = 'ask') = (source_ask_id is not null)),
  check (source_kind = 'ask' or source_issue_key is not null),
  actor_kind text not null,
  actor_id text not null,
  role text not null check (role in ('related', 'decision')),
  rank int not null,
  suggested_kind text not null,
  suggested_id text not null,
  -- The issue owning the suggested item, when it has one: the item's own key for an issue, its
  -- owner's key for an issue-owned ask/document/comment/message, null for a project-level one.
  suggested_issue_key text,
  outcome text not null default 'ignored' check (outcome in ('ignored', 'acted_on', 'overridden')),
  outcome_detail text,
  outcome_at timestamptz
);

-- Both sweep passes and the acted-on-by-citation pass start from the pending set; indexed
-- partially since a resolved row is never looked at again.
create index write_suggestions_pending_source on write_suggestions (source_issue_key) where outcome = 'ignored';
create index write_suggestions_pending_target on write_suggestions (suggested_issue_key) where outcome = 'ignored' and suggested_issue_key is not null;
