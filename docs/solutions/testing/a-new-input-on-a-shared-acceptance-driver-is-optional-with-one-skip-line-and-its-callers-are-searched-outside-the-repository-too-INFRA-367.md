---
title: "A new input on a shared acceptance driver is optional with one SKIP line, and its callers are searched outside the repository too"
category: testing
tags:
  - acceptance-driver
  - contract-census
  - e2e
date: 2026-10-27
status: active
module: packages/dispatch/e2e/acceptance
related_issues:
  - "INFRA-367"
  - "sjawhar/legion#2389"
---

# A new input on a shared acceptance driver is optional with one SKIP line, and its callers are searched outside the repository too

Extends docs/solutions/testing/a-required-option-this-branch-added-is-a-census-item-too-mains-new-constructor-call-sites-fail-at-runtime-not-at-compile-time-LEGION-631.md.

- A driver that other issues run (such as `packages/dispatch/e2e/acceptance/block-graph.sh`) has
  callers that `git grep` cannot see: sibling issues' Dispatch plans and their open pull requests.
  Before you add an input, also run `dispatch search --query '<driver name>'` and
  `gh pr list --state open --search '<driver name>'`. List every hit in the
  `## Contract change census`.
- Ship the input as optional. Unset, the driver runs every other check and prints one
  `SKIP: <section> …` line naming what to set. An unknown value exits 2 with the usage, so a typo
  cannot skip the checks silently.
- Never default an unset input to a mode that asserts something. That mode can fail on a caller's
  server: `none` asserts that the server has no architecture source.
- A plan that makes an input required names the rollout for the callers that do not set it.

## Evidence

sjawhar/legion#2389's plan made `COMPONENT_SOURCE` a required input of `block-graph.sh`. Round 1
blocked it (thread PRRT_kwDORFy7ds6wpCnn): the INFRA-156 tree plan (its Task 2.9 invocation, and
the production run), the INFRA-156 pull request (#2394) and the final check INFRA-371 all run the
driver with only `DISPATCH_URL`, `DISPATCH_TOKEN` and `PROJECT`. 69d138bb made the input optional.
With it unset, the driver exited 0 with 64 PASS lines and one `SKIP: R12 component …` line, and
`COMPONENT_SOURCE=bogus` exits 2. The reviewer's other suggestion, running unset as `none`, would
have failed on any server that holds a source.
