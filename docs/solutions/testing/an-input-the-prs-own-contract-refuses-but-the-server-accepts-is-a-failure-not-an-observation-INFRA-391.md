---
title: "An input the PR's own contract refuses but the server accepts is a failure, not an observation"
category: testing
tags:
  - tester
  - classification
  - contract
  - review-rounds
date: 2026-10-26
status: active
module: skills/legion-worker
related_issues:
  - "INFRA-391"
  - "sjawhar/legion#2209"
---

# An input the PR's own contract refuses but the server accepts is a failure, not an observation

- A probe can show the server accepting an input that the pull request's own text says is refused
  or invalid: a route description, help text, CHANGELOG or census row. Record that as a failure in
  the test handoff, fail the phase, and write the red test. Do not put it under "observed, not
  defects".
- Before you classify a probe result, read the contract the PR wrote for that route. If the text
  and the behaviour disagree, one of them is wrong, and the implementer decides which.

## Evidence

On sjawhar/legion#2209 the route description said a source save proves "the branch exists". The
tester's round-2 run (https://github.com/sjawhar/legion/pull/2209#issuecomment-6406140808) showed
saves on `main~1`, a full SHA and `refs/heads/<branch>` answered `200`, stored, and wrote the
update event. It filed them as "Observed, not defects". The reviewer raised the same behaviour as
the round's Important finding, citing that note. That cost one more implementing, testing and
review round. The fix is in
docs/solutions/github-api/a-branch-exists-only-by-its-exact-ref-read-commits-resolves-any-commit-ish-and-a-fake-never-echoes-the-name-INFRA-391.md.
