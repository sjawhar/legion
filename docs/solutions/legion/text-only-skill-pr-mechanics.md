---
title: "Text-only skill PRs: path-scoped commits that share a file, greppable acceptance phrases, and rules that must agree with their own file"
category: legion
tags:
  - legion
  - skills
  - role-prompts
  - jj
  - jj-split
  - acceptance-grep
  - reviewer
  - planner
date: 2026-09-12
status: active
module: skills
related_issues:
  - "LEGION-18"
  - "sjawhar/legion#953"
symptoms:
  - "jj split <path> puts every hunk of that file in one commit when the plan wanted two commits"
  - "grep -c '<full sentence>' returns 0 although the sentence is present, because the replacement text wrapped it across two lines"
  - "A reviewer finds the same defect in a skill file and in the matching packages/pi-envoy/roles/*.md prompt"
  - "A new lifecycle rule in one paragraph contradicts the same file's later gate section"
  - "A sentence describing code behavior is wrong because it was written from a plan's 'verified' note instead of the source line"
---

# Text-Only Skill PRs

Learned on LEGION-18 (sjawhar/legion#953), nine text fixes to `skills/*/SKILL.md`,
`packages/pi-envoy/roles/*.md`, and two `AGENTS.md` files. Nothing here is about the fixes
themselves; it is about how a Legion worker lands and verifies a documentation-only change.
Pane mechanics (stacked grants, `.omp/config.yml`, bookmark placement, a reviewer's unpushed
commit) are in [worker-pane-shell-gotchas.md](worker-pane-shell-gotchas.md).

## 1. Two path-scoped commits that touch the same file: sequence the edits

Workers commit with `jj -R "$LEGION_WORKSPACE" split -m '…' <paths…>`, never a whole-working-copy
commit (the daemon-provisioned `.omp/config.yml` sits untracked in every issue workspace). Path
scoping is all-or-nothing per file: if two planned commits both touch `skills/legion-worker/SKILL.md`,
`jj split … skills/legion-worker/SKILL.md` takes every hunk in that file into the first commit.

Do the edits in commit order and split between them: apply commit A's hunks, `jj split` A's paths,
then apply commit B's hunks, `jj split` B's paths. If you have already batched all edits (LEGION-18
round 2 did), recover without interactive tools: restore the file's B-hunk to its pre-edit text, split
A, re-apply the B-hunk, split B. Verify the second commit with
`jj -R "$LEGION_WORKSPACE" diff -r <B> --git <file>` — it must show only the B-hunk. Both commits on
#953 pass that check (`962278ef` fix-5 only, `10b4a022` fix-4 only).

## 2. Anything an acceptance check greps for stays on one line

Text-only issues are accepted by greps (`grep -c "<sentence>" <file>` → 1, or a removed-phrase grep →
nothing). `grep` is line-oriented: a sentence the plan's replacement text wraps at column 100 counts
0. The LEGION-18 plan kept both verbatim Sami quotes on single lines for exactly this reason and then
wrapped the fix-6 sentence ("never commit a plan or spec file to the repository") mid-phrase — its own
tester check returned 0 until the implementer reflowed the sentence onto one line (words unchanged).

Planner: when a check names a phrase, write the replacement so the phrase is a single line, and say so
in the plan. Implementer: run every tester grep in the plan before committing, not only the
implementer's own self-checks; a mismatch found here is a whitespace reflow, found by the tester it is
a corrective round. Reviewer: a multi-line construct the rule needs (for example an `event`
parenthetical that wraps) is fine as long as no check greps the whole construct — check the
`testerChecks` before wrapping.

## 3. A sentence describing code behavior is written from the source line, not from the plan

The plan's fix-4 "verified" note said Zod's default object mode strips unknown keys, so the first cut
wrote "fields outside the declared shape … the daemon ignores them". Every phase schema in
`packages/contracts/src/handoff-schema.ts` is `.passthrough()` (lines 186–192 at 8672ab82): undeclared
fields survive `legion handoff read` and reach the next worker; what actually fails is a *declared*
field of the wrong type, which nulls the whole file. The reviewer caught it with the source line.

A planner's `verified` field is an input to re-check, not a fact. Before a skill or role prompt
states what code does, read the line and cite the symbol (`validatePhaseHandoff`, `.passthrough()`),
never the field list — enumerated field names drift from the schema and the architect's instruction
on this issue was to name only durable facts.

## 4. A new lifecycle rule must agree with every other statement of that step in the same file

Fix 5's first cut told the reviewer to submit `APPROVED` on a clean pass. The same file's "Final
review gate" already said the only approval is of the head that differs from the reviewed one by the
`.legion/` deletion alone, and that no implementation change may follow an approval. Read literally,
the new paragraph approved the pre-deletion head, put an implementation push after an approval, and
fired the second `pr-review` wake fix 5 existed to remove. Correct rule (now in both files): one
submission per round; `REQUEST_CHANGES` when a correctness finding stands; `COMMENT` for a clean pass
while the head still carries `.legion/`; `APPROVE` only for the deletion head, named by SHA.

Before committing a rule about a lifecycle step, grep the file for every other mention of that step
(here: `approve`, `.legion/`, `deletion`) and read them together. The mechanics stay the same — one
`POST repos/{owner}/{repo}/pulls/{number}/reviews --input body.json` with `event` inside the body,
because `gh api` moves `-f` fields into the URL when `--input` is used and GitHub would then create a
pending draft review.

## 5. Skill and role prompt are two copies of one rule: edit both, grep both

`skills/legion-worker/SKILL.md` and `packages/pi-envoy/roles/{planner,reviewer,tester}.md` carry the
same rules in different voices (the prompt is what the role boots with; the skill is what it reads).
The reviewer found the fix-5 defect in both copies as two threads, and the corrective commit had to
touch both. When you change a rule in one, `grep -rn '<distinctive phrase>' skills/ packages/pi-envoy/roles/`
and change every hit in the same commit; the tester's greps for the new phrase expect one hit per file.

## 6. Small checks that catch doc-only defects a line diff hides

- `grep -c '^```' <file>` must be even. Root `AGENTS.md` had an unclosed lifecycle fence, so
  `## Documentation` and the Plans row rendered inside a code block; nobody saw it in a diff.
- Every agent or tool a skill names must exist in a Legion pane. The oracle skill named
  `Task learnings-researcher`, `Task Explore`, `Context7 MCP` — none exists. Check
  `~/.omp/profiles/legion/agent/agents/` and your own `task` tool inventory (`scout`, `oracle`, …)
  before writing an agent name.
- A "not yet runnable until PR X merges" caveat needs a removal step tied to PR X. Three skills kept
  theirs for weeks after #873/#923 landed.
- The worker skill's PR-body template names a `pr-checks-result` check. This repository defines no such
  check (its heads settle on the `Tests` and `Legion Envoy and Contracts` workflow runs). Fill the
  `CI:` line with the real run ids and workflow names and say the template name does not exist here.
