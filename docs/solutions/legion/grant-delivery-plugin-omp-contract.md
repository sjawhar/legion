---
title: "pi-legion-envoy 1.17.1 delivers LEGION_GRANT as a bash env field that neither fork release in service applied to the shell"
category: legion
tags:
  - legion
  - legion-grant
  - pi-legion-envoy
  - oh-my-pi
  - bash
  - tool-call-hook
  - probe-image
  - deployment
date: 2026-09-13
status: active
module: pi-envoy
related_issues:
  - "LEGION-32"
  - "sjawhar/legion#983"
  - "LEGION-12"
  - "sjawhar/legion#974"
  - "LEGION-52"
---

# pi-legion-envoy 1.17.1 delivers `LEGION_GRANT` as a bash `env` field that neither fork release in service applied to the shell

This records what was observed and what was checked. LEGION-52 owns the durable fix (a contract
check comparing the installed plugin against the resolved OMP build); nothing here is one.

## Where the delivery shape changed

`@sjawhar/pi-legion-envoy`'s `tool_call` hook (`extensions/legion.ts`) mints a 60-second grant per
bash call and hands it to the shell so `legion gh`, `jj git push`, and `legion handoff complete` can
redeem it. Through 1.16.x it prepended `export LEGION_GRANT='<uuid>'` (and the `GH_*`/`PATH` lines)
to `toolCall.input.command`; the model saw that text as its own and copied it, stacking stale ids
that 403'd. LEGION-12 / #974 (merged 2026-09-13 00:15Z, released as 1.17.1) moved the whole record
into `toolCall.input.env` instead, spread last so a copied `env` cannot displace the fresh grant.
That history, its proof rig, and the model-visibility mechanism are documented by #974 itself in
`docs/solutions/legion/worker-pane-shell-gotchas.md` §1,
`docs/solutions/envoy/omp-tool-call-hook-rewrites-are-model-visible.md`, and
`docs/solutions/testing/driving-a-real-omp-worker-against-a-stand-in-daemon.md`. What follows is
the part #974 did not cover: the `env` shape only works if the OMP build the pane runs applies
`toolCall.input.env` to the shell, and the deployment's build did not.

## What was observed on 2026-09-13 (LEGION-32 implementer pane)

- Session resumed 00:24:07Z by the daemon with `--resume`, on
  `/home/ubuntu/.mise/installs/github-sjawhar-oh-my-pi/18.1.18-sami.20260912-104423/bin/omp`
  (verified: the bash tool runs in-process, so `$$` is the OMP pid; `readlink /proc/$$/exe`). Every
  one of the 19 `omp --mode rpc` processes on the box was that build.
- Installed plugin in the `legion` profile: 1.17.1 (`package.json` mtime 00:16:59Z); its `dist/legion.js`
  carries `LEGION_GRANT: grantId` and no `export LEGION_GRANT=`.
- The bash tool the session exposed had parameters `command`, `i`, `timeout` — no `env`, `cwd`, `pty`.
- Symptom: `legion gh -- …` → `LEGION_GRANT is missing: the Legion worker extension injects it before
  credential commands run`; `jj git push` fails the same way through the credential helper. A probe
  call with the copied `env` map and `echo ${LEGION_GRANT:-unset}` printed `unset`: the field never
  reached the shell.
- Stopgap the worker used, which relies on the grant's 60-second TTL and on `resolveGrant` being a
  pure lookup: carry the previous call's minted grant id (visible in the echoed `env`) into the next
  command as `export LEGION_GRANT='<id>'` in the command text. It works while calls are less than a
  minute apart. It is not a fix.

## What the fork source says (checked, not assumed)

On the knives-managed checkout `/home/ubuntu/oh-my-pi`, `packages/coding-agent/src/tools/bash.ts` is
byte-identical between `v18.1.18-sami.20260912-104423` and `v18.1.18-sami.20260912-203541`, and
`packages/agent` has no diff either (the 22-file diff between the tags is mcp, registry, collab,
docs, and changelog). At *both* tags `bashSchemaBase` declares `"env?": type({ "[string]": "string" })`
and `BashTool.execute` destructures `env: rawEnv` and applies `normalizeBashEnv(rawEnv)`. So the
statement "203541's bash tool accepts `env`, 104423's does not" is not supported by `bash.ts`, and it
was then disproved directly: LEGION-37's implementer, the first worker resumed on OMP 203541 with
plugin 1.17.1, ran the check and posted it on LEGION-52 at 00:51Z on 2026-09-13 — zero `LEGION_GRANT`
in the shell's environment, an empty expansion, and `legion gh` failing with `LEGION_GRANT is missing`,
exactly as on 104423. Moving OMP between these two releases does not fix it. The mechanism that drops
the field — on the fork side (a model-facing schema without `env`, a `tool_call` hook result validated
against it, a profile setting) or on the plugin side — was not located and is LEGION-52's to find.
Record the symptom, not a theory.

## What the deployment did

`/home/ubuntu/.config/legion/sjawhar-legion/legion.yaml` ran
`omp_invocation: "mise x github:sjawhar/oh-my-pi@18.1.18-sami.20260912-104423 -- omp"` from about
11:00Z on 2026-09-12. At 00:35Z on 2026-09-13 (file mtime) the operator moved it to
`github:sjawhar/oh-my-pi@18.1.18-sami.20260912-203541` alongside plugin 1.17.1, with the yaml comment
"203541: bash tool accepts `env` (needed by plugin ≥1.17.1 / #974); carries the rpc fix like 104423".
The direct check above showed 203541 does not deliver the field either, so at 00:52:55Z the operator
rolled the plugin back (the `legion` profile's installed copy is now 1.17.0, the last release before
#974, whose `dist/legion.js` still prepends `export LEGION_GRANT=…` to the command text; the operator's
own record says 1.16.0 — the on-disk version is what a pane loads) and left the deployment on OMP
203541. So as of 00:53Z: OMP 203541, plugin 1.17.0, preamble-shape delivery, gotchas §1 applies. The
daemon's built-in default (`OMP_FORK_PIN`) stayed at 104423, the crash-fix release LEGION-32 was filed
for; moving it again, and the plugin/OMP contract check, are LEGION-52's.

## What `probe-image` does and does not check

`legion probe-image` (and `startDaemon`) proves two things about a build: the isolated probe
extension sees `pi.agents`, and OMP's ambient discovery loads `@sjawhar/pi-legion-envoy`
(`LEGION_PLUGIN_LOADED=yes`). It does not exercise the `tool_call` hook, so a plugin whose grant
delivery the build ignores passes both probes and every worker on that daemon then fails at its first
`legion gh`. A pin bump that passes `probe-image` is therefore not evidence that credential delivery
works on the new build; that is a separate check, and until LEGION-52 lands it is a manual one:
resume or spawn one worker on the candidate build with the installed plugin and run `legion gh -- api
rate_limit` from its pane.

## Recognizing which shape your pane is under

Look at the echoed arguments of your own last bash call. A `command` that begins
`export LEGION_GRANT='…'` is the preamble shape (≤ 1.17.0; read gotchas §1). An `env` object with
`LEGION_GRANT` is 1.17.1; if `legion gh` then says `LEGION_GRANT is missing`, the field is not reaching
the shell on this build either — do not expect a different OMP release to change that (104423 and
203541 both fail). Report the build (`readlink /proc/$$/exe`) and the plugin version
(`jq -r .version ~/.omp/profiles/legion/plugins/node_modules/@sjawhar/pi-legion-envoy/package.json`)
to the architect, cite LEGION-52, and do not debug the shell.
