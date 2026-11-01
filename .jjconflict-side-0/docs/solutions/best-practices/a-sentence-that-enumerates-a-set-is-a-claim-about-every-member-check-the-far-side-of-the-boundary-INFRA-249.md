---
title: "A sentence that enumerates a set is a claim about every member: check the far side of the boundary"
category: best-practices
tags:
  - documentation-drift
  - agent-facing-text
  - review-round
  - cross-service
date: 2026-10-25
status: active
module: packages/envoy
applies_when:
  - You are writing "only on X", "in every case", or a list of what a route answers
  - Your sentence describes one service's statuses and another service's decision
  - You are writing a comment above a test whose condition you did not run on the old code
related_issues:
  - "INFRA-249"
  - "sjawhar/legion#2191"
---

# A sentence that enumerates a set is a claim about every member: check the far side of the boundary

Extends docs/solutions/best-practices/a-documentation-sentence-is-an-assertion-evaluate-its-quantifier-its-derivation-and-its-neighbours-INFRA-212.md.

That note has you evaluate a sentence's quantifier, its derivation and its neighbours. This is the
delta for a sentence that spans a service boundary.

## Rules

- **"Only on X" is a claim about every member of the set, including the ones you did not look at.**
  Enumerate from the code that *decides*, not from the code that *answers*: a route's three
  statuses are not three decisions until you have followed what the caller does to each one.
- **A sentence about a boundary is checked on both sides.** One side's status map and the other
  side's classification of it are two different facts, and the second is where the collapse
  happens.
- **A comment that says what the old code did is an assertion too.** If you did not run the old
  code in that state, write what you ran instead, or say that you did not.
- **Weigh the fix by what loads the file.** An `AGENTS.md` sentence is loaded into every session
  that works the package and is acted on as instruction, so it is worth a round. A `CHANGELOG.md`
  entry is a dated record whose reader is a person looking back, so it is worth the next change
  that touches it — and worth saying so where the next reader will meet it, rather than leaving
  the false sentence as the only description.

## Evidence

Five sentences of this class on one branch, none caught by any check and all by a reader:

1. A comment at `internal/dispatch/api/comment_delivery.go` said an unanswered read "keeps the
   pin". It does not: `resolveDeliveryTarget` returns an empty session id for every error but
   `ErrNotFound`, so no pin is set there at all. Found at review; fixed.
2. `packages/envoy/AGENTS.md` said a read that did not answer "refuses the request `503`" for all
   three decisions. True for a broadcast and a claim; a mention's comment is still created `201`
   with its delivery recorded `failed`. Found at review; fixed.
3. A type carried a one-entry `map[string]envoy.Session` keyed by the holder's own id after the
   read became a read of that one holder — list vocabulary outliving the list. Found by the
   simplify pass; fixed.
4. `packages/envoy/CHANGELOG.md` says the claim "is free only on the bucket's 404; anything else
   is a 503". `(*Client).Session` maps `400` and `413` to `ErrNotFound` as well as `404`
   (`internal/dispatch/envoy/client.go`), `fetchHolderLiveness` then answers `fetched: true` with
   no holder, and `claimBlockedBy` returns nil — so a claim is also free on an id NATS refuses as
   a key. The behaviour is right and deliberate, since no session can register under such a key;
   the sentence enumerated the listener's statuses without following the client's map of them.
   Found at retro, by a fresh reader, after review had already fixed two of its siblings. Left to
   the next change that touches that entry, recorded on the pull request instead
   (sjawhar/legion#2191).
5. `packages/envoy/AGENTS.md` calls `GET /v1/registry/<session_id>` the authoritative per-session
   answer without saying it is authoritative for **absence** rather than for presence:
   `roleHolderSession` answers `200` from the cache when the cache holds the entry and reads the
   bucket only on a miss, so a `200` can be stale by a watcher tick. Found at retro; left for the
   next change in that file, which should add the qualification rather than rediscover it.

Instance 4 is the one worth remembering: its first three words were right, its fourth was a
quantifier, and nothing on the listener's side of the boundary could have shown it.

## Siblings

- `docs/solutions/architecture-patterns/derive-a-refusals-condition-from-what-the-caller-depends-on-not-from-the-mechanism-that-broke-it-INFRA-249.md`
  — the same collapse in a condition rather than in prose.
- `docs/solutions/testing/a-check-is-proven-by-the-control-that-makes-it-fail-not-by-its-own-green-INFRA-250.md`
  (dispatch://INFRA-250, written at that issue's own retro) — the same question asked of a check's
  failure set.
