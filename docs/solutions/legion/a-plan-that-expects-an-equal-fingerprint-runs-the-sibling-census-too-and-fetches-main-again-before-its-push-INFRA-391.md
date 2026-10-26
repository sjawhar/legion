---
title: "A plan that expects an equal fingerprint runs the sibling census too, and fetches main again before its push"
category: legion
tags:
  - planning
  - forward-merge
  - fingerprint
  - sibling-pr
date: 2026-10-26
status: active
module: skills
related_issues:
  - "INFRA-391"
  - "sjawhar/legion#2209"
---

# A plan that expects an equal fingerprint runs the sibling census too, and fetches main again before its push

Extends docs/solutions/legion/sibling-pr-rewrites-your-function-spell-both-shapes-in-the-plan-and-expect-the-fingerprint-to-change.md.

That document gives the sibling census to a plan that changes code. A plan that changes no code
needs it too: a re-run after a reopen, or a confirmation round, plans "fingerprint unchanged,
bare gates only". One sibling that lands on a file of the branch makes that plan wrong before
the next role starts.

- List the open pull requests that touch the branch's files:
  `gh pr list -R <owner>/<repo> --state open --limit 200 --json number,files`, then filter by
  the paths of `jj diff --from "fork_point(main@origin | <head>)" --to <head> --name-only`. For
  each hit, write in the plan where the branch's edits go if it lands first, and that the
  fingerprint then changes.
- Just before you push the plan, run `jj git fetch` and compare `main@origin` with the main SHA in
  your facts. If main moved, intersect `jj diff --from <old main> --to main@origin --name-only`
  with the branch's files. Write the "fingerprint equal" prediction only for the main you
  checked.

## Evidence

- INFRA-391 generation 2's plan (commit `08a478fdb44c`, completed 04:08:20Z) read #2209 as
  `MERGEABLE` at main `135d22e0f63b` at 03:56Z, and expected fingerprint `f1a51e27…` at every
  generation-2 head. #2197 (INFRA-353) had been open since 2026-10-25T15:56:50Z. It changed
  `skills/legion-architect/SKILL.md`, a file the branch also changes, and it merged at
  03:57:33Z: 90 s after the plan's read and 11 min before the plan's push. The plan does not name
  #2197.
- The implementer found #2209 `CONFLICTING` at the start of its round. The forward merge
  `5f5b6c18fb6f` changed the fingerprint to `ef86905a…` in skill text only. The plan's fallback
  (a changed fingerprint means a full round) held. But the round the plan expected, and the
  fingerprint its reviewer step named, were wrong before the plan was pushed, and the architect
  had to set the round's scope by an Envoy message.
