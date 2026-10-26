---
title: "An event stream writes a byte with its headers, because Firefox opens a fetch only on its first body byte"
category: envoy
tags:
  - sse
  - event-stream
  - firefox
  - reconnect
  - fetch
date: 2026-10-26
status: active
module: packages/envoy/internal/dispatch/api/events.go
related_issues:
  - "INFRA-195"
  - "sjawhar/legion#2205"
---

# An event stream writes a byte with its headers, because Firefox opens a fetch only on its first body byte

## Rule

- A streamed response a browser reads with `fetch` (`text/event-stream`, or any long-lived body)
  writes and flushes one no-op body byte with its headers: for SSE, a comment line such as
  `: open\n\n`, which every SSE reader skips. Firefox resolves the `fetch()` of such a response only
  once a body byte arrives; Chromium resolves it on the headers.
- Anything the client gates on the fetch resolving (its `onOpen`, the refresh a reconnect owes every
  query, a "Reconnecting…" state) otherwise waits for the first event or heartbeat. A stream with no
  backlog and nothing new to send gives Firefox nothing until then.
- Pin it on the server: a test that runs the stream with a heartbeat far longer than its deadline
  (a minute against 2 s) and requires the first body bytes to arrive, cold and resuming at the head.
  Pin the browser side with the engine that showed it: a row that needs the reconnect to finish runs
  in the `firefox` project.
- The signature in a Playwright trace: a row that fails in Firefox only, after a server-side stream
  end; the reconnect's `GET` ends with status `-1` (no response recorded), and the refetch it would
  have caused never appears. The server's own log shows it flushed the headers.

## Evidence

- Dispatch's browser suite truncates the event log between scenarios, and the server's stream relay
  answers a reset log by ending every open stream (`event_stream_relay.go`, `resetCursor`). The
  page's reconnect (`web/src/api/sse.ts`) got headers flushed at once and no body until the 15 s
  heartbeat (`defaultStreamHeartbeat`), so in Firefox `readEventStream` (`web/src/api/live.ts`)
  never called `onOpen`, `refreshEverything` never ran, and a session's answer to a model switch,
  committed while the stream was down, never showed. Rows 11a and 11d of
  `e2e/keyboard-session-pickers.e2e.ts` failed in CI's Firefox project only (job 121364414957).
- Scratch instrumentation (a `console.warn` in `settle`/`open`, a stderr line in `streamEvents`)
  showed the stream ending cleanly about 50 ms after the switch's POST, the server flushing the
  reconnect's headers at `since=3`, and the page never reaching `onOpen`.
- Fix (`events.go`, f563ddfd): `: open\n\n` written and flushed after `WriteHeader`. With it, the
  spec at `--project=firefox --repeat-each=4` went from 7 failed of 20 to 20 passed, with the event
  log reset mid-row 19 times; the whole `firefox` project passed 51 of 51.
  `TestSSEStreamOpensAtOnceForABrowserWaitingOnItsFirstByte` fails without the write
  (`first body bytes = "" after 2s`).
- The same gap reaches production: after any reconnect, a Firefox reader stayed on "Reconnecting…"
  and kept stale views for up to 15 s after the server had answered.
- The agent stream (`agent_stream.go`) had no such gap: it writes its `responder` event right after
  its headers.
