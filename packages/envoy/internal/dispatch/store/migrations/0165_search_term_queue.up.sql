-- 0165_search_term_queue.up.sql
-- The items whose search text has changed since each project's word counts were last written,
-- which the related-terms refresh applies instead of counting every item of every project again
-- (api/related_terms.go). In the 24 hours to 2026-10-30 that refresh used 1,882 s of database
-- time, 20% of Dispatch's total, because ts_stat read every issue, document, comment, ask and
-- message of every project every 10 minutes while a handful of them had changed.
--
-- 0167's triggers write one row here for each insert, each delete, and each update of the columns
-- an item's search vector is built from (0057-0061). A refresh pass reads the rows its own
-- snapshot sees, applies them to search_term_stats and search_term_totals, and deletes exactly
-- those.
--
-- No foreign key, to any table. A `references` makes the pre-deploy census size the table it
-- points at (internal/pgmigrate/touched.go), and a key to an item would also refuse the row a
-- delete queues, which is the row that takes a deleted item's words back out of the counts.
create table search_term_queue (
  -- The order the changes were written in, which is the order a pass must read them in: of two
  -- rows for one item, the older one's old_search is the vector the stored counts hold. A writer
  -- holds the item's row lock while it writes, so two changes of one item are written, and become
  -- visible, in this order, whichever transactions wrote them.
  id bigserial primary key,
  -- The transaction that wrote the item, so a pass can tell a change its own snapshot sees from
  -- one committed after it: the pass applies and deletes exactly the rows
  -- pg_visible_in_snapshot(xid, <the snapshot it writes>) is true of, and leaves the rest for the
  -- next pass, so no change is applied twice and none is lost.
  xid xid8 not null default pg_current_xact_id(),
  -- Which kind of item changed, named as api/related_terms.go's searchTermItemsSQL scopes each
  -- kind.
  kind text not null check (kind in ('issue', 'document', 'comment', 'ask', 'message')),
  -- The item, and the owner whose project it takes. The item is
  -- coalesce(item_id::text, issue_key, artifact_id::text): a comment, an ask and a message are
  -- their own id, an issue is its key, a document is its artifact. The owner is the issue or the
  -- artifact whose project the item takes, exactly as searchTermItemsSQL's joins read it, copied
  -- from the row the trigger already has and never read, so a pass still finds the project of an
  -- item that has been deleted. An ask can gain its block's artifact between two of its own
  -- changes, so the owner columns are no part of the item: a pass that grouped its rows by them
  -- would read those two rows as two items and count the ask twice.
  issue_key text,
  artifact_id uuid,
  item_id uuid,
  -- The vector the item held before this change: null for an insert, which held none, and the
  -- empty vector where the item's own search column was null. A pass subtracts it and adds what
  -- the item holds now, so a delete, whose item is gone by the time a pass reads this row, still
  -- takes its words out of the counts.
  old_search tsvector
);
