-- 0125_comments_anchor_block_id_key.up.sql
--
-- comments_anchor_block_id_idx (0023) keys a comment anchor's block id itself, which a B-tree
-- entry cannot hold past 2704 bytes, so a comment anchored inside a block whose id a browser's
-- edit made that long fails its write. It is rebuilt under its name over 0124's block_id_key, as
-- 0124 rebuilds asks_anchor_block_id_idx, keeping its first column, which every read of it leads
-- with. Its own migration, so the transaction holds comments' lock alone (0075's precedent).
drop index comments_anchor_block_id_idx;
create index comments_anchor_block_id_idx
  on comments ((anchor->>'artifact_id'), block_id_key(anchor->>'block_id'))
  where anchor->>'block_id' is not null;
