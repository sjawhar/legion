---
title: "A skill over the byte guard sheds a whole on-demand section to a reference, verbatim, and proves the move with cmp"
category: skill-patterns
tags:
  - skills-guard
  - spill-threshold
  - references
  - architect-skill
  - claude-envoy
date: 2026-10-25
status: active
module: skills/legion-architect
related_issues:
  - "INFRA-376"
  - "sjawhar/legion#2192"
---

# A skill over the byte guard sheds a whole on-demand section to a reference, verbatim, and proves the move with cmp

- **Budget bytes as well as lines.** `packages/pi-shared/test/skills-guard.ts` fails a staged file
  at 51,200 bytes (`SPILL_THRESHOLD_BYTES`, Oh My Pi's spill threshold) and a `SKILL.md` body at
  500 lines. A table row is one line of several hundred bytes, so a table-heavy skill reaches the
  byte limit long before the line limit. Run `wc -c` beside `wc -l` before adding to a skill: the
  line-budget rule in
  `docs/solutions/legion/a-documents-round-re-reads-the-spec-at-its-current-version-dates-the-mechanism-a-sibling-deletes-in-every-copy-and-pays-a-guarded-line-budget-line-for-line-LEGION-634.md`
  pays for lines only.
- **When a skill is over, move a section out; do not trim words.** Trimming buys tens of bytes and
  the next row spends them. Move the largest section an agent reads on an event rather than all the
  time (a wake table, a per-case procedure) to `skills/<name>/references/<file>.md`, whole and
  unchanged.
- **Keep the section's heading in `SKILL.md` as the pointer.** Other texts cite a section by heading,
  not by path (`packages/daemon/internal/prompts/go/architect-common.md:10` names "the `pr-blocked`
  row of `skill://legion-architect`'s wake routing"). Keep `## <Section>` and below it one short
  paragraph linking `skill://<name>/references/<file>.md`, so those citations stay true with no edit.
  Link with `skill://` only: a relative link is read by path and stops at 300 lines.
- **Cut the section mechanically and prove the move with three `cmp` checks.** Extract with
  `jj file show -r <base> <skill> | sed -n '<a>,<b>p' > <reference>`, never by retyping. Then:
  the reference is `cmp`-identical to the base's lines `<a>-<b>`; the new `SKILL.md` is
  `cmp`-identical to the base's lines `1-(a-1)`, then the exact pointer block (`printf '%s\n' …`),
  then the base's lines `(b+2)-$`; and the generated Claude Code copy is `cmp`-identical to the
  source. A reviewer approves that in one pass, and nothing new reaches the private-names check.
- **Regenerate the Claude Code copy in the same commit.** `packages/claude-envoy/legion/skills/`
  is built from the staged partition, so a new `references/` file appears there only after
  `cd packages/claude-envoy && bun run build`, which needs `bun install --frozen-lockfile` first in
  a fresh workspace (otherwise it fails with `Cannot find module 'spdx-license-list/full'`). Then run
  `bun run check-dist`. Without the regenerated copy, an architect on Claude Code loses the moved
  section.
- **Say which open pull requests the move breaks.** Every open pull request that edits a line inside
  the moved range conflicts with it, and its forward merge has to carry its edit into the reference.
  List them in the plan and the pull request body, so their trees know before they merge forward.

## Evidence

`skills/legion-architect/SKILL.md` reached 51,256 bytes at 480 lines on main `14571f6f4aea`. The
pi-legion skills-guard test went red on every pull request that touched `skills/**`. That job is
not one of main's required checks
(`docs/solutions/testing/a-regression-lock-binds-only-as-far-as-its-lane-is-required-INFRA-149.md`),
so the red reached main and then every tree that merged main forward. sjawhar/legion#2192 moved
`## Wake routing` (lines 429-457, 17,786 bytes) to `references/wake-routing.md` and left a six-line
pointer block. `SKILL.md` went to 33,651 bytes. The implementer and the tester each got
`REF-IDENTICAL` and `SKILL-ONLY-MOVE` from the `cmp` checks. Oh My Pi read the staged skills through
a per-run `skills.customDirectories` overlay: `SKILL.md` came back WHOLE and the reference WHOLE
with the `ready-refused` row; main's `SKILL.md` came back SPILLED (the negative control). Four open
pull requests editing rows of the table (#2188, #2163, #2131, #2121) and one that adds its own
`references/wake-routing.md` (#2136) were listed in the pull request body as conflicts. At this
retro, `skills/legion-controller/SKILL.md` is 38,893 bytes and has its own `## Wake routing table`,
so it is the next skill that can grow this way.
