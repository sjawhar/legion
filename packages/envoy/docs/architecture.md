# Architecture

## Phase 1

```text
GitHub webhook  --\
Slack events    ---+--> NATS JetStream --> listener --> session wake / cold resume
Agent messages  --/                         |
                                            +--> per-session routing + dedupe
```

## Core pieces

- `packages/contracts`
  - envelope schema
  - env parsing
  - subject helpers
  - signing / verification helpers where envoy owns them
- `cmd/listener`
  - machine-scoped consumer
  - routing and dedupe
  - hot wake vs cold resume
- `cmd/github`
  - webhook verification
  - event normalization
  - NATS publish
- `cmd/slack`
  - request verification
  - challenge handling
  - event normalization
  - NATS publish

## Runtime shape

- one listener container per machine
- one GitHub receiver container on the public EC2 host
- one Slack receiver container on the public EC2 host
- NATS JetStream cluster across all machines via Tailscale mesh
- JetStream stream `ENVOY_NOTIFICATIONS` with 1h retention for replay on listener restart
- JetStream KV bucket `envoy_interests` with 3 replicas for session subscriptions
- JetStream KV bucket `envoy_sessions` with 5m TTL for session port/host data

## Listener API

All `/v1/*` endpoints return 503 until NATS initialization completes.

| Endpoint | Method | Description |
|----------|--------|-------------|
| `/v1/sessions` | GET | Lists all sessions across all machines — joins `envoy_interests` (topics, dir) with `envoy_sessions` (port) |
| `/v1/interests/subscribe` | POST | Subscribe a session to topics |
| `/v1/interests/unsubscribe` | POST | Unsubscribe a session from topics |
| `/v1/interests/` | GET | List all interests |
| `/v1/interests/{session_id}` | GET/DELETE | Get or delete a session's interests |
| `/v1/registry/{session_id}` | GET | Get a session's registry entry (port, machine) |
| `/v1/messages/send` | POST | Send a direct agent-to-agent message |
| `/v1/messages/publish` | POST | Publish an event to a topic |
| `/healthz` | GET | Health check (always available, even during startup) |
