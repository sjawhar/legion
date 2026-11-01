-- 20261101030538_embedder_active.up.sql
-- The one embedder identity (embed.Embedder.Identity) the embedding queue of every server process
-- works for, so that two builds, or two processes, configured with different embedders do not
-- each drop the vectors the other makes (internal/dispatch/embedqueue, active.go).
--
-- The identity stays active while any live server_processes row (migration 0135, embedder_identity
-- from 20261101030537) carries it. When none does, the identity of the earliest-started live
-- process that carries one becomes active, by a compare-and-set on this row; with no such
-- process, the process that looks records its own, and with no row yet, the first process to look
-- records its own. A process whose identity is not active drops no vector, re-admits no dead row,
-- embeds no queue row and reserves no tokens. since is when the identity became active.
--
-- One row at most: one is the primary key and can only be true.
--
-- A new table: the release before this one neither reads nor writes it, which is what a rolling
-- update requires of a migration (cmd/dispatch/README.md, "Database migrations").
create table embedder_active (
  one boolean primary key default true check (one),
  identity text not null,
  since timestamptz not null default now()
);
