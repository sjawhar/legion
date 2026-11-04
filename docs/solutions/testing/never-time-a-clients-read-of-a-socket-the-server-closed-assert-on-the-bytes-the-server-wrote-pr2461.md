---
title: "Never time a client's read of a socket the server closed; assert on the bytes the server wrote"
category: testing
tags:
  - sockets
  - websocket
  - tcp
  - flaky-tests
  - ci-only
date: 2026-11-04
status: active
module: packages/envoy/internal/dispatch/docs
related_issues:
  - "sjawhar/legion#2461"
---

# Never time a client's read of a socket the server closed; assert on the bytes the server wrote

- A bound on when a slow client sees a closed socket's end measures the kernel, not the server. Once the server closes, its kernel keeps the socket as an orphan in FIN-WAIT-1 and drains what was queued into the receive window the client set at connect (a `SO_RCVBUF` raised after connect does not reopen it), one segment per zero-window probe. On a Linux runner that took about 40 s for 1.6 MB.
- Record the server's side instead: wrap the test server's listener so each accepted connection records its writes and its `Close`. Then assert what the server promises: it closed within the bound, it wrote nothing but the close frame after the event, and the byte stream it wrote is whole frames that end on the expected close code or at a cut. TCP delivers exactly those bytes, in order, and then the FIN.
- A Legion pod's gVisor network stack drains the same bytes in milliseconds, so such a test passes locally and fails only on a stock Linux runner.
- When a failure happens only in CI and the pod cannot reproduce it, push a temporary commit that logs the mechanism's own numbers (bytes written and when, the close time, `ss -tinoem` of both sockets), read them in the failed job's log, and remove that commit's code in the fix. Do not widen the bound or re-run until the numbers explain the failure.

## Evidence

- `TestARevokedSlowPageEndsOnACodeItReconnectsOnAndSyncsWhole` (`packages/envoy/internal/dispatch/docs/schema_admission_test.go`) passed 100 of 100 in the pod and failed in CI's `envoy-go` and `envoy-go-race-docs` with an i/o timeout after its 30 s drain.
- Diagnostics commit c39e75af, read in CI run 41375832410: the server wrote 25 bytes after the revoke (the 4412 close frame) and closed 0 s after it; `ss` showed its socket in FIN-WAIT-1 with no process and Send-Q 1,642,058, still 482,890 at 30 s; the page read about 41 KB/s with Recv-Q 0.
- Fix, 31293b85 (diagnostics removed): a `recordingListener`, `requireNothingWrittenAfterTheRevoke` and `readServerStream`, with a table test of the stream reader. It still fails with the revoke's cancel removed, with the cancel delayed 1.5 s, and on a wrong close code.
