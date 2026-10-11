---
title: "A scripted conflict composition is verified against both parents' hunks, and a table's rows by a census, before it is copied in"
category: legion
tags:
  - conflict-resolution
  - merge-file
  - composition
  - markdown-table
  - verification
date: 2026-10-11
status: active
module: skills/legion-worker
related_issues:
  - "LEGION-663"
  - "sjawhar/legion#1874"
---

# A scripted conflict composition is verified against both parents' hunks, and a table's rows by a census, before it is copied in

Extends docs/solutions/legion/an-adjacent-lines-conflict-in-a-one-line-per-entry-file-is-composed-from-both-parents-and-verified-by-a-diff-against-each-LEGION-629.md.

- Composing a hunk by string edits on `git merge-file -p` output has one failure the merge tool
  never reports: a lost separator. Slicing a hunk at `<<<<<<< ours\n`, `\n=======\n` and
  `\n>>>>>>> theirs\n` strips the newline from each side's last line, so `ours + theirs`,
  `head + clause` or "the other parent's row appended to this parent's row" glues two lines
  into one unless the code puts the `\n` back. The result parses, builds and passes every suite;
  only a reader sees `| row-a … || row-b | …` or ``list on`GET``.
- Verify the composed file the way that document verifies a hand-merge, per parent and per hunk:
  the lines `jj diff --from main@origin --to @ --git -- <file>` adds must each be a line the
  branch's parent contributed verbatim, and `jj diff --from legion/<KEY> --to @ --git -- <file>`
  the same for `main`'s; a line neither parent carries whole is the glue. Do it before the
  `cp` into the workspace, on the scratch `merged` file against the `ours`/`theirs` copies.
- A Markdown table gets a census as well. Count pipes per row (`awk -F'|' 'NF != 4'` on a
  two-column table, allowing for backticked pipes) and tie the rows to their source: for the
  Stage 4b README, every `begin <name>` in `scripts/e2e/stage4b-sandbox-tree.sh` has a `| `<name>` |`
  row and no row carries a second `| `<name>` |` after its first cell. A row glued onto its
  neighbour drops a checkpoint from the rendered table while the script still runs it.
- Grep the joins a prose merge makes: a backtick followed by a letter outside a code span
  (`` `[a-z] ``), a sentence-ending period followed by a capital with no space, `|| `. Three
  patterns, one pass, before the commit.

## Evidence

LEGION-663 (sjawhar/legion#1874), the second forward merge `6da3ade3` (contract 20 → 21 after
#1878). Three files conflicted; `version.go` and `AGENTS.md` were composed by Python edits on
`git merge-file` output and `scripts/e2e/README.md` by rebuilding two hunks from the parents'
lines. The review found `README.md:905` carrying `main`'s `daily-report` row glued onto the
branch's `controller-walk` row (six pipes on a two-column table; the script-versus-table census
showed exactly that row missing), `README.md:693` running `` `STAGE4B_EVIDENCE_DIR` `` into
`keeps`, and `packages/pi-legion/AGENTS.md:106` reading ``list on`GET``. `go build`, both package
test suites, Biome and the four CI workflows had passed at that head. Fixed in `01e4275f` by
restoring the two newlines and splitting the row as `main` wraps them; the census then printed
no missing row.
