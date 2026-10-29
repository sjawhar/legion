---
title: "A runtime's own spawn timeout is no bound on a child: bound it with an async timer that kills and cancels the read"
category: testing
tags:
  - bun
  - subprocess-timeout
  - gvisor
  - claude-envoy
date: 2026-10-29
status: active
module: packages/claude-envoy
related_issues:
  - "sjawhar/legion#2186"
---

# A runtime's own spawn timeout is no bound on a child: bound it with an async timer that kills and cancels the read

- When production code relies on a child finishing within a bound, do not make the runtime's
  synchronous `timeout` option the only bound. Bun 1.3.14's `spawnSync`, `Bun.spawnSync` and
  `execFileSync` sometimes return only when the child exits on its own (measured on linux
  4.19.0-gvisor: about half of eight runs of a 1 s timeout on `sleep 5` returned after 5 s with
  status 0).
- Spawn asynchronously and race the child's answer against a timer. On expiry send `SIGKILL` (a
  wrapper script may ignore `SIGTERM`), cancel the stdout reader, and resolve without waiting for
  the pipe: a grandchild that outlives the killed child keeps the pipe open, and a pending read
  keeps the process alive until it ends.
- A flake that only a host shows still justifies this change when the primitive that flakes is
  the one guarding production; keep the test's bound and assertions as they were.

## Evidence

`packages/claude-envoy/src/session-title.ts` (`queryTmux`) bounds the channel server's start-up
tmux query. With `execFileSync`'s `timeout`, `tests/session-title.test.ts`'s bound row failed 5 of
8 runs in a worker pod; after the async timer (commit 66f1d28f) it passed 16 of 16, with the same
assertions. Measured while writing the fix: without cancelling the reader the child process lived
about 5.1 s; with it, about 1.1 s.
