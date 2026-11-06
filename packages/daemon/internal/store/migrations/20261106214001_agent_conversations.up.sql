-- 0052_agent_conversations.up.sql — every conversation an agent of this daemon's claims has had.
--
-- One row per session: the Legion project token, issue, role and role token it ran for, the
-- harness it ran on, where its conversation lives (the session file's path, which is also its key in
-- Oh My Pi's session table, or the Claude Code transcript's path), when it started, and how the
-- daemon learned of it: 'register' from a claim's registration (started_at the registration's time),
-- 'scan' from the boot's backfill by title (started_at the session header's). A register row is never
-- replaced by a scan, and no close, suspension or retirement deletes a row: the table is the record
-- of who an issue's agents were, after their claims forgot them.
create table agent_conversations (
  session text primary key,
  project text not null,
  issue text not null,
  role text not null,
  token text not null,
  harness text not null check (harness in ('omp', 'claude')),
  location text not null,
  started_at timestamptz not null,
  source text not null check (source in ('register', 'scan')),
  recorded_at timestamptz not null
);

create index agent_conversations_token on agent_conversations (token);
