---
title: "The Postgres 18 entrypoint refuses only a fresh data directory under a mounted data path"
category: infra
tags:
  - postgres
  - postgres-18
  - docker
  - negative-control
date: 2026-10-29
status: active
module: scripts/e2e
supersedes: docs/solutions/infra/the-postgres-18-image-mounts-its-data-one-level-up-so-move-the-mount-with-the-image-pr2364.md
---

# The Postgres 18 entrypoint refuses only a fresh data directory under a mounted data path

This note replaces the superseded one: it carries every rule of that note, so a reader who reaches
this file in its place loses none, and corrects one sentence of it. That note says the entrypoint
"exits 1 when `/var/lib/postgresql/data` is a mount point", which is not the whole condition, and a
negative control written from it can pass while proving nothing.

- With a Postgres 18 image (`postgres:18`, `pgvector/pgvector:*-pg18`), put every tmpfs, volume
  or testcontainers `Tmpfs` at `/var/lib/postgresql`, never `/var/lib/postgresql/data`. The data
  directory is `/var/lib/postgresql/18/docker`.
- The mount path follows the image's major: a `postgres:16` site keeps `/data`. When you bump an
  image's digest or copy a `docker run` line, change the image and the mount in one edit.
- After each forward merge of the base, grep the tree for `/var/lib/postgresql/data` and for the
  old digest: the base keeps adding sites that copy the old line.
- Give a named volume a new name when the major changes (the old volume holds a cluster the new
  major cannot open), and say in the docs how to remove the old one.
- Remove a Postgres container with `docker rm -v`, so the image's anonymous `VOLUME` and its data
  go with it.
- The refusal needs three things together: `PGDATA` at the image default, a data directory that is
  **fresh** (it holds no `PG_VERSION`), and `/var/lib/postgresql/data` being a mount point. A
  restart of a container whose data directory is already initialised never reaches the refusal,
  and a run with no mount at all never reaches it either, because the image's one anonymous
  `VOLUME` is `/var/lib/postgresql`.
- So a negative control for this refusal starts on a **fresh** volume, and a positive statement
  that a site is safe needs the site's own mount shape: a `docker run` with no `--mount` is safe
  on 18 without any edit.
- Match the refusal by a fragment, never by its sentence. The entrypoint prints a heredoc that
  wraps mid-sentence: match `/var/lib/postgresql/data (unused mount/volume)` verbatim, or the line
  `Error: in 18+, these Docker images are configured to store database data in a`.

## Evidence

- The five rules carried from the superseded note rest on its evidence: sjawhar/legion#2364
  moved every `pgvector/pgvector` 0.8.7 pin from `-pg16` to `-pg18`, moved each tmpfs it found to
  `/var/lib/postgresql`, found two more old-path mounts only by the grep after a forward merge,
  and renamed the acceptance compose volume to `dispatch-acceptance-pg18`.
- The image's own `docker-entrypoint.sh` (docker-library/postgres, `18/trixie`, the directory this
  image's `org.opencontainers.image.source` names; the `18/bookworm` copy of the same revision is
  byte-identical): `_main` runs the check only while `DATABASE_ALREADY_EXISTS` is empty, which
  `docker_setup_env` sets from `[ -s "$PGDATA/PG_VERSION" ]`; the
  `/var/lib/postgresql/data (unused mount/volume)` entry is added only when `PGDATA` equals
  `/var/lib/postgresql/$PG_MAJOR/docker`, no older data directory was found, `PG_MAJOR` is at
  least 18, and the path answers its mount-point test.
- sjawhar/legion#2498 left the `docker run` line printed by the daemon's
  four test skip messages mountless, from the image config's
  `Volumes {"/var/lib/postgresql":{}}`, and that line served `select 1` with
  `data_directory /var/lib/postgresql/18/docker` and no volume in its pod at all.
- The negative control of the same pull request, a Job of the pinned 18 image with a fresh
  memory-backed `emptyDir` at `/var/lib/postgresql/data`, exited 1 and printed
  `Counter to that, there appears to be PostgreSQL data in:` followed, on its own line, by
  `  /var/lib/postgresql/data (unused mount/volume)`. The tester reproduced it from a driver of
  its own.
- The same pull request's `postgres:16` control started at `/var/lib/postgresql/data` with no
  refusal, so the refusal is this image paired with that mount and not the path alone.
