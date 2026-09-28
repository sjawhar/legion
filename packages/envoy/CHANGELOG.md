# Changelog

## [Unreleased]

### Added

- A CI record write that runs out of its two-second retry budget logs one JSON line, `ci record exceeded its retry budget`, naming the head (`owner`, `repo`, `number`, `sha`), the record's `checks`, the write's `attempts`, the `observations` it answered 503 and the last attempt's `error`, so an alarm can count the listener's 503s by that cause.
- Added the native Dispatch workspace API, persisted documents and events, retained Dispatch notifications, and daily Postgres backups.
- `POST /v1/roles/set` accepts `"soft": true`: the claim lands only if the role is unheld, held by a session that is no longer live, or held by the declared `previous_session_id`; any other live holder answers 409 with its id.
- Dispatch redelivers the GitHub App webhook's failed deliveries, which GitHub never redelivers on its own. Every two minutes it lists the webhook's attempts whose status is not OK and asks GitHub to redeliver each one the listener answered 5xx or GitHub could not complete. A failed or refused redelivery is retried after a doubling backoff, at most five times, and a 4xx is never redelivered. Requests go a second apart, and a GitHub rate limit stops the sweep until the time GitHub gives. `envoy-dispatch redeliver-webhooks --since <d> [--dry-run]` runs the same sweep over a chosen window.

### Changed

- The CI summary loop publishes a `pr.<n>.checks` settlement for every commit of a pull request
  whose checks settle, not only its current head, carrying the commit's `sha` as before. A head
  pushed with GitHub's `skip-checks` trailer runs no CI, so the commit it replaced settles for it
  after it lands (LEGION-208). Consumers decide which commit a settlement stands for: the
  TypeScript daemon still takes only its head's, and the Go daemon takes a code head's for the
  handoff-only heads after it. The listener no longer records each pull request's head (the
  `head.*` records in the CI bucket); a record an earlier listener wrote is skipped until its TTL.
- Every write of a CI record stamps it `schema: 1`. A record without it was last written by a
  listener that settled only heads, and it settles only when its last event is between the
  debounce and the debounce plus two minutes ago, so a head that finished during the handover
  still settles and the backlog of commits that listener never settled does not: on 2026-09-28
  production held 1,442 such records (595 `pr.<n>.checks` subjects, up to 168 hours old), which
  the first start would otherwise have published at once. Those records stay unsettled, without
  `schema`, until the bucket's seven-day TTL expires them. An observation that changes the record
  stamps it and the commit settles as usual; a stamped record settles however long it waited,
  across a restart too. A rolled-back listener ignores the field. A listener built with #1526 but
  without this change publishes the whole backlog, so every head-gated listener moves straight to
  a build carrying this change.

### Fixed

- A role claim that overtook another session no longer risks deleting a newer claim made in the same instant; old-holder cleanup now touches only the previous holder's topics, never the role row.
- Document version mutations now emit one retained, non-notifying `comment.anchor_refreshed` or `ask.anchor_refreshed` event for each changed open anchor, so stream consumers immediately observe orphaned quotes.
- A webhook redelivery of an event the notification stream already holds no longer publishes a second copy. Webhook envelopes whose dedupe key is the source plus the upstream delivery id (GitHub's `X-GitHub-Delivery`, Slack's `event_id`, Ghost Wispr's `X-GhostWispr-Delivery`) now publish under a JetStream MsgId of that key and topic, the rule Dispatch envelopes already used. Other producers that publish under those source names - CI settlements, the MCP bridge and `/v1/messages/*` callers - still publish every time. GitHub documents that a redelivery carries the original `X-GitHub-Delivery`, and Slack retries under the original `event_id`; that Ghost Wispr resends under its original delivery id is assumed, not documented.
- A GitHub delivery no longer makes an envelope larger than NATS takes, which a body up to the 25 MiB cap could: every text a webhook envelope copies is capped at 2,048 runes, a push's changed paths at 32,768 runes as well as 100 paths, and every text a check run copies into the CI store at 2,048 runes, a name past that keeping a digest of the whole name so two names sharing their first 2,048 runes stay two checks; a ref or workflow file name writes whitespace, `*` and `>` as `_` in its topic. What NATS still refuses however often it is sent (a subject past the server's 4 KiB protocol line, over which the server closed the listener's NATS connection, a message past the server's max payload, or a subject holding whitespace or an empty token) is answered 422 and logged `<source> publish refused` rather than 503 and `<source> publish failed`, so Dispatch's sweep does not redeliver it and the publish-failure line does not fire. A head's CI record is bounded at 384 KiB and its settlement at 960 KiB; a check past either is refused the same way and that head never settles from the listener, and a settlement NATS refuses anyway marks its head the same way instead of being published again every second. `/v1/messages/publish` and `/v1/messages/send` answer a message NATS cannot take whole with 413 naming its size. Every KV handle checks its keys before sending (a key long enough to close the connection, or holding an empty token, is refused), so a `/v1` session id or role NATS would refuse is a 413 or 400 rather than a 503 after the listener's NATS connection closed. A write is held to the longest subject its key makes; a key an earlier build stored past that bound still lists and deletes, the stores skip it where they cannot read it, and the reapers delete it, so such a key never stops a listener from starting. Dispatch's outbox logs a destination NATS refuses once and counts it done instead of retrying it forever.
