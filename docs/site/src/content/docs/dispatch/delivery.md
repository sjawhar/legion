---
title: Delivery
description: The Delivery page's timeline and measures, the Pipeline page's step timings, what each figure counts, the targets, and the routes an agent reads.
sidebar:
  order: 7
---

The **Delivery** page shows how work reaches production. A timeline of every merge, deploy and
failed run sits under a panel of delivery measures. Both read the facts Dispatch stores from
GitHub, and both follow the same window and filters. The **Pipeline** page beside it shows how
long each step of CI and the deploy takes, day by day. Until the deploy repository and its
workflows are set in **Settings → Delivery timeline**, the Delivery page shows that form instead.

![The Delivery page's measures panel: six target cards over a strip of headline numbers](/legion/media/dispatch/delivery-measures.png)

## The window and filters

The page opens on the last seven days. The window, the search box and every filter (Repository,
Parent agent, Session, Dispatch issue, Priority, Component, Author, rework, and whether a pull
request has deployed) are written into the address, so a link shows the same figures to whoever
opens it. Dragging across the timeline narrows the measures to the stretch you dragged over; the
timeline itself keeps the whole window. **clear brush window** puts the measures back.

## The measures

Six cards across the top each judge one figure against its target: **met**, **missed**, or **no
data** when the window holds nothing to measure. Below them, one line carries the headline numbers.
**Definitions & alternatives** opens the other definitions of each figure and a per-day table,
newest day first; a day the window only partly covers is marked partial.

- **Deploy**: a run of the deploy workflow on `main` whose production job succeeded. A run counts
  on the day it started. Runs on other branches count in no figure.
- **Deploys a day**: deploys in the window divided by the window's length in days.
- **Lead time**: from a pull request's merge to the first successful production deploy that
  contains it. The cards show the median and the longest; the fold adds the 90th percentile and
  the time from the first commit and from opening the pull request.
- **Pull request open → merge**: the median and 90th percentile, over every pull request merged in
  the window.
- **Change failure rate**: confirmed failed changes over deploys (and, in the fold, over pull
  requests). Only a flag the dora role holder has confirmed counts; pending flags are shown
  separately, as the rate if every one were confirmed. Until flags are recorded the card says so,
  and the rate is zero.
- **Time to restore**: the median time from a broken pull request's deploy to its fix's deploy,
  over confirmed flags.
- **Rework share**: pull requests whose title starts with `fix`, `revert` or `hotfix`, over all
  pull requests merged in the window.
- **Deploy runs reaching production**: runs that reached the production job, over runs that
  concluded success or failure. A cancelled run is not an attempt, and the card counts it apart.
- **P0 issues with no owner**: open issues of priority P0 with neither a claim nor a route, in
  every project. It follows neither the window nor the filters, and each issue on the card links
  to it.

The filters narrow every figure drawn from pull requests: lead time, rework share and the per-pull
request failure rate. Deploys and runs follow the window alone, and a deploy counts as one that
shipped a pull request when it shipped any pull request merged in the window, whatever the filters.

## The targets

| Figure | Target |
| --- | --- |
| Deploys a day | at least 20 |
| Change failure rate, per deploy | under 5% |
| Merge → production, for every change | under 45 minutes |
| Pull request open → merge, median | under 60 minutes |
| Deploy runs reaching production | at least 90% |
| P0 issues with no owner | 0 |

"Under" is met strictly below the target. "At least" is met at the target.

## For agents

`GET /api/v1/delivery/measures?from=<RFC 3339>&to=<RFC 3339>` answers the same figures, the
targets, whether each is met, and the unowned P0 issues. It takes the timeline's parameters:
`from` and `to` (the last seven days when both are left out, at most 84 days apart), `q` for the
search, and any filter repeated for several values, such as `&repo=acme/widgets&priority=P0`. Any
signed-in person or agent token reads it. `GET /api/v1/delivery/timeline` carries the same
answer as its `measures` for its own window and filters, so an agent reading both needs only the
timeline. The page and the route compute from the same stored facts on every request, so they
always agree. `flags_source` says whether recorded flags fed the change failure rate; `none`
means no flags are recorded yet. The [HTTP API reference](/legion/dispatch/reference/api/) lists
it with every other route.

## Pipeline

The **Pipeline** page (`/pipeline`, **Pipeline** in the sidebar under **Delivery**) shows how
long each step between a pull request and production takes, and how often it succeeds, over the
last seven UTC days. Today is the last day, drawn faded because it is not over. The page follows
neither the Delivery page's window nor its filters.

![The Pipeline page: six timing cards over the slowest-jobs and failing-jobs tables](/legion/media/dispatch/pipeline.png)

Each run counts on the day it started, and each job on its run's day. Only a result that ended in
success or failure is timed; a timed-out result is a failure. A cancelled run or job is counted
beside the timed ones and kept out of the success rate, and anything else (skipped, still
running) is not counted at all. Success is success over success plus failure. Times are wall
clock; a step's median and 90th percentile are rounded to a tenth of a minute.

- **PR CI**: runs of the pull-request checks workflow on a pull request (GitHub's
  `pull_request` event; merge-queue runs are not counted), from the run's start to its
  completion.
- **Staging end-to-end gate**, **Deploy to staging** and **Deploy to production**: the deploy
  workflow's jobs named in **Settings → Delivery timeline**, in its push runs on `main`. A run
  started by hand, or on another branch, is not counted. The first two are optional settings; a
  pipeline that names neither has no such card.
- **Merge to production**: the delivery lead time, by the day each pull request merged. A pull
  request still waiting has none yet. It is judged on the seven days' longest change: the target
  is under 45 minutes for every change.
- **PR open to merge**: every merged pull request, deployed or not, by the day it merged. It is
  judged on the seven days' median: the target is a median under 60 minutes.

**PR CI: slowest jobs** ranks the checks workflow's jobs by their seven-day median, slowest first,
and shows five. A job needs 50 timed results in the seven days to be ranked, so a rarely run job
cannot top the table on a handful of results. Only jobs of runs that ended in success or failure
are counted.

**PR CI: jobs that failed in the last 24 hours** lists every job that failed in a run completed in
the last day, most failures first, with how many of its results there failed and a link to the run
of its latest failure. A cancelled run's jobs are not counted, and neither is the roll-up job
named in **Settings → Delivery timeline**, which only reports the other jobs' result and fails
whenever any of them does, nor a notification or guard step.

For agents, `GET /api/v1/delivery/pipeline` answers the same numbers. Its optional `at`
(RFC 3339) names the day the seven days end on and the moment the failing jobs look back from;
left out, it is now. Any signed-in person or agent token reads it.
