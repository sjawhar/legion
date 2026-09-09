# Envoy client

Shared HTTP transport, tool contract, and inbound delivery renderer for Envoy hosts. Pi and the
Claude bridge consume these modules rather than maintaining host-specific envelope parsers.

## Inbound delivery

`renderInbound(raw, sessionID, subject?)` accepts an additive Envoy envelope and returns a TOON
block with recognized routing and delivery fields. The renderer includes `to` only for a direct
message to the reader, formats sender identity and reply metadata, renders structured payloads
once as `message`, and marks unknown sources or fields with `unrecognised`.

Malformed input is safe: a non-JSON frame becomes a TOON block naming its topic and an
`unrecognised` parse failure. The renderer never emits raw frame bytes. Dispatch events render as
plain-text issue updates and skip events authored by the reader's own session.

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

`dispatch-http.ts` sends the native Dispatch JSON API with the configured bearer token; it URL-encodes
issue references as one path segment and uploads artifacts as multipart form data. `dispatch-execute.ts`
validates each shared `dispatch_*` schema, derives the caller's session origin and Legion issue
reference, and returns the typed tool-result details that host adapters use for subscriptions.

`resolveDispatchConfig` enables Dispatch only when both its URL and bearer token resolve. Configure
`dispatch.token` with `dispatch.serverUrl` in `envoy.json`, or override them with `DISPATCH_TOKEN` and
`DISPATCH_URL`. Invalid configuration, malformed URLs, and empty tokens leave Dispatch disabled and
name the failing source in the returned `error`.

## Tool contract

`tool-contract.ts` is the single source for host tool names, schemas, and topic guidance. Hosts
must use it directly and should not create aliases or duplicate tool descriptions.
