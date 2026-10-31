---
title: "Retro does not freeze a regeneration script: a refused READY can need one more anchor"
category: legion
tags:
  - forward-merge
  - generated-files
  - retro
  - ready
date: 2026-10-28
status: active
module: .legion
related_issues:
  - "sjawhar/legion#2422"
---
# Retro does not freeze a regeneration script: a refused READY can need one more anchor

Extends docs/solutions/legion/a-mass-re-layout-of-a-file-many-pull-requests-edit-is-a-script-and-each-merge-of-main-reruns-it-pr2422.md.

That file's evidence names `7d024719` as the script's final version. The script changed after
that commit; the current pin is below.

- Retro is not the end of a regeneration script's use. READY can still be refused for a
  conflict, and the conflict needs the script again. Before sjawhar/legion#2354 retro also removed
  `.legion/<issue>/`, and with it a script kept there.
- Fetch the script at the commit the pull request body pins. If `--strict` refuses main's new
  files, fix the script (for example, add the anchor a grown item needs), never the output.
- Put the fixed script where the pull request body's pin can reach it, outside the tree the merge
  carries, and move every pin in the body to its new version in the same round. Before
  sjawhar/legion#2354 the script went back under `.legion/<issue>/` in the round's handoff push,
  which changed only `.legion/` and carried `skip-checks: true`, and the next retro removed the
  directory again. Since then a handoff is a Dispatch record, no phase pushes one, and no push
  carries the trailer.

## Evidence

- sjawhar/legion#2422: after the first retro, `restructure.ts` fetched at 7d024719 refused main
  20ba65a6's files. `--strict` exited 1, because #2366 grew the Dispatch issue events item to 59
  screen rows.
- One split anchor (`split-issue-events-author-routes`) was added. The script was committed back
  at handoff 9cd212a6, and the porting recipe now pins `?ref=9cd212a66d864c41705be1339360913a83b47db0`.
- The next conflict, with main a409c128, regenerated with that pinned script unchanged.
