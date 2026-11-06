---
title: "A pod's throwaway Postgres is alive by a connect to its socket, and only the role that started it stops it"
category: testing
tags:
  - issue-pod
  - postgres
  - pid-namespace
  - unix-socket
  - harness
date: 2026-10-29
status: active
module: scripts/dispatch
---

# A pod's throwaway Postgres is alive by a connect to its socket, and only the role that started it stops it

Extends docs/solutions/testing/a-pods-shared-postgres-is-alive-by-its-port-never-by-its-pid-file-INFRA-95.md
and docs/solutions/testing/the-shared-harness-script-starts-postgres-only-when-its-port-is-silent-and-no-spec-changes-while-a-run-loads-INFRA-304.md.
Those two cover a server on a TCP port. This one covers a server that listens only on a unix
socket under the shared `$TMPDIR`.

- **Every role of an issue pod shares `$TMPDIR`, and each role has its own PID and IPC namespace.**
  A server one role starts is visible to the others through files only. Its `postmaster.pid`
  names a process in the starting role alone.
- **Decide liveness with a connect to the socket, written with a tool the block already needs.**
  `python3 -c 'import socket, sys; s = socket.socket(socket.AF_UNIX); s.settimeout(5); s.connect(sys.argv[1])' "$pg/.s.PGSQL.5432"`
  succeeds only when a server listens. A socket file left by a killed server refuses the connect.
  Never use `pg_ctl status`. Never use a PostgreSQL client such as `pg_isready` or `psql` unless
  the block installs it: on a `PATH` without it, "command not found" reads as "no server".
- **A socket-only server has no port to protect it.** Postgres's own data-directory check reads
  the PID in `postmaster.pid` and a SysV shared-memory id. Neither resolves in another role's
  namespaces, so a second role's `pg_ctl start` starts a second postmaster on the same data
  directory.
- **When no server answers but `postmaster.pid` is left, refuse and start nothing.** From another
  role, a live server's file and a dead server's file look the same. Tell the reader to stop the
  server from the shell that started it, or to remove the directory.
- **Run `pg_ctl stop` only in the role that started the server.** `pg_ctl` sends its signal to the
  PID in `postmaster.pid`, and in another role that PID is a different process or none.
- **Prove a pod-shared setup block against a simulated sibling role.** Run it under
  `unshare --user --map-current-user --pid --ipc --fork --mount-proc bash -c "source <block>"`
  while the first role's server runs, and under `env -i PATH=<dir>`, where the directory holds only
  the tools the block names. Link each tool by `type -P <tool>`, because a harness shell can give
  `mkdir`, `head`, `ps` and `tr` as builtins. A proof run in one PID namespace cannot show either
  failure below.

## Evidence

sjawhar/legion#2476, `scripts/dispatch/README.md`, section "Lexeme database".

- At 75e39fdb the block decided liveness with `pg_ctl -D data status`. A sibling run under the
  `unshare` command above printed `pg_ctl: another server might be running; trying to start server
  anyway` and `server started`. `pg.log` then said `database system was interrupted … automatic
  recovery in progress`. The first role's postmaster was still running, but its socket refused
  connections. `postmaster.pid` then held PID 9, which in the first role's namespace is that
  role's `worker-shim`. That role's README stop command would have signalled it.
- At 59377f8f the block used `pg_isready`. With a `PATH` that did not hold it, a second run while
  the server ran printed `pg_isready: command not found`, then the stale-file refusal, and
  exited 1.
- At ba09bc0c, with the `python3` connect and a `PATH` holding no PostgreSQL client:
  - The sibling run printed only the socket path.
  - The sibling's `pg_ctl stop` printed `could not send stop signal (PID: 6507): No such process`.
  - The first role's server kept its `pg_postmaster_start_time()`.
  - Two first runs at once from two roles gave one server.
  - A stale `postmaster.pid` made both roles exit 1 and start nothing.
