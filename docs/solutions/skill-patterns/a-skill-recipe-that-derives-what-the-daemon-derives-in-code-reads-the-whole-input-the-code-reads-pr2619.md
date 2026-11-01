---
title: "A skill recipe that derives what the daemon derives in code reads the whole input the code reads"
category: skill-patterns
tags:
  - skills
  - controller
  - paging
  - event-log
  - prose-code-parity
date: 2026-11-01
status: active
module: skills/legion-controller
related_issues:
  - "sjawhar/legion#2619"
---

# A skill recipe that derives what the daemon derives in code reads the whole input the code reads

Extends docs/solutions/skill-patterns/a-command-inside-an-instruction-is-copied-from-the-code-that-runs-it-and-anchored-INFRA-336.md.
That note copies a command from the code that runs it. A recipe the code never runs — a `curl` an
agent types to reach the same answer the daemon computes in Go — has no source to copy, and its
semantics are held equal to the code's by review alone.

- **When a skill tells an agent to compute a fact the daemon also computes, the recipe's reading
  must be the code's reading: the same input, whole.** A rule over "the newest event" or "the
  whole log" is wrong on one page of a paged API. Say how to page (`&after=<the last event's
  seq>` until a page holds fewer than the limit) and say that the daemon reads the whole log, so
  the reader knows why one page is not enough.
- **Match the code's filters in the prose:** which record types count, case folding, the
  ordering. A recipe that differs in any of these names a different answer on the inputs where it
  matters, and the agent acts on the wrong one while the daemon acts on the right one.
- **Treat the divergence as a defect of the pull request that changed the rule, not a wording
  nit.** The reviewer fixes a prose-only finding in its own commit before its review; the author
  of the rule change checks the skill's recipe against the code's loop before the push.

## Evidence

sjawhar/legion#2619 (LEGION-755) rewrote the controller's setter rule in
`skills/legion-controller/references/triage.md` step 2 to R2's (the newest add transition over
`issue.created`/`issue.updated` events) while the recipe beside it still read one unpaged page of
200 events; the daemon reads the whole log (`packages/daemon/internal/intake/label_setter.go:25`,
over `EventsAfter(key, 0)` in `packages/daemon/internal/daemon/repositories.go:1280`). On a root
with more than 200 events whose label was removed and set again late, the two would name different
setters. The reviewer found it (`.legion/LEGION-755/review.json`, `keyFindings[0]`) and fixed it in
commit `3f22ff3869b12001d80e02c4ff62fc8523ac30f8` (`triage.md:31-35` now pages with `&after=` and
says the daemon reads the whole log), re-running A5's greps and
`packages/pi-legion/src/skills-guard.test.ts` on the edited file.
