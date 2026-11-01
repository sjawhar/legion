---
title: "A test that closes its socket right after writing loses the frame the server has not read"
category: testing
tags:
  - websocket
  - flaky-test
  - tcp
  - dispatch
date: 2026-11-01
status: active
module: packages/envoy
related_issues:
  - "sjawhar/legion#2391"
---

# A test that closes its socket right after writing loses the frame the server has not read

- A client write returns once the bytes are in the kernel, not once the server has read them. When the client then closes a socket that still holds bytes it has not read itself (a frame the server sent it), the kernel resets the connection, and the server's read loop can end on the reset without reading the frame still in its receive buffer.
- So a test that writes an update and then closes the socket must first wait for the server to have applied the update. Read it back through the server's own surface (an API read of the document, a row), with the old timeout kept only as an outer bound.
- A failure with this shape shows the server still holding the old state at the deadline, and often a server-side `write ... broken pipe` from the moment the client left.

## Evidence

Pull request sjawhar/legion#2391, commit 57b0a928. `TestEditNoticeRestart` (`packages/envoy/cmd/dispatch/edit_notice_restart_test.go`) passed 99 of 100 runs. The failing run timed out on `the person's settled edit wrote no edit notice within 10s`. An instrumented run showed the document still at `before\n`, version 1, at the deadline, with the server logging `write to peer failed; closing ... broken pipe`. The server applies a peer's update on that peer's read loop, and the room settles when that loop ends, so a lost frame left the settlement with nothing to write. The test now waits for `GET /api/v1/artifacts/{id}/text` to read the edited text before the editor closes, and passed 100 of 100 runs.
