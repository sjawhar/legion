# Envoy Package

Go-based cross-machine event transport and delivery subsystem.

## Overview

Envoy owns transport, routing, and delivery:

- ingests Slack/GitHub/Ghost Wispr/agent events
- publishes and consumes via NATS/JetStream
- resolves target OpenCode sessions
- delivers by hot `prompt_async` (messages stay in JetStream for retry if session is unavailable)

It does not own Legion workflow policy. The daemon/controller decides what to do; Envoy moves events to the right session.

## Where to look

| Task                   | Location                                  | Notes                                              |
| ---------------------- | ----------------------------------------- | -------------------------------------------------- |
| Receiver behavior      | `cmd/github/main.go`, `cmd/slack/main.go`, `cmd/ghostwispr/main.go` | HTTP ingress, signature verification, publish path |
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
- `github` / `slack` / `ghostwispr` receivers must fail loudly and return 503 when publish cannot be confirmed.
- Ghost Wispr only publishes `session_started`, `session_ended`, and `summary_ready`; other verified events should return 200, log the skip, and not publish.
- `ENVOY_GHOSTWISPR_SIGNING_SECRET` is optional for trusted Ghost Wispr deployments; when unset, skip signature verification explicitly rather than half-verifying missing headers.
- GitHub mention routing is additive: matching comments publish to both `.comment` and `.mention` topics.
- Slack topics must use the real Slack `team_id`, not a workspace slug.
- NATS peer storage uses named Docker volumes, not repo-path bind mounts.
- **Source-specific vs generic ingestion**: Envoy has two ingestion paths: dedicated webhook receivers (`cmd/github/`, `cmd/slack/`, `cmd/ghostwispr/`) and the generic MCP bridge (`cmd/mcp/`). The MCP bridge connects to any MCP server that publishes resources, so it's the low-maintenance default for new sources. Building a dedicated receiver adds maintenance burden — consider whether the cost justifies the benefit over the generic MCP bridge before adding custom source-specific logic to Envoy. When using the MCP bridge, Envoy should stay naive about the message content — the MCP server owns the domain logic.

## Operational notes

- Health endpoints should reflect NATS health, not just process liveness.
- If a session is not live in the registry, delivery fails and the message is NAK'd for retry (up to MaxDeliver attempts over the stream's MaxAge window).
- Cross-machine route correctness depends on valid session registry entries with non-null ports.
