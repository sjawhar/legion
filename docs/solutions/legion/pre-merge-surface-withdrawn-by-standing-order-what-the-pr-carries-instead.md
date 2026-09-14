---
title: "When the production-like surface is withdrawn by a standing order, the E2E line names the withdrawn surface, the unit red/green that stands in, the release, and the operator's post-deploy command"
category: legion
tags:
  - legion
  - e2e
  - standing-order
  - smoke
  - pi-envoy
  - release
  - verification
date: 2026-09-14
status: active
module: skills/legion-worker
related_issues:
  - "LEGION-109"
  - "LEGION-101"
  - "sjawhar/legion#1083"
---

# When the production-like surface is withdrawn by a standing order, the E2E line names the withdrawn surface, the unit red/green that stands in, the release, and the operator's post-deploy command

## The gap, named as the retro skill requires

`skills/legion-retro` step 1 says: if the PR's `E2E` line links only a unit suite, the retro's first
durable learning is that gap. LEGION-109's `E2E` lines link only the unit suite. The surface the
change is reached through — the installed pi-envoy plugin's delivery pump under a live Envoy
listener — was withdrawn for every worker by Sami's standing order of 2026-09-13 (via the operator,
19:00Z): no smoke rig of any kind, no scratch or stand-in daemon, no throwaway NATS container or
tmux server, no throwaway OMP profile or scratch plugin tree, no `scripts/smoke-delivery.sh`, and
`~/.omp/profiles/legion` / `~/.omp/plugins` are the operator's alone. The deployment instructions
say a Sami ruling wins over a skill default, so the issue does **not** go back to the tester; the
gap is real, it is accepted by order, and the record has to say so in a way the merge queue and
the operator can act on.

## What the PR carries when the surface is withdrawn

Both `E2E` paragraphs (implementer's and tester's) and the `Release` paragraph on
sjawhar/legion#1083 carry five things, and a reviewer verified each against GitHub:

1. **The surface, named, and why it is unreachable pre-merge** — "the installed plugin's delivery
   pump under a live listener; pre-merge access withdrawn by Sami's standing order of 2026-09-13
   (LEGION-109 acceptance 5)". Not "n/a", not a unit suite described as the surface.
2. **The unit proof as red/green, not as a count** — the exact `bun test … -t '…'` selection, the
   recorded call sequences it observed, and that the same selection was **5 fail / 0 pass on the
   pre-fix code**. A green count alone is the thing the skill warns is not proof; a test that
   provably failed before the change is at least proof that the change is what it says.
3. **A negative control from the same suite** — `pi.sendMessage` throws after the receipt: one
   warning naming the event id, `_INBOX.failed` still published, dedupe key unrecorded so the
   re-send injected; and a second — the receipt publish itself throws once: logged, message still
   injected, next message acknowledged normally.
4. **The release the merge cuts**, as "the patch release above the latest `pi-legion-envoy-v*`
   tag" with the tag observed at PR open and a promise to write the real one after the merge
   (see [pane-jj-overlay-stale-workspace-git-pointer-and-a-release-number-nobody-knows-yet](pane-jj-overlay-stale-workspace-git-pointer-and-a-release-number-nobody-knows-yet.md) §3 for why any fixed number is wrong by the time it is read).
5. **The operator's post-deploy proof, by command** — `packages/pi-envoy/scripts/smoke-delivery.sh`
   against the installed plugin, its first line `plugin version: <v>` naming the release and its
   last `PASS:`; then the parent issue's production hour (LEGION-101 acceptance 4: `envoy_inbox`
   on three busy architects shows no payload twice). Run at the operator's timing, by the operator.

The plan's `acceptanceProof[4]` and the spec's Testing row said the same, so the tester and the
reviewer had one text to check against instead of each deciding what "withdrawn" permits.

## What this does not license

- It does not make the unit suite a production-like surface. The word "withdrawn" in the `E2E`
  line is what keeps the record honest; do not paraphrase it away into "verified".
- It does not remove the post-merge duty. The implementer is still sent back after the merge to
  record the production observation (`Production:` line, PR comment, `dispatch_message`); the
  standing order withdrew pre-merge rigs, not the production check.
- It does not apply to an issue that *has* a reachable surface. A daemon change still needs the
  smoke rig or a scratch daemon unless the order covers it; ask the architect which order applies
  before writing "withdrawn".

## Related

- [scratch-daemon-rig-proves-what-unit-tests-cannot](../testing/scratch-daemon-rig-proves-what-unit-tests-cannot.md)
  and [proof-rig-loads-the-production-plugin-tree](../testing/proof-rig-loads-the-production-plugin-tree.md)
  — the surfaces this deployment used before the order and will use again when it is lifted.
- [pane-jj-overlay-stale-workspace-git-pointer-and-a-release-number-nobody-knows-yet](pane-jj-overlay-stale-workspace-git-pointer-and-a-release-number-nobody-knows-yet.md) §3
  and [worker-pane-shell-gotchas](worker-pane-shell-gotchas.md) §12 — the release number.
