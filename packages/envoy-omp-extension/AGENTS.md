# OMP Envoy Extension

Tracked OMP adapter for Envoy messaging.

## Overview

This package owns Pi-specific tool registration, direct NATS subscriptions, steering delivery,
and optional self-subscription registration. HTTP transport, tool metadata, envelope parsing, and
subject construction come from the Envoy core packages.

## Where to look

| Task | Location | Notes |
| --- | --- | --- |
| OMP extension entry | `extensions/envoy.ts` | Load with OMP's `-e` flag |
| Extension unit tests | `extensions/envoy.test.ts` | Mocked Pi and NATS surface |
| Shared HTTP/tool behavior | `../envoy-client/src/` | Do not duplicate it here |
| Event subjects | `../contracts/src/subject.ts` | Canonical subject construction |

## Critical conventions

- Register schemas through the injected `pi.zod`; OMP rejects schemas built from another Zod instance.
- Keep direct NATS subscription lifecycle and Pi steering delivery adapter-local. Inbound messages deliver as `steer` so they interrupt an in-flight turn; `triggerTurn` still wakes idle sessions.
- Do not alter `~/.omp` from this package. The README documents the developer symlink transition.
