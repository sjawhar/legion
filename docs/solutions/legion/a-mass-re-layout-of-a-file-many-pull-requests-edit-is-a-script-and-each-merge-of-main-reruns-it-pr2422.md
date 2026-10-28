---
title: "A mass re-layout of a file many pull requests edit is a script, and each merge of main reruns it"
category: legion
tags:
  - forward-merge
  - conflicts
  - generated-files
  - agents-md
date: 2026-10-28
status: active
module: .legion
related_issues:
  - "sjawhar/legion#2422"
---
# A mass re-layout of a file many pull requests edit is a script, and each merge of main reruns it

- Write a re-layout or conversion of a busy hand-edited file as a script. Anchor each edit by text,
  not line number. Give it a strict mode that writes nothing when an anchor is missing or not
  unique.
- Resolve every conflict with main by taking main's version of the file
  (`jj restore --from main@origin <files>`) and running the script again. Never resolve the
  conflict by hand. Record which main commit the regeneration ran on. The reviewer reruns it on
  that commit's files and compares the bytes with `cmp`.
- Expect a rerun to need a new anchor when main grows a part past a limit the script enforces.
  Add the anchor in the script, not in the output.
- Retro deletes `.legion/<issue>/`, so the pull request body fetches the script at a pinned commit
  (`gh api "repos/<owner>/<repo>/contents/<path>?ref=<sha>"`). Check that the pinned commit holds the
  script's final version.

## Evidence

- The issue's `restructure.ts` under `.legion/` (sjawhar/legion#2422) turns main's two guides into the new
  layout. `--strict` writes nothing on a missing anchor. It ran at about eight merges of main, for
  example 7d024719 on d47dbf05 and e6f60222 on e82b013b. Two of them needed a new split anchor
  because a main edit grew an item past 50 screen rows.
- The porting recipe in the pull request body fetches the script with `?ref=7d024719…`. The script
  did not change after that commit.
