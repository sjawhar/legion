# Envoy CI Notifications

Envoy offers both immediate, per-check observations and a debounced, per-commit summary for
each pull request. Consumers that react to a particular check use the raw observation; consumers
that need the overall state use the summary.

## Delivery paths

1. **Publish each PR-associated check.** A `check_run` produces one raw envelope per associated
   PR on `notifications.github.<o>.<r>.pr.<n>.check`. Its `Payload` is JSON with `sha`, `name`,
   `status`, and `conclusion`; `PayloadSummary` carries a compact display string. `check_suite`
   is ignored because it is a per-app rollup without a per-check name, and a check without an
   associated PR has no topic.
2. **Fold each check into per-commit state.** The webhook handler calls
   `cistore.Store.Record(...)`, which performs compare-and-swap read-modify-write in JetStream KV
   bucket `envoy_ci_state`, keyed by `(owner, repo, PR number, head SHA)`.
3. **Emit one summary per quiet burst.** A reconcile ticker (`cistore.StartSummaryLoop`) scans
   cached commit states. When a check set has been quiet for `ENVOY_CI_DEBOUNCE` (default `5s`)
   and changed since its last emission, it publishes a rendered JSON summary on
   `notifications.github.<o>.<r>.pr.<n>.ci`.

All summary coordination state lives in KV, so aggregation is durable, restart-safe, and correct
across listener replicas. The only in-memory state is a rebuildable `WatchAll` read-cache.

## Exactly-once emit across replicas + a stale read-cache

Emit-once **and debounce** are enforced by compare-and-swap against fresh KV, not an in-memory flag:

- `Record` retries its CAS RMW (`kv.Update(key, val, rev)` / `kv.Create`) on revision conflict, so concurrent writers — parallel webhook handlers, multiple replicas — racing on the same commit never lose an update. Retry uses a **time-budgeted, full-jitter backoff** rather than a fixed attempt count: a fixed count starves when many checks for one SHA land at once (a real bug caught in testing — 8 retries lost updates under 12-way concurrency). The budget is **2s**, kept well under the listener's 10s HTTP `WriteTimeout` because `Record` runs synchronously in the webhook handler and one `check_run` can fan out over several PRs sequentially. (The budget bounds only the retry loop; a single hung KV call can still block up to the JetStream `MaxWait` — a systemic limit of the legacy nats.go KV API, shared with `internal/store`.)
- `MarkEmitted(key, hash, debounce)` reads fresh KV and re-validates the caller's decision before the CAS `Update`. It returns `false` (not an error) — meaning "don't emit" — when **any** of these hold: the entry already carries `hash` (already emitted), `fresh.Hash() != hash` (a `Record` landed after the loop rendered → that summary is now stale), `now - fresh.LastEventAt < debounce` (that same late `Record` reopened the quiet window → too early), or the revision moved (CAS conflict). These four guards are what make emit-once **and** debounce hold against the eventually-consistent `WatchAll` cache: a stale or premature summary can never win the CAS. Because success implies `fresh.Hash() == hash`, and the render depends only on the hashed check set plus stable identity, the loop's already-rendered summary faithfully represents what was marked.

The commit hash is an order-independent SHA-256 of `{name, status, conclusion}` across all checks, so a re-run that flips a check back to `in_progress` changes the hash and re-opens emission, while an unchanged set stays quiet. Head-SHA keying means a new push starts a fresh tally.

`MarkEmitted` rechecks the current hash and quiet window inside its compare-and-swap operation, so a summary rendered from a stale `WatchAll` snapshot cannot overwrite newer state or bypass the debounce window.

## The MarkEmitted-then-Publish tradeoff

The loop calls `MarkEmitted` **before** `Publish`. This favors exactly-once over at-least-once: a failed publish drops that summary rather than risking a double-publish. A later changed check set becomes a new candidate for emission. Dropped summaries are logged at WARN with `topic`, `sha`, and `hash`.

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

`Payload` is left empty — `PayloadSummary` carries the complete structured summary.

## Operational hardening

- **Watcher health.** The summary loop reads only the `WatchAll` cache (no KV fallback, unlike `Record`), so a dead watcher would silently stop/stale summaries while `Ping()` still passed. `Ping()` now also returns a sticky error set when `WatchAll()` fails to start or its update stream closes, so the listener's self-health watchdog restarts the task and rebuilds the cache from durable KV.
- **KV TTL = 7 days** (per-key, reset on each write). Long enough that an in-progress commit isn't dropped mid-flight, and a rerun days later still finds prior checks. A key only expires 7d after its *last* check event.
- **Loop lifecycle.** `StartSummaryLoop(ctx, ...)` stops on context cancel; the listener cancels it at the start of shutdown so the ticker doesn't hit a draining NATS conn.
- **Unknown conclusions fail loud.** All documented GitHub conclusions are classified explicitly; an unknown/future completed conclusion is surfaced as **failed**, not silently passed.
- **KV key.** `Key` preserves `.` (a legal KV-key char, unlike in a NATS subject) so repos like `foo.bar` and `foo_bar` don't collide; only truly-invalid chars (`* > space /`) are sanitized.



## Files

- `packages/envoy/internal/cistore/` — `cistore.go` (state, CAS `Record`/`MarkEmitted`, `WatchAll` cache), `render.go` (pure summary rendering), `loop.go` (`StartSummaryLoop`).
- `packages/envoy/internal/contracts/normalize.go` — `GithubCIObservations` extracts per-PR `check_run` facts and `GithubCIEnvelope` builds their raw envelopes.
- `packages/envoy/internal/webhook/github.go` — records each check and publishes its raw observation.
- `packages/envoy/cmd/listener/main.go` — opens the store, wires the recorder behind the readiness gate, starts the loop, pings the bucket in self-health.
