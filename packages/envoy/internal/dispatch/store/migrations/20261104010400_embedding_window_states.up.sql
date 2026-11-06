-- 20261104010400_embedding_window_states.up.sql — the block state each owner's windows were
-- written from.
--
-- INFRA-370 moves meaning search onto windows of an owner's blocks (20261102233625). The
-- block-index tick's window arm gives windows to every owner that holds none
-- (docs.windowDrift), which is every owner written before that change. That test cannot see the
-- one case a rolling deploy makes: the release before this one re-indexes an owner's blocks on
-- an edit and writes no window, and this release's migrations drop the triggers that would have
-- noticed. Such an owner keeps the windows of its old text - a hit's covers can name blocks it
-- no longer holds, and the tick then deletes the whole-text row standing behind those windows -
-- and the arm passes over it for ever, because it does hold windows.
--
-- This table is what the arm reads instead: one row per owner whose windows were written, with
-- the state its blocks were in when they were (refs.writeWindows writes it in the same
-- transaction as the windows). A document's state is the version its blocks are indexed at
-- (document_block_versions.version), an item's the sum of the body they were split from
-- (comment_block_state, message_block_state and ask_block_state's body_sha256) - in each case
-- the value the older build does move when it re-indexes blocks. An owner whose state is not
-- the one its windows carry, and one with no row at all, is an owner the arm windows again.
--
-- The owner key is the windows' own: a document's artifact id, "<kind>/<uuid>" for a comment, a
-- message or an ask, the part of an embeddings window id before its #. There is no foreign key
-- to any of them: one key column cannot reference four tables, and the window rows themselves
-- hold none either. A row whose owner is deleted is left, as its windows are, and nothing reads
-- it.
--
-- A new table no earlier release names: nothing existing is read or rewritten, it takes no lock
-- on any existing table (its ACCESS EXCLUSIVE is on itself, for the moment it is created), the
-- release before this one never names it, so it rolls (cmd/dispatch/README.md, "Database
-- migrations"), and its census answers 0. Its identifier is the UTC moment it was written, as
-- `migrationid` requires of every migration past the numbered series.
create table embedding_window_states (
  kind text not null check (kind in ('document_block', 'comment_block', 'message_block', 'ask_part')),
  owner_key text not null,
  block_state text not null,
  written_at timestamptz not null default now(),
  primary key (kind, owner_key)
);
