---
title: "A decomposed incident's seams are the work, and its cause stays unclaimed until something reproduces it"
category: legion
tags:
  - decomposition
  - issue-tree
  - merge-conflicts
  - incident-response
  - review-round
date: 2026-10-25
status: active
module: packages/envoy
applies_when:
  - You are splitting one incident into child issues and the root keeps a change of its own
  - Two children of one tree are about to touch the same files or the same helper
  - A child is recorded as replacing something a sibling built, and you are deciding who deletes it
  - You are about to write that a change fixed an incident whose cause nobody reproduced
related_issues:
  - "INFRA-141"
  - "sjawhar/legion#2167"
---

# A decomposed incident's seams are the work, and its cause stays unclaimed until something reproduces it

Splitting one incident into children is cheap. What costs is the seams: where two children's
changes meet, and where something one child was recorded as owning turns out to be unreachable
from it. Both are findable before the merge, and neither is found by reading the plan.

## Rules

- **Name what the tree does not claim.** A decomposition closes the defects it enumerates, not the
  incident. Write the undetermined cause down where the change is read — in the pull request body
  and the issue — in the same words every time, so a later reader cannot take the sum of the
  children for a reproduction nobody performed.
- **A child can only delete what existed at its branch point.** Before you record that child B
  replaces a thing child A built, check B's base: if A created it after B branched, B's diff
  cannot contain its removal, and the sentence saying otherwise will survive into main unchallenged.
  `gh api repos/<owner>/<repo>/pulls/<n>/files --jq '.[] | "\(.status) \(.filename)"'` settles it in
  one command — a diff with no `removed` entry removes nothing. The deletion then belongs to
  whoever merges last, usually the root, as part of its own merge.
- **Expect the seams to be textual and the breaks to be semantic.** Two children of one spec, built
  from the same vocabulary, will touch the same files and conflict where the text overlaps. That is
  the cheap half: a marked conflict is read once. The expensive half is what merges clean — a
  struct field one side removed and the other still sets, a constructor guard one side added and
  the other does not satisfy, a shell variable both sides declare `readonly`. **After every merge,
  build and run the whole module, not the packages the diff names.**
- **Check a sibling's file list, not its summary, when your work depends on what it did.** A file
  list is a fact about the diff; a packet's prose is a claim about intent. The two disagreed twice
  in this tree, both times in the direction that would have left work undone.

## Evidence

INFRA-141 was *a session-addressed send fails while the listener session cache is cold, though a
topic publish to the same pane succeeds*. The cause was never one defect, so the tree carried three
children beside the root's own change:

- **INFRA-246** — a wedged pass could strand a registration
  (`docs/solutions/architecture-patterns/a-deadline-that-abandons-without-cancelling-makes-a-serial-queue-concurrent-INFRA-246.md`).
- **INFRA-249** — a cold cache could answer a live session as absent, to Dispatch as well as to the
  listener's own callers
  (`docs/solutions/envoy/a-live-session-list-is-a-cache-view-decide-one-sessions-absence-by-its-own-record-INFRA-249.md`).
- **INFRA-250** — none of it could be proven where this repository's agents run, because the Go
  suite needed a container runtime
  (`docs/solutions/testing/the-envoy-in-process-nats-rig-what-it-guarantees-and-the-three-places-it-does-not-INFRA-250.md`).
- **The root** closed the send path itself: the store read, the named 503, the retry, the request
  record.

All four landed. **The original incident's cause remains undetermined between a wedged pass and
lost cache entries, and nothing in the tree reproduced it** — INFRA-246's merged packet says so,
and this note repeats it because the tree's shape invites the opposite conclusion.

The seams behaved exactly as the rules above describe.

- **They composed without knowing of each other.** INFRA-249 added `GET /v1/registry/<session_id>`
  calling `roleHolderSession`; the root had meanwhile rewritten `roleHolderSession` into a thin
  wrapper over a new `liveSessionEntry`. After the merge INFRA-249's route reads through to the
  bucket, which is what its own comment already claimed. Nobody planned that; the only collisions
  were textual.
- **INFRA-250 could not delete the rig it replaced.** The root had added a temporary
  `internal/testnats/local.go` and an `ENVOY_TEST_NATS_BIN` backend so a pod with no container
  runtime could run the suite at all, and had recorded — in that file's header and in its own pull
  request body — that INFRA-250 would remove it. INFRA-250 branched before the file existed: its 39
  changed files contain no removal. The root deleted it in its final merge and corrected both
  sentences.
- **Two markerless breaks per merge, roughly.** The last merge produced a `readonly` declared twice
  in `scripts/e2e-local.sh` (bash refuses the second, under `set -e`) and a call to a `stop_nats`
  that went with the deleted backend; the merge before it produced an `unknown field Sessions` and
  a constructor panic for a missing deferral set. Every one was clean text. `go vet ./...` over the
  whole module and a run of both scripts found all four; reading the diffs found none.
- **The file list disagreed with the packet twice**, and both times a read-only check during a gap
  between phases is what caught it.

One number, for calibration: of ten review rounds, five were held for a sentence asserting
behaviour the code could not produce — a dead field named as a live mechanism, a `--help` text
advertising two deleted backends, a test comment inverting the invariant it sat above. None was a
logic error. See
`docs/solutions/best-practices/a-completeness-claim-from-a-search-is-only-as-good-as-its-vocabulary-its-window-and-its-order-INFRA-250.md`
for why the sweeps that were supposed to catch them came back clean.
