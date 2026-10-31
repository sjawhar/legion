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

- Retro removes `.legion/<issue>/`, and with it a regeneration script kept there. READY can still
  be refused for a conflict, and the conflict needs the script again.
- Fetch the script at the commit the pull request body pins. If `--strict` refuses main's new
  files, fix the script (for example, add the anchor a grown item needs), never the output.
- Commit the fixed script back under `.legion/<issue>/` in the round's handoff push, which changes
  only `.legion/` and carries `skip-checks: true`. Move every pin in the pull request body to that
  commit in the same round. The next retro removes the directory again.

## Evidence

- sjawhar/legion#2422: after the first retro, `restructure.ts` fetched at 7d024719 refused main
  20ba65a6's files. `--strict` exited 1, because #2366 grew the Dispatch issue events item to 59
  screen rows.
- One split anchor (`split-issue-events-author-routes`) was added. The script was committed back
  at handoff 9cd212a6, and the porting recipe now pins `?ref=9cd212a66d864c41705be1339360913a83b47db0`.
- The next conflict, with main a409c128, regenerated with that pinned script unchanged.
