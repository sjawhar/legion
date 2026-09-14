> **Suspended 2026-09-13.** Sami: "Please shutdown the goddamn legion smoke. It's pointless and it has led to destructive actions twice now." The rig this entry describes no longer exists; keep the learning, not the procedure.

---
title: "A sourced bash harness must pin its copy root and prove absence with a call log"
category: testing
tags:
  - bash-harness
  - set-e
  - fake-binaries
  - call-log
  - glob
  - cwd
  - mutation-testing
date: 2026-09-13
status: active
module: test-harnesses
related_issues:
  - "LEGION-71"
  - "sjawhar/legion#1029"
---

# A Sourced Bash Harness Must Pin Its Copy Root and Prove Absence With a Call Log

The removed rig used a sourced-copy shell harness. These test-design lessons still apply to any
maintained shell fixture.

## Retained lessons

- A copied script that derives paths from `BASH_SOURCE` must have its root pinned to the fixture's
  known project root. Exercise it from outside that root so accidental working-directory behavior
  cannot hide a broken path.
- A `set -e` function exercised through `if` or `&&` needs an explicit subshell-and-status form;
  otherwise the shell can suppress the failure being tested on its success path.
- To prove that a valid override never invokes a tool, put a fake tool on `PATH`, record every
  invocation, and assert the log is empty. A missing executable on one machine proves nothing.
- An absence assertion over a glob first proves its intended population exists. A glob that matches
  no files must fail the fixture rather than report a vacuous pass.
