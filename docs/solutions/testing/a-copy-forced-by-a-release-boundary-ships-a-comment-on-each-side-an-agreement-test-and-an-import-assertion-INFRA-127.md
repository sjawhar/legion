---
title: "A copy forced by a release boundary ships a comment on each side, an agreement test over both real implementations, and an import assertion — and states its containment direction"
category: testing
tags:
  - duplication
  - release-contract
  - agreement-test
  - import-boundary
  - differential-corpus
  - go-list-deps
date: 2026-10-22
status: active
module: envoy-infra
related_issues:
  - "INFRA-127"
  - "sjawhar/legion#2108"
---

# A copy forced by a release boundary ships three things, and states its direction

Sometimes the import is the wrong answer and the copy is right: here, a non-test import of
`packages/envoy/internal/config` by `packages/envoy/infra/core` would add that package to
`go list -deps ./cmd/...`, the plugin's release-cut path list (`packages/envoy/infra/AGENTS.md`,
"Release contract"), so every edit to the listener's configuration would cut a plugin release.
A copy taken for a reason like that is legitimate only with all of this:

- **A doc comment on each side** naming the other side, the reason the copy exists, and the test
  that holds them together. The side you did not change is the one that gets "fixed" later: write
  its comment in the same change, or the next reader deletes the copy by importing the original.
- **An agreement test that drives one table through both real implementations in one process** —
  the actual `config.Load` and the actual `Check`, not a reimplementation of either.
- **A mechanical assertion that the forbidden import has not appeared**: run
  `go list -deps -f '{{.ImportPath}}' .` for the copying package and require the original's path
  to be absent. Without it, unifying the two is a green change that quietly moves a release
  boundary.
- **State the containment direction the code implements, and assert only that.** Mirrors are
  rarely symmetric. Derive the direction from the two bodies before writing a word of prose about
  it, assert `refused there implies refused here`, and give each entry only one side refuses its
  own row with the reason it is deliberate. Never `require.Equal(aErr != nil, bErr != nil)` over
  two implementations nobody has proven equal.
- **Copy the other side's table whole, including its accept cases, or generate one.** A curated
  subset makes an agreement test pass by omission, and the row most likely to be dropped is the
  one that encodes the asymmetry. Before trusting any hand table over two implementations, run
  both over a generated corpus once and classify every asymmetric entry in code, not by eye
  (`docs/solutions/testing/a-throwaway-sandbox-repository-probes-workflow-semantics-with-a-control-job-and-every-production-first-step-rides-the-same-run-and-a-reimplementation-is-differentialled-against-its-oracle-first-LEGION-748.md`
  states the differential discipline for a reimplementation and its oracle).

## Evidence (INFRA-127, sjawhar/legion#2108)

`core.CheckAllowedHosts` mirrors the listener's `parseAllowedHosts` (`internal/config/config.go`),
regular expressions and normalisation included, so a hub is refused at preview rather than
deploying a listener that will not boot. The first round shipped the copy, the agreement test and
the import assertion (`TestCoreCompilesNoListenerConfigPackage`), but only one comment and the
wrong direction.

The two bodies differ by one step: `parseAllowedHosts` trims each entry and skips an empty one
*before* applying the shape rule; `CheckAllowedHosts` applies it to the entry as written, because
`Env` comma-joins the list into the unit's `docker run` argument where a space would split it and
an empty entry would render a stray comma. So the listener accepts `" hub.internal.example "` and
`""` that the plugin refuses: the containment is one-way, and strict by construction — `hostEntry`
matches no string holding a space and no empty string, which the reviewer re-derived rather than
read.

The shipped test asserted the two-way property, and three prose places repeated it. It passed only
because `acceptedAllowedHosts` had copied the listener's accept table without its one padded-and-
empty row (`internal/config/config_test.go:12`, `" Hub.Internal.Example. , ,192.0.2.7,[2001:db8::1]"`)
— the omitted row was exactly the asymmetry. A generated corpus found it from the other direction a
round earlier: 23,630 entries through both implementations, 0 that the plugin accepts and the
listener refuses (the dangerous direction, which deploys a listener that cannot boot), 165 the
other way, all inside the padded-or-empty class, classified in code with
`strings.TrimSpace(entry) != entry || entry == ""`.

The correction added `coreOnlyRefusedAllowedHosts`, per-row expectations for each side, and the
direction asserted on its own; mutation showed the rows bite — making `CheckAllowedHosts` trim and
skip fails exactly those two rows and no others. Note which assertion catches what: the per-row
expectation is fatal and fires first, so the standalone implication guards the *table* (a future
row claiming the listener refuses what the plugin accepts), not the code.

The pressure to delete the copy is real and recurring: a code-quality pass asked twice to move the
rule to one home in `internal/bus`. Deferring it was right — it moves released listener code for no
behaviour change while the release this unblocks is what the deployment waits on — and the decision
is recorded in the pull request body and in both comments, not as a fast-follow.
