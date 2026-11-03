---
title: "A comment that names the main code it relies on is read at every forward merge"
category: best-practices
tags:
  - forward-merge
  - invariants
  - comments
date: 2026-10-31
status: active
module: packages/envoy
related_issues:
  - "sjawhar/legion#2391"
---

# A comment that names the main code it relies on is read at every forward merge

- When your code is correct only because of a property of code you do not own, write a comment that names that code and the property. Say what must change if the property goes, for example "this read is safe only while `events.Broker.Append` holds the commit-order lock; a change that removes the lock must move this sweep to a safe horizon".
- At each forward merge, list the files main changed in the window, and read every such comment whose named code is among them. A merge that compiles, has no conflict in your files and passes your tests can still remove the property.
- When main removed it, the round is a full round, whatever the conflicts were: fix your code in the same pull request, with a test that fails on the merge without the fix.

## Evidence

Pull request sjawhar/legion#2391, merge 6cf038e3 and fix e91f96bf. Main's #2392 removed the commit-order lock from `events.Broker.Append`. This branch's suggestion sweep read `events where id > cursor`, and its comment in `api/suggestion_outcomes.go` named that lock as the only reason the read skipped nothing. #2392 changed `events/broker.go`, so the comment was read at the merge. That merge would otherwise have been treated as a confirmation round, because its conflicts were only in a relay, a test, docs and a constant. The fix caps the read at the safe horizon. `TestTheSuggestionSweepTrailsTheSafeHorizon` failed on the merge without it: `cursor = 6 after a pass run while event 5 could still commit`.
