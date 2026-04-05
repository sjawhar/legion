---
title: "Cross-Machine Delivery: Sentinel Errors as Routing Signals"
category: envoy
tags:
  - envoy
  - go
  - nats
  - cross-machine
  - sentinel-error
  - delivery
  - machine-id
date: 2026-04-05
status: active
module: envoy
related_issues:
  - "sjawhar-legion-229"
symptoms:
  - "3 of 4 listeners NAK every agent-targeted message"
  - "MaxDeliver budget exhausted on wrong-machine retries"
  - "stale file-registry entry causes delivery attempt to wrong machine"
  - "ErrWrongMachine returned from Deliver()"
---

# Cross-Machine Delivery: Sentinel Errors as Routing Signals

When multiple machines run Envoy listeners with separate durable consumers (needed for broadcast),
each listener sees every message. The agent path (`HandleAgentMessage`) already filters by
`interest.MachineID == machineID`, but the fallback paths in `Deliverer.Deliver()` did not —
causing wrong-machine listeners to NAK instead of ACK, wasting `MaxDeliver` budget.

## The Pattern: Sentinel Error for Routing Decisions

`ErrWrongMachine` is not a failure — it's a routing signal. `Deliver()` returns it when the
KV session registry says the session belongs to a different machine. Callers map it to ACK
(skip) rather than NAK (retry).

```go
// In Deliver(): KV check short-circuits file fallback
if entry, err := d.Sessions.Get(interest.SessionID); err == nil {
    if d.MachineID != "" && entry.MachineID != "" && entry.MachineID != d.MachineID {
        return ErrWrongMachine  // caller will ACK, not NAK
    }
}

// In HandleAgentMessage(): both interest and fallback paths handle it
if errors.Is(err, ErrWrongMachine) {
    return HandleAgentResult{Delivered: false}  // ACK without delivery
}
```

**Why this works better than alternatives:**
- A boolean return would conflate "wrong machine" with "session not found"
- A "check before deliver" method would introduce a TOCTOU race
- The sentinel keeps `Deliver()` agnostic about ACK/NAK semantics while giving callers
  enough information to make the right decision

## Critical: KV Check Must Short-Circuit File Fallback

The KV session registry is the authoritative source for machine ownership. When KV says
"session is on machine-B", the file registry fallback must NOT execute — stale file entries
are the whole problem. The ordering is:

1. KV check → wrong machine → `ErrWrongMachine` (return immediately)
2. KV check → right machine, has port → deliver
3. KV check → right machine, no port → fall through to file registry
4. KV miss → fall through to file registry (backward compat)

Moving the machine check below the file fallback would defeat the entire fix.

## Three-Way Guard for Backward Compatibility

```go
d.MachineID != "" && entry.MachineID != "" && entry.MachineID != d.MachineID
```

Both sides must be non-empty for the check to fire. This handles:
- Old deployments where `MachineID` isn't configured on the listener
- KV entries created before `MachineID` was added to `SessionEntry`
- Single-machine setups where `MachineID` is empty

**Known limitation:** Sessions with empty `MachineID` in KV fall through to file fallback
regardless of which machine they're on. This is intentional for migration safety but means
the filter is only active once all KV entries have `MachineID` populated.

## Broadcast Path Was Already Correct

`registry.Match(machineID, topic)` filters interests by `machine_id` before returning results.
The bug was only in the agent fallback paths:
1. `Deliver()` KV lookup didn't check `entry.MachineID`
2. `HandleAgentMessage` file registry fallback had no machine check at all

Knowing which layer already filters correctly saved significant scope.

## Testing Cross-Machine Scenarios

**Integration test environment gotcha:** `setupTestEnv` creates a `Deliverer` with a default
`MachineID`. Cross-machine tests must override `env.deliverer.MachineID` before calling
`startConsumer` to simulate a different machine identity. Forgetting this causes confusing
failures where "local" sessions appear filtered out.

**Proving the short-circuit:** `TestDeliver_WrongMachineSkipsFileFallback` registers a valid
file entry with a live mock server, then proves it was never contacted — this is the strongest
test for the short-circuit behavior.

## HandleAgentResult Has Three States

| State | Fields | Meaning |
|-------|--------|---------|
| Delivered | `Delivered: true` | Success — record in dedupe |
| Retry | `ShouldNAK: true, Err: err` | Delivery failed — NAK for retry |
| Skip | `Delivered: false, ShouldNAK: false` | Not our session — ACK to consume |

The third state (skip) is implicit. `ErrWrongMachine` maps to this state. Future changes
to `HandleAgentMessage` should preserve this three-way distinction.
