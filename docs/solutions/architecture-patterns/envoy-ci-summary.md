# Envoy CI Notifications
Envoy emits one settled CI result per commit of a pull request whose checks settle, the current
head or not, on `notifications.github.<owner>.<repo>.pr.<number>.checks`; the payload's `sha`
names the commit, and consumers decide which commit a settlement stands for. Check runs and check
suites are aggregated first; raw CI observations are not published.

## Durable settlement state machine

Each commit uses the KV key `<owner>.<repo>.pr<number>.<sha>` in
`envoy_ci_state`. The listener keeps no head record: a `head.<owner>.<repo>.<number>` record an
earlier listener wrote is skipped, never cached as state, until its TTL expires. A dot in a segment is written `=` (`sjawhar/.github` keys as
`sjawhar.=github...`): a KV key's tokens must not be empty, and no GitHub name holds
`=`, so `foo.bar` and `foo_bar` keep distinct keys. State contains checks, suites, a state-version `Generation`, `EmittedCount`,
`SettledEmitted`, an optional claim `{hash, generation, claimed_at}`, and `schema: 1`, which
every write of the record stamps.

1. `Record` and `RecordSuite` CAS-update the aggregate. A new record starts at
   generation 0; every later aggregate-hash change and every re-arm advances the
   state version, then clears `SettledEmitted`.
2. The reconcile loop reads its rebuildable cache and selects every record `due` to settle:
   unsettled, terminal, quiet for the debounce and, when it has no `schema`, inside the handover
   grace (see below), whichever commit of the pull request it is for. It checks that before a
   claim older than twice the debounce interval is reclaimed.
3. `ClaimSettlement` re-reads durable state, verifies the expected hash, generation and no live
   claim, applies `due` to the durable record, then CAS-writes the hash-bound claim. A mismatch
   publishes nothing.
4. The claimant renders that durable snapshot and publishes with
   `github.checks.<owner>/<repo>.pr.<number>.<sha>.g<generation>`.
5. `MarkSettled` records the emission in `EmittedCount` and clears its claim. A
   publish failure calls `ReleaseClaim`, which advances the state version; a
   generation or hash change makes cleanup refuse an obsolete claim.

All local cache write-through and watcher updates carry a KV revision and only
apply at or above the cached revision. This prevents an older claim/mark write
from replacing a newer watcher state.

## Records a head-gated listener left

A listener that settled only a pull request's head left every other terminal commit's record
unsettled until the bucket's seven-day TTL; on 2026-09-28 production held 1,442 of them across
595 `pr.<n>.checks` subjects, the youngest six minutes old and the median 65 hours. Settling them
would publish verdicts days late, which an agent waiting on its head can read as its head's. So a
record without `schema` settles only in `[debounce, debounce + 4 min)` after its last event. A
head that finished just before the old listener was replaced is still owed its settlement, and
the window must cover the longest time between the old listener's last tick and the new one's
first. That is the on-prem compose deploy, which is stop-then-start, on its slowest startup path:
a 30 s stop grace, two 30 s fail-open cache gates and the subscribe retry loop's 135 s of backoff,
225 s in all. Production's ECS rollouts take 0.6 to 20.5 s from SIGTERM to ready (once 58.9 s).
Past the band such a record never settles. It stays `settled_emitted: false` with no `schema`
until the TTL expires it; that is expected, and it is history, not pending work. The listener's
`/metrics` gauge `envoy_ci_legacy_records_held` carries how many the last tick held back, and it
logs `checks held back a head-gated listener's unsettled records` with the count the first time a
process holds any. An observation that changes the record (a new check run, a re-run, a
suite) stamps it, and the commit then settles as any other; a redelivery of what the record already
holds writes nothing. A stamped record, of any schema, is never held back, so a settlement pending
across a restart of the listener still publishes; a process-start cutoff would drop those. A record
without a schema whose `last_event_at` is ahead of the listener's clock publishes once wall time
reaches its band, and raising `ENVOY_CI_DEBOUNCE` moves the band's far edge, admitting the records
in the added slice.

A head-gated listener (legion 9de053a2, or 1ad2466c) run after a rollback decodes a stamped record,
ignoring the field it does not know, and does not mistake it for a head record, which it recognizes
by `kind`; its own writes drop the stamp. Seven days after the last head-gated listener stops, no
record without a schema remains. Until then a head-gated listener still running settles backlog
records as their head records expire, and one upgraded to a build carrying #1526 but not this rule
publishes the whole backlog, so every head-gated listener moves straight to a build with this rule.

## Envelope

The payload is the full status summary: every check group and failing check URLs, the attempt set, generation, and snapshot. Check settlement is at-least-once: a settlement can be followed by a `superseded_settlement: "true"` payload. Every settlement carries its attempt set `check_runs` — the latest GitHub check-run id per check name, sorted by name — plus the listener's `generation` (the record's state version) and `snapshot` (the record's hash). Consumers order settlements of one commit by the attempt set, compared per shared name: no id lower and some id higher (or a new name — a new name counts as higher) is newer; every shared id equal and no new name is the same set; no id higher, no new name, and some id lower is older; anything else (a higher or new alongside a lower) is a mixed view and is dropped as a conflict (names only in the stored set are ignored — a check can vanish from GitHub's view, and a record recreated after the seven-day KV TTL starts sparse). Within one producer record per-name ids never decrease, and a consumer's fence is the per-name maximum over every view it has accepted — an accepted set merges into the fence, nothing is pruned — so the fence never decreases either: a newer attempt is newer whatever its completion time, no timestamps take part in ordering, and a name an incomplete view omitted cannot later reappear as new. At the same set the listener's `generation` orders its own settlements: lower is stale; equal is a duplicate when the `snapshot` matches and otherwise a conflict (an equal pair with a different snapshot cannot occur within one record's lifetime; a recreated record may reuse one and is dropped). A live settlement is a possibly incomplete view of the head (a missed webhook, a record recreated after the KV TTL): it decides the outcome of every name it reports — at any id the ordering accepted, including the same run observed in place — and says nothing about the rest: a known failure among them stands (the consumer keeps failure names, not a per-name status map), and the head is red while any failure remains. A consumer that reconciles a verdict from GitHub's rollup compares the rollup's attempt set the same way, but GitHub's read is complete: its failing check runs and failing commit statuses replace the stored ones wholesale. Statuses have no check run and the listener never sees them, so a consumer keeps them apart from check-run failures: a check run that shares a status's name cannot retire it — only GitHub does (likewise a deleted check's failure). A newer rollup set merges into the fence and takes the identity (no listener generation); the same set applies GitHub's verdict and keeps the listener identity for duplicate detection; an older, mixed, or empty-over-fenced set is ignored. A terminal read (green or red) then holds the tie at that set: a live settlement at the same set is accepted only if its effective outcome — the check-run failures it reports plus the stored ones it omits and the stored commit-status failures — agrees with the reconciled verdict, refreshing the listener identity without releasing GitHub's authority; a disagreeing one is stale whatever its generation until the set advances; a pending or cancelled-only read uncertifies a green head, leaves a red one untouched, and holds nothing — it releases any authority held at that set — so the terminal live settlement that follows applies at once, subject to the ordinary generation and duplicate rules (a replay or a lower generation still does not apply). Pending is therefore not a commutative join: a pending read after a live green uncertifies it until the next terminal view. Two remainders. An in-place conclusion change on an existing run id: GitHub's view stands and the listener's is recovered by the next successful, non-skipped read at that set — the dropped delivery is not replayed. A check whose highest run is deleted on GitHub: the fence keeps that id, so a rollup reporting a lower run under the same name is older until a newer run appears. A consumer that orders head changes by the PR's `updated_at` (GitHub's second resolution) accepts a read of a different head at an equal clock — a stale read returning the previous head within the same second as its replacement rewinds that consumer until its next accurate, non-skipped read. A head publishes only when at least one check has a positive run id; legacy checks without one remain in the status groups and failing names but not in `check_runs`. A legacy in-progress check whose completion is never observed holds the head unsettled until it reruns; rerun the affected check to release it. The summary waits for `ENVOY_CI_DEBOUNCE` (default `5s`), all check runs to be terminal, and every observed suite to be `completed`; heads with no suite still settle after terminal checks.
