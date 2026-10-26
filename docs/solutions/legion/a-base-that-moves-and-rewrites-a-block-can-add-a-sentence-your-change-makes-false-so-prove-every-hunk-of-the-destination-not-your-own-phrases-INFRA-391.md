---
title: "A base that moves and rewrites a block can add a sentence your change makes false, so prove every hunk of the destination, not your own phrases"
category: legion
tags:
  - forward-merge
  - skills
  - evidence
  - conflict-resolution
date: 2026-10-26
status: active
module: skills
related_issues:
  - "INFRA-391"
  - "sjawhar/legion#2209"
---

# A base that moves and rewrites a block can add a sentence your change makes false, so prove every hunk of the destination, not your own phrases

Extends docs/solutions/legion/a-base-that-moves-a-block-you-edit-leaves-both-sides-intact-and-the-combination-wrong-INFRA-83.md.

That document covers a base that moves a block verbatim. When the base also rewrites the block on
the way, it can add new text, with no conflict marker, that your change makes false. Carrying your
own insertions into the new file does not fix that text.

- After you carry your edits to the file the block moved to, read the base's new text in that file
  for each claim your change retires (here: a project has one architecture source). Fix each
  sentence that is now false, in the merge commit.
- Prove the destination with
  `jj diff --from <base> --to <merge> --git --context 0 <destination>` and name every hunk in it:
  each carried insertion, and each fix to a sentence of the base. A count of your own phrases
  proves the carry and nothing else.
- Write the fixes to the base's sentences into the handoff's proof as their own row, so that the
  tester and the reviewer check them. "Carried word for word" does not cover them.

## Evidence

- main `af460a46bb16` (#2197, INFRA-353) moved the legion-architect skill's
  "Architecture components" section from `skills/legion-architect/SKILL.md` to
  `skills/legion-architect/references/decomposition.md`, and rewrote it. It added a new first
  sentence: "A project's architecture source is the repository whose `.dispatch/architecture/`
  files Dispatch imports". #2209 makes that sentence false, because a project now has several
  sources. The branch's own SKILL.md never had that sentence, so the conflict did not show it.
- The forward merge `5f5b6c18fb6f` carried the two insertions and changed that sentence to the
  plural. `jj diff --from af460a46bb16 --to 5f5b6c18fb6f --context 0` on `decomposition.md` shows
  3 hunks: the sentence, and the two insertions. The implementer's proof counted only its two
  phrases (`several sources`, `syncs that one`: 1/1 at the merge, 0/0 on main). The tester and the
  reviewer recorded "the insertions carried word for word". No command checked the third hunk.
  A fresh-eyes sweep of every `skills/**/*.md` at `ef44b6d7ce4b` found no other singular claim, so
  the merge left no defect. The gap was in the proof, not in the text.
