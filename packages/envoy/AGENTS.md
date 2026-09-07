# Envoy Package

Go-based cross-machine event transport and delivery subsystem.

## Overview

Envoy owns transport, routing, and delivery:

- ingests Slack/GitHub/Ghost Wispr/agent events
- publishes ordinary notifications through JetStream and role lanes through core NATS
- resolves target OpenCode sessions
- delivers by hot `prompt_async` (ordinary events stay in JetStream for retry if session is unavailable)

It does not own Legion workflow policy. The daemon/controller decides what to do; Envoy moves events to the right session.

## Where to look

| Task                   | Location                                  | Notes                                              |
| ---------------------- | ----------------------------------------- | -------------------------------------------------- |
| Webhook handlers       | `internal/webhook/{github,slack,ghostwispr}.go` | HTTP ingress, signature verification, publish path |
| Webhook config         | `internal/webhook/config.go`                     | ENVOY_WEBHOOKS parsing, startup validation         |
| Listener behavior      | `cmd/listener/main.go`                    | subscribe/match/deliver flow                       |
| NATS client            | `internal/bus/nats.go`                    | reconnect/self-heal logic                          |
| Session delivery       | `internal/session/session.go`             | hot delivery via prompt_async                      |
| Interest storage       | `internal/store/kv.go`                    | JetStream KV subscriptions                         |
| Topic matching         | `internal/routing/match.go`               | wildcard matching                                  |
| Envelope normalization | `internal/contracts/*.go`                 | generated contract + source-specific normalization |
| Deploy/runtime         | `deploy/`                                 | compose, rollout scripts, NATS peer setup          |

## Critical conventions

- `packages/contracts` is the source of truth for event contract shape; regenerate Go output from there.
- Keep Envoy API-level with OpenCode. Do not add DB introspection or OpenCode-specific hidden coupling unless there is no API path.
- Listener message APIs preserve the human `message` as a one-line `payload_summary` of at most 160 characters; when it differs, the complete message is `payload`. A supplied `payload` for `/v1/messages/publish` wins over the derived value.
- Ghost Wispr only publishes `session_started`, `session_ended`, and `summary_ready`; other verified events should return 200, log the skip, and not publish.
- `ENVOY_GHOSTWISPR_SIGNING_SECRET` is optional for trusted Ghost Wispr deployments; when unset, skip signature verification explicitly rather than half-verifying missing headers.
- GitHub mention routing is additive: matching comments publish to both `.comment` and `.mention` topics.
- GitHub issue and issue-comment payloads are content-aware only for dispatch markers: parse them with `internal/dispatch/core` and expose a nonempty `origin.sessionId` as `dispatch_session`.
- Slack topics must use the real Slack `team_id`, not a workspace slug.
- NATS peer storage uses named Docker volumes, not repo-path bind mounts.
- Role lanes use core NATS, not JetStream: the listener queue subscriber resolves the live holder at delivery time, then makes a receipt-backed request to that holder's agent subject. The agent pump returns an empty receipt after accepting the envelope; no receipt within two seconds is `delivery_failed` and emits an exception. Do not add durable role consumers or retry transit for role messages.
- A failed control delivery emits `notifications.envoy.exceptions.<original-topic>`. Its payload preserves `original_topic`, `event_id`, `reason`, `payload_summary`, the original machine `payload`, `dedupe_key`, `source`, and `source_session`; the exception lane is not recursively exceptional. An API publish to an unheld role is rejected synchronously with 404 instead.
- **Source-specific vs generic ingestion**: Envoy has two ingestion paths: listener-hosted webhook handlers behind `readinessGate` (`internal/webhook/{github,slack,ghostwispr}.go`) and the generic MCP bridge (`cmd/mcp/`). The MCP bridge connects to any MCP server that publishes resources, so it's the low-maintenance default for new sources. Building source-specific webhook logic adds maintenance burden — consider whether the cost justifies the benefit over the generic MCP bridge before adding custom source-specific logic to Envoy. When using the MCP bridge, Envoy should stay naive about the message content — the MCP server owns the domain logic.

## Operational notes

- Health endpoints should reflect NATS health, not just process liveness.
- If a session is not live in the registry, delivery fails and the message is NAK'd for retry (up to MaxDeliver attempts over the stream's MaxAge window).
- Cross-machine route correctness depends on valid session registry entries with non-null ports.

## Listener API

| Endpoint | Method | Contract |
| --- | --- | --- |
| `/v1/messages/send` | POST | Sends to a live `target_session`. The response is the envelope plus `recipient` with the full target session ID. |
| `/v1/messages/publish` | POST | Publishes a non-agent topic. A `notifications.role.<role>` topic requires a live holder and returns that session ID in `holder`. |
| `/v1/roles/<role>` | GET | Returns the live role holder and its `last_seen`, or 404 when no holder is live. |
| `/v1/roles/set` | POST | Claims a role for a live session and registers its role topic. |
| `/v1/interests/subscribe` | POST | Persists session topics and route metadata. The response can include `warnings` when a GitHub repository has no retained events. |
| `/v1/interests/unsubscribe` | POST | Removes the supplied topics and returns them in `removed`. |
| `/v1/sessions` | GET | Lists live sessions; optional case-sensitive substring filters are `dir` and `title`. Rows include `roles` and `last_seen`. |
| `/v1/interests/` | GET | Lists persisted interests. |
| `/v1/interests/<session_id>` | GET, DELETE | Gets or removes a session's persisted interests. |
| `/v1/registry/<session_id>` | GET | Gets a live session registry entry. |
| `/v1/sessions/<session_id>` | DELETE | Removes a live session registration. |

`send` and `publish` accept optional `in_reply_to`, `supersedes`, `urgency`,
`expects_reply`, and `expires_at`. `urgency` is `low`, `med`, `high`, or
`blocking`; `expects_reply` is `none`, `optional`, or `required`. Every `/v1`
4xx/5xx response, including the startup readiness gate, is JSON:
`{"error":"<message>","expected":["field"]}`. `expected` appears when the
caller must provide a field.

## Topic shapes

- GitHub repository topics start with `notifications.github.<owner>.<repo>`.
  Pull requests use `pr.<n>` for lifecycle (a closed event carries
  `merged`, `merge_commit_sha`, `merged_by`, and `head_sha`), plus
  `pr.<n>.comment`, `pr.<n>.review`, `pr.<n>.mention`, and
  `pr.<n>.checks` when the head's checks settle. Check settlement is at-least-once: a settlement can
  be followed by a `superseded_settlement` with a higher `generation`; consumers keep the highest
  generation per SHA. `workflow.<file>.<action>` carries only runs without an associated pull request.
- NATS `>` matches one or more trailing tokens, not its base subject. A subscription to a concrete
  `<subject>.>` is registered as the pair `<subject>` and `<subject>.>`, so the recommended
  per-PR default receives lifecycle plus child events.
- Direct agent topics use `notifications.agent.<session_id>`.
- Role topics use `notifications.role.<role>` and are normally published to,
  rather than subscribed to by role holders.
- A failed control delivery publishes
  `notifications.envoy.exceptions.<original-topic>`.
