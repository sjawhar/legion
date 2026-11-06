---
title: "A throwaway Postgres for a lexeme check runs production's major, because 16 and 18 stem English differently"
category: testing
tags:
  - postgres
  - full-text-search
  - bun-sql
  - unix-socket
  - legion-pod
date: 2026-10-29
status: active
module: scripts/dispatch
supersedes: docs/solutions/testing/bun-sql-reaches-a-socket-only-throwaway-postgres-by-the-socket-file-path-pr2200.md
---

# A throwaway Postgres for a lexeme check runs production's major, because 16 and 18 stem English differently

The superseded document starts PostgreSQL 16.2 from `pgserver==0.1.4`. Production runs 18.6,
whose English stemmer gives other lexemes. Its rule for connecting Bun's `SQL` stays and is
repeated below.

- **Run a check that computes lexemes on the major production runs.** `to_tsvector('english', …)`
  and `websearch_to_tsquery('english', …)` give different lexemes on 16 and 18. Start a throwaway
  server with the block in `scripts/dispatch/README.md`, section "Lexeme database": zonky's
  PostgreSQL 18.6.0 binaries, listening only on a unix socket in `$TMPDIR/pg`.
- **Connect Bun's `SQL` with the socket file as `path`, and give `username` and `database`
  explicitly:** `new SQL({ path: "<dir>/.s.PGSQL.5432", username: "postgres", database: "postgres" })`.
  Bun refuses the `postgres://…?host=<dir>` URL form.
- **When you change a text's claim about which version runs, run the command under it in the same
  change.** Read `server_version_num` and one output that the two versions give differently. A
  claim edited without its command can contradict the command, and a version string alone does
  not show which output the check computes.

## Evidence

sjawhar/legion#2476.

- Round 1 changed `scripts/dispatch/README.md` to "Any Postgres 18 works, as production and CI run
  18" and left the `pgserver==0.1.4` command under it.
- The query `select current_setting('server_version_num'), to_tsvector('english','added adding settings'), websearch_to_tsquery('english','added')`
  gives `160002 | 'ad':1,2 'set':3 | 'ad'` on that server. It gives
  `180006|'add':1,2 'set':3|'add'` on the README's 18.6 block.
- The tester compared `ts_lexize('english_stem', w)` over 18,957 distinct words of `docs/` and
  `skills/` markdown. It differs between 16.2 and 18.6 for 3 of them: `added`, `adding` and
  `ettings`.
- `postgresLexer` in `scripts/dispatch/meaning-search-check.ts` connects with the `path` form.
