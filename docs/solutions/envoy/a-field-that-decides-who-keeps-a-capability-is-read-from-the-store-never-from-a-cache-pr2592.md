---
title: "A field that decides who keeps a capability is read from the store, never from a cache"
category: envoy
tags:
  - nats
  - kv
  - cache
  - session-registry
  - role-claims
  - ownership
date: 2026-10-31
status: active
module: packages/envoy/cmd/listener, packages/envoy/internal/session
related_issues:
  - "sjawhar/legion#2592"
---

# A field that decides who keeps a capability is read from the store, never from a cache

Extends docs/solutions/envoy/a-cache-miss-must-not-release-what-another-listener-wrote.md.

That rule is about **presence**: a cache miss may refuse or retry, and may not destroy state
another listener wrote. This is the same rule for a **field**. When a value on a cached record
decides which process keeps a role, a session or any other capability, the cached copy is not an
enrichment of something you already hold — it is the decision.

- **Read the deciding field through the authority, with no cache first answer.** The existing
  helper here reads the cache and falls back to the bucket; the one that decides ownership asks
  the cache nothing at all (`packages/envoy/cmd/listener/role_holder.go:218-236`,
  `bucketSessionEntry`, used by `roleSupersessionVerdict` at `:286`). A cache that trails by one
  write hands the capability to a client the record no longer names.
- **A read-modify-write-back must read from the authority, whatever it decides.** This is the
  half that generalises furthest. The claim route read the claimant's entry from the cache and
  wrote that same entry back, so a trailing cache both read an empty id — demoting every claim to
  the compatibility case — and overwrote a newer registration
  (`packages/envoy/cmd/listener/api.go:1179`).
- **Keep the fallback asymmetric, and say so.** When the authoritative read does not answer, this
  route proceeds on the cached entry rather than refusing, so the claim is never worse off than it
  was before the read existed; the predecessor's read is the opposite and answers 503, because
  that one can only take something away. "Never the cache" would have been the wrong rule; which
  direction the failure costs is what decides.
- **A conditional delete compares at the revision it deletes, and retries exactly once.** Read
  value and revision together and delete at that revision (`nats.LastRevision`); a lost
  compare-and-swap is most often the deleting instance's **own** heartbeat landing in between, so
  answering "kept" would strand the record and refuse the next claim for no reason
  (`packages/envoy/internal/session/registry.go`, `DeleteIfInstance`).
- **One answer for two causes must be true of both.** The route first gave both kept cases the
  same reason, which is false when the cause is the caller's own write. Two causes, two outcomes
  (`registry.go:487-501`, `EntryKeptForAnotherInstance` and `EntryKeptAfterLostRace`). "No client
  reads the reason today" is not a defence: the next reader is a person in a log.
- **A failed `json.Unmarshal` has already written the fields it decoded. Zero the struct before
  you compare it.** A value whose type error comes after the id field decodes that id, so a
  record this build cannot read would have been compared as another instance's and kept
  (`registry.go:554`). Here the same branch deletes it, because a record no reader can act on
  must not block a deregistration — the opposite of a read served to a user, which fails. Which
  one applies is whether the read is a decision about the caller's own resource.
- **Key a new refusal on a field old clients do not send, and the rollout needs no flag.** An
  empty id on either side matches (`packages/envoy/internal/session/instance.go:16`), so a client
  that predates the field keeps the old rule and is never stranded, and the listener logs that
  takeover with both ids. The residual — both sides old — is a stated risk, not a hidden one.
- **Say whether the new token is an authz boundary or a cooperative guard, in the same breath as
  the vulnerability claim.** This id is cooperative: both routes sit behind the same bearer, and a
  token holder can still hard-claim a role or delete a session without naming an instance. A pull
  request that says "closed the hazard" without that sentence overclaims.
- **When you add a field to a record a merge function can discard whole, decide what the discard
  does to it, and pin that in a test.** The claim merge can keep the incumbent entry entirely, so
  the stored id stays the incumbent's; no pair of real clients reaches it today, and the test is
  the record that it was reasoned about
  (`TestMergeForClaim_RejectedClaimLeavesTheIncumbentsClientInstanceIDStored`).

One sentence to correct elsewhere.
`docs/solutions/envoy/a-live-session-list-is-a-cache-view-decide-one-sessions-absence-by-its-own-record-INFRA-249.md`
prices the trade as "a stale present costs a refused send, a wrong absent costs a session its
work". That holds for delivery and not for ownership: once a field of the cached entry decides who
keeps a capability, a stale **present** costs a live process its role.

## Evidence

sjawhar/legion#2592 (dispatch://INFRA-404). `POST /v1/roles/set` took a role from any session a
soft claim named as its predecessor, "live or not", so a process that resumed a session id another
process still held could take its role; a Claude Code `/clear` handoff deleted that live process's
registry entry first, which made the takeover look legitimate. Each client instance now mints 128
random bits at start and sends them on every registration and deregistration; the listener stores
the id on the session entry and reads it, for both the claimant and the named predecessor, from
the `envoy_sessions` bucket.

Proven by `packages/pi-envoy/scripts/smoke-role-takeover.sh`, which drives the whole rule with
real client processes against a listener built from the branch and fails three of its ten rows
against one built from `main`.
