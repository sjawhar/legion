---
title: "A control for an assertion over a run's trajectory restores the end state, and spares the rows it does not name"
category: testing
tags:
  - negative-control
  - attribution
  - playwright
  - dispatch-e2e
date: 2026-10-29
status: active
module: packages/dispatch/e2e
related_issues:
  - "sjawhar/legion#2502"
---

# A control for an assertion over a run's trajectory restores the end state, and spares the rows it does not name

Extends docs/solutions/testing/a-regression-row-fails-for-the-mechanism-it-names-INFRA-66.md.
That document requires the row to fail **and** the failure to be attributable to the mechanism it
names. The delta here is the shape of the fault you inject when the new assertion reads the
samples a run passes through — a scroll trace, a frame log, a call sequence — while the row's
older assertions still read where the run ended.

- **A fault that also breaks the end state proves nothing about the new rule.** The row goes red
  under the rule it replaces too, so the red attributes to neither. Shape the fault so it is over
  before the end-state assertions run: break the trajectory, then put the state back.
- **Run the mirror: the same fault against the rule you replaced, which must pass.** Only that
  pair shows the new rule catches what the old one missed. One red run shows the row can fail.
- **Read a fault an issue or a plan names against the code before you accept it.** A criterion of
  the form "with X removed, row Y fails" is a hypothesis; when X cannot reach Y, say so with the
  line and propose the injection that can, rather than running a control that cannot discriminate.
- **Break a shared path, and also run a row that does not use it.** That row must stay green, or
  the red you recorded could be the harness rather than the path.

## Evidence

sjawhar/legion#2502 made a past-version browser row fail on any large fall of the page's scroll,
not only on a fall to exactly zero (`packages/dispatch/e2e/deep-links.e2e.ts`, the loop over
`window.__scrollEachFrame` ending `expect(falls).toEqual([])`, with
`await expect(highlight).toBeInViewport()` after it).

The plan's control was "scroll the page to half its landing offset once the quote is in view". Run,
it failed the row — on `toBeInViewport`, because the page was left at half and the quote was off
screen. The same injection with the old `offset === 0` filter restored failed the same way, so the
run said nothing about either rule. Rewritten as a dip that returns (`window.scrollTo(0, target /
2)`, then back to `target` 200 ms later), the end state is correct and only the trajectory is
wrong: the new rule failed with `falls [{from: 1980, to: 990}]`, a 990 px fall, and the old filter
under the same dip passed. The tester reproduced both from a dip written independently.

The spec's own acceptance named a different fault — "with the page's jump re-run removed, the
moved past-version rows still fail".
`packages/dispatch/web/src/features/doc/refocus-after-diagrams.ts:24` returns a no-op when no
diagram is waiting, and the moved row has no diagram, so that removal leaves the row green. The
plan recorded the line and proposed the `VersionView` dependency-list mutation instead; the tester
later confirmed the asymmetry in one run, where the diagram-bearing row failed with
`viewport ratio 0` while the no-diagram row passed.

The sparing half came from the same change's API move. With `getArtifactBlocks`' path broken to
`/blokcs`, the row that calls `blockId` failed with the helper's own `404`, and a row of the same
spec that does not call it passed in the same run.
