---
title: "A cmd test of a background connect builds its client with main's options and reads failures from the log"
category: testing
tags:
  - entry-point
  - background-connect
  - test-helpers
  - mutation
date: 2026-10-28
status: active
module: packages/envoy/cmd/dispatch
---

# A cmd test of a background connect builds its client with main's options and reads failures from the log

Extends docs/solutions/daemon/a-dependency-made-optional-for-one-runtime-is-refused-at-construction-for-the-other-and-proven-through-the-entry-point.md.

- A test helper that says it connects "as main does" calls the same options function main calls,
  wrappers included. When main adds a wrapper (a background connect), every helper that built the
  options itself now tests a different code path, and CI stays green while the real path breaks.
- A background connect moves failures out of the constructor's return value. The helper waits for
  the background step to finish and reads a refusal from where that path reports it (the logged
  hook error), not from the returned error.
- Check the helper with a mutation of the production path it claims to cover: break that path
  (here, drop the settings and the keeper from the background branch) and require the tests to
  fail.

## Evidence

After the merge of sjawhar/legion#2390, `cmd/dispatch/main.go` connects Dispatch's publisher with
`serverClient(dispatchStreamOptions(boot, env)...)`, which adds `bus.WithConnectInBackground`. The
helper `startDispatchStream` in `cmd/dispatch/stream_replicas_test.go` still connected
synchronously, and a reviewer's mutation of the background branch (plain stream config, no keeper)
passed every test. Commit 8ec4e3643684 builds the helper with `serverClient(...)`, waits for the
background reconcile, and returns the error from the logged `envoy nats reconnect hook failed`
record (`reconcileFailure`); the same mutation now fails three tests.
