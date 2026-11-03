-- 20261102233625_block_embedding_windows.up.sql
-- Meaning search moves from one vector per whole item to one vector per window of neighbouring
-- blocks (INFRA-370). A whole-text row carries only the first embed.MaxInputRunes runes of its
-- item, so a passage at the end of a long document has no vector and no query reaches it; a
-- window holds at most embed.windowTextChars characters of block text, so every passage of
-- every item is embedded. embed.Windows builds an owner's windows and each indexer writes them
-- through embeddings_enqueue_window below, in the transaction that writes the owner's block
-- rows.
--
-- This migration takes embeddings' ACCESS EXCLUSIVE lock alone, for its short transaction, and
-- never while waiting for another table's: it widens one check, adds one column with no
-- default, creates one index and one function, and rewrites no row.
--
-- The kinds the windows are written under, beside the five the release before this one writes
-- and reads: document_block, comment_block and message_block for a window of a document's, a
-- comment's or a message's blocks, and ask_part for a window of an ask's question, options and
-- answers. Both builds' kinds are admitted at once, because the release before this one keeps
-- writing the whole-text rows while it serves, and a later release deletes them once each
-- owner holds a window whose vector is current.
--
-- The new list admits every kind the old one did, so no row stored under it can fail the new
-- check. It is added `not valid` all the same, the one shape this set adds a constraint in: a
-- validated add reads every row of the table while it holds the ACCESS EXCLUSIVE lock, where
-- `not valid` changes the catalog alone and still refuses every row written or updated after
-- it. Nothing validates it later; there is nothing to validate.
alter table embeddings drop constraint embeddings_kind_check;
alter table embeddings add constraint embeddings_kind_check check (
  kind in ('issue', 'document', 'comment', 'ask', 'message',
           'document_block', 'comment_block', 'message_block', 'ask_part')
) not valid;

-- The block ids this row's vector covers, in the order the window holds them (embed.Window's
-- Covers). Null on a whole-text row, which covers no block. A meaning hit on a window names one
-- of these blocks, the one whose own text ranks highest for the query, so a reader is shown the
-- passage that matched rather than the whole item.
alter table embeddings add column covers text[];

-- A window's id is <owner key>#<the first block it covers>, so the owner key is the id up to the
-- first #. Each indexer, having written its owner's windows, deletes the owner's window rows
-- the new set does not name, with this statement:
--
--   delete from embeddings
--   where kind = $1 and split_part(id, '#', 1) = $2 and id <> all($3)
--
-- which is what this index serves: its key is what that statement filters on, and split_part is
-- immutable, so an expression index can hold it. It is partial over the window kinds alone,
-- because a whole-text row's id holds no # and no such delete names it.
create index embeddings_window_owner on embeddings (kind, split_part(id, '#', 1))
  where kind in ('document_block', 'comment_block', 'message_block', 'ask_part');

-- embeddings_enqueue_window is embeddings_enqueue (0151) with the covers column written beside
-- the text: the same hash of the text alone, so rewriting an owner's windows when none of them
-- changed costs one no-op upsert each and leaves every row's vector, retry state and wait as
-- they stand, and the same announcement on dispatch_embeddings, so the queue
-- (internal/dispatch/embedqueue) wakes for a window exactly as it wakes for a whole-text row.
-- A window whose text is unchanged therefore keeps the covers it was written with, as it keeps
-- its text: the covers of one text are the blocks that text came from.
--
-- The window kinds go through this function and the whole-text kinds through embeddings_enqueue,
-- so neither release's writes pass through the other's path.
create function embeddings_enqueue_window(p_kind text, p_id text, p_text text, p_covers text[]) returns void
language plpgsql as $$
declare
  v_hash text := encode(sha256(convert_to(p_text, 'UTF8')), 'hex');
begin
  insert into embeddings (kind, id, text_snapshot, content_hash, covers)
  values (p_kind, p_id, p_text, v_hash, p_covers)
  on conflict (kind, id) do update
    set text_snapshot = excluded.text_snapshot,
        content_hash = excluded.content_hash,
        covers = excluded.covers,
        attempt_count = 0,
        confirmed_failures = 0,
        next_attempt_at = now(),
        dead = false,
        dead_model = null,
        pending_since = now()
    where embeddings.content_hash is distinct from excluded.content_hash;
  if found then
    perform pg_notify('dispatch_embeddings', '');
  end if;
end;
$$;
