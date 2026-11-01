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

A count is a floor that a start raises to and never writes below what it read. A start opens
every bucket through `bus.EnsureKeyValue`, which creates a missing bucket at the count and raises
an existing one with fewer copies through an update of its stream, `KV_<bucket>`, that writes back
the deployed configuration with only `Replicas` changed. A bucket with as many copies or more is
left alone, so a program whose setting is lower (an older setting, or one restarted before the
others) leaves a raised bucket it reads as it is. JetStream's stream update has no
compare-and-swap, though: a start with a lower setting that reads the bucket before another
start's raise and writes after it puts the old copies back, and the same holds for
`ENVOY_NOTIFICATIONS` (`ensureStreamWithConfig`). The floor therefore holds only when every
program of a deployment carries the same `ENVOY_NATS_REPLICAS`, so a clustered deployment sets it
in every envoy-listener and in the Dispatch server.

Each program that owns the stream (envoy-listener, the Dispatch server) runs a keeper
(`internal/bus/keeper.go`) that every 30 seconds raises the buckets it opens
(`bus.WithKeptBuckets`: the listener's interests, roles, sessions and CI state buckets,
Dispatch's webhook redelivery bucket) back to its `ENVOY_NATS_REPLICAS`, logging
`envoy nats keeper raised a bucket` at WARN with `replicas="1 -> 3"`, and puts the stream's
copies back the same way. A failed pass logs `envoy nats keeper could not raise a bucket` and
the next period tries again; it never stops the program. With mixed settings during a rollout a
bucket can sit below the setting for up to one period after the last start. Lowering is an
operator step that needs every program's setting lowered first, or the keeper raises it back
(`nats stream edit KV_<bucket> --replicas=<n>`).

The raise is a stream update, so it needs its own permission: each Envoy NATS user's grant holds
`$JS.API.STREAM.UPDATE.KV_<bucket>` beside the info and create of each bucket it opens
(`bucketPermissions`, `packages/envoy/infra/core/permissions.go`). A hub whose grant lacks it
answers the raise with nothing, so it times out after 10 seconds, and the start stops with an
error naming the bucket and the setting:
`raise KV_envoy_interests: ENVOY_NATS_REPLICAS=3: context deadline exceeded`. A single server
refuses a replica count above 1 at create and at raise alike (nats-server v2.15.1), and the start
stops naming the setting the same way: more than 1 needs a clustered NATS. A cluster with one of
three servers stopped behaves differently by version: nats-server v2.15.1 creates a new stream at
3 copies, assigning one offline peer, and the start succeeds, where v2.10.29 refuses that create.

## When to Apply

Any time you add a JetStream stream or KV bucket that the code creates itself. The deployment
topology varies (a deployment may run one standalone server, or a cluster that wants more
replicas), so the replica count is configuration, never a constant, and the code that creates the
store also raises it to that count, at start and periodically after, since a start alone cannot
hold it against a concurrent start with another count.
