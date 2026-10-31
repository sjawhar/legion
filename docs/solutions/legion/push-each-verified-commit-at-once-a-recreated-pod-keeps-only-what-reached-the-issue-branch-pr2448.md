---
title: "Push each verified commit at once: a recreated pod keeps only what reached the issue branch"
category: legion
tags:
  - legion-worker
  - implementer
  - jj
  - kubernetes
  - recovery
date: 2026-10-28
status: active
module: skills/legion-worker
related_issues:
  - "sjawhar/legion#2448"
---

# Push each verified commit at once: a recreated pod keeps only what reached the issue branch

- Push the issue branch right after each commit you have verified. Do not keep a stack of local
  commits for one push at the end of the phase. A pod that dies and is recreated rebuilds the
  workspace from `legion/<KEY>@origin`, so every unpushed commit is gone, and so is everything
  under `$TMPDIR`: Postgres, built binaries, downloaded inputs and test harnesses.
- A code push before the pull request opens starts CI on nothing and costs nothing. A code push
  after the pull request opens runs CI on a head that is still in progress, which is fine: the
  daemon judges only the head you complete on.
- Keep the scripts that rebuild your environment and your measurement harness in a form you can
  hand to a subagent again: the exact install, restore and run commands. Run a long measurement
  only after the code it measures is pushed.
- When work is lost anyway, rebuild it from the diffs you already reviewed in your transcript
  (`git apply --recount --check`, then apply). Have workers redo only the parts whose diffs you
  never read.

## Evidence

- sjawhar/legion#2448: the first implementing turn verified and committed the regression tests,
  both code parts and a 1,556-line equivalence test, but did not push them. The process died, and
  the pod came back with an empty workspace and an empty `$TMPDIR`.
- The tests and code parts came back from their reviewed diffs with `git apply --recount` (pushed
  as 33b9fcff, fe2b0ae6 and 8f276d52). The equivalence test and the environment had to be rebuilt
  by subagents.
- Every later commit was pushed as soon as it was verified.
