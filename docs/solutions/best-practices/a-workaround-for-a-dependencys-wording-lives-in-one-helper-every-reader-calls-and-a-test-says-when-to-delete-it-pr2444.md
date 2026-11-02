---
title: "A workaround for a dependency's wording lives in one helper every reader calls, and a test says when to delete it"
category: best-practices
tags:
  - error-handling
  - dependencies
  - workarounds
  - census
date: 2026-11-01
status: active
module: packages/envoy
related_issues:
  - "sjawhar/legion#2444"
---

# A workaround for a dependency's wording lives in one helper every reader calls, and a test says when to delete it

- Before you route one call of a dependency through a helper that corrects its output, list every call of that primitive in the package. Classify each by the data it reads, and route or justify each one in the same change. Write the list where a reviewer can check it.
- Give each path its own test. A test of one path does not show that the others go through the helper.
- Make the helper's removal condition executable: the test fails, saying nothing is left to test, once the dependency stops producing the case. A comment that says "delete when fixed" is not enough.

## Evidence

- ygo's release line from `v1.54.1-sami.1` refuses some cut updates as `crdt: invalid update: crdt: invalid update`. sjawhar/legion#2444 states that refusal once in `decodeStoredState` and `invalidOnce` (`internal/dispatch/docs/websocket.go`).
- Three rounds in a row each found another decode of stored state outside the helper. Tester round 10 found the room's load. Review round 7 found a socket's admission (`loadTree`) and a write's fork (`coldFork`). Tester round 12 found the fold's apply of each stored row (`fold.go`, red test `7c35d610`).
- `decode_reason_test.go` and `fold_reason_test.go` look for a cut that ygo refuses without a reason, and fail with "nothing here is tested" when they find none.
