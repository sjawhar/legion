# Envoy client

Shared HTTP transport, tool contract, and inbound delivery renderer for Envoy hosts. pi-envoy and
claude-envoy consume these modules rather than maintaining host-specific envelope parsers.

## Inbound delivery

`renderInbound(raw, sessionID, subject?)` accepts an additive Envoy envelope and returns a TOON
block with recognized routing and delivery fields. The renderer includes `to` only for a direct
message to the reader, formats sender identity and reply metadata, renders structured payloads
once as `message`, and marks unknown sources or fields with `unrecognised`.

Malformed input is safe: a non-JSON frame becomes a TOON block naming its topic and an
`unrecognised` parse failure. The renderer never emits raw frame bytes. Dispatch envelopes
(`source: "dispatch"`) render through the same TOON path as every other source, nesting the
parsed event under `dispatch: { issue_key, type, actor, payload }` with `payload` typed per event
type; a payload whose shape disagrees with its event type's schema is preserved as its raw parsed
value under `dispatch`, never dropped or reduced to prose. A dispatch envelope with no payload at
all falls back to the same `summary` field every other source gets and marks `unrecognised:
payload`. The renderer skips Dispatch events marked `notify: false` on an issue topic before a host
can steer or record them in its inbox, as well as events authored by the reader's own session.

## HTTP transport

`createEnvoyClient` sends and receives the listener's `/v1` JSON contracts. Send and publish
accept optional `inReplyTo`, `supersedes`, `urgency`, `expectsReply`, and `expiresAt` metadata.
Successful direct sends return both the envelope and listener-selected recipient; publishes return
the envelope and an optional role holder. Network failures and 5xx responses retry once after
250 ms. Listener error responses preserve their `error` and optional `expected` fields.

`subscribe` exposes listener warnings, including a topic that has no event in the configured
stream. `getRole` returns a role holder with its last-seen timestamp, and `listSessions` accepts
optional directory and title filters.

## Dispatch native tools

`dispatch-http.ts` sends the native Dispatch JSON API with the configured bearer
token. `dispatch-execute.ts` validates the shared `dispatch_*` schema, derives
the caller's session origin and Legion issue reference, executes the operation,
and returns typed tool-result details. No result carries a subscription topic:
`dispatch_ask` and an ask reply through `dispatch_comment` return
`details.follows = { ask }` (the session follows that ask server-side), and every
write's text names the `envoy_subscribe notifications.dispatch.issue.<KEY>.>`
line an agent passes to subscribe to the whole issue itself.
`dispatch-subscribe.ts` turns `details.follows` into the one-time host notice.
`dispatch-first.ts` reads the `dispatch-first` skill from a plugin's staged `skills/` and wraps it in
`DISPATCH_FIRST_MARKER`, the text pi-envoy and claude-envoy inject into a session with Dispatch.
Successful write responses may include `advice`. The executor preserves that object as
`details.advice` and appends short pointers after the subscription/follow suffix: a primary spec
with no decision blocks, three or more session writes without a human response (with stronger
wording from six), a write while the issue is still in triage, and up to two still-open asks owned
by the session. Ask replies do not trigger the write-cadence pointer and suppress an open-ask
pointer when replying to that ask; a status-setting issue update does not trigger the triage
pointer. Triage advice is shown once per issue for the lifetime of the executor process; absent
`advice` leaves the prior result text and details unchanged.

`resolveDispatchConfig` enables Dispatch only when both its URL and bearer token
resolve. Set `dispatch.enabled: true`, `dispatch.serverUrl`, and
`dispatch.token` in `envoy.json`. A repository `dispatch.serverUrl` requires
the `dispatch.token` declared in that same repository `.opencode/envoy.json`;
it never combines with a user-file or process-environment credential. To
override a repository endpoint for one process, set `DISPATCH_URL` together
with `DISPATCH_TOKEN` or `DISPATCH_TOKEN_FILE`. `DISPATCH_TOKEN_FILE` (a path;
the trimmed file contents are the token) wins over every other token source —
it is how the Legion daemon hands the bearer to a pane without putting it on
argv. A set `DISPATCH_TOKEN_FILE` that is unreadable or blank disables Dispatch
and names the path in `error`; it never falls back. With `dispatch.enabled:
true` and no `dispatch.serverUrl`, the URL defaults to `http://localhost:8766`,
the Go server's listen address. Invalid configuration, malformed URLs, and
empty tokens leave Dispatch disabled and name the failing source in `error`.

## Tool contract

`@legion/contracts` `src/dispatch-tools.ts` is the single source for the twenty-one
native Dispatch tool names, descriptions, schemas, and subscription behavior:
`dispatch_issue`, `dispatch_issue_update`, `dispatch_claim`, `dispatch_ask`, `dispatch_edit_ask`, `dispatch_resolve_ask`,
`dispatch_resolve_comment`, `dispatch_follow`, `dispatch_comment`, `dispatch_suggest`, `dispatch_message`,
`dispatch_doc_edit`, `dispatch_doc_read`, `dispatch_request_approval`, `dispatch_artifact`, `dispatch_read`,
`dispatch_search`, `dispatch_issues`, `dispatch_architecture_sync`, `dispatch_open_asks`, and `dispatch_whoami`. Hosts build their schema
from those specifications and do not add aliases or host-specific descriptions.
`dispatch_issue_update` reads the issue first and sends `PATCH /api/v1/issues/{key}` as the
session actor: `status` (a Legion lifecycle status), `title`, `labels` (replacing the set),
`priority` (`0` for P0 down to `3` for P3, or `null` to clear it), `route`,
and `external_links`, which it merges into the issue's existing links by URL rather than replacing
them; `rank`, the board's own order, is not exposed. Closing an issue (`status: "done"`)
requires `reason`, which it posts as an issue message before the PATCH, since a closed issue
refuses messages; a failed post sends no PATCH, and a PATCH that fails after the post names the
posted message. The result is one line —
`KEY: status a -> b; priority -> P1; linked <url> (N links)`, with `priority cleared` on a clear
and `reason posted as message <id> (<ref>)` ahead of a close — and a server refusal keeps its
`code` (`INVALID_STATUS`, `ISSUE_CLOSED`, `EXTERNAL_LINK_TAKEN`) at the head of the thrown message.
`dispatch_request_approval` refuses, before it sends anything, while the version the request would
name (the document's latest, `approval.latest_version`) holds a decision block open. It reads the
live document's `ask` blocks (`GET /api/v1/artifacts/{id}/blocks`), then that version's markdown
and the owner's asks (the issue's for an issue document, since the artifact route refuses those),
and judges each block by its `state` on every line of the version that opens it, so a line quoting
the opener can add an open block but never hide one: an answer or a resolution closes the ask at
once but reaches a version only when the document settles or the next edit is written. A block the
version does not hold yet, or one with no ask yet, counts as open. A request over an open block
would be retracted by the version its answer writes. The refusal names each block and its ask and
tells the agent to ask the human to answer or waive it. A document already approved at its latest
version skips the reads and gets the server's answer. `dispatch_doc_edit` refuses, before it sends
anything, a `delete` or `retype` by block id that would take an `ask` block out of the document
(the block itself, or one inside a deleted block) while its ask is open: the edit would write its
version at once and settlement would retract the ask without writing another, so the question
would leave the human's Inbox unanswered and the approval refusal would find no block to judge.
