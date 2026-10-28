---
title: "A test of bad input sends raw bytes through the real read, not a fake that returns the parsed value"
category: testing
tags:
  - bun-test
  - fakes
  - parsing
  - bun-serve
date: 2026-10-28
status: active
module: scripts/dispatch
---

# A test of bad input sends raw bytes through the real read, not a fake that returns the parsed value

- A test of how the code classifies a bad body (not JSON, the wrong shape, an empty body) sends
  the raw bytes through the real read function. An injected fake that returns the
  already-parsed value skips the parse, so the test stays green when the parse regresses.
- Use a local `Bun.serve({ port: 0, fetch })` that answers the bad body, point the real client
  at `http://127.0.0.1:${server.port}`, and stop it in a `finally`.
- Before you trust the test, revert the parse handling once and watch the test go red.

## Evidence

- At 186e6b46 the test "a body that is not JSON is malformed" in
  `scripts/dispatch/__tests__/meaning-search-check.test.ts` passed a string to the fake
  `coverage()`, so `readJson` never ran. If `readJson` threw on bad JSON again, the script would
  print `transport` and the test would still pass.
- 8ab64781 replaced it with "a body that is not JSON is malformed on the real read" (line 531 at
  7e042950). It points `liveDispatch` at a local `Bun.serve` that answers HTML. Making
  `readJson` throw on bad JSON turned that test red, and restoring the code turned it green.
