---
title: "Attribute a job log's lines by its ##[group]Run markers, never by the jobs API's step window"
category: github-api
tags:
  - github-actions
  - job-logs
  - measurement
  - go-test
date: 2026-11-03
status: active
module: .github
related_issues:
  - "sjawhar/legion#2726"
---

# Attribute a job log's lines by its `##[group]Run` markers, never by the jobs API's step window

Extends docs/solutions/github-api/polling-a-re-run-workflow-from-a-pane-job-logs-by-api-and-one-credentialed-call-per-poll.md.

That note gets you the log: the job's own `/actions/jobs/<id>/logs` endpoint, with
`--allow-escape-sequences`, for the attempt that job id belongs to.
It says nothing about reading **within** the log, and the step times sit in the same API response,
one field away, which is the trap.

- **Split the log on its own `##[group]Run <command>` markers.**
  Each marker's timestamp is the step's real start, and a step owns every line from its marker to
  the next one.
- **Never bracket lines by `started_at` and `completed_at` from `/actions/jobs/<id>`.**
  Those are whole seconds, and a tool that buffers output flushes a burst of lines at the instant
  a step ends — which is also the next step's `started_at`, to the same second.
  Every line of that burst falls outside its own step's window and is filed under the next step.
- **`go test` is such a tool.**
  It queues a finished package's result line behind a slower package that started earlier and
  prints the queue when the slow one ends, so most of a step's package lines can carry one
  timestamp.
- **The count is the check.**
  Attribute, then compare each step's line count against what the step should have produced.
  A split that moves when you change the attribution rule is the rule's fault, not the log's.

## Evidence

sjawhar/legion#2726 (dispatch://INFRA-1052) measured each package's wall time in the two Go steps
of the `test` job, over 30 jobs.

- Read by the jobs API's step window, job 125891768215 gave 17 package lines to the plain step and
  48 to the race step, which runs four packages.
  48 of the plain step's 65 lines carry the timestamp `13:00:10.448`, flushed when
  `internal/daemon` finished; the plain step's `completed_at` is `13:00:10Z` and the race step's
  `started_at` is the same second, so every one of them was filed under the race step.
- Read by the in-log markers, the same job gives 65 and 4, and so does every one of the 30.
- The reviewer reached the same split independently on a later job, 126085309424, by the markers
  at log lines 7942 and 8051: 63 package lines in the plain step, 4 in the race step.
- The counts are what exposed it: 17 and 48 for a step that runs four packages is visibly wrong,
  while the per-package times it produced looked ordinary.
