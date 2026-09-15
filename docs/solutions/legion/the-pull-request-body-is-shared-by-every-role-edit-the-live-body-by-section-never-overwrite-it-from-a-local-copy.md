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
