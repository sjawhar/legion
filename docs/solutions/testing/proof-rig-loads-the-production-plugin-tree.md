---
title: "Pre-merge proof runs on the production plugin tree, from a tainted shell: two defects an extension-only rig had passed, and how the grant rig's production mode is built"
category: testing
tags:
  - rig
  - oh-my-pi
  - plugins
  - secretsd
  - legion-grant
  - worker-bin
  - production-like
  - PATH
date: 2026-09-13
status: active
module: pi-envoy
related_issues:
  - "LEGION-54"
  - "sjawhar/legion#992"
  - "LEGION-52"
  - "LEGION-12"
  - "sjawhar/legion#974"
---

# Pre-merge proof runs on the production plugin tree, from a tainted shell

LEGION-12's release (pi-envoy 1.17.1) was proven on a rig that loaded the Legion extension alone
and failed on the first real worker: the `secretsd` plugin replaces the bash tool and drops the
`env` field the fix relied on (the mechanism is in
[omp-tool-call-hook-rewrites-are-model-visible](../envoy/omp-tool-call-hook-rewrites-are-model-visible.md),
lesson 2). Sami's ruling on 2026-09-13 (AGENTC-79) followed: the agent that builds a change proves
it before merge on a production-like surface. LEGION-54 turned that into a rig mode and the mode
caught two more defects the extension-only rig never would have. This note is the pattern, so the
next rig author starts from it rather than from the README's default.

The rig itself (`packages/pi-envoy/scripts/grant-rig/`) and the earlier lessons about driving a
real `omp` against a stand-in daemon are in
[driving-a-real-omp-worker-against-a-stand-in-daemon](driving-a-real-omp-worker-against-a-stand-in-daemon.md);
this note adds only what the production mode changed.

## 1. Load the real plugin tree; swap only the plugin under test

`RIG_PLUGINS=production` copies `~/.omp/profiles/legion/plugins` (`package.json`, `bun.lock`,
`node_modules`, ~240 MB) into the rig profile, deletes the profile's `agent/extensions/` so exactly
one copy of each extension loads, and then swaps only `node_modules/@sjawhar/pi-legion-envoy`:
`RIG_LEGION_BUILD=branch` runs the checkout's `bun run build` and copies `dist/envoy.js` and
`dist/legion.js` over the installed package's `dist/` (the installed `package.json` already names
those files in `omp.extensions`); `RIG_LEGION_BUILD=<version>` runs `npm pack` and extracts the
release in place. Everything else in the tree — `secretsd`, `superpowers`, codegraph, knives —
loads exactly as it does in production, at the pinned versions.

Two consequences worth knowing before the first run:

- The worker launches under the daemon's real prefix (`secrets ANTHROPIC_API_KEY … -- omp`), so
  the rig needs the same key access a pane has. `--no-secrets` exists for a shell that already
  exports the keys; it is not a way around the plugin.
- Copy once, reuse. The tree copy is the slow step; `RIG_REFRESH_PLUGINS=1` re-copies when the
  source profile has changed.

## 2. The before runs use the same profile and the *old* command-line tool

Regression evidence is two runs of the released plugin builds on the same production tree, against
`main`'s `legion` CLI exported read-only (`git archive` of `main@origin` into a sibling directory,
`bun install --frozen-lockfile` there, `setup.sh <that dir> <rig>`). On LEGION-54: 1.17.1 failed
every credentialed command with `LEGION_GRANT is missing` (34 mints, zero redemptions), 1.17.0
showed the imitation counts climbing. Pointing the before runs at the branch's CLI would have hidden
the first regression entirely — the branch CLI reads a file the 1.17.1 plugin never writes, and
fails differently. Keep the before runs honest about which CLI they exercise.

## 3. Launch the rig from a shell that already carries the pane's own PATH prefix

The second defect the production mode found was not a plugin interaction at all. A pane's PATH now
starts with `<state_dir>/worker-bin` (the `gh` shim) for the pane's life, `mise env` keeps an
inherited PATH head, and this rig — like every daemon and worker on the box — runs from inside a
Legion pane. The first branch run showed `legion gh -- --version` resolving `gh` to the shim and
re-entering itself under a scrubbed env (`LEGION_GRANT_FILE is missing`, verdict D FAIL on call 29);
the reviewer then showed the same inheritance reaches a daemon started from a pane, which would
resolve its *own* `gh` to the shim and hand every child pane a second `worker-bin` entry.

So the rig deliberately taints its launch PATH — this pane's `worker-bin` first, a second
`/tmp/other-daemon/worker-bin` last — and the worker's `printenv` probe (verdict G) must show the
rig's `worker-bin` exactly once. The strip that makes this true is one shared function,
`pathWithoutWorkerBin` in `packages/daemon/src/daemon/worker-bin.ts`, applied at the daemon
boundary (`resolveDaemonEnvironment`), in `legion gh` before it spawns `gh`, and in the rig's
`workerEnvironment` — never re-implemented per call site. The general rule for anything a daemon
puts on a pane for life is in
[config-env-keys-that-panes-also-carry](../daemon/config-env-keys-that-panes-also-carry.md); the
rig's job is to start from the environment that rule protects against, not from a clean shell.

## 4. Guard the destructive step against the one variable that aims it at production

`setup.sh` removes and rewrites the rig profile's `plugins/` and `agent/extensions/`. Its first
action after resolving names is a refusal when the rig profile and the source profile resolve to
the same directory (`realpath -m` on both; `RIG_PROFILE=legion` and `RIG_SOURCE_PROFILE=l12rig`
both trip it), exit 2 naming both paths, before any filesystem action. Prove a guard like this
dry: run the refused invocation, fingerprint the protected tree before and after
(`find <dir> -maxdepth 2 -printf '%p %T@ %s\n' | sort | sha256sum`), and paste the equal hashes
into the PR. Never run the refused form against the real profile "to see" — the fingerprint is the
proof.

## 5. Score what the command actually ran under, not what the tool call said

The analyzer's `record-grant` step writes `<grant file contents> <mode> <LEGION_GRANT or ->`, so
each row states what the shell command found in the file (and, on the 1.17.0 before run, what the
text-delivered variable held). The tool call's `env` field is recorded only as an informational
column: nothing reads it any more, and a rig that scored it would have passed 1.17.1. Verdict F
greps every minted id over the raw transcript and OMP log — the stand-in log is the oracle and is
exempt — which is the acceptance-5 "no credential in any transcript" check automated.

## 6. Keep the rig's own state honest across runs

- Clear `standin.log` and `seen-grants.log`, and remove the previous run's grant file from
  `state/secrets/`, before each leg; the analyzer pairs mints to calls in order.
- Run the stand-in and each leg under a supervised process (`hub start` / `hub wait`), never a
  foreground loop; a leg is two to three minutes and the deadline is twenty.
- `run.ts`'s deadline is a cleared `setTimeout`. A pending `Bun.sleep(20 min)` racing the agent's
  end kept the process alive seventeen minutes after the report was printed, which blocks any
  wait on the process.
- `rig-mode.json` (plugin mode, Legion build, checkout commit) is copied into every run's
  `report.json`, so a table can always be tied to the tree that produced it.
