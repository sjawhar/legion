---
title: "The issue's TMPDIR is shared by every role, so never glob-delete in it"
category: legion
tags:
  - tmpdir
  - scratch
  - roles
date: 2026-10-25
status: active
module: legion-worker
related_issues:
  - "INFRA-386"
  - "sjawhar/legion#2196"
---

# The issue's TMPDIR is shared by every role, so never glob-delete in it

Extends docs/solutions/legion/probe-scripts-use-literal-tmp-paths-never-a-variable-to-rm-never-home.md.

- `TMPDIR` in a role container is `/legion/scratch/tmp`, one directory on the issue's volume, and
  every role of the issue uses it at the same time. A tester can be running a sequence there while
  the implementer, between phases, cleans up its own.
- Never remove files there by glob (`rm -f "$TMPDIR"/sequence.*`): the glob also matches the files
  of the other roles.
- Give each live run a private directory, made with `mktemp -d "$TMPDIR/<issue>-<role>.XXXXXX"`.
  Point the script's own `TMPDIR` at it when the script makes files under `${TMPDIR:-/tmp}`, and
  remove only that directory.

## Evidence

sjawhar/legion#2196: during its round-7 runs, the tester saw the sequence files in
`/legion/scratch/tmp` disappear, the run's log with them, and took it for a concurrent role's
clean-up. The implementer's own scratch clean-up, `rm -f "$TMPDIR"/sequence.*`, run once in
round 1 and once in round 7, matched those files. The round-7 one ran while the tester's runs were
under way. From round 8, both roles ran with a private `TMPDIR`, and nothing was lost.
