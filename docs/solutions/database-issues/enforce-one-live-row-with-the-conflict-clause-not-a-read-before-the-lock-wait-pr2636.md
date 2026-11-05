---
title: "Enforce one live row with the conflict clause, never with a read taken before the lock wait"
category: database-issues
tags:
  - read-committed
  - upsert
  - partial-index
  - revocation
  - postgres
date: 2026-11-04
status: active
module: packages/envoy/internal/broker/store
related_issues:
  - "sjawhar/legion#2636"
---

# Enforce one live row with the conflict clause, never with a read taken before the lock wait

Under READ COMMITTED a statement that waits on a row lock still sees the snapshot it took
*before* the wait. So "check it is still live, then write" is not an invariant: the check reads
the world as it was before the concurrent writer committed.

- **Put the predicate in the write.** `on conflict (…) do update set … where <still live> returning 1`,
  and read an empty return as the refusal. The conflict decides under the lock; a prior `select`
  decides under a stale snapshot.
- **Make the index the invariant.** A *partial* unique index on the live rows lets the losing
  writer insert a second live row, because the revoked row has left the index. A full unique
  index with the liveness in the conflict clause does not.
- **When the answer needs a second statement, say which statement covers which case.** Deciding
  *which* refusal it is can take a second read — that read may see a newer world, and the comment
  should say that this changes the words and never the verdict.
- **Before trusting a fix here, write the test that holds the other transaction open** across the
  lock wait. A concurrency invariant that no test interleaves is an assertion, not a guarantee.
- **A revoke may reach its rows by more than one path.** Enumerate them before claiming what a
  revoke ends, and before adding anything that lets a row skip one of them.

## Evidence

In sjawhar/legion#2636 a revoke that raced a trade left a revoked box key with a live sign-in. The
revoke set `revoked_at` and held the row lock; the trade's CTE had already read its snapshot and
found no revoked row; the insert waited on the live index entry; the revoke committed; the new row
version left the partial index `person_signins_live … where revoked_at is null`, so the insert
found no conflict and added a second live row, under which the trade then minted a token. The fix
(`556118ac`) made the index full on `(person, thumbprint)`, kept revoked rows inside it, and let
the conflict decide; `store/person_signin.go:60-105` carries the reasoning, and
`TestATradeRacingTheRevokeThatCommitsFirstIsRefused` holds the revoke's transaction open with
`storetest.AwaitLockWaiters`. Removing the `revoked_at is null` guard makes that test report the
racing trade succeeding, which is the original defect.

The same area later produced the mirror error: a review finding asserted that
`RevokePersonSignIn` ends tokens *only* through the credential, and proposed a fix built on that.
It does not — `store.RevokePersonSignInTx` ends every live token whose stored session names that
person and that box key, in a statement with no `credential_id` in it, and a second statement
reaches a trade that commits while the revoke waits. Reading the mechanism before applying the
suggested patch changed the remedy: the parameter was **refused** for a person's subject rather
than ignored, because `agent-secrets login` reads `credential_id` out of the answer, so a sign-in
that declined one would be a sign-in that does nothing. The live run made the mechanism visible
rather than asserted: running the revoke's own `where` clause as a `SELECT` beforehand selected
**2** rows where a person-only predicate would have selected **5**, and the two ended rows kept
their `credential_id` untouched.
