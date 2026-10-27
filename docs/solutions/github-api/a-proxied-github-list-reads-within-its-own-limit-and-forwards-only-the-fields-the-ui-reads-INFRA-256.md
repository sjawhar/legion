---
title: "A proxied GitHub list reads within its own limit and forwards only the fields the UI reads"
category: github-api
tags:
  - github-api
  - dispatch
  - proxy
  - response-limit
date: 2026-10-26
status: active
module: packages/envoy/internal/dispatch/githubapi
related_issues:
  - "INFRA-256"
  - "sjawhar/legion#2247"
---

# A proxied GitHub list reads within its own limit and forwards only the fields the UI reads

Extends docs/solutions/github-api/a-capped-github-read-asks-for-the-smallest-media-type-validates-the-answer-and-every-fake-answers-by-accept-INFRA-390.md.

When GitHub has no smaller media type for a list the web app needs:

- Trimming in the proxy does not lift the client's cap: `githubapp.Client.Read` refuses a body
  over 1 MiB before the proxy sees it. Read that route with `ReadWithin` and a limit sized for a
  full page, then decode and write only the fields the web app reads.
- A body that does not decode is `502 GITHUB_UPSTREAM`; any other status passes through.
- Allow the route only with the query the web app sends, checked parameter by parameter: each
  name once, each value's exact shape, the required one present.
- Pin both in the proxy's tests: an answer carrying the extra objects comes back without them, and
  a full page over 1 MiB comes back trimmed (and fails with the old limit).
- A new GitHub read can need a new App permission (`actions/runs` needs Actions: read). Without it
  GitHub answers 403, which no test before the deploy shows: name it in the docs and the pull
  request's risks.

## Evidence

- `GET repos/sjawhar/legion/actions/runs?per_page=100` returned 1,291,575 bytes, about 12.8 KB a
  run (each run carries its repository, head repository and head commit with message and emails).
- `packages/envoy/internal/dispatch/githubapi/proxy.go` reads `actions/runs` within
  `workflowRunsResponseLimit` (16 MiB) and writes `total_count` plus `check_suite_id`, `event` and
  `name` per run; `TestProxyTrimsAPageOfWorkflowRunsLargerThanTheGeneralLimit` returns 502 with the
  limit set back to 1 MiB.
