---
title: "A base that moves a block you edit leaves both sides intact and the combination wrong"
category: legion
tags:
  - forward-merge
  - evidence
  - skills
  - review-rounds
date: 2026-10-25
status: active
module: skills
related_issues:
  - "INFRA-83"
  - "sjawhar/legion#2121"
---

# A base that moves a block you edit leaves both sides intact and the combination wrong

Extends docs/solutions/legion/a-record-is-true-when-written-and-read-later-so-pin-each-claim-to-a-command-or-delete-it-INFRA-83.md.

That document's three instruments answer *did the branch's diff change*, *are the branch's
deletions still deleted*, and *did the base bring in text the branch forbids*. **All three are
blind to this one**, so it needs its own detector rather than a sharper reading of theirs.

**The shape.** The base moves a block of text to another file — verbatim, leaving a pointer behind
— while your branch edits a row inside that block. The old file's hunk conflicts, so you resolve
it. The new file arrives from the base **clean, with no conflict marker**, carrying the
pre-branch rows. A resolution that keeps your side of the conflict then **reintroduces a section
the base deleted**, and your edit is in the copy nobody reads. Both sides are individually intact;
only the combination is wrong, and nothing marks it.

**Resolve theirs-for-structure, ours-for-content:** the base decides where things live, your
branch decides what the row says. Then prove it with two reads, because the merge will not:

- **Count the moved thing in the file it left.** Zero rows is the pass; any is a resurrection.
  Here `grep -c '^| \`' skills/legion-architect/SKILL.md` → `0`, and a byte count against the base
  accounting for the difference by your own known edits (33,662 against the base's 33,651 — eleven
  bytes, one `--paginate` flag).
- **Check your content is present in the file it went to**, by the thing that makes it yours: a
  row the base's copy has no counterpart for. Here the `merge-waiting` row, in
  `skills/legion-architect/references/wake-routing.md` — the notice kind is named in four other
  files of this change, so search for the *row*, not the word. **Run both checks against the
  generated copy too** (`packages/claude-envoy/legion/skills/…`): it is built from the source, so
  a resurrection there means the build ran on a resurrected source.

**Why each standing instrument misses it.** The fingerprint moves either way, and a per-path
comparison shows the signature — one path `only after`, another `both, differing` — only if
someone reads it rather than the hash. The deletion-survival sweep looks for lines the *branch*
removed; the base's move removed none of yours. A standing lint over the files reads sizes, links
and patterns, not where a block of text lives — and a `skill://…/SKILL.md` link stays valid when
the thing it describes has moved out of that file. Measured on the false sentence: the lane read
`11 pass, 0 fail` — **a link checker proves a target exists, never that the claim around it is
true**, and that is the whole gap, not a weakness in the lane.

One clause the same night earns, because it is the organising rule turned on its sharpest user:
**a sceptical reading offered as the strongest case against your own change still has to be
checked.** This change's boundary audit named, as its one concession, that its new hour-timer was
"the only place the daemon measures elapsed time to tell a human something". False:
`ghautomerge.Lagging.Stalled` is `now.Sub(l.Since) > LagBound` and feeds `merge-stalled`. The
true form is stronger — the new timer is the only place the daemon measures elapsed time against
**its own record of when it acted** (`record.ReadyMerge.Queued`), where `Stalled` measures
against GitHub's clock — so the audit's conclusion held while its sharpest sentence did not. An
unchecked claim is not made safe by being the one against yourself.

**The corollary, which cost this change a defect.** After such a merge, a sentence of yours that
*names where something lives* is false without being edited. `references/merge-notices.md:6` — a
file this branch adds — still said the wake-routing table was in `SKILL.md`. A link check passes
it, because the file exists and only the claim about its contents is wrong. **After a relocation,
grep your own added text for the path that moved**, not only for the lines you deleted.

Two things make that sweep usable. **Separate dated history from a live claim**: a document
saying the table *was* in `SKILL.md` on a date is true and stays; one saying it *is* there is not.
A sweep that cannot tell them apart gives either false hits or a false all-clear — here every
surviving mention was under `docs/plans/` or `docs/solutions/`, which is why the all-clear meant
something. And **prefer the sentence that names content over the one that names a location**:
`prompts/go/architect-common.md:10` says "the `pr-blocked` row of `skill://legion-architect`'s
wake routing" — it names no file, and it stayed true through the relocation **without being
edited**, which is the standard. This change's own repair falls short of it: "the wake-routing
table, in `skill://legion-architect/references/wake-routing.md`, gives each notice its move in
one sentence" is true now and still names a file, so the next move falsifies it again. A pointer
that names the thing rather than its address is the only fix that outlives the next relocation,
and a tree where files move weekly is where that distinction stops being pedantic.

## Evidence

sjawhar/legion#2192 (dispatch://INFRA-376) moved the architect skill's whole `## Wake routing`
table byte for byte into `skills/legion-architect/references/wake-routing.md`, leaving a
three-line pointer under the heading. This branch edits three of that table's rows and **adds** a
fourth, `merge-waiting`, for which the base's table has no counterpart — the notice kind itself is
named in four other files of this change, which is why the presence check searches for the row and
not the word. The fifth forced forward merge of this change conflicted on
`skills/legion-architect/SKILL.md` alone; `references/wake-routing.md` arrived clean.

The resolution took the base's pointer and re-placed the rows and their paragraph in the
reference, then proved it by the two reads above. The near-miss is the point: a resolution keeping
this branch's `SKILL.md` would have shipped **eighteen** resurrected rows — the base's table minus
this change's addition — with no conflict, no failing lane, no moved deletion, and a fingerprint
change that attribution would have explained as *the relocation* either way.

Siblings, since this one is discoverable only by following them:
`docs/solutions/skill-patterns/a-skill-over-the-byte-guard-sheds-a-whole-on-demand-section-to-a-reference-verbatim-and-proves-the-move-with-cmp-INFRA-376.md`
is the same event from the side doing the move, and
`docs/solutions/legion/a-forward-merge-into-a-branch-that-moved-files-ports-mains-edits-to-the-new-paths-LEGION-247.md`
is the mirror case, where the branch moved files and the base edited them.
