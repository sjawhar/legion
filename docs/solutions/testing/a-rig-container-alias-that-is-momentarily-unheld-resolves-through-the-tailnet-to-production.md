---
title: "A rig's container alias that is momentarily unheld resolves through the tailnet to production: give it a name nothing else knows, an unroutable resolver, and a per-connection guard"
category: testing
tags:
  - docker
  - dns
  - tailscale
  - magicdns
  - nats
  - envoy
  - rehearsal
  - test-rig
  - production-safety
date: 2026-09-25
status: active
module: packages/envoy
related_issues:
  - "LEGION-279"
  - "sjawhar/legion#1311"
---

# A rig's container alias that is momentarily unheld resolves through the tailnet to production

A rig that restarts a server by replacing its container, and has clients reach it by a Docker
network alias, will sometimes point those clients at whatever else answers to that name. On a
devbox joined to the tailnet, that can be production.

## Mechanism

On a user-defined Docker network, the embedded DNS server (`127.0.0.11`) answers container names
and network aliases itself. A name no container on the network holds is forwarded to the host's
resolvers. On this devbox those include Tailscale's MagicDNS, whose search domain
(`tailb86685.ts.net`) turns a bare name into a tailnet host name. So `envoy-nats` resolves to the
rig's container while one holds the alias, and to `envoy-nats.tailb86685.ts.net`, production's
Envoy NATS (`100.127.163.46`), while none does.

A restart rehearsal creates that window by design. The old NATS container stops, and for the few
seconds before the new one starts nothing holds the alias. A NATS client reconnecting in that gap
re-resolves the name and connects to production. Production NATS accepts anonymous clients
(LEGION-279), so nothing refuses it.

## The incident (2026-09-25)

A #1311 pin rehearsal named its NATS container's alias `envoy-nats`. In the 15 s stop-to-start gap
the rig's listener reconnected to production NATS for about a minute. It created a durable consumer
there (`listener-probe-listener`, then deleted), consumed 961 notifications without delivering any,
and took one live `notifications.role.cos` message out of the role queue group, which was lost.

## Fix: all four parts, before any run

1. **An alias nothing else resolves.** Name the rig's containers and aliases after the rig
   (`l208r-bus-<label>`), never after a service (`envoy-nats`, `postgres`, `dispatch`). Any name a
   production host carries is a name MagicDNS can complete.
2. **An unroutable resolver on every rig container.** Pass `--dns 192.0.2.1` (TEST-NET-1, which
   routes nowhere) to each `docker run`. Docker's embedded DNS still answers the network's own
   names, and a name it would forward now times out instead of reaching the host's resolvers.
3. **A pre-flight check from inside the rig**, while no container holds the alias. Both the rig's
   alias and the production name must fail to resolve, or the run stops:

   ```bash
   for name in "$alias" envoy-nats; do
     if docker run --rm --network "$net" --dns 192.0.2.1 natsio/nats-box:latest nslookup "$name" >/dev/null 2>&1; then
       echo "pre-flight FAILED: $name resolves inside the rig" >&2; exit 1
     fi
   done
   ```

4. **A per-connection guard for the whole run.** The pre-flight proves the start, and the
   fall-through happens mid-run, in the restart gap. Every half second, read the client container's
   live TCP peers on the server's port from `/proc/net/tcp` (and `tcp6`). Kill the client at once if
   any peer is outside the rig's subnet, and fail the run. Give the rig network an explicit subnet
   (`docker network create --subnet ...`) so "the rig's own container" is a checkable fact. Also fail
   the run if the client ever logs a server URL that is not the rig's alias.

   ```bash
   # port 4222 is 0x107E; /proc/net/tcp holds addresses as little-endian hex
   for h in $(docker exec "$client" cat /proc/net/tcp | awk 'NR>1 {split($3,a,":"); if (a[2]=="107E") print a[1]}' | sort -u); do
     ip=$(printf '%d.%d.%d.%d' "0x${h:6:2}" "0x${h:4:2}" "0x${h:2:2}" "0x${h:0:2}")
     case "$ip" in "$rig_prefix".*) ;; *) docker kill "$client"; echo "foreign peer $ip"; exit 1 ;; esac
   done
   ```

Distinct subnets per rig also stop two rigs on one box from sharing addresses. Pick a range the
box's other Docker networks do not already use, since `docker network create` refuses an
overlapping pool.

## Checking a rig

Before trusting a restart rig on a tailnet-joined machine:
- `getent hosts <every name the rig's clients dial>` on the host must come back empty;
- during the run, the guard's peer list must show only rig addresses;
- afterwards, list what the run could have touched in production (consumers, KV keys, stream
  subjects) and confirm none of it is the rig's.
