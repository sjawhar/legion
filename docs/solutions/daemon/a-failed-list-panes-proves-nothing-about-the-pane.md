---
title: "A failed list-panes proves nothing about the pane: absent only on the PANE_GONE stderr contract, anything else throws through every probe and stop caller"
category: daemon
tags:
  - tmux
  - list-panes
  - liveness-probe
  - error-handling
  - no-silent-fallback
  - tmux.ts
  - runtime-tmux.ts
  - proc-stat
date: 2026-09-13
status: active
module: packages/daemon
problem_type: correctness
severity: high
related_issues:
  - "LEGION-27"
  - "sjawhar/legion#981"
applies_when:
  - A liveness or existence check shells out (tmux, /proc) and the command can fail for reasons unrelated to the subject
  - A caller of such a check clears state, kills a process, or launches a replacement on its answer
  - A test fakes the command and needs to mean "gone"
---

# A failed list-panes proves nothing about the pane: absent only on the PANE_GONE stderr contract, anything else throws through every probe and stop caller

## The bug shape

`tmux.panePid` returned `undefined` on *any* nonzero `list-panes` exit, and the runtime read
`undefined` as "pane gone". A tmux client killed by the runner's 30 s timeout, a `server not
responding`, an unparseable row -- each made `probe` report a live root dead (a second copy
resurrected beside it) or `stop` skip the graceful ask and clear the locator over a still-live pane.
A check that cannot complete had been folded into one of its two answers.

## The contract

`tmux.lookupPane(server, paneId)` returns one of three verdicts, and only the first two are about
the pane:

- `present` -- the pane's own row was parsed: `{ pid }`.
- `absent` -- a proof the pane is not there: a successful listing without the pane's row, or a
  nonzero exit whose stderr matches `PANE_GONE_STDERR` (`can't find pane`, `no server running`,
  `error connecting to … (No such file or directory)`). That regex lives in `tmux.ts` and is the
  one contract both the lookup and `stop`'s `kill-pane` failure path apply.
- `failed` -- anything else, with the argv and stderr as `detail`. Nothing is known.

`verifyPaneProcess` turns `failed` into the `listing-failed` verdict, and the two public paths
refuse to fake an answer for it:

- `TmuxRuntime.probe` **throws** `cannot verify pane %N: list-panes -t %N exited 1: …` -- never
  alive, never gone.
- `TmuxRuntime.stop` sends the graceful ask (the socket is independent of tmux), then throws
  `ProcessStopFailed` with the same sentence, exactly as a failed `kill-pane` does.

`probedWindowId` is the one caller allowed to shrug: it simply does not reuse a window it could not
verify, because opening a fresh window is not destructive.

## Every probe caller owns the throw

Making `probe` throw means every caller has to say what it does with a probe that did not complete.
The answer is always "log, keep everything, retry later" -- and the retry is whichever clock already
covers that caller:

| caller | on a throw |
| --- | --- |
| resync `onProbe` | logs `resync failed`, next tick |
| root registration deadline (`retireUnconfirmedRoot`) | logs, re-arms the same generation's deadline |
| resurrection retry after the deadline (`escalateOrRetryUnconfirmedRoot`) | logs, re-arms while the tree is still active and unconfirmed |
| boot watchdog (`probeAlive`) | logs, treats the interval as alive-but-unconfirmed, re-arms; the registration deadline still bounds it |
| `closeTree` (the probe runs inside the stop's own try) | `StopFailed`, tree stays `lingering` with its locator untouched, the sweep retries |
| core-NATS exception lane (`handleException`) | logs naming the role token; that lane has no redelivery, so the log is the whole record and the resync/deadline retries |

The list is in `probeLocator`'s doc comment and the daemon `AGENTS.md`; when a new probe caller is
added, add its row. Review found two callers the first sweep missed (`handleException`, and the
resurrection's own re-probe), each a stranded process until fixed.

## The same rule for `/proc` reads

`readPaneStartTicks` and `isOmpPane` catch exactly `ENOENT`/`ESRCH` (`isProcessGoneError`) -- the
process exited between the listing and the read, a fact about the process. EACCES, EIO, EMFILE are
faults of the host and propagate out of `probe` and `stop`; reading them as "not the recorded
process" would fail every pane at once and resurrect every root against a healthy tmux. A
malformed stat line throws from the parser: that is a bug, never "dead".

## The fixture rule this imposes

A fake `list-panes` that means "the pane is gone" must say so on stderr:

```ts
function paneGone() {
  return { stdout: "", stderr: "can't find pane", exitCode: 1 };
}
```

A bare `{ stdout: "", exitCode: 1 }` is now a listing failure the runtime refuses to act on, and a
test built on it either throws where it expected a clean stop or -- worse -- passes for the wrong
reason. The sweep over `processes.test.ts` touched 33 fakes; two survivors were found two rounds
later. When the contract of a fake's real counterpart changes, grep for every fake of it.

## Related

- `a-tmux-pane-id-is-not-a-process-record-and-verify-its-identity.md` -- the identity check this
  verdict feeds.
- `boot-probe-kill-is-transient-not-a-verdict.md` -- the same "a killed command is not an answer"
  rule for the daemon's OMP boot probe.
- `recovery-timers-rearm-on-a-throw-and-recheck-cancel-after-an-await.md` -- what "retry later"
  has to mean for the deadline and watchdog callers.
