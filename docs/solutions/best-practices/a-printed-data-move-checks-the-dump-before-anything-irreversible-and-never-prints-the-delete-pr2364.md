---
title: "A printed data move checks the dump before anything irreversible and never prints the delete; prove it by replaying the printed lines on real clusters"
category: best-practices
tags:
  - postgres
  - pg_dumpall
  - shell
  - printed-commands
  - testing
date: 2026-10-28
status: active
module: packages/envoy/scripts
---

# A printed data move checks the dump before anything irreversible and never prints the delete; prove it by replaying the printed lines on real clusters

Writing the sequence a person pastes:

- Never print the command that deletes the only other copy of the data. Keep the old container
  stopped and renamed, and say on the last line, as text, which command removes it once the new
  one works.
- Before the old server stops, check that `pg_dumpall` finished:
  `grep -c -x -- '-- PostgreSQL database cluster dump complete' <dump>` prints 1 only for a
  finished dump. A failed dump's redirect leaves an empty file, and `psql -f` restores an empty
  file without an error.
- Compare a listing of the databases and a `<tables>|<rows>` count of the data you care about,
  before the dump and after the restore. A table count alone passes a dump that stopped after its
  schema. Say what the count covers, and tell the reader to stop writers first.
- Start a stopped server and wait for it (`pg_isready -h 127.0.0.1`) before the dump, and name the
  errors the restore always prints (`role "postgres" already exists`, `current user cannot be
  dropped`).

Testing it:

- Replay each printed line through `bash -c`, in order, with a fake `docker` first on `PATH` that
  maps the container name to real `initdb`/`pg_ctl` clusters: the old one until the rename, the new
  one from `docker run` on.
- Inject each way the dump can fail (none, empty, cut at the first `COPY`) and assert that the
  checks printed after a failed move differ from what they printed before the dump.
- Find the replayed lines by their command prefix, not their query, so the test judges whatever
  query the script prints.
- Print SKIP where no server binaries exist, and FAIL under `GITHUB_ACTIONS=true`, whose ubuntu
  runners ship `/usr/lib/postgresql/*/bin`.

## Evidence

- `packages/envoy/scripts/dev-postgres.sh` prints the move of a stale `dispatch-pg` onto the
  Postgres 18 image: the listing and count at lines 88-89, the dump check at 98, no printed
  removal of the old container.
- The tester's red tests (commits 4cbf4534, an empty dump, and db0a45bd, a dump cut after its
  schema) failed heads e365a3b4 and 469388f8. The checks printed then (`dispatch postgres 1 180006`,
  later `dispatch postgres 1 1 180006`) were the same after a lost move as after a whole one.
- `packages/envoy/scripts/dev-postgres.test.sh` holds the real-cluster replay; the dump check came
  in 7ae2e23e, and the move stopped printing `docker rm -v` of the old container in bc8cf8ad.
