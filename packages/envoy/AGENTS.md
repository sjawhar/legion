# Envoy Package

Go-based cross-machine event transport and delivery subsystem.

## Overview

Envoy owns transport, routing, and delivery:

- ingests Slack/GitHub/agent events
- publishes and consumes via NATS/JetStream
- resolves target OpenCode sessions
- delivers by hot `prompt_async` (messages stay in JetStream for retry if session is unavailable)

It does not own Legion workflow policy. The daemon/controller decides what to do; Envoy moves events to the right session.

## Where to look

| Task                   | Location                                  | Notes                                              |
| ---------------------- | ----------------------------------------- | -------------------------------------------------- |
| Receiver behavior      | `cmd/github/main.go`, `cmd/slack/main.go` | HTTP ingress, signature verification, publish path |
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
- `github` / `slack` receivers must fail loudly and return 503 when publish cannot be confirmed.
- GitHub mention routing is additive: matching comments publish to both `.comment` and `.mention` topics.
- Slack topics must use the real Slack `team_id`, not a workspace slug.
- NATS peer storage uses named Docker volumes, not repo-path bind mounts.

## Operational notes

- Health endpoints should reflect NATS health, not just process liveness.
- If a session is not live in the registry, delivery fails and the message is NAK'd for retry (up to MaxDeliver attempts over the stream's MaxAge window).
- Cross-machine route correctness depends on valid session registry entries with non-null ports.
