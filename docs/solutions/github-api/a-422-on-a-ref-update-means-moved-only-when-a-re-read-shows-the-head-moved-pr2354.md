---
title: "A 422 on a ref update means the branch moved only when a re-read shows the head moved: otherwise fail at once with GitHub's answer"
category: github-api
tags:
  - github
  - git-refs
  - "422"
  - retry
  - non-forced-update
date: 2026-10-28
status: active
module: packages/daemon/internal/handoffimport
---

# A 422 on a ref update means the branch moved only when a re-read shows the head moved

## The rule

1. When a non-forced `PATCH /repos/{owner}/{repo}/git/refs/heads/<branch>` (`force: false`)
   answers 422, read the branch's head again before you decide anything. GitHub answers 422 both
   for "not a fast forward" and for a ruleset or validation refusal.
2. Retry, from the new head, only when the head you read now differs from the one your attempt
   started from, or the branch is gone. Otherwise fail at once and quote GitHub's answer body to
   the operator.
3. Bound the retries, since each one writes a new commit object that nothing will reference.
4. Test the case where the update answers 422 and the head stayed where it was: expect one update,
   no retry, no second commit, and a failure that quotes GitHub's message.

The same holds for any write whose error status has more than one cause: re-read the resource,
and act on what changed.

## Evidence

- sjawhar/legion#2354, review round 1: `legion handoffs import` read every 422 from the ref update
  as "the branch moved". A refusal was retried three times, each retry left an orphan commit, and
  the operator saw "moved under the removal commit 3 times" instead of GitHub's reason.
- The fix, commit f887c783: `handoffimport.go` `move` reads the branch again with `ghbranch.Head`
  and compares the SHA. The unit test `TestARefusalThatMovedNothingFailsAtOnceWithGitHubsAnswer`
  covers it.
