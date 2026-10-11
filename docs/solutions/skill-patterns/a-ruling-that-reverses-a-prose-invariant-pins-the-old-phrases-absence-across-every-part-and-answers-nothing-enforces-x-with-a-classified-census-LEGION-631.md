---
title: "A ruling that reverses a prose invariant pins the old phrase's absence across every part, and answers \"nothing enforces X\" with a classified census"
category: skill-patterns
tags:
  - prompts
  - skills
  - structural-test
  - census
  - ruling
date: 2026-10-10
status: active
module: packages/daemon/internal/prompts
related_issues:
  - "LEGION-631"
  - "sjawhar/legion#1843"
---
# A ruling that reverses a prose invariant pins the old phrase's absence across every part, and answers "nothing enforces X" with a classified census

- When a decision reverses a rule the prompts and skills state in words ("Legion never merges"),
  add a structural test that walks every composed part and every staged skill for the old phrase's
  shapes — a case-insensitive pattern over the sentence family, not the new sentence's presence
  alone. The old wording lives in files the change never opens (a docs-site concept page, a skill
  for another role), and the worker rewriting the rule reintroduces it in new words.
- Keep the pattern's source from spelling the phrase when a repository-wide grep for the phrase is
  itself an acceptance check: `(?i)never merg(es)|merg(e|es) nothing` passes the grep the sentence
  would fail.
- A ruling of the form "nothing in Legion enforces a restriction the repository does not" is
  answered by a census, not a hit count: each search pattern, each hit, and each hit's class —
  enforcement (a hook, a wrapper, a tool-op or route refusal: delete), guidance (a sentence with
  nothing behind it: keep), the workflow's own gate (READY on handoff hygiene: keep, and say so),
  the upstream's own rule read back (required checks, a thread only the author's App may resolve:
  keep), credential plumbing (which identity acts, not what it may do: keep). A row the ruling may
  or may not cover (`READY_HEAD_CARRIES_HANDOFFS`) goes to the architect as a question with its
  class proposed, not deleted on a reading.

## Evidence

sjawhar/legion#1843, spec v8–v9. `TestNoPartSaysLegionNeverMerges`
(`packages/daemon/internal/prompts/roles_test.go`) and the skills-guard test in
`packages/pi-legion/src/skills-guard.test.ts` walk every part and staged skill; the guard failed on the
rewriting worker's own first sentence in `merge-gate.md` ("the merger never merges by hand") before it
pinned anything else, and `docs/site/src/content/docs/legion/concepts.md:16` was the last plain hit.
The restriction sweep's rows are the PR body's `## Contract change census` ("enforced GitHub
restrictions beyond the repository's"): 0 enforcement left, each kept hit classified; the architect
ruled `READY_HEAD_CARRIES_HANDOFFS` a workflow gate (envoy cfc3d239).
