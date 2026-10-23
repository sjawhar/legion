---
title: "An execFile timeout sends SIGTERM and waits, so a child that keeps waiting is unbounded: race the call and kill it with SIGKILL"
date: 2026-10-23
category: envoy
module: envoy-client
problem_type: runtime_error
component: tooling
symptoms:
  - "A `dispatch` call from a tmux pane of a loaded shared tmux server took up to 85 s, though the pane lookup passed `timeout: 5_000` to execFile"
  - "The same call with TMUX and TMUX_PANE unset answered in well under a second"
  - "A `tmux display-message` client stays alive after SIGTERM while its server is too busy to accept"
root_cause: "Bun's execFile `timeout` sends SIGTERM and then waits for the child to exit; a tmux client waiting to connect to a server that is not accepting catches SIGTERM and keeps waiting, so the call waited with it"
resolution_type: code_fix
severity: high
tags: [execfile, timeout, sigterm, sigkill, abortsignal, tmux, bun, fake-timers, dispatch]
related_components:
  - claude-envoy
---

# An execFile timeout sends SIGTERM and waits, so a child that keeps waiting is unbounded: race the call and kill it with SIGKILL

## Problem

The `dispatch` command asks tmux for its pane before every call
(`resolveOrigin` in `packages/envoy-client/src/dispatch-cwd.ts`). The exec it used passed
`timeout: 5_000` to the promisified `execFile`, and its comment said that kept a broken tmux from
hanging the call. On a shared tmux server that was too busy to accept, every call from a pane took
up to 85 s.

## Symptoms

- A `dispatch` call from a tmux pane hangs for tens of seconds; the same call outside tmux is fast.
- The tmux client is still running after the execFile timeout should have ended it.

## What Didn't Work

- **execFile's `timeout`.** It signals the child and then waits for it to exit. Measured with tmux
  3.7b against a unix socket that listens with a full backlog and never
  accepts: `timeout -s TERM -k 6 2 tmux -S <socket> display-message -p '#S'` ended only at the SIGKILL,
  after 8.0 s, while `timeout -s KILL 2 …` ended it at 2.0 s; Bun 1.3.14's execFile with
  `timeout: 2000` was still waiting after 9 s. The blocked client sits in `unix_wait_for_peer`, its
  `/proc/<pid>/status` shows SIGTERM caught (not ignored), and it was still alive 2 s after a SIGTERM.
- **`killSignal: "SIGKILL"` with an AbortSignal.** Bun's execFile kills an aborted child with
  `killSignal`, but Node's does not: under Node 22.22.2 a child that ignores SIGTERM was still alive
  300 ms after the abort, `killSignal: "SIGKILL"` set. Code that relies on it is bounded under Bun
  only.
- **Awaiting a lookup that never settles under Bun's fake timers.** Bun 1.3.14's per-test timeout
  does not fire while `jest.useFakeTimers()` is on, so such a test hangs the run instead of failing.

## Solution

Put the bound in the caller, not in the child's cooperation: race the exec against a timer, abort
its signal at the bound, and return without waiting for the child. The real exec kills its child
itself on abort, with SIGKILL, which no process can catch.

```ts
async function tryExec(exec, file, args, cwd, limitMs) {
  const controller = new AbortController();
  const expired = Promise.withResolvers<null>();
  const timer = setTimeout(() => {
    controller.abort();
    expired.resolve(null);
  }, limitMs);
  try {
    const answer = await Promise.race([
      exec(file, args, { cwd, signal: controller.signal }),
      expired.promise,
    ]);
    return answer?.stdout ?? null;
  } catch {
    return null;
  } finally {
    clearTimeout(timer);
  }
}

export const defaultExec: ExecFn = (file, args, { cwd, signal }) => {
  const call = execFileAsync(file, args, { cwd, signal });
  signal.addEventListener("abort", () => call.child.kill("SIGKILL"), { once: true });
  return call;
};
```

The tmux pane lookup is bounded at 1 s and the call goes on without the pane; the jj and git
lookups keep 5 s each. claude-envoy's own tmux query (`queryTmux` in
`packages/claude-envoy/src/session-title.ts`) already used SIGKILL for the same reason.

## Why This Works

The race ends the wait at the bound whatever the child does, so no child, wrapper or injected exec
can hold the call. The SIGKILL ends the tmux client even inside its connect, and sending it from the
exec's own abort listener makes that independent of the runtime's execFile.

## Prevention

- Test a bound with a stand-in that never heeds its abort, under fake timers, and check at the bound
  minus 1 ms and at the bound. Assert the lookup has settled before awaiting it, so a regression fails
  at once rather than hanging under fake timers.
- Test the kill with a real child that ignores SIGTERM (`trap '' TERM; exec sleep 30`), synchronised
  through two FIFOs: one it writes once its trap is set, one it holds open until it dies. Reading the
  second to its end proves the death with no clock (`packages/envoy-client/src/__tests__/dispatch-cwd.test.ts`).

## Related Issues

- `docs/solutions/testing/bounded-command-must-wait-for-leader-kill-group-and-read-after-kill.md`:
  the synchronous command runner's bound, which also refuses to wait on descendants.
