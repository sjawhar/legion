# Changelog

## [Unreleased]

### Changed

- `dispatch_request_approval` requires `summary`, the proposals in the document's latest version
  the human hasn't already agreed to (LEGION-387). `SPEC_SECTIONS` is removed: `dispatch_issue`'s
  `spec` and `dispatch_doc_edit` point at the dispatch skill's "Writing a spec" instead of listing
  headings.
- `dispatch_request_approval`'s description says the call is refused while the document holds an
  open decision block, even when a human asked for approval (LEGION-387); its `summary`
  description no longer repeats that rule. `dispatch_doc_edit`'s description says a `delete` or
  `retype` that would take an ask block out of the document while its ask is open is refused.

### Added

- Added the plan handoff's two plan checks (LEGION-421): `gapAnalysis` (`findings`, each a
  `finding` with the plan's `answer`, or the failed call's `error`) and `planReview` (`verdict`
  `approved`, `rejected` or `failed`, `rounds`, `remainingIssues` of `{issue, evidence}`, `error`),
  with `PLAN_REVIEW_MAX_ROUNDS` (3) and `PLAN_REVIEW_VERDICTS`. `describePhaseHandoffWriteProblems`
  refuses a plan missing either, a rejection recorded before the last round or without the issues
  it named, an approval with issues standing, and a failure without its error. The record is the
  planner's own report: the write checks its shape, not that the checks ran. Reading stays
  tolerant of a plan committed before the checks, and refuses a record that is there without the
  fields its type requires (a review's `verdict` and `rounds`, each finding's `finding` and
  `answer`, each remaining issue's `issue` and `evidence`).
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
- Added `PROJECT_KEY_PATTERN`, a whole project key as the Dispatch server creates them; `dispatch_search`'s `project` must now be empty or match it (`project must be a project key such as CORE`), where any other value was sent and answered with no results.
- Added `maxHint` to `SchemaApi.string`'s options: text appended to the over-cap message, saying what to send instead.
- Added `LegionGoChildRequest`, the body of the Go daemon's `POST /legion/v1/children/park` and `/rerun` (an architect's `park_child` and `rerun_child`), whose answers are `LegionGoEmptyResponse`.
- Added optional `legionAppLogins` to `LegionGoGitHubTokenResponse` and to `LegionDaemonApi.GitHubToken`'s response: each Legion role App's login keyed by its App role (`{implement, review}`), on `/legion/v1/gh-token`, which `legion threads resolve` keeps out of its bot-thread rule and whose `review` login's `Accepted:` closes a bot's thread (LEGION-208).
- Added `IssueSummaryPage` (`{issues, total, limit, offset}`), the answer of
  `GET /api/v1/issues?limit=&offset=`, and `MAX_ISSUE_PAGE_LIMIT` (250) and
  `DEFAULT_ISSUE_PAGE_LIMIT` (50), generated into Go as `contracts.MaxIssuePageLimit` and
  `contracts.DefaultIssuePageLimit`, so the server's bounds and the `dispatch_issues` tool's `limit`
  are one pair of numbers (LEGION-406).

### Changed

- The `dispatch_issues` description says Dispatch pages the listing and the answer names how many
  issues match, where it said the rows were paged after the server returned the full response.

### Removed

- Removed `HandoffMessage`, `validateHandoffMessage`, and `MESSAGES_DIR_NAME`: the `legion handoff message|messages` commands they served are gone, and nothing else read `.legion/messages/`.
- Removed `legionNoticeSubject`: the Go daemon publishes no notice on an issue's topic any more, and its one caller, `legionControllerNoticeSubject`, now builds the controller topic itself.
