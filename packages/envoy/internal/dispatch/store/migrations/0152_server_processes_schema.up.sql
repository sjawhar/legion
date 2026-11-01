-- 0152_server_processes_schema.up.sql
-- INFRA-558: what each registered server process (migration 0135, internal/dispatch/processes)
-- reads and writes, so the processes that serve one database can decide together what a client
-- may do while two builds serve it.
--
-- schema_version is the document manifest version the process reads (pmdoc.SchemaVersion, V) and
-- schema_creatable the highest `since` among the elements it may create (pmdoc.SchemaCreatable,
-- R). A document socket is writable only when the page reads what every live process creates and
-- every live process reads what the page creates (docs.Service.authorize). embedder_identity is
-- the identity of the process's embedder (embed.Embedder.Identity), '' when it has none.
-- file_store is whether the process stores uploaded files in a bucket.
--
-- A row at the defaults (0, 0, '', null) is a process of a build before this one, which writes
-- none of the four: the admission leaves it out of the fleet's numbers, as it leaves out a build
-- that does not register at all, and a reader of the other two treats it as carrying no identity
-- and as lacking the bucket.
--
-- Four columns with constant defaults: Postgres records each default in the catalog and rewrites
-- no row, so the ALTER takes ACCESS EXCLUSIVE on server_processes only for the catalog write. The
-- release before this one names its columns in every insert and select it makes on the table, so
-- it neither reads nor writes these, which is what a rolling update requires of a migration
-- (cmd/dispatch/README.md, "Database migrations").
alter table server_processes
  add column schema_version integer not null default 0,
  add column schema_creatable integer not null default 0,
  add column embedder_identity text not null default '',
  add column file_store boolean;
