---
title: "The daemon's NATS tests run in a Docker-less pod against a local nats-server named by two variables"
category: testing
tags:
  - worker-pod
  - nats
  - testnats
  - LEGION_TEST_NATS_URL
  - postgres
  - go-test
date: 2026-10-25
status: active
module: packages/daemon
related_issues:
  - "INFRA-388"
  - "sjawhar/legion#2203"
---

# The daemon's NATS tests run in a Docker-less pod against a local nats-server named by two variables

Extends docs/solutions/testing/an-embedded-postgres-from-the-zonky-jar-runs-the-pods-postgres-lanes-and-the-nats-only-tests-are-skipped-by-name-LEGION-668.md: its NATS skip list is no longer needed in `packages/daemon`. Its Postgres recipe stands.

- Install and start NATS on the issue volume:
  `GOBIN=$TMPDIR/gobin go install github.com/nats-io/nats-server/v2@v2.10.22`, then
  `$TMPDIR/gobin/nats-server -js -a 127.0.0.1 -p <port> -m <monitor port> -sd $TMPDIR/natsdata`
  as a service.
- Export `LEGION_TEST_NATS_URL=nats://127.0.0.1:<port>` and
  `LEGION_TEST_NATS_MONITOR_URL=http://127.0.0.1:<monitor port>`, both or neither (one alone fails
  the test), then run `go test -p 1`. `testnats.URL` resets the server for each test: it waits for
  `/connz` to show no client, then deletes every stream. So two test binaries on one server fail
  each other's resets. Start a second server on other ports for a parallel run.
- The daemon's real-process rigs (`check_wake_test.go`'s boot of `run`) and the
  `intake`/`workflow` consume tests then run in the pod.
- Nine `internal/daemon` tests still fail in a gVisor pod because of the environment. Skip them
  by name and say so in the handoff:
  - Docker (`testnats.StartNkeyAuthorized*` or testcontainers): `TestDaemonPermissionsOnRealServers`,
    `TestChooseNATSConnectionNamesTheUserTheDaemonConnectsAs`, `TestNoLaunchCarriesTheDaemonSeed`,
    `TestTheWorkflowConnectsAsTheDaemonSeedOverThePaneSeed`,
    `TestTheWorkflowConnectsAsTheNatsSeedTheDaemonResolved`,
    `TestAMergedRootsCloseWaitsForTheRestOfItsTreeLive`
  - git's credential-helper list: `TestTmuxBootRewritesEveryOlderClonesCredentialHelper`
  - no `SigIgn` in `/proc`: `TestATerminalAttemptIgnoresSIGTSTP`,
    `TestATerminalAttemptLeavesLaterChildrenSIGTTOUsDefault`
- A faster Postgres than the zonky jar is the `pgserver` wheel (Postgres 16.2), which needs
  Python 3.12:
  `UV_CACHE_DIR=$TMPDIR/uvcache UV_PYTHON_INSTALL_DIR=$TMPDIR/uvpy uv venv -p 3.12 $TMPDIR/pgvenv`,
  then `VIRTUAL_ENV=$TMPDIR/pgvenv uv pip install pgserver`. Its `pginstall/bin/initdb -E UTF8 --auth=trust`
  and `pg_ctl` run as the zonky ones do. Raise `max_connections`, because the rigs open many pools.
- A persistent shell keeps what you `export`. An earlier `export LEGION_GH_PATH=...` for a
  `--check-config` run breaks the pod's `gh` wrapper for every later command in that shell.

## Evidence

- `packages/daemon/internal/testnats/nats.go` (`external()`, from #2203), `README.md`'s
  Development paragraph.
- INFRA-388's implementer ran `go test -p 1 ./internal/daemon -skip '<the nine above>'` (ok,
  516 s), `./internal/intake` and `./internal/workflow` (ok, unfiltered) in the pod, against NATS
  2.10.22 and pgserver Postgres 16.2.
