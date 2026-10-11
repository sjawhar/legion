---
title: "A documents round re-reads the spec at its current version, dates the mechanism a sibling deletes in every copy, and pays a guarded line budget line for line"
category: legion
tags:
  - documentation-deliverable
  - spec-versions
  - sibling-issue
  - skills-guard
  - prompt-fragments
  - review-rounds
date: 2026-10-09
status: active
module: skills/legion-worker, packages/pi-legion/AGENTS.md, packages/daemon/internal/prompts/roles
related_issues:
  - "LEGION-634"
  - "sjawhar/legion#1865"
  - "LEGION-631"
---

# A documents round re-reads the spec at its current version, dates the mechanism a sibling deletes in every copy, and pays a guarded line budget line for line

Extends docs/solutions/legion/a-comment-states-the-head-it-ships-at-and-names-a-sibling-pull-requests-change-in-the-future-tense-LEGION-578.md.
That record says a comment names a sibling pull request's change in the future tense; this one is
for a sentence that *describes today's mechanism* while a sibling issue is deleting it, and for the
budgets a skill body and a prompt fragment are held to.

## The rules

- **Read the spec at its current version when a round starts, not the plan alone.** The plan
  handoff was written against the spec of its day; the architect can revise the spec after
  planning (here from version 1 to 2, folding in the planner's departures and adding a wording
  constraint), and the plan document on Dispatch is revised with it. A round that reads only
  `plan.json` implements the old version's constraints.
- **Name today's mechanism as what the sibling issue deletes, never as the standing rule, in every
  copy of the sentence.** "Today such a command runs on the per-call grant …; LEGION-631 replaces
  that grant with the role's mounted token file" survives the sibling's merge; "its bash runs
  `legion` commands on the grant …" is false the moment it lands. Grep the sibling's key across the
  documents afterwards: one hit per file that carries the sentence, none in a file that does not.
  The clause is forward-dated; whichever tree merges second sweeps it.
- **Name the state the committed assertion prints.** "Closes nothing" is what
  `phaseEntries() → [{state: "open"}, {state: "quiet"}]` shows; "leaves the stall open" names a
  state the transcript never holds after a settle. Drop a quoted code path the real surface never
  reaches: the `legion` tool's refusal text is unreachable by a subagent whose model is never
  offered the tool, so the document says it has no `legion` tool.
- **Check a guarded skill body's budget before writing, and pay for a clause line for line.**
  `packages/pi-shared/test/skills-guard.ts` fails a `SKILL.md` whose `split("\n").length` is 500 or
  more, so `wc -l` must stay at 498 or below. Rewrite a paragraph with the same line count; pay for
  a new clause by removing a sentence the surface shows unreachable or by folding an adjacent
  paragraph in, never by deleting a rule.
- **A prompt fragment keeps its tested literals.** `packages/daemon/internal/prompts/roles_test.go`
  requires `op: "handoff_complete"` and `When your phase is done, stay in this session
  afterwards:` in `mechanics/headless.md`, forbids the capitalised word `Inspect` and any
  `agent="…"` that names no shipped agent in a reusable part, and wants a heading first and one
  trailing newline. Keep the first sentence that carries a literal verbatim and rewrite after it.
- **A line you cannot grep for is read against the code's state table.** Fixed-string greps for the
  old clauses and the new terms (`HANDOFF_ALREADY_RECORDED`, `60 seconds`, the sibling's key) catch
  survivals and omissions; what a sentence *means* is checked against the module that implements
  it (`src/phase-stall.ts`'s `settle`/`inbound-event`/`handoff-complete` transitions,
  `handoff-actions.ts`'s exit-0 gate), and the tester reads the paragraphs that way.

## Evidence

- LEGION-634's spec version 2 (2026-10-09) added: "Write the subagent sentence so it stays true once
  LEGION-631 replaces the grant with the role's mounted token file: name the grant as what that
  issue deletes, not as the rule." The implementer's round 1 read `plan.json` and the plan document
  as planned against version 1 and wrote "its bash runs `legion …` on the grant your last
  credentialed call wrote, within its 60 seconds" in four files; the tester's one failure
  (`.legion/LEGION-634/test.json`, round 1): `rg -n -e 'LEGION-631' -e 'mounted token' -e 'token
  file'` over the five files printed nothing.
- Round 2 (sjawhar/legion#1865 at `40d8393953f8`): `LEGION-631` once each in
  `skills/legion-worker/SKILL.md:66`, `packages/pi-legion/AGENTS.md:319`,
  `packages/daemon/internal/prompts/roles/mechanics/headless.md:23` and
  `packages/pi-legion/CHANGELOG.md:27`; none in `packages/pi-envoy/AGENTS.md`, whose one changed
  clause carries no grant sentence. The reviewer recorded the clause as forward-dated (`notProven`),
  stale if #1843 merges first.
- `skills/legion-worker/SKILL.md` went 493 → 498 lines in round 1 (the Completion section's second
  paragraph folded into the first) and stayed at 498 in round 2: the subagent paragraph (7 lines)
  and the Completion paragraph (12 lines) were each rewritten line for line, the LEGION-631 clause
  paid for by dropping the parenthesis that quoted `legion is available only to this session's
  registered claim`, which the tester's real-binary drive showed a subagent's model never reaches
  (its tool list has no `legion`).
- `go test -count=1 ./internal/prompts/...` held `headless.md:23`'s first sentence and the
  stay-in-session line through both rounds; `grep -c 'Inspect\|agent="'` on the fragment printed 0.
