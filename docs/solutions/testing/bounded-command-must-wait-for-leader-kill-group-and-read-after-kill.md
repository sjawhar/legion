---
title: "A bounded spawned command must end its leader wait, kill its group on every outcome, and read its value after cleanup"
category: testing
tags:
  - bun
  - subprocess
  - process-group
  - timeout
  - stdout
  - cleanup
  - child-process
date: 2026-10-13
status: active
module: packages/envoy-client/src/envoy-config.ts, packages/pi-legion/src/jj-attribution.ts
related_issues:
  - "LEGION-707"
  - "sjawhar/legion#1934"
problem_type: runtime_error
component: tooling
symptoms:
  - "a bounded command returns its token only after a background child closes inherited stdout"
  - "a timeout ends the wrapper but leaves its forked helper running"
  - "a successful command returns while a helper with redirected stdio remains alive"
root_cause: incomplete_process_cleanup
resolution_type: code_fix
severity: high
---

# A bounded spawned command must end its leader wait, kill its group on every outcome, and read its value after cleanup

## Problem

A timeout does not by itself bound a spawned command's resource lifetime. Under the pinned Bun
1.3.14 runtime, three independently plausible shapes were wrong:

1. `Bun.spawnSync` with piped stdout waits for EOF, so `sleep 30 & echo token` took about 10.2
   seconds even though the leader had already printed the value, then left the child running.
2. `execFile`'s `timeout` and `killSignal` signal the direct child only. A wrapper such as
   `sh -c 'sleep 1 & echo $!; wait'` returned after about 206 ms while its grandchild was still
   alive.
3. Killing a detached group only when the timer expires misses a helper that closes stdin, stdout,
   and stderr. Its leader can print a valid value and exit in about 5 ms while that helper remains
   alive.

## What Didn't Work

- **Piped stdout plus a timeout:** an inherited pipe makes the stream's EOF depend on every
  descendant, not on the leader that owns the command result.
- **A timeout that kills only the leader:** the direct wrapper is not the process tree it created.
- **Group cleanup only on timeout:** successful and non-zero paths still have descendants to reap.

## Solution

The synchronous command runner in `packages/envoy-client/src/envoy-config.ts` makes the wait end
with the shell alone: it sends stdout to a mode-0600 file in a mode-0700 `mkdtemp` directory,
ignores stdin and stderr, and starts the shell detached. In a `finally` after `Bun.spawnSync`
returns, it kills the negative PID process group regardless of success, failure, or timeout. Only
then does it read and validate the output file.

```ts
const output = path.join(directory, "stdout");
const fd = openSync(output, "wx", 0o600);
const result = Bun.spawnSync(["/bin/sh", "-c", command], {
  stdin: "ignore",
  stdout: fd,
  stderr: "ignore",
  timeout: COMMAND_TIMEOUT_MS,
  detached: true,
});
closeSync(fd);
try {
  process.kill(-result.pid, "SIGKILL");
} catch {
  // The group is already empty.
}
const value = readFileSync(output, "utf-8").trim();
```

For an asynchronous command that needs streams, the pending change in
[sjawhar/legion#1934](https://github.com/sjawhar/legion/pull/1934) applies the same ownership
rule in `packages/pi-legion/src/jj-attribution.ts`: spawn detached, race the leader and both
streams against the deadline, and attach group cleanup to `child.exited.finally`. That finalizer
runs before either a successful value or a non-zero result can resolve, so the process group is
also reaped when the leader exits before the timer.

## Why This Works

The value is meaningful only after the command's descendants no longer own resources that could
extend the wait or outlive the caller. Redirecting stdout to a private file decouples synchronous
leader completion from descendant-held pipes. An isolated process group gives the caller one
handle for every descendant. Killing that group on every terminal path establishes the lifetime
invariant; reading the value after cleanup prevents a successful result from racing a still-live
helper.

## Prevention

Test the three failure shapes separately and record the helper PID in each case. The regression
rows in `packages/envoy-client/src/__tests__/envoy-config.test.ts` prove both successful and
timed-out commands return within their bound and that the recorded child is gone afterwards. The
pending `packages/pi-legion/src/jj-attribution.test.ts` coverage in #1934 adds both a helper that
holds inherited streams and a successful wrapper whose helper closes them. A test that observes
only the leader's exit cannot prove group cleanup.
