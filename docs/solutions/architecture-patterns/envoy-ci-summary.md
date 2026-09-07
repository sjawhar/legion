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

The payload is the full status summary: every check group and failing check URLs, the attempt set, generation, and snapshot. Check settlement is at-least-once: a settlement can be followed by a `superseded_settlement: "true"` payload. Every settlement carries its attempt set `check_runs` — the latest GitHub check-run id per check name, sorted by name — plus the listener's `generation` (the record's state version) and `snapshot` (the record's hash). Consumers order same-head settlements by the attempt set, compared per shared name: no id lower and some id higher (or a new name) is newer; every shared id equal and no new name is the same set; no id higher and some lower is older; anything else is a mixed view and is dropped as a conflict (names only in the stored set are ignored — a check can vanish from GitHub's view, and a record recreated after the seven-day KV TTL starts sparse). Per-name ids never decrease within a head, so a newer attempt is newer whatever its completion time and no timestamps take part in ordering. At the same set the listener's `generation` orders its own settlements: lower is stale; equal is a duplicate when the `snapshot` matches and otherwise a conflict (an equal pair with a different snapshot cannot occur within one record's lifetime; a recreated record may reuse one and is dropped). A consumer that reconciles a verdict from GitHub's rollup compares the rollup's attempt set the same way: a newer set replaces the stored fence (no listener identity); the same set applies GitHub's verdict and keeps the listener identity for duplicate detection; an older, mixed, or empty-over-fenced set is ignored. A terminal read (green or red) then holds the tie at that set: a live settlement at the same set is accepted only if its verdict and failing set agree — refreshing the listener identity without releasing GitHub's authority — and a disagreeing one is stale whatever its generation until the set advances; a pending or cancelled-only read uncertifies a green head and holds nothing, so the terminal live settlement that follows applies at once. The undecidable remainder is an in-place conclusion change on an existing run id: GitHub's view stands and the listener's is recovered by the next successful, non-skipped read at that set — the dropped delivery is not replayed. A head publishes only when at least one check has a positive run id; legacy checks without one remain in the status groups and failing names but not in `check_runs`. A legacy in-progress check whose completion is never observed holds the head unsettled until it reruns; rerun the affected check to release it. The summary waits for `ENVOY_CI_DEBOUNCE` (default `5s`), all check runs to be terminal, and every observed suite to be `completed`; heads with no suite still settle after terminal checks.
