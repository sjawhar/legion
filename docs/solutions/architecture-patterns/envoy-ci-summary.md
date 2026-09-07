# Envoy CI Notifications
Envoy emits one settled CI result per pull-request head on
`notifications.github.<owner>.<repo>.pr.<number>.checks`. Check runs and check
suites are aggregated first; raw CI observations are not published.

## Durable settlement state machine

Each commit uses the KV key `<owner>.<repo>.pr<number>.<sha>` in
`envoy_ci_state`; the PR head is a separate durable `head.<owner>.<repo>.<number>`
record. State contains checks, suites, a state-version `Generation`, `EmittedCount`,
`SettledEmitted`, and an optional claim `{hash, generation, claimed_at}`.

1. `Record` and `RecordSuite` CAS-update the aggregate. A new record starts at
   generation 0; every later aggregate-hash change and every re-arm advances the
   state version, then clears `SettledEmitted`.
2. The reconcile loop reads its rebuildable cache and selects only a quiet,
   terminal, current-head state without an emitted or live claim. A claim older
   than twice the debounce interval is reclaimed.
3. `ClaimSettlement` re-reads durable state and head, verifies the expected hash,
   generation, terminality, and head identity, then CAS-writes the hash-bound
   claim. A mismatch publishes nothing.
4. The claimant renders that durable snapshot and publishes with
   `github.checks.<owner>/<repo>.pr.<number>.<sha>.g<generation>`.
5. `MarkSettled` records the emission in `EmittedCount` and clears its claim. A
   publish failure calls `ReleaseClaim`, which advances the state version; a
   generation or hash change makes cleanup refuse an obsolete claim.

All local cache write-through and watcher updates carry a KV revision and only
apply at or above the cached revision. This prevents an older claim/mark write
from replacing a newer watcher state.

## Envelope

The payload is the full status summary: every check group and failing check URLs,
the durable `snapshot` hash, and `latest_completed_at`, the maximum RFC3339
`ObservedAt` among its completed checks. Consumers order summaries for one SHA
lexicographically by `(latest_check_run_id, generation)`, with the former the
largest known check-run ID in the settlement. An equal pair is a duplicate only
when its `snapshot` matches. A consumer that reconciles a verdict from GitHub's rollup keeps the listener identity it last accepted (for duplicate detection) and raises a completion watermark from GitHub's `completedAt`; an equal-id settlement must then pass BOTH orderings — a higher generation AND a `latest_completed_at` not earlier than the watermark — and against a watermark GitHub set with no listener identity an equal completion is also stale (GitHub's view wins the tie). A genuinely newer settlement completing within the same second as GitHub's read is therefore delayed until the next reconciliation, not lost. The summary waits for `ENVOY_CI_DEBOUNCE` (default
`5s`), all check runs to be terminal, and every observed suite to be `completed`;
heads with no suite still settle after terminal checks.

After the seven-day KV TTL recreates a record, its generation restarts at 0 (consumers keep the higher generation they already hold at that id, so such settlements stay rejected until a newer run raises the id). If
its first observation updates an existing lower-ID run, both are dropped by
consumers; the consumer's verdict is repaired when it next reads GitHub, and its live fence moves again only when a newer run raises the id. A head settles only when at least one completed check carries a GitHub completion time (`completed_at`; for records written before this release, `updated_at` of a completed check): a head whose completed checks all lack one stays silent until a check with a completion time is observed — rerun one. A legacy check that was in progress at
cutover and whose completion is never observed keeps the head unsettled until
the check reruns; rerun the affected check to release it.
