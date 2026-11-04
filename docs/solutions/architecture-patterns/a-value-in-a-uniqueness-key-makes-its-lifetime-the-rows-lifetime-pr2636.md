---
title: "A value you put in a uniqueness key makes that value's lifetime the row's lifetime"
category: architecture-patterns
tags:
  - upsert
  - conflict-target
  - key-design
  - credentials
date: 2026-11-04
status: active
module: packages/envoy/internal/broker
related_issues:
  - "sjawhar/legion#2636"
---

# A value you put in a uniqueness key makes that value's lifetime the row's lifetime

Adding a field to a natural key, an upsert's conflict target, a cache key or an idempotency key
changes what the row *is*. A key that was "one per machine" silently becomes "one per key", then
"one per process", with no line of the key's own code changing.

- **Before you add the field, enumerate who creates its values, who destroys them, and what
  deletes the rows.** A value made fresh at every start turns one row into one row per start, and
  if nothing deletes them they accumulate wherever they are listed.
- **Ask what happens to the things already pointing at the old key.** Rows keyed by the old value
  do not follow it; whatever held them must either be found by something else, or be stranded.
- **An opt-out that declines part of a write must be checked against every invariant the declined
  part was implied by — per caller type, not globally.** A parameter that is safe for one subject
  can break an invariant for another.
- **Check each consequence against the deployment's real callers**, because "one per process" is
  cheap for a daemon and ruinous for a command invoked per hook.

## Evidence

In sjawhar/legion#2636 the key thumbprint went into `MachineSubject`
(`exchange/exchange.go:218`) so a copied Compute Engine token could no longer move a machine's
launcher credential onto the copier's key; the upsert conflicts on `subject`
(`enroll/enroll.go:252-258`). That fix was right, and it produced the next round's two findings.

A helper that made a fresh key at every start inserted a *new* credential at each restart and
left the old one, stranding every session that had outlived the restart — one trace retrying a
revoke for ever because `enroll.Revoke` answers `ErrNotEnrollingCredential` when the enrolling
credential is not the caller's, another through externally enrolled boxes whose renewer treats
the not-live code as terminal. The chosen fix kept the identity key on disk so the subject is
stable again, on the strength of a recorded human decision about where that key may live rather
than a fresh judgement.

The same keying made every TypeScript trade leave a `launcher_credentials` row and an audit row
that nothing deletes, because that client makes a key per process *and* per session — one for each
CLI call and each hook run — all of them listed as machine logins for an hour. The fix let a trade
decline the credential, and that opt-out in turn let a *person's* trade mint a token carrying no
credential, which one of the revoke's two statements could not reach; it was closed by refusing
the parameter for a person's subject rather than globally.
