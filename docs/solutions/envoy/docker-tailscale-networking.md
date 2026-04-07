---
title: "Docker bridge networking cannot resolve Tailscale MagicDNS hostnames"
category: envoy
tags:
  - docker
  - tailscale
  - nats
  - networking
  - bridge-networking
  - magicdns
  - pulumi
date: 2026-04-04
status: active
module: envoy
related_issues:
  - "204"
symptoms:
  - "wget: bad address 'hostname:port'"
  - "DNS_RESOLVE_FAILED inside docker container"
  - "NATS cluster routes failing to connect between peers"
  - "containers cannot reach other Tailscale hosts by name"
---

# Docker Bridge Networking Cannot Resolve Tailscale MagicDNS Hostnames

## The Problem

Containers running in Docker's default **bridge networking** mode use Docker's built-in DNS
resolver, not the host's DNS stack. Tailscale MagicDNS hostnames (e.g., `sami-agents-mx`)
are only resolvable from the host's network namespace — bridge-networked containers can't
see them.

This caused a P0 bug where NATS cluster routes used MagicDNS hostnames (`nats://sami-agents-mx:6222`).
The NATS containers couldn't resolve peer hostnames, so the cluster never formed.

## The Two Address Spaces

In a Tailscale + Docker environment, there are two distinct address spaces:

| Context | DNS Available | Use |
|---------|---------------|-----|
| Host-level (SSH, `network_mode: host`) | MagicDNS hostnames work | Docker provider SSH transport, host-networked services |
| Bridge-networked containers | Docker DNS only — **no MagicDNS** | Must use Tailscale IPs (100.x.x.x) or switch to host networking |

## The Pattern

```typescript
interface MachineConfig {
  sshHost: string;      // "ssh://user@hostname" — MagicDNS OK (host-level SSH)
  name: string;         // MagicDNS hostname — used for host-networked inter-machine addressing
}
```

- **NATS cluster routes** (port 6222, host networking): use `machine.name` (MagicDNS hostname)
- **NATS client URLs** (port 4222): local peer gets `127.0.0.1`, remote peers get `machine.name`
- **Docker provider SSH transport**: use MagicDNS hostname in `sshHost` (runs on host, not in container)
- **Host-networked containers** (NATS, listener, receivers with `network_mode: host`): use MagicDNS hostnames directly

## The Rule

Any container using bridge networking that needs to reach another machine over Tailscale
**must** use the Tailscale IP, not the hostname. Host-networked containers (including NATS
since April 2026) can use MagicDNS hostnames directly since they inherit the host's DNS.

## Verification

Test DNS resolution from inside a bridge-networked container:

```bash
# This FAILS in bridge networking:
docker exec nats-container wget -q -O- http://hostname:8222/healthz
# wget: bad address 'hostname:8222'

# This WORKS:
docker exec nats-container wget -q -O- http://100.64.0.1:8222/healthz
# OK
```

## Related

- NATS `nats:2.11-alpine` has `wget` (BusyBox) but NOT `curl` — healthchecks must use `wget -q --spider`
- Listener/receiver containers (debian:bookworm-slim) have `curl` installed — healthchecks use `curl -f`

## Update: NATS Host Networking (April 2026)

NATS containers were switched from Docker bridge networking to host networking
(`network_mode: host`) in issue #310. This sidesteps the bridge DNS limitation:

- NATS can now resolve MagicDNS peer hostnames directly
- Hardcoded Tailscale IPs (`tailscaleIp` in MachineConfig) were removed
- Cluster routes and client URLs use `machine.name` (= MagicDNS hostname)
- The bridge networking rule above still applies to any future bridge-networked containers
- Host networking removes container network namespace isolation; NATS ports (4222, 6222, 8222)
  are exposed directly on host interfaces, protected by Tailscale's network boundary
