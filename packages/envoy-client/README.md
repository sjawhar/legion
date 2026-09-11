# Envoy client

Shared HTTP transport, tool contract, and inbound delivery renderer for Envoy hosts. Pi and the
Claude bridge consume these modules rather than maintaining host-specific envelope parsers.

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
and returns typed tool-result details. Mutations return
`details.topic = notifications.dispatch.issue.<KEY>.>`; host adapters subscribe
only to that returned topic.

`resolveDispatchConfig` enables Dispatch only when both its URL and bearer token
resolve. Set `dispatch.enabled: true`, `dispatch.serverUrl`, and
`dispatch.token` in `envoy.json`, or override the URL and token with
`DISPATCH_URL` and `DISPATCH_TOKEN`. `DISPATCH_TOKEN_FILE` (a path; the trimmed
file contents are the token) wins over both — it is how the Legion daemon hands
the bearer to a pane without putting it on argv. A set `DISPATCH_TOKEN_FILE` that
is unreadable or blank disables Dispatch and names the path in `error`; it never
falls back. With `dispatch.enabled: true` and no `dispatch.serverUrl`, the URL
defaults to `http://localhost:8766`, the Go server's listen address. Invalid
configuration, malformed URLs, and empty tokens leave Dispatch disabled and name
the failing source in `error`.

## Tool contract

`@legion/contracts` `src/dispatch-tools.ts` is the single source for the twelve
native Dispatch tool names, descriptions, schemas, and subscription behavior:
`dispatch_issue`, `dispatch_ask`, `dispatch_edit_ask`, `dispatch_resolve_ask`, `dispatch_comment`,
`dispatch_suggest`, `dispatch_message`, `dispatch_doc_edit`, `dispatch_doc_read`,
`dispatch_artifact`, `dispatch_read`, and `dispatch_search`. Hosts build their schema
from those specifications and do not add aliases or host-specific descriptions.
