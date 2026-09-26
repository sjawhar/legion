# Changelog

## [Unreleased]

### Added

- Added the native Dispatch workspace API, persisted documents and events, retained Dispatch notifications, and daily Postgres backups.
- `POST /v1/roles/set` accepts `"soft": true`: the claim lands only if the role is unheld, held by a session that is no longer live, or held by the declared `previous_session_id`; any other live holder answers 409 with its id.

### Fixed

- A role claim that overtook another session no longer risks deleting a newer claim made in the same instant; old-holder cleanup now touches only the previous holder's topics, never the role row.
- Document version mutations now emit one retained, non-notifying `comment.anchor_refreshed` or `ask.anchor_refreshed` event for each changed open anchor, so stream consumers immediately observe orphaned quotes.
- A webhook redelivery of an event the notification stream already holds no longer publishes a second copy. Webhook envelopes whose dedupe key is the source plus the upstream delivery id (GitHub's `X-GitHub-Delivery`, Slack's `event_id`, Ghost Wispr's `X-GhostWispr-Delivery`) now publish under a JetStream MsgId of that key and topic, the rule Dispatch envelopes already used. Other producers that publish under those source names - CI settlements, the MCP bridge and `/v1/messages/*` callers - still publish every time. GitHub documents that a redelivery carries the original `X-GitHub-Delivery`, and Slack retries under the original `event_id`; that Ghost Wispr resends under its original delivery id is assumed, not documented.
