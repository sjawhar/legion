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

The payload is the full status summary: every check group and failing check URLs, the attempt set, generation, and snapshot. Check settlement is at-least-once: a settlement can be followed by a `superseded_settlement: "true"` payload. Every settlement carries its attempt set `check_runs` — the latest GitHub check-run id per check name, sorted by name — plus the listener's `generation` (the record's state version) and `snapshot` (the record's hash). Consumers order same-head settlements by the attempt set, compared per shared name: no id lower and some id higher (or a new name) is newer; every shared id equal and no new name is the same set; no id higher and some lower is older; anything else is a mixed view and is dropped as a conflict (names only in the stored set are ignored — a check can vanish from GitHub's view, and a record recreated after the seven-day KV TTL starts sparse). Within one producer record per-name ids never decrease, and a consumer's fence is the per-name maximum over every view it has accepted — an accepted set merges into the fence, nothing is pruned — so the fence never decreases either: a newer attempt is newer whatever its completion time, no timestamps take part in ordering, and a name an incomplete view omitted cannot later reappear as new. At the same set the listener's `generation` orders its own settlements: lower is stale; equal is a duplicate when the `snapshot` matches and otherwise a conflict (an equal pair with a different snapshot cannot occur within one record's lifetime; a recreated record may reuse one and is dropped). A live settlement is a possibly incomplete view of the head (a missed webhook, a record recreated after the KV TTL): it decides the outcome of every name it reports — at any id the ordering accepted, including the same run observed in place — and says nothing about the rest, whose last known outcome stands; the head is red while any failure remains. A consumer that reconciles a verdict from GitHub's rollup compares the rollup's attempt set the same way, but GitHub's read is complete: its failing check runs and failing commit statuses replace the stored ones wholesale. Statuses have no check run and the listener never sees them, so a consumer keeps them apart from check-run failures: a check run that shares a status's name cannot retire it — only GitHub does (likewise a deleted check's failure). A newer rollup set merges into the fence and takes the identity (no listener generation); the same set applies GitHub's verdict and keeps the listener identity for duplicate detection; an older, mixed, or empty-over-fenced set is ignored. A terminal read (green or red) then holds the tie at that set: a live settlement at the same set is accepted only if its verdict and failing set agree — refreshing the listener identity without releasing GitHub's authority — and a disagreeing one is stale whatever its generation until the set advances; a pending or cancelled-only read uncertifies a green head, leaves a red one untouched, and holds nothing — it releases any authority held at that set — so the terminal live settlement that follows applies at once, subject to the ordinary generation and duplicate rules (a replay or a lower generation still does not apply). Pending is therefore not a commutative join: a pending read after a live green uncertifies it until the next terminal view. Two remainders. An in-place conclusion change on an existing run id: GitHub's view stands and the listener's is recovered by the next successful, non-skipped read at that set — the dropped delivery is not replayed. A check whose highest run is deleted on GitHub: the fence keeps that id, so a rollup reporting a lower run under the same name is older until a newer run appears. A head publishes only when at least one check has a positive run id; legacy checks without one remain in the status groups and failing names but not in `check_runs`. A legacy in-progress check whose completion is never observed holds the head unsettled until it reruns; rerun the affected check to release it. The summary waits for `ENVOY_CI_DEBOUNCE` (default `5s`), all check runs to be terminal, and every observed suite to be `completed`; heads with no suite still settle after terminal checks.
