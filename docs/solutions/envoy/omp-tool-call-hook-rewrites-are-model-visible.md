---
title: "An Oh My Pi tool_call hook that rewrites a tool's text input is writing the model's next prompt: deliver secrets and machinery through structured fields, spread last"
category: envoy
tags:
  - oh-my-pi
  - extension
  - tool_call
  - legion-grant
  - bash
  - secrets
  - model-imitation
date: 2026-09-12
status: active
module: pi-envoy
related_issues:
  - "LEGION-12"
  - "sjawhar/legion#974"
  - "LEGION-9"
  - "LEGION-16"
  - "LEGION-29"
---

# An Oh My Pi `tool_call` hook that rewrites a tool's text input is writing the model's next prompt

The Legion extension for Oh My Pi (`packages/pi-envoy/extensions/legion.ts`) attaches a one-time credential to every
bash command a phase worker runs. For months it did so by prepending shell text — an `export LEGION_GRANT='…'` line
and five more — to the command. Pushes, `legion gh`, and `legion handoff complete` then failed with 403 more and more
as a session went on, and three issues' worth of workers wrote shell workarounds for it (LEGION-9, LEGION-18,
LEGION-22). LEGION-12 measured the cause and moved the credential into the bash tool's per-command `env` argument.
This note is the part that applies to any extension, not just Legion's.

## The mechanism

Oh My Pi lets a `tool_call` hook return a revised `input`. The agent loop then writes that revised input back into
the assistant message's tool-call block (`prepareToolCallDispatch` in `packages/agent/src/agent-loop.ts`, documented
at `ToolCallEventResult.input` in `packages/coding-agent/src/extensibility/shared-events.ts`), because the transcript
must record what actually ran. From then on the model sees the hook's text as something *it* wrote. On later calls it
imitates it — with a literal `'...'` placeholder, a counting-pattern id, or a verbatim copy of an earlier call's value.
The hook's real block was always first; the imitations followed; a shell keeps the last `export`; the command ran
under a value the daemon never issued.

Measured on LEGION-12's rig (`packages/pi-envoy/scripts/grant-rig/`), on the unfixed code: exactly one hook invocation
and one credential minted per bash call, every call, while the credential lines in the model's command text climbed
1,2,1,2,2,2,3,3,4,… up to 7 in 31 calls. The hook never multiplied; the model did. The same growth appeared in an
interactive terminal session with no subagent spawns (the LEGION-16 controller), so it is not a headless-mode or
`task`-spawn artefact.

**The rule:** anything a hook puts into a tool's free-text argument (`command`, `content`, a prompt) becomes model
context. Secrets, paths, and machinery that must hold a specific value belong in a structured field the tool itself
defines — for bash, the per-command `env` record — never in text.

## What the fixed hook does, and why each part

```ts
const modelEnv = isPlainObject(toolCall.input.env) ? toolCall.input.env : {};
return { input: { ...toolCall.input, env: { ...modelEnv, ...workerGhEnvironment(grant, stateDir, workerBin, process.env.PATH) } } };
```

- **Structured channel.** Oh My Pi's bash tool declares `env?: Record<string, string>` and applies it to that one
  command only (`apply_command_env` in `crates/pi-shell/src/shell.rs` pushes a command-scoped exported overlay and pops
  it afterwards). The model still sees the `env` object in its transcript, and it does imitate that too — the tester
  saw an imitated `env.LEGION_GRANT` on 26 of 31 calls — but an object is a place where precedence can be enforced.
- **Hook keys spread last.** Whatever the model already put in `env` is kept for unrelated keys (the test keeps
  `KEEP: "model-value"`) and overridden for the hook's own six. Order of spread is the whole security property; the
  presence of the field is not.
- **Base `PATH` on the pane's own `process.env.PATH`, never the model's.** A naive `modelEnv.PATH ?? workerBin` lets an
  imitated call drop the daemon's prefix and break the `gh` shim.
- **Idempotent under re-feed.** The regression test feeds the hook's own revised `{command, env}` back in under the
  same tool call id and asserts a fresh mint, `command` still untouched, and the shim directory in `PATH` exactly
  once. Any hook that string-concatenates on each call would grow `PATH` forever under imitation.
- **Empty string clears a variable.** `env` cannot unset, so `GH_TOKEN`, `GITHUB_TOKEN`, and `GH_HOST` are set to `""`;
  `gh` (go-gh) treats an empty value as absent, and `legion gh` scrubs and re-sets them itself.

## How to prove a change like this

- **Count side effects, not text.** A stacked or duplicate hook registration would multiply daemon mints without
  changing the command text at all, because Oh My Pi's `emitToolCall` passes the same event to every handler and keeps
  only the last result; the in-process test fixture mirrors that. The rig therefore scores three things per call
  separately — hook log lines per tool-call id, mints attributed to the call, credential text found in the command —
  and only their combination distinguishes "the hook multiplies" from "the model imitates". Two permanent debug lines
  make this measurable forever: an instance id on `extension instance loaded`, and one `legion tool_call hook` line
  per invocation (one small secret-free line per tool call).
- **Run both legs.** A headless `omp --mode rpc` worker and an interactive `omp` under a private tmux server. The
  evidence that reopened this bug came from a terminal session; a fix proven only headless is half proven.
- **Reproduce on the unfixed code first**, from a read-only export of `main`'s tree, before running the fixed one.

## What stays true after the fix

- **A resumed session keeps its old transcript.** A worker resumed from a transcript recorded before the fix still
  sees its own old credential blocks and may keep writing them; such a line is text the shell executes, so for that one
  call it still overrides the `env` value. The fix removes the seed for fresh sessions; it cannot rewrite what a model
  already sees. Nothing in the hook detects a stray `export LEGION_GRANT=` in `command` — by design: inspecting or
  rewriting the model's text is the channel that caused the problem.
- **The credential is not single-use.** The daemon's `CapabilityService.resolveGrant` checks existence and a 60-second
  expiry (`GRANT_TTL_MS` in `packages/daemon/src/daemon/api.ts`) and nothing else; a live grant redeems any number of
  times. So a 403 within seconds of minting means the id was never minted (imitation), while a 403 after a slow command
  ran ahead of the redeeming one means expiry. A stand-in daemon must copy that rule exactly; a single-use stand-in
  would 403 the real daemon's own legitimate double redemption inside `handleGitCredential`.
- **Per-command scope is the host's contract.** With `env` delivery, no variable set in one bash call survives into
  the next — the hook's six or a worker's own `export`; the tester proved both. Workers set what a command needs in
  that command. This was decided, not deferred.
- **`modelEnv` values are only shape-checked.** The hook keeps whatever the model put in `env` for keys it does not
  own; a non-string value there reaches the bash tool's own validation (`normalizeBashEnv` rejects bad names, not bad
  value types). Not observed in any run; noted so nobody is surprised.
