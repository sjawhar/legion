---
title: "A condition over shared state is a data-model change, and its worst case is traced to the sink"
category: envoy
tags:
  - shared-mutable-state
  - invariants
  - role-claims
  - review-rounds
  - pi-envoy
date: 2026-10-25
status: active
module: packages/pi-envoy, packages/envoy/cmd/listener
related_issues:
  - "INFRA-246"
  - "INFRA-404"
  - "sjawhar/legion#2164"
---

# A condition over shared state is a data-model change, and its worst case is traced to the sink

When a bug in a loop over shared mutable state looks like a missing condition, the condition is
almost never the fix. Four were written for one loop here, in four consecutive review rounds, and
each was wrong in a different direction. Do these instead.

- **State what the thing is before you write a condition about it.** Name the facts that identify
  one entry — here, a hold is its role topic and the session id it was registered under, and
  nothing else — and make every ledger key on exactly those facts. A map keyed on fewer of them
  silently drops entries that differ in the rest, which is a defect no condition can repair.
- **List every variable your condition reads, then check whether the same loop writes any of
  them.** If it does, you cannot distinguish "settled earlier" from "written by this loop a moment
  ago", and the condition is a data-model problem wearing a boolean's clothes.
- **Prefer deleting the decision to conditioning it.** Removing the parameter that carried the
  wrong value beats guarding its use: a stale capture that is absent from a signature cannot
  return, and a reviewer can confirm it by reading the signature. Where a value must be judged,
  read it at the moment of use (`sessionOpen(id)`) rather than capturing it at the start of the
  operation.
- **Bound a worst case by tracing the value forward to its sink, not by stopping at the write.**
  "What does this entry eventually make an external API do?" is the question. An audit that stops
  at the write reports a bounded redundancy for a value that, four hops later, is a capability.
- **A soft claim's named predecessor is a capability, not a hint.** Envoy's listener once put a
  claimant's named `previous_session_id` into its supersedable set *before* it tested liveness,
  so any session id that reached `previousSessionID` took the role from whoever held it, running
  or gone. A client may name only a session it can show is still its own; a session id it once
  held is not that, because another process can resume it. INFRA-404 closed the hazard: each
  client instance mints a client instance id, sends it on every registration, and the listener
  stores it on the session entry, so a named predecessor gives up the role only when it has no
  live entry, when its entry carries the claimant's own instance id, or when either side carries
  no instance id (`roleSetHandler`, `packages/envoy/cmd/listener/api.go:1195-1225`, applied at
  `SetRoleWithPrevious`, `packages/envoy/internal/store/kv.go:950`).
- **Write the reviewer's trace as a failing test before choosing a fix.** A case written from the
  reported sequence, red before any design exists, is the only thing that can judge a design it
  was not written for. Keep each round's trace as a case: a condition judged only against the
  newest trace breaks an older one.
- **Measure what a test does not prove.** Mutate the product and count which cases go red. Here
  the trace case stayed green when the adoption gate was removed, so a different case is that
  gate's judge — and the pull request said so rather than implying the trace covered it.

## Evidence

`settleEndedHolds`, then in `packages/pi-envoy/extensions/envoy.ts` and now in
`packages/pi-envoy/src/registration-state.ts`. Four conditions over four rounds:
restore every failed hand-back; skip the topic this process holds (which that same loop writes
when it adopts a hold, so "confirmed" and "adopted a moment ago" were indistinguishable); revert
to the first; and delete the branch that names a predecessor from memory. The third let a hold
whose session the process had left be restored, adopted, and named as `previousSessionID`, which
took a role from the live process that had resumed that session. The fourth was measured and
dropped: it failed 75 existing cases, because the same branch carries this process's own hold into
a session it resumes, where the lineage is one and no other branch names a predecessor — a resume
losing its role for up to the listener's five-minute session TTL.

What ended it was three structural edits: both ledgers keyed `<topic>@<session>`; every failed
hand-back abandoned, so the classifying `host` parameter left the signature; and adoption gated on
a freshly read `sessionOpen(hold.id)`, in a loop that reads neither variable it writes. The
re-keying fixed a collision — two holds of one role under two sessions sharing one key, one
silently dropped, after which the pane records a refusal it can never clear — that none of the
four conditions had named and no round had looked for.

Left open by that change and closed since by INFRA-404: the session-moved branch of the plugin's
role claim still records the hold under a session this process has left
(`adoptClaimAfterSessionMoved`, `packages/pi-envoy/src/registration-state.ts:380`), and a later
pass can name that session as a predecessor. What tells such a claim from another process's is
not the `last_seen` stamp that issue's `plan.md` v4 designed but the client instance id above:
the claim lands only while the predecessor's entry is still this instance's, or once it has gone.
The extraction that gave this state one tested home is
`packages/pi-envoy/src/registration-state.ts`, from
dispatch://INFRA-49/ask/c7051816-2e86-4cb6-ad56-02ccb01e9355.
