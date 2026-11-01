---
title: "The daemon instance lock is a kernel flock, not a pid file: flock(2) through bun:ffi, a numeric O_CLOEXEC, an idempotent release"
category: daemon
tags:
  - instance-lock
  - flock
  - bun-ffi
  - o-cloexec
  - pid-file
  - single-instance
  - bun-compile
date: 2026-09-13
status: active
module: packages/daemon/src/daemon/instance-lock.ts
applies_when:
  - A process must be the only one of its kind per directory, project, or resource
  - A pid-file protocol needs a stale-lock takeover and you are about to write compare-and-swap around rename/link/unlink
  - You need a libc call Bun does not expose (flock, errno) from source and from a `bun build --compile` binary
  - A descriptor must not leak into spawned children and you reach for `fs.constants.O_CLOEXEC` under Bun
related_issues:
  - "LEGION-35"
  - "sjawhar/legion#1004"
---

# The Daemon Instance Lock Is a Kernel flock, Not a Pid File

## Context

`acquireInstanceLock(stateDir)` keeps two Legion daemons from sharing one project (they would
both bind the same durable JetStream consumer and clobber each other's state). Until LEGION-35 it
was a pid file: `writeFile(lockFile, pid, { flag: "wx" })`, and on `EEXIST` read the pid, check
`process.kill(pid, 0)`, and if dead `rename` the file to a unique stale name, re-read it, `link`
it back if it had changed under you, `unlink` it otherwise, retry up to eight times. Every step
was individually atomic and the whole was still wrong: a contender that read a dead pid could
move the file aside *after* another contender had already written a live lock, and until the
restore the name was absent for a third contender's exclusive create. Two daemons held the lock.
Under CPU/IO load with three contenders the pre-fix test failed 18 of 50 runs (see
`docs/solutions/testing/widen-the-contender-count-before-calling-a-race-unreproducible.md`).

## The decision: let the kernel hold the lock

The replacement has no protocol. It opens `<stateDir>/daemon.lock` once with
`O_RDWR | O_CREAT | O_CLOEXEC` — never `O_EXCL`, never `O_TRUNC`, never a temporary name — calls
`flock(fd, LOCK_EX | LOCK_NB)`, and on success truncates and writes its pid and keeps the
descriptor open for the daemon's lifetime. `release()` only closes the descriptor. The kernel
drops the lock the moment the process dies, so a crashed daemon leaves nothing to reclaim, and
the file is never renamed, hard-linked, or unlinked: **a fresh inode under the same name would be
an unlocked file**, so every contender's lock must attach to the one inode under that name. The
race test asserts the inode is unchanged after every round and every release.

Why `flock` and not `fcntl` record locks: a POSIX record lock belongs to the process and is
released when the process closes *any* descriptor of that file; `flock` belongs to the open file
description, which is what the tests exploit — contenders inside one test process conflict exactly
as separate daemons would, because each `acquireInstanceLock` call opens its own descriptor.

The pid in the file is informational — it names the holder in the refusal
`Legion daemon already running for this project (pid N, lock file X)` — and the lock is the
truth, not the text. Because the holder writes its pid three synchronous calls *after* locking, a
refused contender re-reads the file up to 20 × 25 ms before falling back to
`… (holder pid not recorded yet, lock file X)`. That bound is scheduling slack for an error
message, never a correctness step; under extreme host contention the fallback text can appear for
a live holder, which is a message-quality issue only.

## Reaching flock from Bun

Bun exposes neither `flock` nor `errno`; `bun:ffi` does:

```ts
const { symbols } = dlopen("libc.so.6", {
  flock: { args: [FFIType.i32, FFIType.i32], returns: FFIType.i32 },
  __errno_location: { args: [], returns: FFIType.ptr },
});
const errno = () => read.i32(symbols.__errno_location(), 0);
```

Read `errno()` immediately after the failed call, before `closeSync` or anything else on the
thread. `LOCK_EX = 2`, `LOCK_NB = 4`, `EWOULDBLOCK = 11` on Linux. Darwin's row
(`libSystem.B.dylib`, `__error`, `EWOULDBLOCK 35`, `O_CLOEXEC 0x1000000`) comes from the headers
and is unverified on a Mac; any other platform throws at acquire time naming the platform. A
missing library or symbol throws naming both — never a fall back to the pid-file protocol. The
constants are hard-coded from headers; when a new platform or architecture appears, re-run the
probe (open twice, flock both, expect `-1` and `errno === wouldBlock` on the second) before
trusting the row.

`dlopen` is lazy — inside `acquireInstanceLock`, memoized per process — because the compiled
`legion` CLI imports this module for every subcommand and must neither pay for nor risk a
`dlopen` at import. The tester proved the compiled form: `bun build --compile
--target=bun-linux-x64` of an entry importing the module produces a binary that acquires, a second
copy exits 1 with the exact refusal, and after `SIGKILL` of the holder a third copy acquires on the
same inode with no cleanup.

## Two Bun 1.3.14 surprises about O_CLOEXEC

1. **`fs.constants.O_CLOEXEC` is `undefined` under Bun.** `flags | undefined` is `flags | 0`, a
   silent no-op. The module spells the flag numerically per platform (Linux `0o2000000`).
2. **Bun's `openSync` does not set CLOEXEC on its own** (`/proc/self/fdinfo/<fd>` shows
   `flags: 0100002`); it does forward extra numeric open flags, so `| 0o2000000` yields
   `02100002`.

Bun's spawn closes non-stdio descriptors anyway, so a missing CLOEXEC bit is invisible to a
"child does not inherit it" test. The test therefore also asserts the bit on the live descriptor
(find it via `readlink /proc/self/fd/*` against the `realpath` of the lock file, parse the octal
`flags:` line of its `fdinfo`, and check `& 0o2000000`). The daemon's private tmux server and every
pane are children; none may hold the lock past the daemon's death.

## release() is idempotent

`index.ts` tears a failed boot probe down through `stop()`, which releases the lock, and then
rethrows into `startDaemon`'s catch, which releases again. A bare `closeSync(fd)` the second time
throws `EBADF` and *replaces the probe's error* — or, worse, closes an unrelated descriptor that
reused the number. A `released` flag makes the second call a no-op. Every teardown step in `stop()`
is idempotent for the same reason (see
`docs/solutions/daemon/launch-hold-serve-state-spawn-nothing-until-proven.md`).

A write failure after the lock (`ENOSPC`) closes the descriptor — dropping the lock — before
rethrowing, so a daemon that could not record its pid does not keep the lock.

## Reviewer's check for the never-rename invariant

```
grep -nE 'rename|unlink|link\(|O_EXCL|"wx"|O_TRUNC' packages/daemon/src/daemon/instance-lock.ts
```

must print nothing. Prose in the doc comment counts: "never renamed … or unlinked" matches
`rename`/`unlink`, so the comment says "its name is never moved, hard-linked, or removed".

## Related

- `docs/solutions/testing/bun-spawn-vfork-window-wait-for-the-child-to-pass-exec.md` — why a
  test releasing this lock right after `Bun.spawn` flaked, and the fix.
- `docs/solutions/testing/widen-the-contender-count-before-calling-a-race-unreproducible.md` —
  reproducing the pid-file race under load.
