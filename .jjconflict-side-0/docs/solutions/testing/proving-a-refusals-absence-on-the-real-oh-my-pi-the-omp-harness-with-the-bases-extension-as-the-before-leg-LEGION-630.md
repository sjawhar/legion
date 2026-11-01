---
title: "Proving a refusal's absence on the real Oh My Pi: the omp harness from a worker pod, scripted tool turns, the base's extension copied beside the branch's as the before-leg, and the pin's tool contracts"
category: testing
tags:
  - omp-harness
  - real-binary
  - negative-control
  - before-leg
  - tool-call-hook
  - worker-pod
  - hashline-edit
date: 2026-10-08
status: active
module: packages/pi-legion
related_issues:
  - "LEGION-630"
  - "sjawhar/legion#1845"
---

# Proving a refusal's absence on the real Oh My Pi: the omp harness with the base's extension as the before-leg

- A change to what the extension's `tool_call` hook lets through is proven on the real Oh My Pi
  binary through `@legion/pi-shared/test/omp-harness` (`packages/pi-shared/test/omp-harness.ts`:
  `ompRoot`, `serveStandin`, `writeStandinProfile`, `spawnRpc`, `messageStream`), shaped on
  `packages/pi-legion/extensions/legion-phase-stall-omp.test.ts`. It runs in a worker pod on
  `$LEGION_OMP_PATH` (`LEGION_TEST_OMP=$LEGION_OMP_PATH bun test <file>`), and CI's `pi-legion` job
  runs it on the pin every push; no scratch daemon, tmux or NATS is needed.
- "No refusal" is not provable from the head alone: a test that asserts `is_error: false` cannot
  tell a deleted rule from a path the scripted turns never reached. Run the same scripted turns a
  second time against the pre-change extension — `jj file show -r <base>@origin
  packages/pi-legion/extensions/legion.ts > packages/pi-legion/extensions/legion-<base>-control.ts`
  (a sibling file, so its relative imports resolve), a throwaway copy of the test loading it —
  and quote the refusal strings it returns. Delete both files before the handoff commit; the PR
  body and `proof[].negativeControl` carry the output.
- Keep one live-hook control in every pane: the last scripted call is an operation-log rewrite
  (`jj -R "$LEGION_WORKSPACE" undo`), whose refusal must come back `is_error: true`. Without it a
  pane whose extension failed to load would pass every "no refusal" assertion.
- Assert from the `tool_result` blocks the host sends back to the gateway (the Messages request
  bodies the stand-in records), not from the RPC frames: that is the text the model saw.
- Read each tool's contract from the `tools` array of the first Messages request before scripting
  it; on the pin (omp 18.6.0): every tool's schema has `additionalProperties: false` and requires
  `i` (a one-line intent), `edit` takes one string `input` in the hashline form
  `[path#TAG]\nPUT 1.=1:\n+new text` whose header is copied from the previous `read` result's
  first line (so the edit turn is a function of the read turn's result), `write` and `read` take a
  `path` relative to the pane's cwd, and `apply_patch` is not registered in the harness profile
  (`Tool apply_patch not found` comes from the host, not the extension) — a claim about
  `apply_patch` is pinned by the unit matrix only, and the PR body says so.
- The harness pins the pane's `PATH` to `<bin>:/usr/local/bin:/usr/bin:/bin`; symlink the `jj` the
  test process finds (`Bun.which("jj")`) into the pane's `bin`, since CI's jj lives in
  `~/.local/bin`. Make the pane's workspace a jj repository with one described commit before the
  run (`jj git init`, `jj describe -m …`, with `JJ_USER`/`JJ_EMAIL` on the spawn), so `jj log` has
  something to print.
- A phase-worker pane whose run settles without a `handoff_complete` gets the phase stall's
  follow-up turn; end the scripted replies with a text starting `WAITING:` so the run ends on the
  scripted turns alone.
- Add the new `*-omp.test.ts` to `packages/pi-legion/tsconfig.json`'s per-file `include` beside
  `legion-phase-stall-omp.test.ts`; the typecheck job sees only the files that list names, and
  CI's real-binary run does not type-check the file.
- Record which build each figure came from (`"$LEGION_OMP_PATH" --version` beside the result, and
  `.omp-pin`), per
  `docs/solutions/testing/a-worker-pods-oh-my-pi-is-the-fleets-pin-not-the-branchs-fetch-the-branchs-build-and-say-which-pin-each-figure-came-from-LEGION-247.md`.

## Evidence (LEGION-630, sjawhar/legion#1845)

The spec named two surfaces for acceptance 1 (a pod from the branch's image; a two-leg scratch
rig); neither is reachable from a worker pod (`docs/solutions/testing/a-worker-pods-oh-my-pi-…`),
so the plan proved it on the harness. `packages/pi-legion/extensions/legion-role-tools-omp.test.ts`
runs three panes — a root architect (`LEGION_TREE === LEGION_ISSUE`), a reviewer and a merger —
each with its own `/legion/v1/claims/register` answer carrying the pane's role, tree and issue
(the claim session takes the role from that answer). The architect's turns: `bash` `jj log`, `bash`
`cat /proc/self/cgroup`; every pane: `write` `scratch.txt`, `read` it, `edit` it from the read's
`[scratch.txt#TAG]` header, `bash` `jj -R "$LEGION_WORKSPACE" undo`, then `WAITING: done.`. On
omp/18.6.0 in the implementer's pod: 3 pass in 9.4 s; every result before the control
`is_error: false` with no `block`/`refused`; the edit landed (`[scratch.txt#9391]\n1:edited line`,
the file reading `edited line`); the control `is_error: true` with "…rewrite the jj operation log,
which every Legion issue workspace shares…". The tester re-ran it (3 pass, 67 s on a loaded pod)
and drove a five-pane throwaway probe of its own (sub-architect and tester panes added; chained
bash; the shell `legion handoff complete` against the stand-in).

The before-leg: `main@origin`'s `legion.ts` as `legion-main-control.ts` beside the extension and a
copy of the test pointing at it. The architect's `jj log`, `cat`, `write` and `edit` all came back
`is_error: true` "the architect delegates all code work to phase workers"; the reviewer's
`write`/`edit` "the reviewer edits no code; its only commits are its review handoffs, made via
bash"; the merger's "the merger only verifies and reports"; `scratch.txt` never written (its
`read` failed `Path 'scratch.txt' not found`); `jj undo` refused by the same operation-log reason in
both legs. 33 s for the three panes.

Cost in the pod: the three-pane after-leg 7–14 s once Bun has the harness warm; CI's `pi-legion`
job ran the same file on the pin (run 37729581825, `omp/18.6.0`, 191 tests, 0 fail).
