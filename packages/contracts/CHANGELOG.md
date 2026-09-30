# Changelog

## [Unreleased]

### Added

- Added the plan handoff's two plan checks (LEGION-421): `gapAnalysis` (`findings`, each a
  `finding` with the plan's `answer`, or the failed call's `error`) and `planReview` (`verdict`
  `approved`, `rejected` or `failed`, `rounds`, `remainingIssues` of `{issue, evidence}`, `error`),
  with `PLAN_REVIEW_MAX_ROUNDS` (3) and `PLAN_REVIEW_VERDICTS`. `describePhaseHandoffWriteProblems`
  refuses a plan written without either, a rejection recorded before the last round or without
  the issues it named, an approval with issues standing, and a failure without its error; reading
  stays tolerant, so a plan committed before the checks still loads.
- `LegionGoControllerSecretResponse` carries `designGate` (`root-issues` or `off`): the Go daemon
  tells `legion controller start` its design gate policy, which the controller's take comment
  reads before it promises a design approval.
- Added `claimHolds(claim, titles)` and its `ClaimHolding` answer (`holds`, `lapsed`, `unknown`):
  whether a claim still holds its issue against the live agent registry. A person's always holds;
  a session's holds while the loaded registry lists it; with no registry it is `unknown`. The
  dashboard's claim chip and the agent tools' claim lines both judge a claim with it.
- Added shared schemas and descriptions for the nine native Dispatch tools.
- Added the `Agent` row type behind Dispatch's `GET /api/v1/agents`.
- Added required `Ask.opened_event_id`, the canonical event ID for an ask's opening turn.
- Added `MAX_BROADCAST_RECIPIENTS`, the most sessions one `POST /api/v1/broadcasts` sends to, generated into Go as `contracts.MaxBroadcastRecipients`.
- Added `SEARCH_QUERY_MAX`, the longest `dispatch_search` query (`GET /api/v1/search`'s `q`), generated into Go as `contracts.SearchQueryMax`; `dispatch_search`'s `query` now refuses a longer one before any request (LEGION-386).
- Added `SEARCH_QUERY_HINT`, the sentence a refusal over `SEARCH_QUERY_MAX` adds, generated into Go as `contracts.SearchQueryHint`.
- Added `maxHint` to `SchemaApi.string`'s options: text appended to the over-cap message, saying what to send instead.
- Added `LegionGoChildRequest`, the body of the Go daemon's `POST /legion/v1/children/park` and `/rerun` (an architect's `park_child` and `rerun_child`), whose answers are `LegionGoEmptyResponse`.
- Added optional `legionAppLogins` to `LegionGoGitHubTokenResponse` and to `LegionDaemonApi.GitHubToken`'s response: each Legion role App's login keyed by its App role (`{implement, review}`), on `/legion/v1/gh-token`, which `legion threads resolve` keeps out of its bot-thread rule and whose `review` login's `Accepted:` closes a bot's thread (LEGION-208).

### Removed

- Removed `HandoffMessage`, `validateHandoffMessage`, and `MESSAGES_DIR_NAME`: the `legion handoff message|messages` commands they served are gone, and nothing else read `.legion/messages/`.
- Removed `legionNoticeSubject`: the Go daemon publishes no notice on an issue's topic any more, and its one caller, `legionControllerNoticeSubject`, now builds the controller topic itself.
