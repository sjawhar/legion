# Envoy Condensed CI Summary (`pr.<n>.ci`)

How Envoy turns the per-`check_run` webhook firehose into a single debounced, per-commit, status-sectioned CI summary — so a subscriber sees the checks "build up" instead of drinking every `check_run` transition.

## Problem

`notifications.github.<o>.<r>.pr.<n>.ci` originally published **one raw envelope per `check_run`/`check_suite` webhook** attached to a PR. A single CI run fires dozens of these (each job: `queued` → `in_progress` → `completed`, plus re-runs), so a subscriber waiting on a PR received a torrent of near-duplicate events and had to reassemble the pipeline state itself.

## Approach: ingest-side KV aggregation + debounced emit

GitHub webhooks land centrally on the Fargate listener, so aggregation is simplest at ingest.

1. **Stop publishing raw CI events.** `contracts.GithubEnvelopes` returns no envelope for `check_run`/`check_suite`.
2. **Fold each `check_run` into per-commit state.** The webhook handler calls `cistore.Store.Record(...)`, which does a compare-and-swap read-modify-write into a JetStream **KV** bucket (`envoy_ci_state`), keyed by `(owner, repo, PR number, head SHA)`. `check_suite` is ignored — it is a per-app rollup with no per-check name, so it would add a redundant row next to the `check_run`s it aggregates. GitHub Actions emits per-job `check_run`s, so nothing is lost.
3. **Emit one rendered summary per quiet burst.** A reconcile ticker (`cistore.StartSummaryLoop`) scans cached commit states each tick; when a commit's checks have been quiet for the debounce window (`ENVOY_CI_DEBOUNCE`, default `5s`) *and* its check set changed since the last emit, it publishes one rendered summary to `pr.<n>.ci`.

All coordination state lives in KV, so the aggregation is durable, restart-safe, and correct across multiple listener replicas. The only in-memory state is a rebuildable `WatchAll` read-cache (mirrors `internal/store`).

## Exactly-once emit across replicas + a stale read-cache

Emit-once **and debounce** are enforced by compare-and-swap against fresh KV, not an in-memory flag:

- `Record` retries its CAS RMW (`kv.Update(key, val, rev)` / `kv.Create`) on revision conflict, so concurrent writers — parallel webhook handlers, multiple replicas — racing on the same commit never lose an update. Retry uses a **time-budgeted, full-jitter backoff** rather than a fixed attempt count: a fixed count starves when many checks for one SHA land at once (a real bug caught in testing — 8 retries lost updates under 12-way concurrency). The budget is **2s**, kept well under the listener's 10s HTTP `WriteTimeout` because `Record` runs synchronously in the webhook handler and one `check_run` can fan out over several PRs sequentially. (The budget bounds only the retry loop; a single hung KV call can still block up to the JetStream `MaxWait` — a systemic limit of the legacy nats.go KV API, shared with `internal/store`.)
- `MarkEmitted(key, hash, debounce)` reads fresh KV and re-validates the caller's decision before the CAS `Update`. It returns `false` (not an error) — meaning "don't emit" — when **any** of these hold: the entry already carries `hash` (already emitted), `fresh.Hash() != hash` (a `Record` landed after the loop rendered → that summary is now stale), `now - fresh.LastEventAt < debounce` (that same late `Record` reopened the quiet window → too early), or the revision moved (CAS conflict). These four guards are what make emit-once **and** debounce hold against the eventually-consistent `WatchAll` cache: a stale or premature summary can never win the CAS. Because success implies `fresh.Hash() == hash`, and the render depends only on the hashed check set plus stable identity, the loop's already-rendered summary faithfully represents what was marked.

The commit hash is an order-independent SHA-256 of `{name, status, conclusion}` across all checks, so a re-run that flips a check back to `in_progress` changes the hash and re-opens emission, while an unchanged set stays quiet. Head-SHA keying means a new push starts a fresh tally.

> **Why the extra `MarkEmitted` guards (from review):** an earlier version only short-circuited on `LastEmitHash == hash`. That closed the *duplicate*-emit race but not a *stale/premature* one: a `Record` landing between the loop's `List()` snapshot and `MarkEmitted` could stamp the old hash onto newer durable state and publish a summary that no longer matched KV (and skip the debounce). Re-checking `fresh.Hash()` and `LastEventAt` inside the CAS closes it.

## The MarkEmitted-then-Publish tradeoff

The loop calls `MarkEmitted` **before** `Publish`. This favors exactly-once over at-least-once: a failed publish drops that summary rather than risking a double-publish. Acceptable for a non-critical status summary — the next `check_run` event advances the hash and re-opens emission. A dropped publish is logged at WARN with `topic`, `sha`, and `hash` so the loss is traceable. If at-least-once ever matters more, invert to publish-then-`MarkEmitted` and dedupe downstream on `DedupeKey`.

`DedupeKey` is `github.ci.<owner>/<repo>.pr.<number>.<sha>.<hash>` — it includes the PR number, not just `<sha>.<hash>`. A `check_run` can attach to multiple PRs, so two PRs sharing a head SHA + identical check set would otherwise collide on the key, and a wildcard subscriber's `(DedupeKey, SessionID)` dedupe would suppress the second PR's summary. The PR number keeps each PR's summary independently deliverable.

## Notification shape (JSON)

`PayloadSummary` is a compact JSON object (same `summaryJSON` convention as every other envoy event — not a rendered text/ASCII summary). Each status is `{count, checks}` with the full sorted name list (nothing collapsed); every status is always present (`{"count":0,"checks":[]}` when empty).

```json
{
  "kind": "ci_summary",
  "repo": "sjawhar/legion",
  "number": "13728",
  "sha": "a1b2c3d9999999",
  "failed":  { "count": 1, "checks": ["infra-tests"] },
  "running": { "count": 2, "checks": ["build-image", "snapshots"] },
  "passed":  { "count": 6, "checks": ["auto-approve", "classify", "detect-changes", "pr-checks-result", "review", "vercel"] },
  "queued":  { "count": 1, "checks": ["task-tests"] },
  "skipped": { "count": 12, "checks": ["skip-a", "...", "skip-l"] }
}
```

`Payload` is left empty — `PayloadSummary` carries the complete structured summary. An earlier revision rendered an ASCII text summary with a lossy skipped-count collapse; that was replaced with this JSON per review (JSON matches the codebase convention and keeps every check name + count).

## Operational hardening (from review)

- **Watcher health.** The summary loop reads only the `WatchAll` cache (no KV fallback, unlike `Record`), so a dead watcher would silently stop/stale summaries while `Ping()` still passed. `Ping()` now also returns a sticky error set when `WatchAll()` fails to start or its update stream closes, so the listener's self-health watchdog restarts the task and rebuilds the cache from durable KV.
- **KV TTL = 7 days** (per-key, reset on each write). Long enough that an in-progress commit isn't dropped mid-flight, and a rerun days later still finds prior checks. A key only expires 7d after its *last* check event.
- **Loop lifecycle.** `StartSummaryLoop(ctx, ...)` stops on context cancel; the listener cancels it at the start of shutdown so the ticker doesn't hit a draining NATS conn.
- **Unknown conclusions fail loud.** All documented GitHub conclusions are classified explicitly; an unknown/future completed conclusion is surfaced as **failed**, not silently passed.
- **KV key.** `Key` preserves `.` (a legal KV-key char, unlike in a NATS subject) so repos like `foo.bar` and `foo_bar` don't collide; only truly-invalid chars (`* > space /`) are sanitized.


## Contract break

`pr.<n>.ci` changed shape (single raw check event → rendered multi-check summary) and cadence (per-event → debounced). The only consumers were docs and the `legion-controller` skill; both were updated. No code subscriber asserted the old per-event payload.

## Deferred gap: no GitHub-API reconcile

A dropped `check_run` webhook means a check is missing from the summary until its next transition. This is pure webhook + debounce; a GitHub-API reconcile (poll check-runs to backfill missed webhooks) was deliberately deferred. Add it if gaps appear in practice.

## Files

- `packages/envoy/internal/cistore/` — `cistore.go` (state, CAS `Record`/`MarkEmitted`, `WatchAll` cache), `render.go` (pure summary rendering), `loop.go` (`StartSummaryLoop`).
- `packages/envoy/internal/contracts/normalize.go` — `GithubEnvelopes` drops raw CI; `GithubCIObservations` extracts per-PR facts from `check_run`.
- `packages/envoy/internal/webhook/github.go` — `CIRecorder` param; records instead of publishing.
- `packages/envoy/cmd/listener/main.go` — opens the store, wires the recorder behind the readiness gate, starts the loop, pings the bucket in self-health.
