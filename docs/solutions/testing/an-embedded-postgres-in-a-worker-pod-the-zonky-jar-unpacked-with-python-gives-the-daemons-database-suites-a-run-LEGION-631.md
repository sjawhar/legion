---
title: "An embedded Postgres in a worker pod: the zonky jar unpacked with Python gives the daemon's database suites a run"
category: testing
tags:
  - postgres
  - LEGION_TEST_PG_DSN
  - worker-pod
  - gvisor
  - go-test
date: 2026-10-10
status: active
module: packages/daemon
related_issues:
  - "LEGION-631"
  - "sjawhar/legion#1843"
---
# An embedded Postgres in a worker pod: the zonky jar unpacked with Python gives the daemon's database suites a run

- The worker image carries no Postgres, `unzip` or `xz`, and a pod has no Docker, so the daemon's
  database-backed suites (`internal/admit`, `internal/store`, the outbox and sandbox tests of
  `internal/daemon`) `t.Fatal` on `LEGION_TEST_PG_DSN is required` or skip. A static Postgres is
  one download away: the zonky embedded-postgres binaries jar from Maven Central, unpacked with
  Python's `zipfile` and `lzma`+`tarfile`, run with its own `lib/` on `LD_LIBRARY_PATH`:

  ```sh
  V=16.4.0; mkdir -p /tmp/pg && cd /tmp/pg
  curl -sSfL -o pg.jar "https://repo1.maven.org/maven2/io/zonky/test/postgres/embedded-postgres-binaries-linux-amd64/$V/embedded-postgres-binaries-linux-amd64-$V.jar"
  python3 -c "import zipfile; zipfile.ZipFile('pg.jar').extractall('.')"
  python3 -c "import lzma,tarfile; t=tarfile.open(fileobj=lzma.open('postgres-linux-x86_64.txz'),mode='r|'); t.extractall('.')"
  export LD_LIBRARY_PATH=/tmp/pg/lib
  ./bin/initdb -D data -U postgres --auth=trust -E UTF8
  ./bin/pg_ctl -D data -o "-p 54329 -k /tmp/pg -c listen_addresses=127.0.0.1" -l pg.log start
  export LEGION_TEST_PG_DSN='postgres://postgres@127.0.0.1:54329/postgres?sslmode=disable'
  ```

- Run it before a forward merge's full `go test ./...`: the suites it unlocks are the ones that
  exercise a merged runtime end to end (a Sandbox closed `done` releasing its volume, the orphan
  sweep, the outbox), where a resolution's runtime refusal shows and a compile does not.
- What it does not give: the pgvector extension (the scratch-Dispatch outbox cases), rootless
  Docker (testcontainers, NATS) and cgo (the supervise table tests type-check `net`); skip those by
  name and say so in the proof. The pod's `/tmp` does not survive a relaunch — the setup is
  rerun, not found.

## Evidence

sjawhar/legion#1843, round 9: without it, 70 tests across `internal/admit`, `internal/daemon` and
`internal/supervise` failed or panicked in this gVisor pod; with Postgres 16.4 on 127.0.0.1:54329
every package was green but the Docker-, cgo- and scratch-Dispatch-bound cases, and the run surfaced
the one defect of the merge, main's `TestAChildClosedDoneReleasesItsVolumeWhileItsParentRuns`
refused by `sandbox.New` for its missing `GitHubCredential`. Round 7's proof had used the same
Postgres on the same port without recording the recipe; the pod had been relaunched since and the
binary was gone.
