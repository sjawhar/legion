---
title: "A required check reported from several events keys its concurrency by event and runs only on events GitHub evaluates"
category: github
tags:
  - ci
  - concurrency
  - required-checks
  - github-actions
date: 2026-10-26
status: active
module: .github/workflows
related_issues:
  - "INFRA-256"
  - "sjawhar/legion#2247"
---

# A required check reported from several events keys its concurrency by event and runs only on events GitHub evaluates

- Trigger a required check only on events whose checks GitHub evaluates for a pull request:
  `push`, `pull_request`, `pull_request_review`, `pull_request_target`, `deployment`,
  `deployment_status`. An `issue_comment` or `pull_request_review_comment` run spends a runner and
  decides nothing: `issue_comment` reports on the default branch's latest commit, and a review
  comment's check is left out of the pull request's checks.
- To react to an inline review comment, use `pull_request_review`: GitHub sends a
  `pull_request_review` `submitted` event with an inline comment posted alone too.
- Key the concurrency group by event and pull request,
  `<workflow>-${{ github.event_name }}-${{ github.event.pull_request.number }}`, with
  `cancel-in-progress: true`. That holds one running and one pending run per event.
  - Never key it by `github.run_id`: nothing is deduplicated, and every review adds a run.
  - Never share one group across events: GitHub keeps the newest run per event, so a review run
    that cancels a running `pull_request` run leaves that event's newest check cancelled, which
    blocks the merge until the next push or edit.
  - Cancel in progress only when the newest run repeats every check the cancelled one would make
    (it reads live state, or its own event's payload is the newer one).
- An `issue_comment` run uses the default branch's workflow file, so a change to that trigger shows
  only after the merge: name its proof as the production check. A `pull_request` or
  `pull_request_review` run uses the branch's file, so a scratch pull request in the same
  repository proves those before the merge.

## Evidence

- `.github/workflows/pr-title.yaml` before sjawhar/legion#2247 grouped every non-`pull_request`
  run by `run_id`; at 11:29Z on 2026-10-26 the repository had 326 queued runs, most of them PR
  Title, and required checks waited for hours.
- Main's old workflow: one review with inline comments made run 40326253514
  (`pull_request_review`) plus 40326253720 and 40326255677 (`pull_request_review_comment`). An issue
  comment on scratch #2248 made run 40327858645 on main's `d59d119a`, not on the head.
- One shared group (`pr-title-<number>`, scratch #2250 at `672e3e1a`): review runs cancelled
  `pull_request` run 40329132323 and the rollup stayed `FAILURE`.
- Per-event group (scratch #2255 at `88e0dffe`): two reviews 1 s apart left 40343824683 cancelled
  and 40343828930 passed, the queued `pull_request` run 40343578035 was cancelled only by the next
  `pull_request` run, an inline comment alone made one `pull_request_review` run (40343748246), and
  the head had no `pull_request_review_comment` run.
