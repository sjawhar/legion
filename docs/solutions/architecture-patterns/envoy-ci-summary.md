# Envoy CI Notifications

Envoy emits one useful CI event per pull-request head:
`notifications.github.<owner>.<repo>.pr.<number>.checks`. Agents that subscribe
to `pr.<number>.>` therefore receive the settled CI result without filtering
raw check traffic or intermediate snapshots.

## Delivery path

1. A PR-associated `check_run` records its name, attempt ID, URL, status, and
   conclusion in the per-head CI state.
2. A PR-associated `check_suite` records its ID, status, conclusion, and app
   ID in the same state.
3. The reconcile loop waits for `ENVOY_CI_DEBOUNCE` (default `5s`), confirms
   the SHA is the PR head, and emits `checks` only when every check run is
   terminal and every recorded suite is `completed`.

Some CI providers send no suites. When no suite was observed for a head, the
terminal check-run rule is sufficient. A check run is always required.

## Exactly-once episodes

`MarkSettled` uses a compare-and-swap against fresh JetStream KV state. It
rechecks the rendered state hash, debounce window, terminal check runs, and
suite gate before setting `SettledEmitted`, so stale WatchAll state cannot
publish a false result and listener replicas cannot both publish the same
episode.

The claim happens before the publish. This deliberately favors exactly-once
delivery: a failed publish is logged as a warning rather than retried as a
potential duplicate; a subsequent CI change creates a new settlement episode.

A changed check run or a suite returning to a non-completed state re-arms a
settled head. The next completed episode emits another `checks` envelope with
`"superseded_settlement":"true"` in `Payload` and ` (re-settled)` appended to
the prose summary.

## Envelope

```text
Topic: notifications.github.example-org.example-repo.pr.42.checks
PayloadSummary: checks settled on example-org/example-repo#42 @ abcdef1: 1 passed, 1 failed, 0 cancelled, 0 skipped; failing: lint
```

`Payload` is JSON rendered from the complete state:

```json
{
  "kind": "checks",
  "repo": "example-org/example-repo",
  "number": "42",
  "sha": "abcdef1234567890abcdef1234567890abcdef12",
  "is_head": true,
  "failed": { "count": 1, "checks": ["lint"] },
  "running": { "count": 0, "checks": [] },
  "passed": { "count": 1, "checks": ["build"] },
  "queued": { "count": 0, "checks": [] },
  "cancelled": { "count": 0, "checks": [] },
  "skipped": { "count": 0, "checks": [] },
  "failing_checks": [{ "name": "lint", "url": "https://example-host/checks/301" }]
}
```

`PayloadSummary` is one line of prose capped at 160 characters. JSON belongs
only in `Payload`.

## Operational behavior

- CI state is in the `envoy_ci_state` JetStream KV bucket, keyed by owner,
  repository, PR number, and head SHA.
- The WatchAll cache is rebuildable. A failed watcher makes `Ping` fail so the
  listener self-health watchdog restarts the loop.
- State expires seven days after its last CI event.
- Unknown completed conclusions are classified as failed; unknown active
  statuses are treated as queued.

## Files

- `packages/envoy/internal/contracts/normalize.go` extracts check-run and
  check-suite observations.
- `packages/envoy/internal/webhook/github.go` records observations without
  publishing raw CI envelopes.
- `packages/envoy/internal/cistore/` stores state, applies the settlement CAS,
  renders JSON, and publishes `checks`.
