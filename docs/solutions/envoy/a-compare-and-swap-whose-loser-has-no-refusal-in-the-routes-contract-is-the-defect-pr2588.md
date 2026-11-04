---
title: "A compare-and-swap whose loser has no refusal in the route's contract is the defect"
category: envoy
tags:
  - role-claims
  - jetstream-kv
  - api-contract
date: 2026-11-04
status: active
module: packages/envoy/internal/store
related_issues:
  - "sjawhar/legion#2588"
---

# A compare-and-swap whose loser has no refusal in the route's contract is the defect

Extends docs/solutions/envoy/a-field-that-decides-who-keeps-a-capability-is-read-from-the-store-never-from-a-cache-pr2592.md.

Before you add a retry around a lost compare-and-swap, ask what the route answers the loser.

- **Name the refusal the caller gets.** Where the route's contract has one — the caller asked to
  take a role from a holder it had read, or named the revision it sequenced its decision on — the
  compare-and-swap is the point, and its loser is answered with that refusal.
- **Where the contract has no refusal, the write is the wrong shape.** A last-write-wins call has
  no "you lost" answer, so the store's lost-CAS error reaches the caller as a storage failure:
  a 500 the route never promised, for an operation that was never contested in the caller's terms.
  Write it unconditionally and the race is gone, not narrowed.
- **A bounded retry does not rescue it.** One conditional write lands per revision, so N callers
  of one key need N rounds and the unluckiest loses N−1; no constant bound covers that.
- Keep the two in one write, selected by what the call asked for, and write the reasoning beside
  the selection: the next person to see an unconditional write will otherwise restore the
  compare-and-swap.

## Evidence

sjawhar/legion#2588 measured this on Envoy's role bucket. A hard claim on
`POST /v1/roles/set` is last-claim-wins and has no refusal at all in its contract. Written with a
compare-and-swap, two honest concurrent hard claims gave the loser JetStream's wrong-last-sequence
error (10071, matching `nats.ErrKeyExists`), which `storeAnswered` counts as the store's own
answer, so the route replied 500.

The settled write selects on what the call asked for
(`packages/envoy/internal/store/kv.go:1128`): `case !soft && ifClaimRevision == nil` writes with
`Put`, and every other claim keeps `Create`/`Update`. A soft claim keeps the compare-and-swap
because its refusal is the point — it read a holder it was allowed to take the role from, and a
claim that landed since must not be taken silently — and a claim carrying `if_claim_revision`
keeps it, hard or soft, because the revision it requires is weighed a second time by the write.
Both losers are answered: `ErrRoleHeld` for the soft claim, `ErrRoleClaimRevisionMismatch` for the
conditional one. The whole decision, the abandoned bounded retry included, is written above the
selection (`kv.go:1110-1126`).
