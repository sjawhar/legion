-- 20261101222725_workspace_bundles.up.sql — the unpushed work of a released issue's workspace,
-- one row per issue, held between the release that saved it and the restore of the pod that
-- replaces it. The bundle is a git bundle the issue's own shim built in its pod, carrying every
-- visible commit no remote branch holds and the working copy's own change, with the commits a new
-- pod's workspace fetch already holds as its bases; it never reaches GitHub.
--
-- issue is the key, so a second release of the same issue replaces the row rather than growing the
-- table. bundle is the git bundle's bytes and size_bytes their length, which the release records
-- and the 40 MiB bound reads; at is when the shim took it. head is the commit id the working copy
-- sat on, tips the commit ids of the heads the bundle carries, and bookmarks the local bookmark
-- names that pointed into the saved set, each with its commit: a bundle that only unbundles leaves
-- every saved head out of jj's view, since jj's visible heads live in .jj/repo, which died with the
-- pod, so the restore needs all three.
create table workspace_bundles (
    issue      text primary key,
    bundle     bytea       not null,
    size_bytes bigint      not null,
    at         timestamptz not null,
    head       text        not null,
    tips       jsonb       not null default '[]'::jsonb,
    bookmarks  jsonb       not null default '{}'::jsonb
);
