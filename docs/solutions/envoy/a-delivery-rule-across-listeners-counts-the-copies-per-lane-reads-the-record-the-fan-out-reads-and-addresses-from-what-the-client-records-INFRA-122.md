---
title: "A delivery rule across listeners counts the copies per lane, reads the record the fan-out reads, and addresses from what the client records"
category: envoy
tags:
  - listener
  - serving-port
  - ownership
  - durable
  - host-bridge
  - delivery
date: 2026-10-26
status: active
module: packages/envoy
related_issues:
  - "INFRA-122"
  - "sjawhar/legion#2199"
  - "INFRA-429"
---

# A delivery rule across listeners counts the copies per lane, reads the record the fan-out reads, and addresses from what the client records

Extends docs/solutions/envoy/an-ad-hoc-topics-transport-and-reach-are-judged-from-the-binarys-compiled-stream-subjects-INFRA-117.md.

That note's point 2 ("a core-NATS topic reaches only a session that holds its own NATS
connection") no longer holds for every listener: since INFRA-122, a serving-port session gets a
topic the stream does not store through the listener that owns it, when that listener runs with
`ENVOY_SUBSCRIBE_ALL_TOPICS=1`. The shared hub's listener does not. Point 2 still holds under any
other listener.

Before you write or change any rule for who delivers to a serving-port session, or at what
address:

1. **Count the copies, lane by lane.** Write one row per lane: the stored durable
   (`listener-<machine>`, one per machine, `bus.ListenerDurable`), the inbox on
   `notifications.agent.<id>` (on that same durable), the role queue group, and the live queue
   group (`envoy-listener-live-<machine>`). In each row say how many listeners hold a copy, which
   one acts on it, and what each of the others does with its copy: deliver, report, NAK or ack.
   "Any listener that can reach the session delivers" gives one prompt per machine on every lane
   that is per machine. A non-owner that returns "not delivered" turns into a `no_holder`
   exception and a NAK, once per non-owner. It needs an outcome of its own
   (`HandleAgentResult.OwnedElsewhere`) that acks silently.
2. **Every decision about a session reads the record the fan-out reads.** Two records name a
   listener. `interest.MachineID` is fixed at the first subscribe (`store`'s `mergeForUpsert`
   keeps the current item), and `Registry.Match(own machine id, topic)` fans stored topics out on
   it. `SessionEntry.MachineID` is the last listener the session registered through. An owner
   rule that reads the entry splits a re-registered session in two: the listener that matches its
   topics refuses it, and the one the entry names matches nothing. The owner is the interest's
   machine id, else the entry's (`session.Deliverer.DeliverWithResultForEntry`). A re-registration
   does not move the interest; dispatch://INFRA-429 does.
3. **Derive the address from where the session runs, using what the real client sends.** The
   inputs are the owner, the registrant, the recorded `host`, and the owner's machine id and
   bridge. Write their cross-product as a cell matrix in the first plan, and give each cell a test.
   Fill `host` from the client's code, not from the server's story: envoy-client sends
   `machineID()`, its `ENVOY_MACHINE_ID` or else the kernel hostname
   (`packages/envoy-client/src/machine.ts`). A session that re-registered through another listener
   has not necessarily moved machine. When it records the owner's own machine id, it runs beside
   the owner and is reached through the owner's bridge (`hostFor`'s `stayedLocal`).

Known limit of rule 3: the stayed-local test compares the recorded host with the owner's
`ENVOY_MACHINE_ID` (letter case ignored). A plugin with no `ENVOY_MACHINE_ID`, on a host whose
kernel hostname differs from the listener's machine id (a full domain name against a short name),
is not recognised as local, and its owner dials the hostname instead of the bridge.

## Evidence

sjawhar/legion#2199 took five implementation rounds and four review rounds. Each defect after the
first was one row or one cell that the three rules above would have shown at the start:

- **Copies per lane.** The approved spec let any listener that could reach a session deliver to
  it. The tester measured two prompts per inbox message and committed the red test
  `TestAnInboxMessageReachesAServingPortOnceWithAnotherMachinesListenerOnTheBus`. The fix
  (15d70d42, owner-only delivery) departs from the spec. Review round 2 then found that each
  non-owner published a `no_holder` exception for a message its owner had delivered. Fixed in
  095cb522 with `OwnedElsewhere`.
- **The record the fan-out reads.** 15d70d42 took the owner from the session entry. Review round 2:
  after a re-registration through B, A matched the topic and got `ErrWrongMachine`, logged ERROR
  and NAKed until the durable gave up, and B matched nothing, so every stored topic was lost.
  Fixed in 095cb522: the owner is the interest's machine id first.
- **The address.** Review round 3: `hostFor` returned `ENVOY_HOST_BRIDGE` whenever one was set,
  and every compose host listener sets `127.0.0.1`, so A dialled its own loopback for a session on
  B's machine. Round 5 (4d0301a9) limited the bridge to sessions registered through the listener.
  The generation-3 plan read `machine.ts` and found the stayed-local case: a session that stayed
  on A records A's name. Round 5's rule dialled that name from host networking instead of the
  bridge. Fixed in 24cd57ef. The six-cell `TestAMovedSessionReceivesEachKindOnceWhateverTheBridges`
  first appeared in generation 3: round 4's `hostFor` fails exactly cells 2 and 4, and round 5's
  fails exactly cell 5.
