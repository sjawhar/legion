---
title: "A long branch loses the serial number it reserved: the published claim decides, not the branch's seniority — and the same question answered twice, oppositely, was right both times"
category: legion
tags:
  - daemon-api-contract
  - forward-merge
  - release
  - decision-rules
date: 2026-10-21
status: active
module: packages/daemon/internal/api
related_issues:
  - "INFRA-31"
  - "sjawhar/legion#2085"
---

# A long branch loses the serial number it reserved, and the registry decides who moves

Extends docs/solutions/legion/daemon-api-contract-collision-renumber-when-the-release-declaring-the-number-lacks-your-shapes.md.

That note gives the rule. This records what applying it twice on one branch, to opposite effect,
teaches — and the forward-merge discipline that goes with it.

- **The same rule on different inputs, not a reversal — though it looks like one.** At round 4
  this branch held daemon API contract 33 and `npm view @sjawhar/pi-legion@10.14.0 legion`
  answered `{ daemonApiVersion: 32 }`: nothing published claimed 33, so rewriting entry 33 was
  correct and bumping to 34 would have been wrong. At round 21 the same query against v10.16.0
  answered `{ daemonApiVersion: 33 }` — main had landed LEGION-781 and released it — so the
  branch renumbered to 34. One decision rule, two readings of the registry, two opposite correct
  answers; anyone reading the two rounds side by side will take it for inconsistency, so say in
  the commit message which reading drove which. Re-read the registry at **every** forward merge,
  not once per branch: the rule names what decides, and the forward merge is when the input can
  have moved.
- **The manifests that carry the number are the two plugins that declare one.**
  `packages/pi-legion/package.json` and `packages/claude-envoy/package.json` hold
  `legion.daemonApiVersion`; `@sjawhar/pi-envoy` carries no `legion` block at all, and the boot
  gate reads pi-legion's. Each is held to `packages/contracts/fixtures/daemon-api/version.json`
  — the golden of the daemon's own constant — by a `daemon-api-version.test.ts` of its own.
- **Both entries stay.** The renumbered file keeps the other change's entry verbatim above its own,
  each saying what it carried, so a reader two months out can see that 33 and 34 are different
  shapes rather than one renaming.
- **A branch open for a day against a repository that releases daily should expect this.** The
  number is a reservation nothing enforces until a release declares it; the branch that merges
  second is the one that moves, however long it held the number.
- **Everything the number touches moves with it, and the generated artefacts are regenerated, not
  edited.** Here: two plugin manifests, the contract fixture (by the Go golden test's `-update`),
  two changelog entries, the prose in a package's `AGENTS.md`, and `dist/envoy-channel.js` rebuilt
  with `bun run build` and verified by `bun run check-dist`. A bundle resolved by eye in a merge is
  how a wrong artefact ships; the two contract pins (`daemon-api-version.test.ts` in each package)
  are what catch a manifest left behind.

## When a base merge is allowed

Reintegrating the base is for a real conflict only, and on a tree where an architect has forbidden
it for the night the two rules collide: a reviewer's verdict can legitimately require the merge
the standing order forbids without asking. The form that survives both, settled here:

> **A base merge reads the base's own CI first; after that, a reviewer's verdict naming the merge
> is authorisation enough, and you tell the architect rather than ask.**

Merging a red base is how a branch inherits a failure it cannot explain, which is the hazard the
order existed for; running the full suite at the merge head answers the same question from the
other side. An order of the form "ask me first" is incomplete until it says which role's
instruction outranks it.

## Naming what you could not run

A verification section is read by someone who was not there, so a check you could not run is named
with its reason rather than skipped or counted:

- `internal/daemon`'s suite needs Docker, which no role container has — named as CI's.
- `check-bun-version.sh` needs a Python `yaml` module this image lacks — named as CI's.
- A `noTemplateCurlyInString` warning in a test file the branch does not touch is named as
  pre-existing and left alone, neither fixed to make the output clean nor counted as this
  change's.

Each of those is one sentence, and together they are the difference between "everything green" and
a report a reader can check.
