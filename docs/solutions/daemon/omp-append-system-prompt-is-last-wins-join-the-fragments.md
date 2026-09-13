---
title: "Oh My Pi's --append-system-prompt keeps only the last value: pass every pane one joined flag, and prove what the model sees with omp -p"
category: daemon
tags:
  - append-system-prompt
  - oh-my-pi
  - system-prompt
  - processes
  - deployment-instructions
  - real-surface-defect
date: 2026-09-13
status: active
module: packages/daemon/src/daemon/processes.ts
related_issues:
  - "LEGION-20"
  - "sjawhar/legion#975"
---

# Oh My Pi's `--append-system-prompt` keeps only the last value: pass every pane one joined flag, and prove what the model sees with `omp -p`

## What went wrong

The daemon built every pane's launch command with several `--append-system-prompt` flags: the
packaged role prompt (`roles/<role>.md`), then the addressing line (which for a root architect
also carries the design-gate policy sentence), and since pull request #956 the deployment
`instructions:` text. On both Oh My Pi fork pins in service — `18.1.18-sami.20260912-203541`
(production panes) and the repository pin `18.1.18-sami.20260912-104423` — the binary's argv
handler for that flag is an assignment, `e.appendSystemPrompt = s`, so the last flag wins and the
earlier ones are discarded. With `instructions:` configured (production LEGION has it), a pane
received only the deployment instructions: no role prompt, no addressing line, no gate policy.

Nothing in the unit suites could see this. `processes.test.ts` asserted the command *string*,
which contained all three flags exactly as intended. The tester found it on the real surface: an
armed-gate architect on the smoke rig skipped the gate, and its own reasoning said why — "the gate
policy line isn't visible at the end of my system prompt". PR #975 fixed it (commit `b1c9960f`).

## The fix

`systemPromptArguments` in `processes.ts` now emits exactly one `--append-system-prompt` whose
value is every fragment joined in the existing order, separated by a blank line. The value is one
double-quoted shell word: the file-backed parts stay `$(cat <path>)` so the pane's own shell
expands them (never inlined into the command — size and quoting), the inline addressing text is
escaped for the double quotes (`\`, `"`, `$`, and the backtick), and the blank lines are literal
newlines inside the word, which `dash` and `bash` both accept. Nothing else in the argv changed,
and the fix holds on any Oh My Pi build, including one that might later concatenate repeated flags.

## Reusable rules

1. **A flag's repeat semantics belong to the binary, not to you.** Before passing the same flag
   twice, read the receiving program's argv handler. "Append" in a flag name does not promise
   accumulation across flags — here it meant "append to the base system prompt", once.
2. **A test on the command string proves the command, not the delivery.** Keep one real-shell
   proof beside the string assertions. `real-deployment-instructions-e2e.test.ts` launches the
   controller through a real tmux pane and a real `legion worker-shim` into an argv recorder and
   asserts the recorder received *one* `--append-system-prompt` whose value is the role prompt, a
   blank line, then the materialized instructions. The mocked tests assert the exactly-one-flag
   property too (`launches.map(l => l.split("--append-system-prompt ").length - 1)` equals
   `[1, 1, 1]` for root, worker, controller).
3. **Prove what the model sees, directly.** A one-shot on the pinned build settles it in a minute
   and needs no rig: give the model three marked parts and ask it to list the markers.

   ```sh
   OMP_PROFILE=<throwaway-name> mise x github:sjawhar/oh-my-pi@<pin> -- omp -p \
     --model claude-haiku-4-5 \
     --append-system-prompt "$part1

   $part2

   $part3" 'List every marker word your system prompt gives you; words only.'
   ```

   Joined form → `ALPHA-ROLE, BRAVO-ADDRESSING, CHARLIE-INSTRUCTIONS` on both pins. Control
   with three flags → `CHARLIE-INSTRUCTIONS` only. `OMP_PROFILE` must be a *name* (it is not a
   path), and run it with the pane's `LEGION_*`/`DISPATCH_*` variables unset so the Legion
   extension stays inert.
4. **When a pane's behaviour is inexplicable, ask for its system prompt.** The architect's own
   transcript named the missing line. A role that ignores its skill, its addressing, or a policy
   sentence is a system-prompt delivery problem before it is a model problem.

## Where the words now live

Every description of the launch command says "the last part of the pane's one
`--append-system-prompt` value" rather than "the last `--append-system-prompt` fragment":
`packages/daemon/src/daemon/AGENTS.md` (the `instructions` paragraph names both pins and the
failure), `config.ts`, `deployment-instructions.ts`, `processes.ts`, and
`packages/pi-envoy/AGENTS.md`. A future fragment goes into `systemPromptArguments`'s list, never
into a second flag.
