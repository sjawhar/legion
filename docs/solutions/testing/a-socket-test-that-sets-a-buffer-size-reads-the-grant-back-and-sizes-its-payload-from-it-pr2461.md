---
title: "A socket test that sets a buffer size reads the grant back and sizes its payload from it"
category: testing
tags:
  - sockets
  - so_sndbuf
  - linux
  - gvisor
  - flaky-tests
date: 2026-11-04
status: active
module: packages/envoy/internal/dispatch/docs
related_issues:
  - "sjawhar/legion#2461"
---

# A socket test that sets a buffer size reads the grant back and sizes its payload from it

- Never hard-code a payload that must fit in a socket buffer you set. Ask for stock Linux's `net.core.wmem_max` (212992), read `SO_SNDBUF` back (`syscall.GetsockoptInt` through the connection's `SyscallConn`), and size the payload from what was granted, with headroom.
- Stock Linux caps a `SO_SNDBUF` request at `wmem_max` and grants twice what it kept (425984 for that request); a host with a raised cap grants twice the request; gVisor (a Legion pod) applies no cap. An explicit `SO_SNDBUF` also turns off send-buffer autotuning, so the grant is all the socket will ever hold. A test that passes in a pod and on a CI runner with raised limits can fail on every run on a stock kernel.
- When the grant is too small for the payload the test needs, fail naming the grant rather than shrinking the payload below what the assertion requires.
- To prove the test on small grants from a pod that cannot change `wmem_max`, lower the request in a scratch copy and confirm the read-back.

## Evidence

- `TestARevokedSlowPageEndsOnACodeItReconnectsOnAndSyncsWhole` (`packages/envoy/internal/dispatch/docs/schema_admission_test.go`) asked for a 4 MB send buffer in 31293b85 and seeded a 1.68 MB sync message. A stock kernel granted 425984 bytes, so the server's one write blocked and the test failed on every run there (reproduced by the reviewer by forcing that grant), while it passed in the pod and in CI.
- Fix, 7cb8e306: `serverSendBufferRequest = 212992`, `setSendBuffer` reads the grant back, `sendBufferGrant` probes it on a loopback connection before seeding, and `slowPageMessageSize` makes the message a quarter of the grant (never under 96 KB). It passes 5 of 5 at grants 425984, 212992 and 196608.
