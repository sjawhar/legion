---
title: "A wait added to a shared send path is charged to every test of the package and to every client deadline on the path"
category: testing
tags:
  - go-test-timeout
  - retry-window
  - test-knob
  - client-deadline
  - envoy-go
date: 2026-10-30
status: active
module: packages/envoy/internal/dispatch/api
related_issues:
  - "INFRA-562"
  - "sjawhar/legion#2387"
---

# A wait added to a shared send path is charged to every test of the package and to every client deadline on the path

Extends docs/solutions/testing/an-envoy-go-timeout-at-10m0s-in-dispatch-api-is-the-whole-package-near-go-tests-default-limit-on-ci-too-not-the-test-it-names-INFRA-391.md.

That note reads a `test timed out after 10m0s` in `internal/dispatch/api` as the package's size and
says to re-run the job. When your branch added a wait to a path many tests take, the timeout is
yours and a re-run only repeats it.

- **Time the package, not your tests.** Before you push a retry window, backoff or sleep on a
  function many existing tests reach, run the whole package at the merge base and at your head. The
  budget is `go test`'s 10-minute default less what the package already costs on CI, not what your
  new tests add: a per-call wait multiplies by every test that meets the failure it waits out.
- **Make the wait a setting whose zero value is production's.** A field on the server's
  dependencies, read with `cmp.Or(field, productionConstant)`, that no deployment's construction
  sets; the package's test rig defaults it to a short value that still allows one retry. Only the
  tests that prove the window itself pass the production value. Re-time the package after.
- **Bound the worst case against every deadline on the path.** The answer time is the window plus
  the inner client's own timeout for the try that starts just inside it. Compare it with each
  caller's abort (a CLI's request deadline, a browser test's assertion timeout, a proxy): a caller
  that aborts after the write committed sends it again. If one request waits for N items, wait for
  them together under one window, never N windows in turn, and pin that in a test that names the
  caller's deadline. Raise the browser specs that assert a failure, since a failure now settles one
  window later.
- **State the bound where a lease holds it.** A loop that runs such sends under a lease holds that
  lease for up to `batch × (window + client timeout) / concurrency`; write the figure beside the
  batch constant.

## Evidence

`listenerTakeoverWindow = 12 * time.Second` (`packages/envoy/internal/dispatch/api/envoy_resolve.go`)
made every existing api test whose send met `no live session` or a refused connection wait the
window: sixteen tests went from 13 s at the merge base to about 214 s, and `envoy-go` failed with
`FAIL github.com/sjawhar/envoy/internal/dispatch/api 600.169s` on a package that took 462.6 s on
main. Making it `Deps.ListenerTakeoverWindow` (`api/server.go`), defaulted to 750 ms by
`api/server_test.go`, brought the sixteen back to 13.1 s and the package back under the limit
(`ok … 512.639s`). A comment mentioning two ended sessions first answered after 23.6 s, two windows
in turn; `TestACommentMentioningSeveralEndedSessionsAnswersWithinOneTakeOverWindow`
(`api/comment_mention_takeover_test.go`) pins one window against the dispatch CLI's 60 s abort
(`packages/envoy-client/src/dispatch-http.ts`). The delivery sweep's worst-case pass under the window
is stated beside `deliverySweepBatch` in `api/delivery_sweep.go`. Pull request sjawhar/legion#2387.
