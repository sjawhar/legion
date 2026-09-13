---
title: "Driving a real Oh My Pi phase worker against a stand-in Legion daemon: role tokens, the bash interceptor, the gh shim's double redemption, and the pane's borrowed session id"
category: testing
tags:
  - rig
  - oh-my-pi
  - stand-in
  - legion-grant
  - envoy
  - tmux
  - bash-interceptor
date: 2026-09-12
status: active
module: pi-envoy
related_issues:
  - "LEGION-12"
  - "sjawhar/legion#974"
  - "LEGION-13"
  - "sjawhar/legion#978"
---

# Driving a real Oh My Pi phase worker against a stand-in Legion daemon

LEGION-12 needed proof on the real host — a real `omp` process loading the branch's extension, driven through 30+ bash
calls with `task` spawns — without the real daemon, real GitHub, or the live rig. The result is
`packages/pi-envoy/scripts/grant-rig/` (a stand-in daemon, a driver for a headless and a terminal leg, a shared
analyzer, and a README with the layout). The rig itself is documented there; this note is the set of things that cost
time on the way and will cost the next rig author the same time unless written down. The smoke rig's own notes are in
[smoke-rig-fakes-and-live-run-notes](smoke-rig-fakes-and-live-run-notes.md); this rig is smaller and fakes the daemon,
not tmux. What LEGION-54 added — the production-plugin-tree mode, the tainted-PATH launch, the before runs against
`main`'s CLI, and the profile guard — is in [proof-rig-loads-the-production-plugin-tree](proof-rig-loads-the-production-plugin-tree.md);
§3 below (the shim's double redemption) is the symptom that mode later traced to the same `PATH` inheritance.

## 1. Encode the role token exactly as the daemon does, or the worker exits at boot

The stand-in's `/legion/v1/worker/started` response carries the `roleToken` the worker will claim on the real Envoy
listener (the rig inherits the pane's `ENVOY_URL`). The first attempt returned `legion-l12rig-RIG-1-implementer` and
the worker died during bootstrap with `role must match ^[a-z0-9][a-z0-9_-]*$` — Envoy rejects uppercase, and the
extension exits the process outright on a bootstrap failure, so the run produced a transcript with zero bash calls and
an analyzer table that looked half-passing. Build the token with `roleToken(project, issueKey, role)` from
`@legion/contracts`, which lower-cases the issue key the way the daemon does, and make the analyzer fail every verdict
when a run has no bash calls at all (a vacuous pass hides exactly this).

## 2. The profile's bash interceptor blocks `… > file` — and the old credential text used to hide that

The rig copies the `legion` profile's `config.yml`, which has `bashInterceptor.enabled: true`. Its rule blocks a bash
command that redirects output to a file ("Use the `write` tool instead of echo/cat redirection"). On the unfixed
extension this never fired for the rig's `legion credential get > $RIG/cred-N.txt` step, because the hook's six lines of
prelude preceded the command and the rule did not match; on the fixed extension the command text is exactly what the
prompt said, the rule matched, and two probe steps were blocked before they ran. The rig's credential step now writes
to stdout. General form: any command-text preprocessor in the profile sees a different string after a fix that stops
rewriting text, so re-check every prompt step against the interceptor rules on the fixed code.

## 3. `legion gh` redeems the grant twice through the worker's `gh` shim

`legion gh` spawns `gh` with the worker's shim directory still first on `PATH`; that shim is itself `exec legion gh --
"$@"`, so the grant is redeemed twice per `legion gh` call (both 200 — a live grant redeems repeatedly for 60 seconds).
Harmless, one extra daemon round-trip, but an analyzer that expects exactly one redemption per probe command will flag
it. Count mints per bash call (one), not redemptions per command. The fix belongs in `buildGitHubTokenEnv`
(`packages/daemon/src/daemon/github-app-env.ts`): strip the shim directory from the `PATH` handed to `gh`. Recorded as
pull request #974's one fast-follow.

## 4. The stand-in must mirror the daemon's rules, including the ones the spec got wrong

Two rules the spec described inaccurately and the daemon's source settled:

- A grant is **not single-use**. `CapabilityService.resolveGrant` (`packages/daemon/src/daemon/api/auth.ts`) checks
  existence and a 60-second expiry (`GRANT_TTL_MS`) and nothing else; `handleGitCredential` resolves the same grant
  twice on purpose. A single-use stand-in would 403 the daemon's own legitimate path and prove nothing.
- The credential redemption routes parse their bodies with a **strict** schema (`LegionDaemonApi.GitHubToken.request`
  for both `/git-credential` and `/gh-token`, `PhaseComplete.request` for `/phase/complete`): an extra key or a missing
  `grantId` is 400, not a lenient pass. Reuse the contract schemas from `@legion/contracts` in the stand-in rather than
  hand-reading fields, so the stand-in drifts with the daemon instead of away from it.

Check the daemon's route table (`packages/daemon/src/daemon/api.ts`) for the request schema each path uses before
writing the stand-in's version of it.

## 5. The Oh My Pi binary is the live daemon's, not the repository's pin

`OMP_FORK_PIN` in `packages/daemon/src/daemon/omp-pin.ts` is the default a daemon falls back to when its `legion.yaml`
sets no `omp_invocation`; the live daemon on this box sets one, and the two differed (18.1.18 live versus 18.1.15
pinned) when the rig was written. The host whose write-back behaviour you are measuring is the one the live daemon's
panes run, so read the build from `omp_invocation` in that daemon's `legion.yaml` and resolve it with `mise where`.
Never hard-code a build in a run book; record the one a run used in that run's report.

## 6. Two legs, a persistent shell that resets, and the pane's borrowed session id

- **Both legs.** `run.ts drive` runs `omp --mode rpc` over stdio (protocol v2, `rpc_chunk` reassembly, close stdin to
  end the process — there is no shutdown command on the stdio transport); `run.ts tui` runs the interactive `omp` in a
  private `tmux -L l12rig` server and pastes the prompt with bracketed paste. The first terminal attempt reused the
  headless argv and put an RPC-mode process in the pane; the mode must be explicit per leg.
- **The worker's persistent shell resets.** In the implementer's own pane (still on the old extension) the shell
  function that kept only the first credential per call was silently gone after a tool syntax error and after long
  idle stretches, and the next `legion …` 403'd. On an old-plugin pane, re-check `type export` before every
  credentialed command.
- **`$OMP_SESSION_ID` in a pane is the daemon's inherited id, not yours.** Six review-thread replies went out with the
  wrong session id in their attribution footer and had to be edited. Take the id from `envoy_whoami` or from
  `legion state`; see [worker-pane-shell-gotchas § 10](../legion/worker-pane-shell-gotchas.md).

## 7. Keep the evidence where the next phase can read it

Transcripts and OMP logs live under the throwaway profile (`~/.omp/profiles/<rig profile>/…`) and vanish with it. Copy
each run's transcript, OMP log, stand-in log, and `seen-grants.log` into the run directory under the rig root before
cleanup, name the paths in the handoff, and leave the rig root in place for the tester and reviewer — the tester on
LEGION-12 built its own rig from the README and reproduced every number independently, which is the point.
