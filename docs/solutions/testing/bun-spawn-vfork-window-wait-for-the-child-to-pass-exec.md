---
title: "Bun.spawn returns before the child's exec closes inherited descriptors: wait for a line the child prints before asserting per-descriptor state"
category: testing
tags:
  - bun-spawn
  - vfork
  - close-on-exec
  - flock
  - flaky-tests
  - file-descriptors
  - proc-fdinfo
date: 2026-09-13
status: active
module: packages/daemon/src/daemon/__tests__/instance-lock.test.ts
applies_when:
  - A test spawns a child and then, right away, asserts something about file descriptors, kernel locks, or /proc/<pid>/fd
  - A per-open-file-description lock (flock) is released immediately after a Bun.spawn and re-acquired
  - A test that passes 98% of the time under load reports a lock or descriptor "still held" by the test process itself
  - You want to prove a descriptor is close-on-exec and a child-inheritance check keeps passing regardless
related_issues:
  - "LEGION-35"
  - "sjawhar/legion#1004"
---

# Bun.spawn Returns Before the Child's Exec Closes Inherited Descriptors

## Symptom

The instance-lock test "is never inherited by a spawned process" did
`Bun.spawn(["sleep", "30"])`, then `lock.release()`, then `acquireInstanceLock(tempDir)` again —
and failed 2–3% of runs under load with

```
Legion daemon already running for this project (pid <the test process's own pid>, lock file …)
```

The module was correct: the descriptor was `O_CLOEXEC` (fdinfo `02100002`), and once the child's
`execve` completed it held nothing. The test's *predicate* was wrong.

## Mechanism (tester's strace and kernel reading)

`Bun.spawn` uses `vfork`. The child marks inherited descriptors close-on-exec with
`close_range(3, ~0, CLOSE_RANGE_CLOEXEC)` — it does not close them — and relies on `execve` to
close them. The kernel resumes the vfork parent from `exec_mmap()` (`begin_new_exec`,
`fs/exec.c`), which runs **before** `do_close_on_exec()`. So `Bun.spawn` can return while the
child still holds a dup of the parent's descriptor. `flock` is per open file description, so the
parent's `close` drops nothing until the child's dup closes at the tail of its exec. Measured
directly: in 3 of 2000 spawns the parent's CLOEXEC lock descriptor was still listed in
`/proc/<child>/fd` immediately after `Bun.spawn` returned. Instrumented copies of the test ran
100/100, 200/200, and an in-process loop 0/1300 without reproducing — the window is narrow and
scheduler-dependent, which is exactly why "the spawn call returned" is not a safe predicate for
"the child is past exec".

## Fix: observe the child past exec, never assume it

Spawn something that prints once it is running, and read that line before touching the state:

```ts
const child = Bun.spawn(["sh", "-c", "echo ready; exec sleep 30"], {
  stdout: "pipe",
  stderr: "ignore",
});
children.push(child);
expect(await firstLine(child, "spawned child")).toBe("ready");
await lock.release();
const again = await acquireInstanceLock(tempDir); // an inherited dup would refuse this
```

`sh` printing anything proves its `execve` completed, so every inherited dup is gone; `exec
sleep 30` then keeps a live child for the rest of the test. `firstLine` reads the stdout stream
until the first `\n` and throws with the exit code if the stream ends first (a child that died
before reporting is the error, not a hang). No `sleep`, no retry, no polling of `/proc`: one
observation from the child. The same helper serves the fixture holder's `locked` line.

The rule generalizes past flock: any assertion about descriptor tables, kernel locks, socket
ownership, or `/proc/<pid>/fd` made right after a spawn is racing the child's exec tail. Make the
child tell you it is running.

## Assert close-on-exec directly, because spawn hygiene hides its absence

Bun's spawn closes every non-stdio descriptor in the child regardless of CLOEXEC, so a
"child does not hold the descriptor" test passes even when the flag was never set. The test
therefore also checks the bit on the live descriptor (Linux only, gated on `process.platform`):

```ts
const lockFile = path.join(realpathSync(tempDir), "daemon.lock"); // /tmp may be a symlink
const held = readdirSync("/proc/self/fd").filter((fd) => {
  try { return readlinkSync(`/proc/self/fd/${fd}`) === lockFile; } catch { return false; }
});
expect(held).toHaveLength(1);
const [, flagsOctal = "0"] =
  /flags:\s+(\d+)/.exec(readFileSync(`/proc/self/fdinfo/${held[0]}`, "utf8")) ?? [];
expect(Number.parseInt(flagsOctal, 8) & 0o2000000).not.toBe(0);
```

Under Bun 1.3.14 `fs.constants.O_CLOEXEC` is `undefined` and `openSync` does not set the flag
itself, so this assertion is what fails when someone "cleans up" the numeric constant in the
module (see `docs/solutions/daemon/instance-lock-is-a-kernel-flock-not-a-pid-file.md`).

## Related

- `docs/solutions/testing/socket-tests-observe-the-peer-not-the-clock.md` — the same principle
  for sockets: wait on the peer's observable act, not on elapsed time.
- `docs/solutions/testing/widen-the-contender-count-before-calling-a-race-unreproducible.md` —
  the load recipe under which this flake was reproduced and its fix proven (150/150).
