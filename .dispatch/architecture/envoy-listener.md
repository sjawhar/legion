---
title: Envoy listener
parent: envoy
depends_on: [contracts]
paths: [packages/envoy/cmd/listener, packages/envoy/internal/webhook, packages/envoy/internal/routing, packages/envoy/internal/session, packages/envoy/internal/bus, packages/envoy/internal/cistore, packages/envoy/internal/store]
---
Ingests GitHub, Slack and Ghost Wispr webhooks, publishes ordinary notifications through JetStream and role lanes through core NATS, resolves live sessions and delivers by `prompt_async`; tracks PR check settlement.
