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
related_issues:
  - "sjawhar/legion#2498"
---

# The Postgres 18 entrypoint refuses only a fresh data directory under a mounted data path

Every rule of the superseded note still holds; this note corrects one sentence of it. That note
says the entrypoint "exits 1 when `/var/lib/postgresql/data` is a mount point", which is not the
whole condition, and a negative control written from it can pass while proving nothing.

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

- The image's own `docker-entrypoint.sh` (docker-library/postgres, `18/bookworm`): `_main` runs
  the check only while `DATABASE_ALREADY_EXISTS` is empty, which `docker_setup_env` sets from
  `[ -s "$PGDATA/PG_VERSION" ]`; the `/var/lib/postgresql/data (unused mount/volume)` entry is
  added only when `PGDATA` equals `/var/lib/postgresql/$PG_MAJOR/docker`, no older data directory
  was found, `PG_MAJOR` is at least 18, and the path answers its mount-point test.
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
