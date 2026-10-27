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

**Rule of thumb**: default the replica count to 1, which works on a single server, and raise it
per deployment by configuration. `ENVOY_NATS_REPLICAS` (1 to 5) is one setting that every Envoy
program of a deployment reads with the same value: the listener passes it (`cfg.NATSReplicas`) to
the stream and to each store through its functional option (`store.WithReplicas`,
`session.WithSessionReplicas`, `cistore.WithReplicas`), and Dispatch's server reads it from its
settings table for the stream and `envoy_webhook_redelivery` (`redeliver.OpenState`).

A count is a floor, never lowered by a start. A start opens every bucket through
`bus.EnsureKeyValue`, which creates a missing bucket at the count and raises an existing one with
fewer copies through an update of its stream, `KV_<bucket>`, that writes back the deployed
configuration with only `Replicas` changed. A bucket with as many copies or more is left alone, so
a program whose setting is lower (an older setting, or one restarted before the others) never
scales a raised bucket back; lowering is an operator step
(`nats stream edit KV_<bucket> --replicas=<n>`). `ENVOY_NOTIFICATIONS` holds the same floor
(`ensureStreamWithConfig`).

The raise is a stream update, so it needs its own permission: each Envoy NATS user's grant holds
`$JS.API.STREAM.UPDATE.KV_<bucket>` beside the info and create of each bucket it opens
(`bucketPermissions`, `packages/envoy/infra/core/permissions.go`). A hub whose grant lacks it
refuses the raise, and the start stops with an error naming `ENVOY_NATS_REPLICAS=<n>`. A single
server refuses a replica count above 1 at create and at raise alike (nats-server v2.15.1), and the
start stops naming the setting the same way: more than 1 needs a clustered NATS.

## When to Apply

Any time you add a JetStream stream or KV bucket that the code creates itself. The deployment
topology varies (a deployment may run one standalone server, or a cluster that wants more
replicas), so the replica count is configuration, never a constant, and the code that creates the
store also raises it to that count and never lowers it.
