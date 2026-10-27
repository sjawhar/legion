---
title: Envoy
parent: legion
paths: [packages/envoy]
---
The Go-based event transport: webhook ingress, NATS routing and delivery, the native Dispatch server, and the MCP bridge. Its NATS hub comes in two forms, both deployed by `envoy-infra` (`packages/envoy/infra`, a Go module of its own that builds a Pulumi plugin and the `envoy-infra` command, and compiles only `daemonpermissions`, `internal/bus` and `internal/contracts` from Envoy): `gcp:NatsHub`, one nats-server on a Google Cloud VM that joins the tailnet, and `gcp:ClusteredNatsHub`, three nats-server pods on a GKE cluster whose optional Tailscale Service hosts also run the session listener. Both render their NATS users through the one function in `packages/envoy/infra/gcp/natshub.go`, from the permission sets `packages/envoy/infra/core` builds.
