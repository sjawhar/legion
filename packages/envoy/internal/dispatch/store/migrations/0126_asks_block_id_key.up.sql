-- 0126_asks_block_id_key.up.sql
--
-- A block id is a string a browser's edit can make any length, and a B-tree index entry holds at
-- most 2704 bytes, so asks_block_id_unique (0018) and asks_anchor_block_id_idx (0023), which key
-- the id itself, fail every write of an incompressible id past that: a settlement opening the ask
-- of an ask block, an ask anchored inside a block. Both are rebuilt under their names over
-- block_id_key, the id's SHA-256, 32 bytes at any length. asks_block_id_unique keeps
-- (block_artifact_id, block_id) unique, since two ids under one key would be a SHA-256 collision,
-- and an id stored under the old index, a long one it held compressed included, is held by the
-- new one. Each index keeps its first column, which every read of it leads with; a read of one
-- block id matches block_id_key(block_id) too (docs/copied_asks.go). Its own migration, so the
-- transaction holds asks' lock alone (0075's precedent); 0127 rebuilds comments' anchor index.
--
-- An index expression must be immutable. block_id_key reads the id's UTF-8 bytes as decode's
-- escape format reads text, each other character as its own bytes and a doubled backslash as one,
-- which is immutable; convert_to, which gives the same bytes, is only stable. The backslashes are
-- E'' literals, which read alike whatever standard_conforming_strings says when the body runs.
create function block_id_key(id text) returns bytea
  language sql immutable strict parallel safe
  as $$ select sha256(decode(replace(id, E'\\', E'\\\\'), 'escape')) $$;

drop index asks_block_id_unique;
create unique index asks_block_id_unique
  on asks (block_artifact_id, block_id_key(block_id))
  where block_id is not null;

drop index asks_anchor_block_id_idx;
create index asks_anchor_block_id_idx
  on asks ((anchor->>'artifact_id'), block_id_key(anchor->>'block_id'))
  where anchor->>'block_id' is not null;
