---
title: "Moving a tailnet name from a node to a Tailscale Service needs a client grant to the Service — the hosts' auto-approval gives no access"
category: infra
tags:
  - tailscale
  - tailscale-services
  - grants
  - migration
date: 2026-10-27
status: active
module: packages/envoy/infra/gcp
related_issues:
  - "INFRA-560"
  - "sjawhar/legion#2368"
---

# Moving a tailnet name from a node to a Tailscale Service needs a client grant to the Service

- A Tailscale Service is a grant destination of its own
  ([Tailscale Services](https://tailscale.com/kb/1552/tailscale-services), "Configure Service
  access"). Grants on the hosts' tags cover the hosts, not the Service's address. When a name a
  tagged node used to hold becomes a Service, every client that reached it through a grant on that
  tag loses access after the move.
- Before the old machine goes, give clients access in the tailnet policy: a grant with
  `dst: ["svc:<name>"]` and the ports, or a tag on the Service that their grants already name as a
  destination.
- The `autoApprovers.services` entry only lets the hosts' tags host the Service. It is a separate
  line of the policy and gives no client access.
- Declare the Service with every port it serves (`tcp:443` for HTTPS Serve). Tailscale refuses a
  Service whose name a machine still holds, so the old machine is deleted before the Service is
  declared, and the move is an outage between the two.

## Evidence

- In sjawhar/legion#2368 the first move runbook (`packages/envoy/infra/README.md`, "Moving from a
  single front-door node") declared the Service with an auto-approval and nothing else. Review
  round 1 found that clients would be cut off. The fix (`c40c8634`) put the client grant and the
  `tcp:443` port into the runbook's first step and the CHANGELOG's Breaking entry.
