---
title: "Bun SQL reaches a socket-only throwaway Postgres by the socket file path"
category: testing
tags:
  - postgres
  - bun-sql
  - pgserver
  - unix-socket
  - legion-pod
date: 2026-10-28
status: active
module: scripts/dispatch
---

# Bun SQL reaches a socket-only throwaway Postgres by the socket file path

- A pod has no Postgres server. Start a throwaway one under `$TMPDIR`:
  `uvx --python 3.12 --from pgserver==0.1.4 python -c 'import pgserver; print(pgserver.get_server("<dir>", cleanup_mode=None).get_uri())'`.
  It gives Postgres 16.2 that listens only on a unix socket in `<dir>`.
- Connect Bun's `SQL` with the socket file as `path`, and give `username` and `database`
  explicitly: `new SQL({ path: "<dir>/.s.PGSQL.5432", username: "postgres", database: "postgres" })`.
  Do not use the `postgres://…?host=<dir>` URL that pgserver prints.
- Stop the server by the exact PID that `pgrep -af 'postgres -D <dir>'` shows when the work is
  done. The data directory can be reused by the next `get_server` call.

## Evidence

- `postgresLexer` in `scripts/dispatch/meaning-search-check.ts` (line 608 at 7e042950) uses the
  `path` form. Its comment records that Bun refuses the `?host=` URL; the pull request's plan
  measured that refusal as "Connection closed".
- Each production proof of the meaning search check (bc359229 to 7e042950) measured its lexemes
  through this connection, against a throwaway Postgres 16.2 started as above.
