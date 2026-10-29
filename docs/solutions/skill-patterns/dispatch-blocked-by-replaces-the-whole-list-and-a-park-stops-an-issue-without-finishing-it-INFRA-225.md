---
title: "Dispatch's --blocked-by replaces the whole list, and a park stops an issue without finishing it"
category: skill-patterns
tags:
  - dispatch
  - blocked-by
  - legion-architect
  - park-child
  - rerun-child
date: 2026-10-26
status: active
module: skills/legion-architect
related_issues:
  - "INFRA-225"
  - "sjawhar/legion#2188"
---

# Dispatch's --blocked-by replaces the whole list, and a park stops an issue without finishing it

Legion holds an issue while Dispatch shows it blocked by an issue that is not `done`. A skill
that tells an agent to set or change those links:

- Sets every dependency in one call after reading the issue's links:
  `dispatch issue-update --issue <key> --blocked-by <a> --blocked-by <b>` (or `--blocked-by` on
  `dispatch issue`). Each call replaces the list; `--clear-blocked-by` empties it. One call per
  dependency keeps only the last, and the issue starts when that one closes.
- Treats only `done` as finished. A park (`backlog`, `icebox`) stops an issue; whatever waits on
  it stays held, and nobody is told.
- Before parking a child for good, sets each dependent's list again without it, or parks the
  dependent too.
- Keeps the links through a park that precedes `rerun_child`: the dependents wait through the
  rerun and start when its merge closes the child. Removing them there starts the dependents
  before the rerun's work exists.
- Does not hold a child on another repository's change with a link when its task must name the
  merged commit: Legion starts it at the close, before anyone writes the commit in.
- Checks each claim about a CLI against its `--help` and its handler before writing it.

## Evidence

- Review round 4 at e2c36e93: the architect skill said to link "each child it depends on" with
  one `--blocked-by <other>` call each. `dispatch issue-update --help` reads "Replacement list …
  [] clears it", and `writeBlockedBy` deletes every link before inserting. The tester saw a parked
  child leave its dependent sibling held at `admitted` with no notice.
- Review round 5: the round-4 park rule applied to every park, including the one `rerun_child`
  requires (`CHILD_RUNNING` until parked). Scoping it to a park for good fixed it.
