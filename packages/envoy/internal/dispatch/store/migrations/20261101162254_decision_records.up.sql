-- Decision records on the block graph: the two tables the Decisions view writes.
--
-- decision_keeps is one row per decision a person agreed with: the heading block of a decision of
-- the issue's decisions.md, the sum of the decision's whole text at the moment they pressed Keep,
-- and the changed_version it then stood at. The key is the block rather than the person, because
-- Keep records that the decision has been reviewed and not who reviewed it: a second person's
-- Keep moves the row, and kept_by and kept_at say who last did.
--
-- The sum, not the version, is what tells a kept decision from one waiting for review again. A
-- decision's changed_version is the greatest of the blocks it holds NOW, so it does not move when
-- a block is removed from the decision or moved into or out of it: the text changes and every
-- remaining block keeps its version. The sum covers all three, since it is taken over the
-- decision's text as a whole. changed_version is kept beside it for the version a reader sees.
--
-- user_document_reads is one row per person per document: the version they last read. The Spec
-- tab marks each block that changed since it, so it is keyed on the person as the per-issue and
-- per-agent read state are (user_issue_state, user_agent_read), and login holds the canonical
-- lowercase email every identity names a person by (model.CanonicalLogin).
--
-- Two new tables: the release before this one neither reads nor writes either, nothing existing is
-- read, locked or rewritten, and no stored row can violate them, which is what a rolling update
-- requires of a migration (cmd/dispatch/README.md, "Database migrations").
create table decision_keeps (
  artifact_id uuid not null references artifacts(id) on delete cascade,
  block_id text not null,          -- the decision's heading block
  text_sha256 text not null,       -- the decision's whole text when it was kept
  changed_version integer not null,
  kept_by text not null,
  kept_at timestamptz not null default now(),
  primary key (artifact_id, block_id)
);
create table user_document_reads (
  login text not null,
  artifact_id uuid not null references artifacts(id) on delete cascade,
  version integer not null,
  read_at timestamptz not null default now(),
  primary key (login, artifact_id)
);
