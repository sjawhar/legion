---
title: "A never-ending HTTP request from a browser page pins Chrome's six connections once its certificate verifier updates"
category: dispatch-web
tags:
  - chrome
  - http2
  - server-sent-events
  - websocket
  - connection-pool
  - live-updates
date: 2026-10-25
status: active
module: packages/dispatch/web/src/api
related_issues:
  - "INFRA-258"
  - "INFRA-292"
  - "https://issues.chromium.org/issues/559364921"
---

# A never-ending HTTP request from a browser page pins Chrome's six connections once its certificate verifier updates

## Symptom

In Chrome, every Dispatch page hung for 46 minutes. A refresh never loaded, a send never arrived,
and the Reconnecting pill showed. The server and the network were healthy the whole time: a fresh
connection from the same machine (curl, another browser) got an answer in about 0.1 s.

## Cause

Chrome marks every open HTTP/2 session "going away" each time its certificate verifier gets new
data: a CRLSet, a Certificate Transparency log list, a root store update, or a network-time
reading (Chromium `services/cert_verifier/cert_verifier_service_factory.cc`,
`UpdateVerifierServices`, then `net/spdy/spdy_session_pool.cc`, `OnSSLConfigChanged`, with
reason `ERR_CERT_VERIFIER_CHANGED`, -716).

A going-away session takes no new requests, but it stays open until every stream on it has ended,
and it sends the server no GOAWAY before then. While it stays open it holds one of the six
connection slots Chrome allows per host.

Dispatch's page held two server-sent-event streams that never end: the event feed and the agent
live view. Each verifier update stranded the session carrying them and opened a new one, so after
enough updates all six slots held going-away sessions. Every new request then waited inside Chrome
with no time limit. Nothing reached the server, so nothing in the server's logs or metrics showed
the hang. The Chromium bug is <https://issues.chromium.org/issues/559364921> (open).

## Diagnosis recipe

1. During the hang, record `chrome://net-export` and open the JSON.
2. `polledData.spdySessionInfo`: the host's entries show `availability_state: GoingAway` and
   `error_on_unavailable: -716`.
3. `polledData.socketPoolInfo`: the host's group shows `active_socket_count: 6` and a large
   `pending_request_count`.
4. The event log holds `SOCKET_POOL_STALLED_MAX_SOCKETS_PER_GROUP`.

`chrome://net-internals/#sockets` → **Flush socket pools** clears the hang at once. That recovers
the tab; it does not fix the cause.

## Fix

Specs: dispatch://INFRA-258 and dispatch://INFRA-292.

- **One WebSocket per tab for live updates.** Chrome pools WebSockets apart from HTTP
  connections, with a limit of 255 per host (`net/socket/client_socket_pool_manager.cc`), so a
  live socket never takes one of the six slots that ordinary requests need. The event feed and the
  agent live view are subscriptions on that one socket. Server:
  `packages/envoy/internal/dispatch/api/live_socket.go`, `live_events.go`, `live_agent.go`.
  Page: `packages/dispatch/web/src/api/live-socket.ts`, `event-feed.ts`. This holds only while the
  front proxy does not advertise HTTP/2 extended CONNECT (`SETTINGS_ENABLE_CONNECT_PROTOCOL`):
  a browser that sees it runs the socket inside an HTTP/2 session, and the hang can return.
  `bun scripts/e2e/lib/h2-settings.ts https://<dispatch>` exits 1 when the proxy advertises it.
- **A random age set by the server for each connection.** Each socket gets an age between
  `contracts.LiveSocketMinAge` and `LiveSocketMaxAge` (30 and 60 minutes) when it opens, then
  closes with 4001 (`LiveSocketMaxAgeClose`). The page opens a new socket at once and resumes every
  subscription from its cursor, and shows nothing to the user. No connection lives forever, even
  if a later browser pools WebSockets like HTTP.
- **Deadlines on ordinary requests.** A GET the page makes is held to `REQUEST_DEADLINE_MS`
  (30 s, `packages/dispatch/web/src/api/client.ts`) of silence rather than to a total: that long
  for its response's headers, then that long again for each piece of its body, the timer starting
  over as every chunk arrives. A request stalled inside the browser, and a body that stops
  mid-download, both fail with `RequestTimeoutError` and are retried instead of hanging the page,
  while a slow body that keeps arriving downloads however long it takes.
- **One keyed path for every write** (dispatch://INFRA-258/ask/4cb71926-38d5-467b-bc7e-6ca1b3305399).
  A request that times out or loses its answer says nothing about whether it ran, so a retry is
  only safe when the server can tell it from a new write. The page and the `dispatch` CLI mint an
  `Idempotency-Key` per intent (a press, a command's write) and retry under it by one rule,
  `retriesUnderKey` in `@legion/contracts`: a transport failure, an attempt silent too long, a
  5xx, a 408, a 429 and a `409 KEY_IN_FLIGHT`. One server layer
  (`packages/envoy/internal/dispatch/api/request_keys.go`, a `request_keys` row per key) runs a
  key's request once, replays its stored answer to a repeat with `Idempotency-Replayed: ?1`,
  refuses the key reused for another request (`422 KEY_REUSED`), keeps no 5xx, and keeps each key
  24 hours. Every transaction a keyed request commits marks its key in that same transaction, so a
  write that landed but lost its answer is never run again: its repeat is `409 KEY_COMMITTED`,
  which the page treats as a landing and reads again, and the `dispatch` command reports as made,
  telling the session to read it back. A write that also reaches outside the
  database (the single-session send, the broadcast, the issue message and a comment, each of which
  delivers to agents) commits a recovery point with what it made, so a repeat resumes what its
  delivery still owes and answers what it made as it stands, even across a crash between its commit
  and its answer. The page keeps each unconfirmed write in `sessionStorage` so a reload resumes it; a kept
  write belongs to the login that pressed it and resumes on its own only within five minutes, so a
  tab reopened the next morning never sends yesterday's change by itself. The two hand-written key
  paths that came before it, one per send, are gone.

## The rule

Do not keep a long-lived HTTP request (server-sent events, a long poll, a streaming fetch) open
from a browser page. The browser can strand the connection that carries it, and that connection
still takes up one of a small set of slots per host. Use a WebSocket for live data. If a
long-lived request must exist, the server must set a limit on its lifetime, so that a stranded
connection ends without help from the user.

A server that is healthy and answers fresh connections tells you nothing about the tab. When only
one browser hangs, look at that browser's connection pool first (`chrome://net-export`).
