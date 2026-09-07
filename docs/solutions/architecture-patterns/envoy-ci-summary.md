# Envoy CI Notifications
Envoy emits one settled CI result per pull-request head on
`notifications.github.<owner>.<repo>.pr.<number>.checks`. Check runs and check
suites are aggregated first; raw CI observations are not published.

## Durable settlement state machine

Each commit uses the KV key `<owner>.<repo>.pr<number>.<sha>` in
`envoy_ci_state`; the PR head is a separate durable `head.<owner>.<repo>.<number>`
record. State contains checks, suites, `Generation`, `SettledEmitted`, and an
optional claim `{hash, generation, claimed_at}`.

1. `Record` and `RecordSuite` CAS-update the aggregate. A terminal-picture
   change while settled or claimed re-arms it: increment `Generation`, clear
   `SettledEmitted`, and clear the claim. A generation above zero marks a
   re-settlement.
2. The reconcile loop reads its rebuildable cache and selects only a quiet,
   terminal, current-head state without an emitted or live claim. A claim older
   than twice the debounce interval is reclaimed.
3. `ClaimSettlement` re-reads durable state and head, verifies hash,
   generation, terminality, and head identity, then CAS-writes the claim.
   A mismatch publishes nothing.
4. The claimant renders that durable snapshot and publishes with
   `github.checks.<owner>/<repo>.pr.<number>.<sha>.g<generation>`.
5. `MarkSettled` CAS-marks the same generation emitted and clears its claim.
   A publish failure calls `ReleaseClaim`, so the next tick retries. A
   generation change makes either cleanup refuse the obsolete claim.

All local cache write-through and watcher updates carry a KV revision and only
apply at or above the cached revision. This prevents an older claim/mark write
from replacing a newer watcher state.

## Envelope

The payload is the full status summary: every check group and failing check URLs.
Consumers order summaries with `latest_check_run_id`, the largest check-run ID among the retained checks.
The summary waits for `ENVOY_CI_DEBOUNCE` (default `5s`), all check runs to be terminal, and every observed suite to be `completed`; heads with no suite still settle after terminal checks.
