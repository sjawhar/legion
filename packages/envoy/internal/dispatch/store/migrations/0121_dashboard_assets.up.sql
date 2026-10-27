-- INFRA-559: every server process keeps its build's dashboard assets in the database, so a page of
-- one build is served its chunks by a process running another. A rolling update and a rollback run
-- two builds behind one load balancer at once, and each image carries its own build's web dist
-- alone: a tab holding build A's page asks for A's content-hashed chunks, and whichever process
-- answers must have them. Every process publishes its dist's assets at boot, before it listens
-- (internal/dispatch/dashboard.Publish), and answers a local miss under /assets/ from here
-- (routes.AssetStore).
--
-- dashboard_builds is one row per build that has served: its build id (the hex of the first 16
-- bytes of sha256 of its index.html, the bytes the page's ETag hashes), the entry chunk its page
-- names (`/assets/index-*.js`, which a tab compares to tell builds apart), the commit its image was
-- stamped with ('' for an unstamped build), and when a process first and last started on it, on
-- the database clock. The boot's prune keeps the builds a live process runs and the 20 started most
-- recently (dashboard.Prune), so last_started_at is what a restart renews.
--
-- dashboard_assets holds each file once by its path below the dist (`assets/<relative path>`, the
-- key the static handler asks for), with its size and sha256: Vite writes a changed file under a
-- new name, so two builds that hold one path hold one file, and a path's bytes are written once
-- and never updated. A path stored with another hash is a collision the publish logs and leaves.
--
-- dashboard_build_assets links each build to every file of its dist. A build row is written in the
-- transaction that writes its links, so a build row present is a build complete, and a restart on
-- it writes no bytes. The cascade drops a pruned build's links; the prune then deletes every asset
-- no build links, and the asset's foreign key refuses to delete one a build still holds.
--
-- Three new tables: nothing existing is read, locked or rewritten, and the release before this one
-- never reads or writes them, so it serves beside this one unchanged while both run, and a rollback
-- is an image rollback alone (cmd/dispatch/README.md, "Database migrations").
create table dashboard_builds (
    build text primary key,
    entry text not null,
    commit text not null default '',
    first_started_at timestamptz not null default now(),
    last_started_at timestamptz not null default now()
);

create table dashboard_assets (
    path text primary key,
    content bytea not null,
    size integer not null,
    sha256 bytea not null,
    created_at timestamptz not null default now()
);

create table dashboard_build_assets (
    build text not null references dashboard_builds on delete cascade,
    path text not null references dashboard_assets,
    primary key (build, path)
);

-- The prune's sweep of unlinked assets finds a path's links by path; the primary key leads with
-- the build.
create index dashboard_build_assets_path on dashboard_build_assets (path);
