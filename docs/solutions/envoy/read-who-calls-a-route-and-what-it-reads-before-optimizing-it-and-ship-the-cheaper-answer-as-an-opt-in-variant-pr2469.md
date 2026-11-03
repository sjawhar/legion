---
title: "Read who calls a route and what it reads before optimizing it, and ship the cheaper answer as an opt-in variant"
category: envoy
tags:
  - dispatch
  - api-design
  - etag
  - cpu
  - measurement
date: 2026-10-29
status: active
module: packages/envoy/internal/dispatch/api
related_issues:
  - "sjawhar/legion#2469"
---

# Read who calls a route and what it reads before optimizing it, and ship the cheaper answer as an opt-in variant

Extends docs/solutions/testing/measure-how-often-a-reuse-fixs-precondition-holds-under-the-replay-before-building-the-path-the-spec-names-pr2453.md.

- **Before you make an expensive answer cheaper, count who asks for it and read what each caller
  uses.**
  - **Who asks:** count the route's lines in production's access log by user agent and status,
    including conditional requests (304s). Dispatch's lines carry `route=`, `user_agent=`,
    `status=` and `bytes=`. They carry the path without its query (`accesslog.go`,
    `r.URL.EscapedPath()`), so read the query from the callers' code.
  - **What each uses:** find in each caller's code the fields it reads.
  - **What follows:** when one caller is all the traffic and reads a small part of the answer,
    the fix that removes the most is an answer of that part. It is not a faster encoder or an
    ETag-first path, which saves work only for requests that send `If-None-Match`.
- **Ship the cheaper answer as an opt-in parameter, and change the default answer by no byte.**
  Every other client then sees nothing change. Prove it with a base-versus-head diff of status,
  headers except `Date`, ETag and body over real-size data.
- **Give the variant its own ETag namespace.** Hash one extra line naming the variant, and leave
  the default answer's ETag input as it was. Without the line, a client that has cached one
  variant gets a 304 for the other.
- **Refuse a value the server does not know, and a repeated parameter, with `400`.** Read the
  parameter after authentication and before any database read. A misspelled value (`?versions=last`)
  that falls through to the default answer gives the caller the expensive answer with a 200, and
  the caller never learns that its request was ignored. Document the refusal in a contract change
  census.
- **Change the client that makes the traffic in the same pull request, in every copy it ships in.**
  The production gain then depends only on that client's release reaching its users.

## Evidence

- sjawhar/legion#2469 (spec departure 1 in its plan):
  - **The access log:** 2026-10-28 12:16–12:34Z had 479 requests on
    `route=/api/v1/issues/{key}/references`. All 479 came from the CLI (`Bun/*`), all were 200,
    none was conditional, and none came from a browser. The median answer was 2,431,963 bytes.
  - **What the CLI reads:** it prints each member's project, slug, depth and via, and never its
    `versions` (`packages/envoy-client/src/dispatch-execute.ts`).
- `GET …/references?versions=latest` keeps only each document's latest version.
  `referencesETag` writes `versions=latest\n` for that variant only. Over the load test's 1,000
  seeded issues, the full answers stayed byte-identical to main's, and the trimmed ones equalled
  the full answer cut to each latest version. The trimmed answers were 48.6 MB against 370 MB.
- The route's share of Dispatch's CPU fell from 8.75% to 1.70%, with every head run below every
  base run.
- Review round 1 found that `?versions=latest&versions=bogus` answered 200. A repeated parameter
  answers `400 INVALID_QUERY` since a3cd6ffc58f6, as `asks.go` (`since`) and `server.go`
  (`limit`, `offset`) already did.
- Testing round 1 found that `packages/claude-envoy` then committed its own copies of the CLI.
  They were rebuilt in the PR, and dropped from it after a forward merge once main started
  building them itself.
