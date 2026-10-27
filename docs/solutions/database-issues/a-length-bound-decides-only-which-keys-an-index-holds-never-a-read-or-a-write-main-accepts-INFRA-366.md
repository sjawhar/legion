---
title: "A length bound decides only which keys an index holds: it never fails a read or refuses a write main accepts, and it is tested with incompressible data on a database main wrote"
category: database-issues
tags:
  - postgres
  - btree
  - index-row-size
  - length-bound
  - hash-index
  - dispatch
date: 2026-10-26
status: active
module: packages/envoy/internal/dispatch
related_issues:
  - "INFRA-366"
  - "sjawhar/legion#2210"
---

# A length bound decides only which keys an index holds: it never fails a read or refuses a write main accepts, and it is tested with incompressible data on a database main wrote

- A Postgres B-tree entry holds at most 2704 bytes. Any primary key, unique or plain B-tree index
  over text a user chooses fails the insert past that (SQLSTATE 54000, `index row size … exceeds`).
  Before you index such a column, decide what holds it:
  - Every read is an equality: use `using hash`. Its entry holds a 4-byte hash.
  - It needs uniqueness: use a unique index over an `immutable` digest function, and make every
    read use that same expression, or the index is not used. `convert_to` is only `stable`, so it
    cannot be in an index expression. `sha256(decode(replace(id, E'\\', E'\\\\'), 'escape'))`
    gives the same UTF-8 bytes and is immutable.
- When you need a length bound anyway, put it only on the path that builds the derived index.
  Skip the row that is too long and log how many you skipped. Never put it on:
  - a request's route or argument parse: a read of a long name answers as it did on main;
  - a write main accepts: it still stores.

  The only refusal you may add is for a value that is new, cannot be stored, and that main also
  fails to store.
- Test every byte bound with random, incompressible data, never `strings.Repeat("a", n)`.
  Postgres compresses a long repeated value into the index entry, so it passes where real text
  fails, and main has already stored such values. Your bound must keep accepting them.
- Prove a bound on a database main's build wrote: write the long, odd and compressible rows with
  main, migrate with the head, then read and edit them with the head. A migration test seeds the
  same rows before it runs the new migration.
- Rebuild one table's index per migration, so each transaction holds one table's lock.

## Evidence

INFRA-366 (sjawhar/legion#2210) made each block of a Dispatch document a graph node keyed
`<ref_key>#<block id>`.

- **The first head:** it put that node id under a B-tree index, and a long reference key overflowed
  the index. 384ff72f made `document_blocks.node_id` and `block_refs.from_node` hash indexes
  (`store/migrations/0121_block_graph.up.sql`, the comment above `document_blocks`).
  28bb6e9a keyed ask block rows and anchors by `block_id_key()`
  (`store/migrations/0127_asks_block_id_key.up.sql`). The read was changed to match, in
  `docs/copied_asks.go`.
- **Review rounds 1-2:** the next fix bounded block ids at 256 bytes and targets at 2048 bytes
  everywhere. That made reads of long names fail, and refused document writes main accepted.
  - 53505a76 took the write refusal out: `refs/blocks.go` `uniqueBlocks` skips a long id and
    counts it.
  - 98206c03 kept the target bound only in `text.Extract` (`text/refs.go`). `text.ParseRef`, used
    by requests, parses unbounded.
- **The one remaining refusal:** a new artifact name or key over 2692 bytes, which is 2704 minus
  the 8-byte tuple header and the 4-byte length word (`api/artifacts.go`,
  `maxNewArtifactKeyBytes`). An artifact main already stored past it, under a key Postgres
  compressed, keeps working. `store/block_id_key_migrations_test.go` and
  `api/artifact_name_bound_test.go` use random bytes for exactly this reason.
