---
title: "An identity check that reads the filesystem breaks when storage moves: record what this process booted, keep the on-disk check as the fallback"
category: legion
tags:
  - pi-envoy
  - subagent
  - session-storage
  - process-local-state
  - globalThis-symbol
date: 2026-09-14
status: active
module: packages/pi-envoy/extensions/legion.ts (isSubagentSession, recordBootstrappedSession)
related_issues:
  - "LEGION-80"
  - "sjawhar/legion#1081"
---

# An identity check that reads the filesystem breaks when storage moves

## The bug that never fired

`@sjawhar/pi-legion-envoy` must tell a `task` subagent's session apart from the Legion session
that booted the process: the subagent loads a fresh instance of the extension in the **same OS
process** and inherits the pane's `LEGION_*` environment, so without a guard it would run the
root/worker bootstrap again with the already-consumed boot token, be refused, and `exit(1)` —
killing the parent that is waiting on it.

The guard was `fs.existsSync(path.dirname(sessionFile) + ".jsonl")`: OMP puts a subagent's
transcript inside a directory named after its parent's transcript file. Correct for file
storage. Under `session.storage: sql` no transcript is on disk, the check answers "not a
subagent" for every subagent, and the parent dies — on a configuration axis (where sessions are
stored) orthogonal to the code being exercised, so no existing test could have caught it.

## The pattern

1. **Record the identity this process established, in the process.** `bootstrapRoot`,
   `bootstrapWorker`, and the controller's `session_start` write the bootstrapped session's
   transcript path to `globalThis[Symbol.for("legion.pi-envoy.bootstrapped-session")]` — the
   same convention as `LEGION_ROLE_CLAIM_BRIDGE` and `LEGION_LOADED_MARKER`, because each
   extension instance is a separate module instance with its own closure state and only
   `globalThis` is shared. Write it **before** the daemon call that consumes the token, so no
   subagent can observe an empty slot.
2. **Any later `session_start` in the same process with a different path is a subagent.**
   Compare the path string; it is the same string whether it names a file or a SQL row key.
3. **Keep the old check as an OR, not a replacement.** A process that has not bootstrapped
   anything (a plain `omp` with no Legion identity that spawns a task) still needs the on-disk
   layout to recognise its subagents. Behaviour under file storage is unchanged.
4. **Give tests a reset.** The record outlives every extension instance; a suite that boots
   several Legion sessions in one process clears it in `afterEach`
   (`resetLegionBootstrappedSessionForTests`, mirroring `setLegionBootstrapExitForTests`).
5. **Prove it red-green.** The test boots a root on a transcript path nothing wrote, then a
   second extension instance's `session_start` on a different unwritten path: no daemon route,
   no role claim, no exit, no `legion` tool, an ungated bash call. Disable the process-local
   comparison (`if (false && …)`) and the same test fails — the on-disk fallback alone does not
   cover it. The tester repeated that disable as its own negative control.

## Generalisation

Any check of the form "who am I / who is my parent" that consults the filesystem is a check on
the storage layout, not on identity. When a storage backend can move (files → rows, local →
remote), the durable signal is what this process did at boot, recorded where every instance in
the process can read it. The filesystem stays as the fallback for the case that never booted.
