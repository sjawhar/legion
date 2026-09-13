---
title: "Smoke rig: a mode without an event feed blocks its checkpoints and never guesses; the OMP pin has one home and the rig verifies it the way the daemon does"
category: legion
tags:
  - smoke-rig
  - checkpoints
  - webhook-mode
  - dispatch-ingress
  - omp-pin
  - mise
  - stop-gap
  - single-source-of-truth
  - LEGION-10
date: 2026-09-13
status: active
module: scripts/smoke
related_issues:
  - "LEGION-10"
  - "sjawhar/legion#957"
  - "LEGION-32"
  - "sjawhar/legion#983"
  - "LEGION-39"
  - "LEGION-40"
  - "LEGION-41"
  - "LEGION-60"
---

# Smoke Rig: Modes Gate Checkpoints, and the Pin Has One Home

LEGION-10 (sjawhar/legion#957) made the smoke rig under `scripts/smoke/` start again on this
machine. Two of its fixes carry rules beyond the rig.

## 1. A mode that lacks an event feed blocks its checkpoints; it never fails them and never guesses

The rig has three webhook modes. `envoy` bridges the production Envoy NATS into the rig, so
GitHub events *and* Dispatch issue events arrive. `forward` runs `gh webhook forward`: GitHub
events only. `none` has no feed at all. The daemon admits an issue only once it has ingested
that issue's Dispatch events (`resync.ts` skips keys it never saw), so in `none` and `forward`
the root issue `up.sh` creates over HTTP is never admitted, and checkpoints 1–4 and 12 — which
read daemon state the root only reaches after admission — can never pass. The README said
"resync-driven checkpoints 1–4 remain usable"; a tester following it saw four false reds.

Three rules, all now in `checkpoints.sh` and its harness:

- **Block, never fail.** A checkpoint the recorded mode cannot reach prints
  `CHECKPOINT <n> SKIPPED-BLOCKED: <reason>` and exits 3, before any network request and before
  its own `require_env`. A false red misleads exactly as a false green does. The reason names
  the mode and the remedy (`use SMOKE_WEBHOOK_MODE=envoy`); the same sentence appears in
  `up.sh`'s start-up line, `checkpoints.sh`, the harness assertions and the README, verbatim.
- **Gate the default mode too.** `forward` is what `up.sh` picks whenever `gh webhook forward`
  is installed, so the first human on a normal machine hits the default. A gate that covers
  only the explicit `none` leaves the default path lying.
- **Never guess a mode.** `stored_webhook_mode` used to fall back to `forward` when
  `${SMOKE_DIR}/webhook-mode` was absent. Once `forward` gated five checkpoints, a run against a
  scratch directory `up.sh` never populated printed `SMOKE_WEBHOOK_MODE=forward: …` for a mode
  nobody recorded or exported — a fabricated fact pointing at a setting that does not exist.
  It now stops: `no recorded webhook mode at <path>; run up.sh, or export
  SMOKE_WEBHOOK_MODE=envoy|forward|none`. Repository rule: no silent fallbacks.

Checkpoints whose gate is something else (8: branch protection; 13: secrets on panes) stay
out of the mode arm, and the harness proves it — adding `8` to the `none` arm fails a case.

## 2. The OMP pin has one home; a stop-gap rooted in one operator's home directory is not shippable

The rig's controller crashed on its first prompt because the pinned Oh My Pi release predated
the headless-worker fix. The fix went through three designs:

1. **Hand-built default.** `resolve_omp_path` fell back to
   `~/.local/state/legion/sjawhar-legion/omp/omp-<hash>-rpcfix`, the binary production ran on
   this box. It made the rig start here and nowhere else, and its remedy text ("bump
   `omp_pin`") was a dead end because the function never consulted the pin — a reviewer caught
   that an operator following it would get the identical failure.
2. **Literal pin in `up.sh`.** Meanwhile `main` made `packages/daemon/src/daemon/omp-pin.ts`
   the single home of the pin ("Bump the pin here and nowhere else") and taught `up.sh` to read
   it with `bun`. A second copy in the rig would drift from it.
3. **Read the one pin; verify it the way the consumer does.** LEGION-32 (#983) moved the
   constant to a release that carries the fix. `up.sh` now reads `omp_pin` from `omp-pin.ts`,
   and `resolve_omp_path` runs the daemon's own lookup in preflight: `mise where "$omp_pin"`
   must succeed and `<dir>/bin/omp` must be executable; otherwise it stops naming the exact
   remedy, `mise install <pin>`. It exports nothing to the daemon, which resolves the same pin
   itself. `LEGION_OMP_PATH` remains an explicit operator override (absolute executable path,
   exported to the daemon only). `grep -rn 'sami.2026' scripts/smoke/` prints nothing.

Rules:

- **A value someone else owns is read from its one home, never copied.** The proof that no
  copy exists is a grep for the literal, run at every gate.
- **Re-derive through the consumer's own mechanism.** The daemon resolves the pin with
  `mise where` and never installs; preflight does exactly that, so the two cannot disagree and a
  missing install fails before NATS and the listener are up rather than after.
- **A default that only exists under one operator's `$HOME` is a local workaround, not a fix.**
  Sami's direction, relayed by the Legion PO: "A rig that falls back to a file under one
  operator's ~/.local/state cannot run on any other box and should not ship." If the real fix
  (a tagged release) is not available yet, the spec should say so and the rig should fail
  closed naming the remedy — never fall back.
- **Remedy text must describe an action that actually changes the outcome.** "Bump the pin"
  was true of the system and false of the function. Write the remedy from the code path that
  prints it.

One caveat for whoever runs the rig: `mise where` reads `MISE_DATA_DIR`. From a stripped shell
(no `MISE_DATA_DIR`, e.g. `bash --norc` inside a kernel) the same pin reports "not installed";
run `up.sh` from a normal operator shell, as the daemon's own environment does.

## 3. Branch mechanics, briefly

Merging `main` into the branch preserved every commit SHA the review replies and PR body cited,
which is why it was the first choice after review round 1. Once GitHub reported the PR
`DIRTY` and stopped running `pull_request` CI, the rule in
`docs/solutions/github/conflicting-pr-gets-no-pull-request-ci.md` applied and the branch was
rebased (`jj rebase -s <first branch commit> -d main`), resolving conflicts bottom-up with
edit-and-squash and keeping `main`'s change wherever both sides touched a line. The old merge
commit survived the rebase harmlessly (its second parent is now an ancestor of `main`). Review
findings landed as their own commits on top, as
`docs/solutions/legion/stacked-base-and-review-folded-into-rebase.md` describes.

## Operational lessons that became their own issues

Recorded here as pointers so this retro does not restate them:

- LEGION-39: the daemon's 5-second ready-time connect to the controller shim socket times out
  under host load (`failed to connect controller shim socket on ready`), harmless but noisy.
- LEGION-40: `DISPATCH_TOKEN` is not a secretsd key on this machine, so the README's
  `secrets … DISPATCH_TOKEN --` form cannot run as written.
- LEGION-41: the NATS container name `legion-smoke-nats` is hard-coded, so two rigs on one
  machine collide.
- LEGION-60: an acknowledged prompt was treated as delivered.

## Related

- `scripts/smoke/README.md`: the operator runbook these rules are written into (modes table,
  "Which OMP build the rig runs", Checkpoints).
- `docs/solutions/testing/bash-harness-cases-that-pass-for-the-wrong-reason.md`: how the
  harness cases guarding these rules were made to fail for their own reasons.
- `docs/solutions/testing/smoke-rig-fakes-and-live-run-notes.md`: the LEGION-6 rig notes; its
  live-run section now records which of its workarounds #957 retired.
- `docs/solutions/daemon/omp-pin-bump-behavioral-proof.md`: proving a pin bump on the daemon
  side (LEGION-32).
