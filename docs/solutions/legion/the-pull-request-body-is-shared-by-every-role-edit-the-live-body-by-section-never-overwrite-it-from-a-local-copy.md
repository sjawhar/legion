---
title: "The pull-request body is shared by every role: edit the live body by section, never overwrite it from a local copy"
category: legion
tags:
  - pr-body
  - ready-format
  - implementer
  - tester
  - gh-pr-edit
  - e2e-line
date: 2026-09-15
status: active
module: skills/legion-worker (PR body and merge-queue discipline)
related_issues:
  - "LEGION-179"
  - "sjawhar/legion#1122"
---

# The pull-request body is shared by every role: edit the live body by section, never overwrite it from a local copy

## What nearly happened

The implementer writes the READY-format body when the pull request opens and keeps a local copy
of it (`--body-file`). Later rounds ask the implementer to update a few lines — the `CI` line to
the new head's runs, `Threads`, `Thermo`, `Fast-follow`. By then the tester has written its own
`E2E (tester)` line into the live body on GitHub, and the reviewer may have too. Re-running
`legion gh -- pr edit --body-file <the local copy>` would have replaced the tester's line with
the implementer's stale `<pending the tester>` — silently, with no diff anyone reviews, and the
reviewer's approval refuses a head whose `E2E (tester)` line is missing.

## The rule

Before any `pr edit`, fetch the live body (`legion gh -- pr view <n> --json body --jq .body`),
change only the sections the task names, assert the sections you must not touch are byte-equal
to what you fetched, and write that back. Sections are the `**Name:**` headers of the READY
format; a small script that replaces a section by regex and asserts the `E2E (implementer)` and
`E2E (tester)` sections are unchanged is enough. The local copy is a draft for the first write,
never the source of truth afterwards.

The same holds in the other direction: a tester or reviewer appending its line must edit the
live body too — the implementer's `CI` line may already name a newer head after a
conflict-forced rebase.

**Superseded 2026-10-23 (dispatch://INFRA-83):** only the reason is superseded — the implementer's
`CI` line naming a newer head after a conflict-forced rebase — because the body carries no `CI:`
line (`skills/legion-worker/references/pr-body.md`), and no role records a check result for a later
step to trust: the tester and the reviewer each read CI on GitHub at the head they are judging and
record none of it (the reviewer's own four GitHub reads are in
`skills/legion-worker/references/merge-gate.md`). The rule itself is untouched, and so is the rest
of this learning: a tester or reviewer appending its line must edit the live body too, because the
body carries the reviewer's `Look at first` and `Not proven / risk`, the implementer's `Threads`
and `Size`, and both `E2E` lines, every one of which another role may have written or changed since
you fetched it.
