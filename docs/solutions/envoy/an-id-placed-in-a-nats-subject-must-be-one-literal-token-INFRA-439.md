---
title: "An id placed in a NATS subject must be one literal token"
category: envoy
tags:
  - nats
  - authorization
  - wildcards
  - listener
date: 2026-10-27
status: active
module: packages/envoy
related_issues:
  - "INFRA-439"
  - "sjawhar/legion#2228"
---

# An id placed in a NATS subject must be one literal token

## Rule

- Before you build a subject from a caller's id (`notifications.agent.<session_id>`) and grant
  "your own subject" by comparing subjects, refuse an id that is not one literal NATS token: one
  holding `.`, `*`, `>`, whitespace or a control character. Answer 400 naming the field.
- Every predicate that decides "this subject is the caller's own" also counts a wildcard id as no
  one's own, so a second route that forgets the 400 still refuses it.
- Judge a requested topic against the private subjects by wildcard overlap, not by prefix or
  equality: `notifications.>` reaches every session's inbox.

## Evidence

- In sjawhar/legion#2228, before 5ba62eb8, `POST /v1/replay` took `session_id: ">"`. Its own agent
  subject was then `notifications.agent.>`, which passed the "caller's own subject" check, so the
  caller read every session's stored inbox.
- 5ba62eb8 refuses such an id with 400 (`replaySessionToken` in
  `packages/envoy/cmd/listener/replay.go`). `bus.ReachesPrivateStreamSubject`
  (`packages/envoy/internal/bus/stream.go`) also treats an id containing `*` or `>` as no one's own.
  The subscribe route and the stored-row merge use the same overlap rule.
