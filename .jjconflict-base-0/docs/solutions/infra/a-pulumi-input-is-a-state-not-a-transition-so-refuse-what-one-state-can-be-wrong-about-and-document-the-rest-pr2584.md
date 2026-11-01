---
title: "A Pulumi input is a state, not a transition, so refuse what one state can be wrong about and document the rest"
category: infra
tags:
  - pulumi
  - component
  - validation
  - operator-docs
date: 2026-10-31
status: active
module: packages/envoy/infra
related_issues:
  - "sjawhar/legion#2584"
---

# A Pulumi input is a state, not a transition, so refuse what one state can be wrong about and document the rest

An input that an operator walks through several applies — a staged rotation, a phased cutover —
tempts a resolver into guarding the walk. It cannot: a resolver sees the state it is given and
never the one before it.

- **Refuse exactly what one state can be wrong about**, by name and before any resource registers.
  For an ordered list that becomes a setting a server parses, that is a value naming no resource
  and a repeated entry the server would refuse at boot.
- **Enumerate the hazards you cannot refuse, in the resolver's own doc comment**, each with what it
  costs: the skipped step, the step done too early, the step done too late, the rollback to a
  value whose resource is destroyed. A reader of the code then knows the silence is deliberate.
- **Repeat that split in the operator document as preconditions of the procedure, never as
  guarantees the component makes.** An operator who reads "the component refuses an unsafe
  rotation" will run one.
- **Prove the whole procedure end to end rather than arguing it.** Walk every apply of the
  intended sequence against the real processes, and assert what the last step costs instead of
  pretending it is free.

## Evidence

sjawhar/legion#2584 gave `envoy-infra`'s `gcp:Dispatch` a `signingKeyGenerations []int` input whose
rotation is three applies, `[0]` → `[0, 1]` → `[1, 0]` → `[1]`.

- `newDispatchSigningKeyGenerations` (`packages/envoy/infra/gcp/dispatch.go:1135`) refuses a
  negative generation and a repeated one — the two things one list can be wrong about — and its
  doc comment then names the four it cannot: `[0]` → `[1]` with no add step (every pod refuses to
  start beside a live one, and at one replica, whose strategy is `Recreate`, that signs everyone
  out instead), `[0, 1]` → `[1]`, dropping the old generation sooner than a session cookie's
  30-day life, and a rollback to a generation whose key the provider destroyed.
- `packages/envoy/infra/README.md` repeats that split under the rotation's three applies, and
  `docs/site/src/content/docs/dispatch/running-dispatch.md` points the operator document at it.
- `TestAStagedKeyRotationAcrossThreeAppliesKeepsPeopleSignedIn`
  (`packages/envoy/cmd/dispatch/multiprocess_test.go:1746`) walks all three applies against two
  real `envoy-dispatch` processes on one Postgres, reading every cookie on **both** processes
  after each rolling apply, and asserts 401 for the cookies the last apply does sign out rather
  than claiming the drop is free.
