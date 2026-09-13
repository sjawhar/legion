---
title: "A finding outside the spec goes to the architect and comes back as a spec version: never absorbed into the diff, never deferred, never re-litigated once Rejected"
category: legion
tags:
  - process
  - spec-versions
  - architect
  - planner
  - reviewer
  - scope
  - rejected-list
  - no-deferrals
date: 2026-09-13
status: active
module: skills/legion-worker
related_issues:
  - "sjawhar/legion#1023"
---

# A finding outside the spec goes to the architect and comes back as a spec version: never absorbed into the diff, never deferred, never re-litigated once Rejected

LEGION-70's spec went from version 4 to version 8 during one issue, and every step was a phase
worker finding something the spec did not cover and routing it to the architect instead of
deciding alone. The result: the pull request never widened silently, nothing was pushed to a
"follow-up", and no decision was made twice. The pattern is worth keeping because the failure
modes it avoids are the usual ones — a worker quietly fixing an adjacent thing (scope creep that
the reviewer then has to reverse-engineer), a worker deferring it ("noted, out of scope") so it
dies, or two workers arguing the same point in two review rounds.

## What each role did

**Planner → architect (three findings, before any code).** The plan ended with a section
literally titled "Reported to the architect (decisions outside this issue's spec)":

1. CI never ran `packages/workspace` (fact F8). Recommendation: add it in this PR. Architect: keep
   it (spec v5 "New since we talked"). Nine lines of YAML that turned every acceptance check into
   something CI enforces.
2. A pre-existing jj hazard the fix would expose (F6: the fetch abandons a merged branch's commits
   under a live workspace and leaves it stale). Recommendation: separate issue — changing what a
   worker sees after a merge is a policy decision. Architect: out of scope, **re-filed through the
   controller as an independent issue**, "do not add `git.abandon-unreachable-commits` or any
   other mitigation for it" (spec v5). The implementer kept the test that shows the current
   behaviour, since it is the production shape, and touched nothing else.
3. `bookmark set` versus `bookmark create` on the fresh path. Recommendation: follow the spec
   (`set`); recorded "so nobody re-litigates it". Architect: `set` stays (spec v5).

**Architect → implementer (mid-task correction).** While the implementer was inside Task 1 the
architect sent one Envoy message: `jj workspace forget --cleanup --force` is invalid on both
pinned binaries (reported by the LEGION-44 architect, verified against jj's reference). The
implementer verified it on both binaries before applying (`error: unexpected argument
'--cleanup'`, exit 2), dropped the flags, and the spec became v6. The correction landed in the
same commit as the work it corrected — no second round, no fast-follow.

**Reviewer → architect (two spec-level edge cases, not a REQUEST_CHANGES).** The round-1 review
was a `COMMENT` that said the diff was clean against spec v6 and then listed, under "Two edge
cases for the architect — spec-level, not blocking", the two shapes the rule "any `jj bookmark
list` output means present" got wrong (a conflicted or deleted-with-remote-row bookmark; a fresh
workspace whose bookmark survived). It did not fix them, did not block on them, and did not ask
the implementer to widen the change. The architect took both as one rule change (spec v7:
resolve to exactly one commit before any `workspace add`), and the tree went back to the
implementer as a corrective round with the new contract and a full test list.

**Implementer → architect (a verified deviation, recorded not hidden).** Spec v7 named
`present(legion/<KEY>)`. The implementer verified it on both binaries, found it exits 1 on a
conflicted bookmark instead of listing two targets (so the spec's "more than one commit" branch
would be dead code), used `bookmarks(exact:…)` instead, and recorded the full comparison in the
handoff and PR body. The architect folded that into spec v8's acceptance line and Rejected list.

## Why it worked

- **The spec's `Rejected` list is the memory.** Each version appended what was considered and
  why it lost (`present()`; "any stdout means present"; starting at `main` with a surviving
  bookmark; pinning to the remote head; dropping creation entirely; `create` over `set`). A later
  worker reads the list and does not re-propose; a reviewer reads it and does not re-open. The
  round-2 review re-checked the two edge cases against the new code and opened zero threads.
- **A `COMMENT` review can carry a spec question without blocking.** The reviewer separated
  "the diff matches the spec" (approve-able, modulo the `.legion/` deletion) from "the spec has a
  hole" (the architect's call). Blocking would have made the implementer decide spec; ignoring
  would have shipped the hole.
- **Corrections arrive as spec versions, so the implementer's contract is always a document,
  never a chat.** The implementer re-read `dispatch://LEGION-70/spec` at the start of each round
  and implemented the version number it named; the PR body and handoff cite the version.
- **"Out of scope" always names where the work went.** F6 is out of scope *and* an issue number
  the controller holds; the reviewer's `update-stale` observation is out of scope *and* has a
  written reachability boundary (four conditions, the daemon cannot supply the fourth) so the
  next reader knows what would make it in scope. Neither is a bare "follow-up".

## When you are the one who found it

Planner: put it in a "Reported to the architect" section with a recommendation and the fact it
rests on; do not fold it into a task. Implementer: verify on the real surface, implement the spec
version you were given, and where you deviate, record the verified reason in the handoff so the
architect can version it — never widen the diff on your own. Reviewer: separate the diff verdict
from the spec question; a clean diff against a spec with a hole is a `COMMENT` with the hole
described for the architect, not a `REQUEST_CHANGES` and not silence. Architect: answer with a
spec version whose Rejected list grows, or with an issue number; "later" is not an answer.
