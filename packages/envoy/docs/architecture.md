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
