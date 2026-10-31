---
title: "A status a GitHub client maps is mutated away in every package that fakes the route, each fake defaults to GitHub's answer, and each status in a doc comment names its source"
category: github-api
tags:
  - github-rest
  - test-fakes
  - mutation-check
  - doc-comments
  - dispatch
date: 2026-10-25
status: active
module: packages/envoy/internal/dispatch/githubapp
related_issues:
  - "INFRA-390"
  - "sjawhar/legion#2202"
---

# A status a GitHub client maps is mutated away in every package that fakes the route, each fake defaults to GitHub's answer, and each status in a doc comment names its source

Extends docs/solutions/github-api/a-capped-github-read-asks-for-the-smallest-media-type-validates-the-answer-and-every-fake-answers-by-accept-INFRA-390.md.

- **Prove a status mapping by removing it in every package whose fakes serve the route.** When a
  client method gains or changes a status mapping (`Ref`: 422 → `ErrNoBranch`), cut that one arm
  with `go test -overlay` and run every package that fakes the route, not only the client's own.
  A package that still passes has a fake answering some other status. Set
  `DISPATCH_TEST_DATABASE_URL` for that run: the importer and API tests skip without it and look
  green.
- **A fake's zero value answers what GitHub answers.** Model the real answer as the default, and
  make every other answer an opt-in field named for the case it models (a hidden repository), not
  a status number whose zero means something GitHub never sends.
- **Each status a doc comment names says where it came from:** observed live (the command or the
  pull request comment), or GitHub's docs (the page). When a fix round rewrites the comment, drop
  every clause you cannot source; never carry one forward.

## Evidence

- Round 2 (821c825d) answered the first retro's finding with a 422 row in githubapp's
  `fakeGitHub` and `TestRefMissingBranchIsErrNoBranch`, and proved it with the 422 arm cut,
  running `-run 'TestRef' ./internal/dispatch/githubapp/` only (round 2's `implement.json`; the
  round-2 review mutated the same package). The importer's `fakeSource` and the API's
  `fakeGitHubApp` still answered 404 for a missing branch, so `TestSyncMissingBranchIsAnAccessFailure`
  and `TestArchitectureSourceSyncRoute` passed with the arm cut. The architect sent the issue back
  for round 3 (d440d8bd) five minutes into the second retro. With both fakes answering GitHub's 422
  body, the same one-line overlay fails both tests (`status=200` instead of 409 `SOURCE_ACCESS`).
- After round 3, githubapp's `fakeGitHub.missingBranchStatus` still treats 0 as "404 with no body"
  (`client_test.go:54-57`), so `TestRefResolvesTheBranchCommit`'s missing-branch check
  (`client_test.go:673`) runs on a status GitHub does not send for a missing branch. GitHub's 422
  body is written out three times (`client_test.go:188`, `importer_test.go:57`,
  `architecture_source_test.go:92`).
- Round 2's rewritten `Ref` comment kept "also an empty repository or a malformed ref" for 422.
  Nobody had observed that; GitHub's docs give 409 for an empty repository, and round 3 dropped
  the clause. The 422 for a missing branch was observed live (tester comments 6400671435 and
  6401570230 on sjawhar/legion#2202).
