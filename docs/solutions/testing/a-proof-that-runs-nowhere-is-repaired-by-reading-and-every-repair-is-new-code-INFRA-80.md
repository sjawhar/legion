---
title: "A proof that runs nowhere is repaired by reading, and every repair to it is new code"
category: testing
tags:
  - e2e
  - live-proofs
  - evidence
  - untrusted-input
date: 2026-10-25
status: active
module: scripts/e2e
related_issues:
  - "INFRA-80"
  - "sjawhar/legion#2173"
---

# A proof that runs nowhere is repaired by reading, and every repair to it is new code

A live proof that CI never runs has no failing test to tell you it is broken. A change that alters
what such a proof reads has to find every reader by reading, and each repair enters the change as
new code rather than as a fix to reviewed code.

- **When a change alters what a never-run proof reads, enumerate its readers as a class before
  fixing any one of them**, and publish the search with its command. The instance you were handed
  is rarely the only one: a sweep that found ten call sites found four more while the fix was being
  written, in helpers whose names did not mention the thing being read.
- **Treat each repair as new code and give it the scrutiny new code gets.** A repair is read as a
  fix to something already reviewed, which is exactly the reading that lets a defect through. Here
  the repair of a broken transcript reader shipped a path traversal, and the fix for *that* shipped
  a second one a layer up, in the destination rather than the name.
- **The harness's own unit tests are not independent evidence of the repair.** They were written by
  the same reading that wrote it. Say so where the proof is claimed, rather than letting a passing
  suite stand in for a run.
- **Report a category checked and found clean with its command, not silently.** Half of a sweep's
  value is the part that needed nothing; without the command, a later reader cannot tell "checked"
  from "not looked at".

## Evidence (INFRA-80, sjawhar/legion#2173)

The change made every pod keep its Oh My Pi session as a row in a database rather than as a file on
a disk. `scripts/e2e/stage4b-sandbox-tree.sh` read every agent transcript with `pod_exec … cat` on
the claim's recorded `sessionFile`, which under the new store is a row key and not a path any pod
holds, so the proof could not run at all.

It was repaired three times, each time because the harness was written against the world the change
replaces, and the repairs themselves produced two security findings:

1. The rewritten reader wrote each session at its own database key under the destination. A key is
   a value an agent sets — the API requires only that `ompSessionFile` be non-empty, and a role
   re-registering its live session assigns it — so a key of `<prefix>../../x` placed a host file
   outside the evidence directory. Fixed by deriving the filename from the key rather than using
   it, refusing rather than sanitising, and asserting containment before any write.
2. The derived name could still collide with a directory the collector itself owned, and the
   collector's own `mkdir -p`/`cp -R` were unchecked, so a run could lose every transcript and
   report success. Fixed by giving the pods a directory of their own rather than reserving the
   collector's names — a reserved list is an enumeration over a set that grows.
3. The **destination** itself was steerable: a stem taken from the same agent-settable path
   relocated the directory, after which every guard worked correctly against the wrong directory.
   Fixed by having the function resolve its own root from a keyword and refuse any component that
   is not a plain name, so no caller can supply a path at all.

Each form was invisible from inside the fix for the one before it: the first reasoned about keys,
the second about names against directories, and neither could see a hole one level up in the path.

Across the change, eight readings of that harness found eight defects in it and it ran at no head.
The pull request says so in the sentence a stranger meets, beside the two behaviours that are
provable only on a cluster: *the tree-level proof is repaired and unexecuted, and each repair was
found by reading rather than by running.*
