---
title: "An Oh My Pi tool_call hook that rewrites a tool's text input is writing the model's next prompt — and the tool's structured fields belong to whichever plugin installed the tool: a secret goes to a side channel the pane already knows"
category: envoy
tags:
  - oh-my-pi
  - extension
  - tool_call
  - legion-grant
  - bash
  - secrets
  - model-imitation
  - secretsd
date: 2026-09-13
status: active
module: pi-envoy
related_issues:
  - "LEGION-12"
  - "LEGION-52"
  - "LEGION-54"
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
LEGION-22). LEGION-12 measured the cause and moved the credential into the bash tool's per-command `env` argument
(pi-envoy 1.17.1). That failed in production on the first worker: every `legion` command answered `LEGION_GRANT is
missing`, and the profile was pinned back to 1.17.0 (LEGION-52). LEGION-54 (pi-envoy 1.17.3) moved the credential
out of the tool call altogether, into a file the pane already knows. This note is the part that applies to any
extension, not just Legion's: two lessons, one about text and one about structured fields.

## Lesson 1: text the hook writes is text the model imitates

Oh My Pi lets a `tool_call` hook return a revised `input`. The agent loop then writes that revised input back into
the assistant message's tool-call block (`prepareToolCallDispatch` in `packages/agent/src/agent-loop.ts`, documented
at `ToolCallEventResult.input` in `packages/coding-agent/src/extensibility/shared-events.ts`), because the transcript
must record what actually ran. From then on the model sees the hook's text as something *it* wrote. On later calls it
imitates it — with a literal `'...'` placeholder, a counting-pattern id, or a verbatim copy of an earlier call's value.
The hook's real block was always first; the imitations followed; a shell keeps the last `export`; the command ran
under a value the daemon never issued.

Measured on LEGION-12's rig (`packages/pi-envoy/scripts/grant-rig/`), on the 1.17.0 code: exactly one hook invocation
and one credential minted per bash call, every call, while the credential lines in the model's command text climbed
1,2,1,2,2,2,3,3,4,… up to 7 in 31 calls. The hook never multiplied; the model did. The same growth appeared in an
interactive terminal session with no subagent spawns (the LEGION-16 controller), so it is not a headless-mode or
`task`-spawn artefact.

## Lesson 2: a tool's structured fields belong to whichever plugin installed the tool

Oh My Pi's own bash tool declares `env?: Record<string, string>` and applies it to that one command. The 1.17.1 hook
returned `{ input: { ...toolCall.input, env: { ...modelEnv, ...workerGhEnvironment(...) } } }` and was proven on a rig
that loaded the Legion extension alone. In the `legion` profile it never ran against that tool: the `secretsd` plugin
(`github:sjawhar/forward#v3.0.2`) replaces the bash tool at session start with Oh My Pi's legacy
`legacy-pi-coding-agent-shim`, whose schema is `{command, timeout}` and which discards the tool call's `env`. So the
hook minted a grant per call, the daemon logged every mint, and the shell never saw a `LEGION_GRANT` — `legion gh`,
`jj git push`, and `legion handoff complete` all failed `LEGION_GRANT is missing` on every call, in 1.17.1 and 1.17.2
alike. The operator pinned the profile back to 1.17.0 (text delivery, imitation and all) to keep workers moving.

Two facts settled on LEGION-52 (the four-case comparison, one binary, with and without `secretsd`: comments
`62e6b3de-122f-4ba4-9f8b-a6788fbdd5f2` and `8ad46677-6df9-4314-b327-d9a7a3c6af6c`), relayed by the LEGION-12 root
architect:

- The Oh My Pi 203541 build does not change the legacy shim: `legacy-pi-coding-agent-shim.ts` and `tools/bash.ts`
  are byte-identical between the 104423 and 203541 tags; env delivery fails with `secretsd` loaded on both.
- On the 1.17.1 build (OMP 203541), in three fresh headless sessions with one persistent shell across calls, an
  exported variable, a plain assignment, and a PATH change made in one bash call were all empty in the next — "every
  variable a command sets is discarded when that command ends while the hook supplies a per-command env." That was
  1.17.1's behaviour, not a property of the pane's shell; file delivery removes per-command injection entirely, and
  the persistent shell behaves as a shell does.

Making `secretsd` stop overriding the tool, or fixing the legacy shim to forward `env`, were both considered and set
aside (the operator's decision on LEGION-12; the shim fix is filed separately): Legion's own credential must not
depend on which bash tool any plugin installs.

**The rule:** a secret the tool call must not carry goes to a side channel the pane already knows — a 0600 file whose
path is on the pane's own environment — not to any field of the tool call, text or structured. Whoever owns the tool
owns its fields; the hook owns only what it writes elsewhere.

## What the fixed hook does (pi-envoy 1.17.3, LEGION-54)

```ts
const grantFile = process.env.LEGION_GRANT_FILE;
if (grantFile === undefined) return { block: true, reason: "LEGION_GRANT_FILE is not set on this pane: …" };
const grant = await roleDaemon().grant({ tree, issue, sessionId, secret });
await writeGrantFile(grantFile, grant.grantId); // temp `<file>.<pid>.<uuid>`, 0600, rename over the named file
return undefined;                                 // neither `command` nor `env` is touched
```

- **The daemon names the file; the extension writes it; `legion` reads it first.** Every root, worker, and controller
  pane launches with `LEGION_GRANT_FILE=<state_dir>/secrets/<role token>-grant` beside its `LEGION_BOOT_TOKEN_FILE`
  (`ProcessManager.credentialProcessEnvironment`). The daemon creates nothing for it but prunes it with the pane's other
  secret files when the locator clears. `legion credential`, `legion gh`, and `legion handoff complete` read
  `LEGION_GRANT_FILE` (trimmed) ahead of `LEGION_GRANT`; set-but-unreadable or blank is an error naming the variable
  and the path, never a fallback — the same `X_FILE` rule as `DISPATCH_TOKEN_FILE`.
- **Atomic and concurrent-safe.** The temp name carries pid and a uuid; two bash calls in flight for one session both
  mint and both rename, and whichever a command reads is a live grant of the same session. A write failure blocks
  the command naming the path — nothing runs under a stale credential.
- **The static keys moved to the pane at spawn.** `GH_CONFIG_DIR=<state_dir>/gh`, `PATH` with `<state_dir>/worker-bin`
  first (the `gh` shim, now installed by the daemon at startup), and `GH_TOKEN`/`GITHUB_TOKEN`/`GH_HOST` set empty are
  on the pane for life. Nothing per command remains, so nothing about the credential depends on the bash tool's
  arguments — a plugin that replaces the tool has no effect.
- **Nothing inspects the model's text.** A model that writes its own `export LEGION_GRANT=` into a command changes
  nothing: `legion` ignores the shell variable when `LEGION_GRANT_FILE` is set. Detecting or rewriting the model's
  text is the channel that caused lesson 1.

## How to prove a change like this

- **Count side effects, not text.** A stacked or duplicate hook registration would multiply daemon mints without
  changing the command text at all, because Oh My Pi's `emitToolCall` passes the same event to every handler and keeps
  only the last result; the in-process test fixture mirrors that. The rig therefore scores per call, separately: hook
  log lines per tool-call id, mints attributed to the call, credential text found in the command, and what the grant
  file held when the command read it — and only their combination distinguishes "the hook multiplies" from "the model
  imitates" from "the plugin's tool dropped the field". Two permanent debug lines make this measurable forever: an
  instance id on `extension instance loaded`, and one `legion tool_call hook` line per invocation (one small
  secret-free line per tool call).
- **Run against the production plugin tree.** Sami's ruling after 1.17.1 (2026-09-13, AGENTC-79): pre-merge proof runs
  on a rig that loads the real `legion` profile's plugins, `secretsd` included (`RIG_PLUGINS=production` in the
  grant rig), never the extension alone. That is how #974 shipped broken.
- **Run both legs.** A headless `omp --mode rpc` worker and an interactive `omp` under a private tmux server. The
  evidence that reopened the imitation bug came from a terminal session; a fix proven only headless is half proven.
- **Reproduce on the unfixed code first**, from a read-only export of `main`'s tree and the released plugin builds
  (1.17.1 fails `LEGION_GRANT is missing`; 1.17.0 imitates), before running the fixed one.

## What stays true after the fix

- **A resumed session keeps its old transcript.** A worker resumed from a transcript recorded under 1.17.0 still sees
  its own old credential blocks and may keep writing them; with `LEGION_GRANT_FILE` set, `legion` ignores that shell
  variable, so the line is harmless text. The fix removes the seed for fresh sessions; it cannot rewrite what a model
  already sees.
- **The credential is not single-use.** The daemon's `CapabilityService.resolveGrant` checks existence and a 60-second
  expiry (`GRANT_TTL_MS` in `packages/daemon/src/daemon/api.ts`) and nothing else; a live grant redeems any number of
  times. So a 403 within seconds of minting means the id was never minted (imitation, or a fabricated id in the file),
  while a 403 after a slow command ran ahead of the redeeming one means expiry. A stand-in daemon must copy that rule
  exactly; a single-use stand-in would 403 the real daemon's own legitimate double redemption inside
  `handleGitCredential`.
- **Deployment order matters, once.** The new plugin blocks every bash call on a pane without `LEGION_GRANT_FILE`
  (launched by an older daemon), and the new `legion` refuses on a pane whose plugin never writes the file (an older
  plugin under the new daemon). Install the released plugin in the profile, then restart the daemon, immediately, in
  that order; panes started before the restart carry no `LEGION_GRANT_FILE` and keep working through `LEGION_GRANT`.
