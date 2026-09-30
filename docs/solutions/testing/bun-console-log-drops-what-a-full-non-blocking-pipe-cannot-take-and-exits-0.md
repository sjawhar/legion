---
title: "Bun's console.log drops what a full non-blocking pipe cannot take and still exits 0: a Bun script hands its result back in a file the caller names, not on stdout"
category: testing
tags:
  - bun
  - console-log
  - pipes
  - non-blocking-io
  - truncated-output
  - flaky-tests
  - load-dependent
date: 2026-09-30
status: active
module: packages/envoy/internal/dispatch/pmdoc/gen
applies_when:
  - A Go (or any) test runs a Bun script and parses what it prints, and fails now and then on invalid JSON or a bad encoding while the script exited 0
  - The failing case changes between runs and is always one of the larger outputs
  - A Bun script's output is cut at a round size (4096, 8192, 65536 bytes) only on a loaded machine
  - You are about to reach for Bun.write(Bun.stdout, ...) to "flush" a script's output
---

# Bun's console.log Drops What a Full Non-Blocking Pipe Cannot Take

## Symptom

At load ~190 on the devbox, `TestUpdateFromEmptyEqualsAuthoredByBrowser` (pmdoc) failed because
`bun run gen/decode.ts` exited 0 with its JSON cut mid-string. A footnote fixture failed on one
run and `empty-items-holding-the-next-line` on the next: lost output, not bad data.

## Mechanism

1. **fd 1 is non-blocking.** `@legion/proof-editor/headless` imports `unified`, which imports
   `vfile`, whose `#minproc` is `export {default as minproc} from 'node:process'`. At Bun 1.3.14,
   loading `node:process` as an ES module switches the script's stdout to `O_NONBLOCK`:
   `/proc/self/fdinfo/1` reads `flags: 01` before and `04001` after. A bare `import process from
   "node:process"` does the same, and so does the first `process.stdout.write`; using the global
   `process` does not.
2. **console.log on a full non-blocking pipe loses the rest.** It writes what the pipe has room
   for, gets `EAGAIN` for the remainder, drops it, and the script exits 0. strace of a 20,000-byte
   `console.log` into a one-page pipe: `write(1, …, 20000) = 4096`, `write(1, …, 15904) = -1
   EAGAIN`, `write(1, "\n", 1) = -1 EAGAIN`, `exit_group(0)`. On a blocking stdout the same
   `console.log` waits for the reader, which is why a plain scratch script never reproduces it.
3. **Pipes on a busy box are small.** A pipe holds 64 KiB, but once a user's pipes pass
   `/proc/sys/fs/pipe-user-pages-soft` (16384 pages) Linux gives new ones two pages. On the
   devbox `F_GETPIPE_SZ` on a fresh pipe read 8192 at some moments and 65536 at others. Every
   fixture that failed has ProseMirror JSON past 8 KiB (`empty-items-holding-the-next-line` 11.7
   KiB; of the footnote fixtures, `footnote-placement` 12.2 KiB and
   `empty-footnote-definitions-in-quoted-items` 8.3 KiB pass it), and the largest of the 81 is 24
   KiB, under 64 KiB. So, inferred rather than watched in the failing run: the test binary's
   reader goroutine ran late, and a 12 KiB write met a full 8 KiB pipe.

## Bun.write(Bun.stdout, ...) is not the fix

On the same non-blocking pipe, `await Bun.write(Bun.stdout, text)` writes 4096 bytes, gets
`EAGAIN`, then another thread writes the whole text again from its first byte (strace:
`write(1, …, 20001) = 4096`, `write(1, …, 15905) = -1 EAGAIN`, then from a second thread
`write(1, …, 20001) = -1 EAGAIN`) and the promise never settles: the script hung for as long as it
was watched with its reader draining the pipe. Had that second write succeeded, the reader would have
received the first 4096 bytes twice.

## Fix: hand the result back in a file

`decode.ts` and `edit-blocks.ts` take an output path as their last argument and `writeFileSync`
their result there, as `differential.ts` and `gen.ts` already did, so no gen script a test reads
returns anything over a pipe. A regular file takes the whole write, and a write that fails throws
and exits non-zero. The Go runner (`genResult`, `update_test.go`) reads the file after exit 0 and
fails the test when the script printed anything to stdout or exited 0 without writing the file,
so a script moved back to `console.log` fails every test that runs it.

A Bun script that has to print its result awaits `process.stdout.write`'s callback: Bun keeps
what the pipe cannot take and writes it as the reader drains, and the callback runs after the
last byte (measured: 9.6 s after the write, against a reader that waited 10 s) or with the error
that stopped it. Never `Bun.write(Bun.stdout, ...)`.

## Reproduce it without load

Do not load the box. Shrink the pipe and stall the reader:

- `fcntl(F_SETPIPE_SZ, pagesize)` on the pipe before the child starts, so any output past one
  page must wait for the reader.
- Read nothing until the child has exited, or the pipe has been full (`TIOCINQ`, Linux's
  `FIONREAD`, equals `F_GETPIPE_SZ`) for a second.

A script that drops what does not fit exits with exactly one page read; one that waits is still
there, blocked, and delivers everything once the reader drains. A Go harness built this way read
exactly 4096 bytes from `decode.ts` and `edit-blocks.ts` on 7 of 7 runs while they printed with
`console.log`, received everything once they awaited `process.stdout.write`, and found the
`Bun.write` version still running a minute after it began draining the pipe.

## Related

- `docs/solutions/testing/a-loaded-devbox-stretches-every-wall-clock-budget-in-the-envoy-and-pi-envoy-suites.md`:
  the load that exposed this. Here the budget was the pipe, not a timer.
