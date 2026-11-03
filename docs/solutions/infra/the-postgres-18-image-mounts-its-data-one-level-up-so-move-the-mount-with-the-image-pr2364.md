---
title: "The Postgres 18 image mounts its data one level up, so move the mount with the image"
category: infra
tags:
  - postgres
  - postgres-18
  - docker
  - pgvector
  - testcontainers
date: 2026-10-28
status: active
module: packages/envoy
---

# The Postgres 18 image mounts its data one level up, so move the mount with the image

- With a Postgres 18 image (`postgres:18`, `pgvector/pgvector:*-pg18`), put every tmpfs, volume
  or testcontainers `Tmpfs` at `/var/lib/postgresql`, never `/var/lib/postgresql/data`. Its
  entrypoint exits 1 when `/var/lib/postgresql/data` is a mount point, and the data directory is
  `/var/lib/postgresql/18/docker`.
- The mount path follows the image's major: a `postgres:16` site keeps `/data`. When you bump an
  image's digest or copy a `docker run` line, change the image and the mount in one edit.
- After each forward merge of the base, grep the tree for `/var/lib/postgresql/data` and for the
  old digest: the base keeps adding sites that copy the old line.
- Give a named volume a new name when the major changes (the old volume holds a cluster the new
  major cannot open), and say in the docs how to remove the old one.
- Remove a Postgres container with `docker rm -v`, so the image's anonymous `VOLUME` and its data
  go with it.

## Evidence

- sjawhar/legion#2364 moved every pin of `pgvector/pgvector` 0.8.7 from `-pg16` to `-pg18`. The
  devbox scripts (`scripts/e2e/*.sh`), `packages/pi-legion/scripts/skill-scenarios/rig.sh` and
  `packages/envoy/internal/testpg/testpg.go` mounted tmpfs at `/var/lib/postgresql/data`, and
  each moved to `/var/lib/postgresql`.
- A Kubernetes Job of the 18 image with its `emptyDir` at `/var/lib/postgresql` printed
  `data_directory=/var/lib/postgresql/18/docker`; the same Job with it at `/var/lib/postgresql/data`
  exited 1 with the entrypoint's refusal (recorded in the pull request body's implementer proof).
- The forward merge 95693430 brought two new `/data` mounts from the base on the 18 image
  (`testpg.go`'s testcontainers tmpfs and `scripts/e2e/stage4a-sandbox-runtime.sh`'s session
  database); only the post-merge grep found them.
- The acceptance compose file's volume became `dispatch-acceptance-pg18`
  (`packages/envoy/deploy/compose/dispatch.acceptance.compose.yml`), and `packages/dispatch/README.md`
  says how to remove the old `dispatch-acceptance-pg` volume.
