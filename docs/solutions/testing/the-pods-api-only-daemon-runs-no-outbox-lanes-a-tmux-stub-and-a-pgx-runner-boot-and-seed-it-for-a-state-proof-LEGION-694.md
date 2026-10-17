---
title: "A pod's real-binary state proof: the zonky jar has no psql, the daemon needs a tmux stub to boot, Stage 1's script refuses the pod's port range, and an API-only daemon runs no outbox lanes, so a seeded row proves the projection and nothing else"
category: testing
tags:
  - worker-pod
  - embedded-postgres
  - legion-state
  - real-binary-proof
  - tmux
  - outbox
  - proof-environment
date: 2026-10-16
status: active
module: packages/daemon
related_issues:
  - "LEGION-694"
  - "sjawhar/legion#1999"
---

# A pod's real-binary state proof: the zonky jar has no psql, the daemon needs a tmux stub to boot, Stage 1's script refuses the pod's port range, and an API-only daemon runs no outbox lanes, so a seeded row proves the projection and nothing else

Extends docs/solutions/testing/an-embedded-postgres-from-the-zonky-jar-runs-the-pods-postgres-lanes-and-the-nats-only-tests-are-skipped-by-name-LEGION-668.md.

- The zonky jar unpacks `initdb`, `pg_ctl` and `postgres` only: no `psql`, no `pg_isready`. Seed and
  read the daemon's tables with a throwaway Go program in `/tmp` whose own `go.mod` requires the
  daemon's `github.com/jackc/pgx/v5` version (`GOFLAGS=-mod=mod go mod tidy` fetches the sums; the
  modules are already in the pod's cache), taking each SQL statement as an argument and printing
  rows tab-separated. Delete the rows it seeds when the proof ends: the daemon's own test suites
  use the same database.
- `legion start` under `runtime: tmux` refuses to boot without `tmux` on its PATH
  (`tmux runtime: tmux is not on the daemon's PATH`), and the pod has none. A stub on PATH that
  answers `-V` and exits 0 for every other command lets the daemon boot for a proof that launches
  no agent, beside the plugin-gate `omp` stub Stage 1 writes; say in the proof that both are stubs.
- `scripts/e2e/stage1-skeleton.sh` cannot run in a pod: its `scripts/e2e/lib/free-port.sh` refuses
  an ephemeral range starting under 21000 (the pod's starts at 16000), `ss` and `tmux` are absent.
  Boot the daemon the way the script does — a `legion.yaml` with `project`, `port`, `postgres_dsn`,
  `state_dir`, `operator_token_file`, a port under 16000 — and cite CI's `Stage 1 live proof` step
  for the script itself.
- A daemon booted with no `dispatch_url` serves the API and runs no workflow, so no outbox lanes
  run: an outbox row seeded by SQL is never claimed. That is what makes a seeded foreign lease a
  usable fixture for `legion state`'s `heldOutboxRows`, and what the proof can claim: the
  projection read through the real binary, the wire shape and the plain line — never the lanes
  themselves, whose only surface short of a devbox Stage 3 is the daemon package's Postgres harness.

## Evidence

LEGION-694 (sjawhar/legion#1999) proved criterion 7 (`legion state --json` lists a due lifecycle
row held behind its issue's running one) on the `legion` binary built from the head over the pod's
embedded Postgres 16.4: a `tmux` stub in `/tmp/legion-proof/bin`, the Stage 1 `omp` stub, port
15694, a pgx runner in `/tmp/pgexec` inserting the issue and two `supervise` rows (one with
`lease_token = 'another-daemons-lease'` and `lease_until = now() + interval '20 minutes'`, one due
150 s ago); `legion state --json | jq -c .heldOutboxRows` gave
`[{"id":14,"kind":"supervise","issue":"PROOF694-1","waitedSeconds":155,"waitsOn":13}]` and the
plain fourth line `held lifecycle rows: 1; …`; setting the running row's `lease_until` to the past
gave `[]` and `held lifecycle rows: none`. The rows sat untouched for the daemon's whole life
(`attempts 0`, no lease) because the API-only daemon ran no lanes — which the first proof attempt
had expected the sweep to disprove. The tester reproduced the same proof on an embedded Postgres
of its own (port 54332, a fresh database so migration 0043 ran at boot) and parsed the wire document
through the strict `LegionStateResponse` with `bun run`. The lanes' behaviour (criteria 1–6) was
proven on `internal/daemon`'s Postgres harness in the pod and in CI, including the race-detector
step the pull request added.
