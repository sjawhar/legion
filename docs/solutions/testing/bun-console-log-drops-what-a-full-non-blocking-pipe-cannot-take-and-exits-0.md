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
  - "A Bun script writes a result something parses with console.log, whatever the script reads: a dependency that reads process.stdout (a colour library checking isTTY is enough), or an earlier Bun command on the same pipe that did, leaves the pipe non-blocking"
  - You are about to reach for Bun.write(Bun.stdout, ...) to "flush" a script's output
---

# Bun's console.log Drops What a Full Non-Blocking Pipe Cannot Take

## Symptom

At load ~190 on the devbox, `TestUpdateFromEmptyEqualsAuthoredByBrowser` (pmdoc) failed because
`bun run gen/decode.ts` exited 0 with its JSON cut mid-string. A footnote fixture failed on one
run and `empty-items-holding-the-next-line` on the next: lost output, not bad data.

## Mechanism

1. **The first read of `process.stdout` makes fd 1 non-blocking.** At Bun 1.3.14 the first read
   of the `process.stdout` getter switches fd 1 to `O_NONBLOCK` (`/proc/self/fdinfo/1` reads
   `flags: 01` before and `04001` after), and the first read of `process.stderr` does the same to
   fd 2, whether the read goes through the `process` global or an import. Importing `node:process`
   as an ES module reads both getters. Measured in script files with both fds on pipes: a bare
   default import, a namespace import and vfile's `export {default as minproc} from
   'node:process'` flip both; `process.stdout.isTTY` or a first `process.stdout.write` through the
   global flips fd 1, and `process.stderr` flips fd 2; `console.log`, `console.error`,
   `process.argv` and `require("node:process")` flip neither. That does not make a script that
   reads only `process.argv` safe: the flag can come from a dependency, or from another process
   on the same pipe (below), so a script that writes a result another process parses must not use
   `console.log`, whatever it reads. Any dependency can read `process.stdout` without saying so,
   and whether it does can depend on the environment.
   picocolors 1.1.1 reads `process.stdout.isTTY` unless `NO_COLOR` or `FORCE_COLOR` is set, and
   flipped fd 1 with `CI=true` alone but not with either of those. Here the import comes from
   `@legion/proof-editor/headless`, which imports `unified`, which imports `vfile` and its
   `node:process` re-export.

   The flag belongs to the pipe, not to the process. `O_NONBLOCK` is set on the open pipe, which
   every process writing to it shares, and Bun leaves it set when it exits, so every later writer
   to that pipe inherits it: the next command in the same `$(...)`, or a later command in the same
   `{ ...; } | reader` group or CI step. Measured at Bun 1.3.14 into a reader stalled for 2 s:
   `bun -e 'process.stdout.isTTY'; bun -e 'console.log("x".repeat(200000))'` delivered 65,536 of
   200,001 bytes and exited 0, where the same pair with `bun -e '1'` first delivered all 200,001;
   a command run between the two found `flags: 04001` on the pipe (`01` after `bun -e '1'`); and a
   second command that awaited `process.stdout.write`'s callback delivered all 200,001 with the
   flag set. How much the later command loses is the pipe's size when it writes, and on this box
   two sets of runs at the same Bun disagreed about that size, for a reason nobody found. The
   code-quality review's (https://github.com/sjawhar/legion/pull/1622#issuecomment-5920832376):
   with nothing between the two commands 4 of 6 runs lost output, and with any command between
   them the second delivered all 200,001 bytes in 9 of 9; `F_GETPIPE_SZ` read 1,048,576 after the
   first command exited where it had read 65,536 before (2 runs), one fresh pipe read 1,048,576
   before any Bun ran, and 2,000,001 bytes came through as exactly 1,048,576, exit 0. The
   implementer's (https://github.com/sjawhar/legion/pull/1622#issuecomment-5920990160): with
   `sleep 0.2`, `sh -c true` or a probe between the two, the second delivered 65,536 of 200,001 in
   10 of 10 runs and 65,536 of 2,000,001 in one, and `F_GETPIPE_SZ` never read anything but
   65,536. Both `strace`s of the first command show only `F_SETFL O_NONBLOCK`, nothing that
   resizes the pipe, and the resize did not reproduce on demand. So a probe between the commands
   may or may not show the loss, and a run that delivers everything disproves nothing: it shows
   only that the pipe was big enough that time. Auditing a script and its dependencies is not
   sufficient on its own: the script loses output to a process it knows nothing about. No
   consumer in this repository is hit today, because no parsed capture shares its pipe with an
   earlier Bun process: Go's `exec.Cmd` makes a pipe per command, a Bun parent that reads a
   child's output hands it a fresh socket, each `$(...)` that runs Bun holds one Bun process, and
   a Go program started on a non-blocking pipe waits rather than drops (the deep review's census,
   https://github.com/sjawhar/legion/pull/1622#issuecomment-5919556239).
2. **console.log on a full non-blocking pipe loses the rest.** It writes what the pipe has room
   for, gets `EAGAIN` for the remainder, drops it, and the script exits 0. strace of a 20,000-byte
   `console.log` into a one-page pipe: `write(1, …, 20000) = 4096`, `write(1, …, 15904) = -1
   EAGAIN`, `write(1, "\n", 1) = -1 EAGAIN`, `exit_group(0)`. On a blocking stdout the same
   `console.log` waits for the reader, which is why a plain scratch script never reproduces it.
   `console.error` loses output on fd 2 the same way: after a read of `process.stderr`, a slow
   reader got 65,536 of 200,001 bytes and the exit status was 0. The gen scripts' stderr is read
   only to show a failure, so there it can shorten a failure message but not change a verdict.
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

A dependency, or an earlier Bun command on the same pipe, can make stdout non-blocking, and
whether it does can depend on the environment, so a Bun script's stdout is not a safe place for a
result a program parses. `decode.ts` and `edit-blocks.ts` take an output path as their last
argument and `writeFileSync` their result there, as `differential.ts` already writes its result
to the file it is given, so no gen script a test reads returns anything over a pipe.
`writeFileSync` writes the whole result or throws: on a full disk the file takes what fits and the
call then throws `ENOSPC`, so the script exits 1. `fs.writeSync` returns a short count without
throwing: on a 16 KiB tmpfs it wrote and returned 16,384 of a 25,141-byte line. `differential.ts`
writes each line with it, so when the short write fell on its last line it exited 0 with that line
cut; on an earlier line, the next `writeSync` threw `ENOSPC` and it exited 1. It now checks the
count and throws, exiting 1 with the short write named. The Go runner (`genResult`,
`update_test.go`) reads the file after exit 0 and fails the test when the script printed anything
to stdout or exited 0 without writing the file, so a script moved back to `console.log` fails
every test that runs it.

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
