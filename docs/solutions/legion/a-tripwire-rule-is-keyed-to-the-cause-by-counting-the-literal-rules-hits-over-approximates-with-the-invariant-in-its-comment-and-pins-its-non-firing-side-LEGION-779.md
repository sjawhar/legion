---
title: "A tripwire rule is keyed to the cause by counting the literal rule's hits over the real tree, over-approximates with the invariant in its comment, accepts every spelling of the known next fix, and pins its non-firing side"
category: legion
tags:
  - check-release-credentials
  - tripwire
  - rule-design
  - mutation-testing
  - expression-language
  - string-literals
date: 2026-10-16
status: active
module: .github/scripts/check-release-credentials.sh
related_issues:
  - "LEGION-779"
  - "sjawhar/legion#1975"
  - "LEGION-748"
---

# A tripwire rule is keyed to the cause by counting the literal rule's hits over the real tree, over-approximates with the invariant in its comment, accepts every spelling of the known next fix, and pins its non-firing side

Extends docs/solutions/legion/a-superseded-mechanism-does-not-retire-its-rule-and-a-gate-that-reads-shell-is-one-pass-proven-against-bash-before-review-LEGION-748.md.
That document derives a gate's tokenizer from the interpreter. This one is for the rule the gate
enforces: how to state it, what it may refuse, and how its harness pins both sides.

## The rule

1. **Prototype the spec's literal rule over the real tree and count before writing it.** A spec
   written from an incident states the symptom's text ("reads `needs.<job>.outputs` without
   `always()` and that job's `result`"). Run that over the files the gate reads: a hit count far
   from the spec's own Done-when (nine jobs against three named, three of them correct, and an
   idiom fifteen jobs of another workflow share), or a known-bad shape it passes (the result-only
   condition), means the spec named the symptom. Restate the rule as the hazard it guards
   against, record the departure in the plan, and rewrite every job the restated rule flags in
   the same pull request, since the required lint lane runs the gate over the repository and
   would otherwise go red at the merge.
2. **A hazard clause over-approximates, and its call-site comment states the invariant, not a
   defect.** 6b counts any upstream job with an `if:` key as skippable, one on `always()`
   included, and checks no job without an `if:` of its own: cheap, stable under later edits,
   never wrong in the direction that matters. Where it rewrites a condition that was never broken
   (`release-envoy-infra.yaml`'s `publish`, whose only ancestor was its build), the comment says
   "the repository requires this shape", not "the old one skipped"; the reviewer reads a
   fix-shaped comment on a non-bug as a wrong claim.
3. **Before a required lint accepts exactly one spelling, enumerate what it forbids.** Rule 6
   accepts `always()` alone, so the required lint now refuses `!cancelled()`, the one-line shape
   that would close the cancelled-run exposure the same pull request recorded as not proven. If
   an alternative the rule forbids is the already-identified next fix, accept it in the same
   change or file the widening before the lint lands.
4. **Scanning an expression language: strip string literals first, keep the index key in the
   same pass, and prove the strip on the empty-literal shape.** `'(?:[^']|'')*'` is right for
   GitHub's `''` escape and for `'' '…'` sequences, but for the opposite reason to intuition: the
   pattern's leading `'` consumes the first quote, so `''` closes a literal rather than escaping
   inside one; verify it on `''`, `'it''s'`, `'' 'x'` and `'a' x 'b'` before relying on it. The
   strip's first alternative keeps `needs['x']` so the index-form readers still see the job. A
   harness case for a quoted token needs a double-quoted YAML scalar: a plain scalar cannot
   start with a quote.
5. **Red cases pin the firing side only; pin the non-firing predicate and the per-file loop.** A
   rule with a predicate that decides whether it fires (`"if" in jobs[ancestor]`) needs a green
   case that turns red when the predicate is deleted — a plain `if:` whose ancestor chain carries
   no `if:`. A loop over several files needs one case in a second file, else scoping it to one
   file stays green. For each mutation that stays green, name the net that catches it: "the lint
   lane over the real workflows" is a legitimate answer, "nothing" is a fast-follow item.
6. **When a rule fires only on a fixture's topology, pin the topology with its own checks.** Rule
   6's red cases exist only because the green fixture's build job gained an `if:`; four checks in
   the first case assert that `if:`, the publish jobs' shape, the dangling `needs` name and its
   absence as a job, so a later fixture tidy-up cannot disarm every red case silently.

## Evidence

- LEGION-779 plan handoff, `gapAnalysis.findings[0]` and `specDepartures[0..1]`: the literal rule
  flagged `legion_build`, `legion_publish`, `worker_image`, `worker_image_release_note`,
  `envoy_plugin_build`, `envoy_plugin_publish`, `pi_envoy_build`, `pi_legion_build` and infra
  `publish`; the two-clause rule flags six; `pr-and-main.yaml:66` runs the checker in the required
  `lint` job.
- sjawhar/legion#1975 review handoff: `keyFindings[0]` (`!cancelled()` refused; the plan names it
  the architect's one-line change), `keyFindings[1]` (the two mutations that leave 221/221 green:
  deleting the skippable predicate, caught by the lint lane naming the three `*_build`
  conditions; scoping the loop to `release.yaml`, caught by nothing), `keyFindings[4]` (the infra
  comment reading as a fix for a non-bug).
- LEGION-779 test handoff, `observations[3]` and probes p1/p12: the literal strip verified in
  Python on four shapes; `observations[0]`: `needs.x.result != 'failure'` passes 6a and GitHub
  runs it on a skipped `x` (run 38836147591, `hazard_ne_failure`), the limit of a presence test.
- `.github/scripts/check-release-credentials.test.sh` at sjawhar/legion#1975: `write_green`'s
  `pi_envoy_build` `if:` and the four fixture checks in the first case; the case
  `'always()' in a string literal is text` with its double-quoted scalar.
