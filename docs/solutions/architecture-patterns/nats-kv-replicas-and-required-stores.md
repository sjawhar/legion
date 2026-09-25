---
title: "NATS KV replicas must fit the peer count, and the listener requires its KV stores"
category: architecture-patterns
tags:
  - nats
  - kv
  - envoy
  - replicas
  - operational
date: 2026-04-04
status: active
module: envoy
related_issues:
  - "206"
symptoms:
  - "no suitable peers for placement"
  - "listener crashes on startup"
  - "KV bucket creation fails with single NATS peer"
---

# NATS KV Replicas Must Fit the Peer Count

## Context

The Envoy listener keeps three JetStream KV buckets: interests (`envoy_interests`, with its role
bucket), sessions (`envoy_sessions`) and CI state. Each store's `Open` creates its bucket when it is
missing, and the listener exits (`log.Fatal`) when any of them cannot open: delivery, role routing
and CI summaries all read these caches, so there is no reduced mode to run in. A store is therefore
never nil in a running listener, and its methods do not guard a nil receiver; a caller that holds
an optional store checks for nil itself.

## Operational Gotcha: Replicas Must Match Peer Count

NATS JetStream KV requires `Replicas <= available_peers`. A bucket created with `Replicas: 3` on a
single-peer deployment fails with "no suitable peers for placement", and because the store is
required, the listener crashes on startup.

**Rule of thumb**: default the replica count to 1, which works everywhere, and raise it by
configuration for HA deployments. The listener passes `ENVOY_NATS_REPLICAS` (`cfg.NATSReplicas`)
to each store through its functional option (`store.WithReplicas`, `session.WithSessionReplicas`,
`cistore.WithReplicas`).

## When to Apply

Any time you add a JetStream stream or KV bucket that the code creates itself. The deployment
topology varies (production runs one standalone server; an HA cluster wants more replicas), so the
replica count is configuration, never a constant.
