# Changelog

## [Unreleased]

### Added

- Added the native Dispatch workspace API, persisted documents and events, retained Dispatch notifications, and daily Postgres backups.
- `POST /v1/roles/set` accepts `"soft": true`: the claim lands only if the role is unheld, held by a session that is no longer live, or held by the declared `previous_session_id`; any other live holder answers 409 with its id.

### Fixed

- A role claim that overtook another session no longer risks deleting a newer claim made in the same instant; old-holder cleanup now touches only the previous holder's topics, never the role row.
- Document version mutations now emit one retained, non-notifying `comment.anchor_refreshed` or `ask.anchor_refreshed` event for each changed open anchor, so stream consumers immediately observe orphaned quotes.
- A webhook redelivery of an event the notification stream already holds no longer publishes a second copy. This covers GitHub and Ghost Wispr resending under the original delivery id, and Slack retrying under the original `event_id`. GitHub, Slack and Ghost Wispr envelopes now publish under a JetStream MsgId of their dedupe key and topic, the rule Dispatch's already used.
