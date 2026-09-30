# Changelog

## [Unreleased]

### Changed

- `dispatch_request_approval` requires `summary`, the proposals in the document's latest version
  the human hasn't already agreed to (LEGION-387). `SPEC_SECTIONS` is removed: `dispatch_issue`'s
  `spec` and `dispatch_doc_edit` point at the dispatch skill's "Writing a spec" instead of listing
  headings.

### Added

- Added `claimHolds(claim, titles)` and its `ClaimHolding` answer (`holds`, `lapsed`, `unknown`):
  whether a claim still holds its issue against the live agent registry. A person's always holds;
  a session's holds while the loaded registry lists it; with no registry it is `unknown`. The
  dashboard's claim chip and the agent tools' claim lines both judge a claim with it.
- Added shared schemas and descriptions for the nine native Dispatch tools.
- Added the `Agent` row type behind Dispatch's `GET /api/v1/agents`.
- Added required `Ask.opened_event_id`, the canonical event ID for an ask's opening turn.
- Added `MAX_BROADCAST_RECIPIENTS`, the most sessions one `POST /api/v1/broadcasts` sends to, generated into Go as `contracts.MaxBroadcastRecipients`.
- Added `LegionGoChildRequest`, the body of the Go daemon's `POST /legion/v1/children/park` and `/rerun` (an architect's `park_child` and `rerun_child`), whose answers are `LegionGoEmptyResponse`.
- Added optional `legionAppLogins` to `LegionGoGitHubTokenResponse` and to `LegionDaemonApi.GitHubToken`'s response: each Legion role App's login keyed by its App role (`{implement, review}`), on `/legion/v1/gh-token`, which `legion threads resolve` keeps out of its bot-thread rule and whose `review` login's `Accepted:` closes a bot's thread (LEGION-208).

### Removed

- Removed `HandoffMessage`, `validateHandoffMessage`, and `MESSAGES_DIR_NAME`: the `legion handoff message|messages` commands they served are gone, and nothing else read `.legion/messages/`.
- Removed `legionNoticeSubject`: the Go daemon publishes no notice on an issue's topic any more, and its one caller, `legionControllerNoticeSubject`, now builds the controller topic itself.
