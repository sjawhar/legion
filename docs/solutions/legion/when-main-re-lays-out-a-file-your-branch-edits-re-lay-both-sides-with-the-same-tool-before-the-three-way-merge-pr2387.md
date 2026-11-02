---
title: "When main re-lays out a file your branch edits, re-lay both sides with the same tool before the three-way merge"
category: legion
tags:
  - forward-merge
  - re-layout
  - semantic-line-breaks
  - merge-conflict
date: 2026-10-31
status: active
module: packages/envoy
related_issues:
  - "sjawhar/legion#2387"
---

# When main re-lays out a file your branch edits, re-lay both sides with the same tool before the three-way merge

Extends docs/solutions/legion/a-mass-re-layout-of-a-file-many-pull-requests-edit-is-a-script-and-each-merge-of-main-reruns-it-pr2422.md.

That note is for the pull request that owns the re-layout script. This one is for every other
branch that edited the file and merges main after the re-layout landed: it has no script to rerun,
and a line-based merge conflicts on every paragraph it touched.

- Take three copies of each file: the merge base, your branch and main. Run main's layout tool on
  the base and branch copies in the mode that changes only whitespace (for the two Envoy guides,
  `bun scripts/semantic-line-breaks.ts --write <file>`, which needs `bun install` first). Then merge
  the three with `git merge-file -p branch base main`. Only the passages main changed in words or
  structure (a table made a list) still conflict.
- In each of those, take main's text and apply your own edits to it word for word. Find your edits
  with a word-level diff of the base and branch copies inside the conflict.
- Check the result two ways. A word-level diff of each file against main
  (`git diff --no-index --word-diff`) shows your edits and nothing else. Main's layout check passes
  on the file. An edit that makes a list item longer can break a length rule main's text met: split
  the item, change no words, and say so on the pull request.
- Read what the re-layout changed besides the layout. A new rule that moves content (here, "a new
  migration describes itself in the header comment of its own `.up.sql`") moves your content too:
  put each fact only your paragraph held into its new home before you delete the paragraph.

## Evidence

Main's #2422 relaid `packages/envoy/AGENTS.md` and `packages/envoy/cmd/dispatch/README.md` one
sentence per line and made their large tables lists. Merging it into sjawhar/legion#2387 by lines
left 11 conflict blocks. Relaying the base and branch copies first left 4, each a table #2422 made
a list. The pull request's nine edited rows went into main's list items word for word (merge
`a8c1b638`). The word-level diff against main showed only those edits. One `AGENTS.md` item grew to
54 screen rows with the branch's importer text, over the tool's 50 (`R3`), and became two items. The
README's migration-log paragraphs for `0150`–`0153` moved into those migrations' `.up.sql` headers
(census `0`, image rollback, the earlier release's behaviour), and the SQL did not change.
