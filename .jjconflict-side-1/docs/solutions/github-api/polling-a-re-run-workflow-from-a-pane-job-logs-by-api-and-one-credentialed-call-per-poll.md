---
title: "Polling a re-run workflow from a pane: read a finished attempt's job log through the API once a later attempt is running, and never loop legion gh inside one bash call"
category: github-api
tags:
  - github-actions
  - run-attempt
  - rerun
  - job-logs
  - gh
  - legion-gh
  - grant
  - polling
  - tester
date: 2026-09-14
status: active
module: legion gh, .github/workflows
related_issues:
  - "LEGION-94"
  - "sjawhar/legion#1084"
symptoms:
  - "run <id> is still in progress; logs will be available when it is complete"
  - "Unable to redeem LEGION_GRANT (403) after about a minute inside a while loop"
  - "gh run rerun refused while an attempt is in progress"
---

# Polling a Re-Run Workflow from a Pane

LEGION-94's acceptance was ten consecutive green attempts of one workflow run: after the push's
own attempt, `legion gh -- run rerun <run>` nine more times, each started only once the previous
attempt had completed, and for every attempt the `dispatch` job's conclusion plus the browser
step's `N passed / M skipped` summary line from its log. Two tool behaviours shaped how that has
to be done from a Legion pane.

## 1. `gh run view --job <id> --log` refuses a finished job while a later attempt runs

`legion gh -- run view --job <job-id> --repo <owner>/<repo> --log` reports
`run <run-id> is still in progress; logs will be available when it is complete` for a job that
completed in attempt *n* as soon as attempt *n+1* has been started. `gh` gates the log on the
**run's** status, and a re-run puts the run back in progress. The REST endpoint for the job itself
does not:

```sh
legion gh -- api --allow-escape-sequences \
  repos/<owner>/<repo>/actions/jobs/<job-id>/logs \
  | grep -E 'Running [0-9]+ tests|^[[:space:]]*[0-9]+ (passed|failed|flaky|skipped)'
```

`--allow-escape-sequences` is needed because the log carries ANSI colour. Job ids per attempt come
from `repos/<owner>/<repo>/actions/runs/<run>/attempts/<n>/jobs` — the attempt-scoped listing;
the unscoped `/runs/<run>/jobs` defaults to `filter=latest` and shows only the latest attempt
(`?filter=all` lists every attempt's jobs, undifferentiated).
So the protocol for attempt *n* is: `…/runs/<run>/attempts/<n>/jobs --jq '.jobs[] |
select(.name=="dispatch") | {id, conclusion, started_at, completed_at}'`, then the job's log
through `/actions/jobs/<id>/logs`. Read each attempt's log as soon as it completes, or read all
ten through the API at the end; do not plan on `run view --log` once the first rerun has started.

`gh run rerun` itself is refused while an attempt is in progress; poll
`repos/<owner>/<repo>/actions/runs/<run> --jq '{run_attempt,status,conclusion}'` until `status`
is `completed` before issuing the next one.

## 2. One credentialed call per poll — never a loop inside one bash command

The pane's credential is a grant the extension mints when a bash call starts, and it lives sixty
seconds (`GRANT_TTL_MS`; the mechanism and its other symptoms are in
[`legion/worker-pane-shell-gotchas.md`](../legion/worker-pane-shell-gotchas.md) §1). A single bash
call of the shape

```sh
while true; do legion gh -- api …/runs/<run> --jq .status | grep -q completed && break; sleep 30; done
```

works for its first minute and then every `legion gh` inside it answers
`Unable to redeem LEGION_GRANT (403)` until the call ends. Ten attempts at seven to eight minutes
each is over an hour of polling, so the loop form fails on the first attempt it waits for.

Rule: the wait and the credentialed read are separate bash calls. Sleep uncredentialed in one call
(`sleep 420`, no `legion gh` in it), then read the status in the next call — the extension mints a
fresh grant for each. The same holds for any `jj git push`, `legion gh`, or `legion threads
resolve` placed after a slow command in the same call: the grant's clock started before the slow
command did.

## 3. What the ten-attempt record looks like

For each attempt: the attempt number, the `dispatch` job id, its conclusion, `started_at`,
`completed_at`, the browser step's summary line, and whether the failure-trace upload step ran
(`skipped` on a green attempt). The run's `artifacts` listing (`…/runs/<run>/artifacts --jq
.total_count`) at the end is the check that no attempt uploaded traces — and the attempt-numbered
download refusing (`no valid artifacts found to download`) is the negative control that the
artifact naming from
[`github/run-download-by-name-picks-one-of-several-same-named-artifacts.md`](../github/run-download-by-name-picks-one-of-several-same-named-artifacts.md)
would have let a red attempt be fetched by name.
