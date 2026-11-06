---
title: "When the base moves the block you edit, the conflict is at the old site and taking the base's side drops your edit"
category: legion
tags:
  - forward-merge
  - merge-conflicts
  - skills
  - silent-loss
  - jj
date: 2026-10-25
status: active
module: skills
related_issues:
  - "INFRA-251"
  - "sjawhar/legion#2163"
---

# When the base moves the block you edit, the conflict is at the old site and taking the base's side drops your edit

Extends `docs/solutions/legion/a-forward-merge-into-a-branch-that-moved-files-ports-mains-edits-to-the-new-paths-LEGION-247.md`,
which covers the opposite direction: the branch moved files and the base edited them. This is the
direction a branch meets far more often, and it is the dangerous one, because the obvious
resolution is the step that deletes the work.

- **The conflict names the old file, never the new one.** The base cut a block out of file A and
  pasted it into file B. Your branch edits a line inside that block, so A conflicts — and the
  right resolution at A is the base's side, the pointer or the empty space the block left. B does
  not conflict at all: your branch never touched it, so it merges cleanly, carrying the base's
  **older** copy of your line. Resolve A, push, and the edit is gone with no marker, no warning
  and no failing check.
- **Enumerate your hunks in A before you resolve, and place each one against the moved range.**
  `jj diff --from <fork point> --to <your tip> <A>` lists them. Only the hunks inside the range
  move; the rest stay in A and must be confirmed present afterwards, each by its own text rather
  than by a line number the merge has shifted.
- **Then measure, three ways, and do not skip the third.** Your wording appears exactly once in B
  (and once in each generated copy of B); it appears zero times in A; every hunk outside the range
  is still in A. A clean conflict list is evidence of nothing here.
- **Ask first whether the move already carries your wording.** Another branch that edits the same
  block and lands before you does the relocation for you, and re-applying it then is a diff the
  reviewer must read for nothing.
- **Say in the fingerprint comment that the change is a relocation**, naming the one row or block,
  both files and that its wording did not change. The unchanged-diff fingerprint moves for a
  relocation exactly as it moves for a rewrite, so nothing but the prose tells the reviewer which
  it was.
- **No check guards this.** A later merge that drops the block from both files passes every lane.
  The protection is the measurement above, written into the handoff.

## Evidence (INFRA-251, sjawhar/legion#2163)

`main` took INFRA-376 (sjawhar/legion#2192), which moved the Wake routing table out of
`skills/legion-architect/SKILL.md` into `skills/legion-architect/references/wake-routing.md` so the
skill would fit the 51,200-byte spill guard. This branch carried three hunks in that skill, at
`main`'s lines 160, 447 and 470, of which only the 447 one — the `design-changes-requested` row of
that table — was inside the moved range.

The forward merge reported two conflicts, `skills/legion-architect/SKILL.md` and its generated
mirror under `packages/claude-envoy/legion/`. It reported none in `references/wake-routing.md`,
which arrived from `main` holding `main`'s older wording of the row. Taking the base's pointer in
the skill — the correct resolution there, and the only one the conflict offered — would have ended
the merge with the branch's row deleted and every lane green.

The three measurements that caught it, run after the merge and before the commit:

```
grep -c '^| `design-approved`' skills/legion-architect/SKILL.md      # 0: the table is gone from the skill
grep -n 'design-changes-requested' skills/legion-architect/references/wake-routing.md   # main's older wording, line 18
grep -c 'then `register_gate` it' skills/legion-architect/SKILL.md   # 1: hunk outside the range survived
grep -c 'Open the root-level `dispatch ask` or the root approval' skills/legion-architect/SKILL.md  # 1: the other one
```

The row was then placed into the reference word for word, and the generated mirror regenerated
with `bun run build` rather than hand-edited (`bun scripts/build.ts --check` prints
`dist/ and legion/ match a fresh build`). Against `main`, the reference now differs by that one
line and the skill by the two hunks outside the range. The row's wording came into this branch from
another pull request it was built on, so the branch that owns it faces the same move.
