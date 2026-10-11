---
title: "A package split holds each partition to what it ships, and every filter that guards a partition names the other partition's paths it reaches into"
category: testing
tags:
  - package-split
  - skills-partition
  - skills-guard
  - path-filter
  - bundled-agents
  - oh-my-pi
  - ci-filters
date: 2026-10-07
status: active
module: packages/pi-shared
applies_when:
  - One package becomes two and a list that meant "all of it" (skills, agents, CI path filters, release-job paths, a gate's required names) is copied per package
  - A skill, prompt or agent definition in one partition dispatches an agent or links a skill
  - A guard runs in a CI job whose path filter is narrower than the files that can break it
related_issues:
  - "LEGION-247"
  - "sjawhar/legion#1831"
---

# A package split holds each partition to what it ships

- A split turns every shipped text into a statement about one partition. Resolve each
  partition's `task(agent="…")` dispatches against the agents that package ships plus the agents
  the pinned host binary bundles, and read the bundled list off the binary at test time
  (`omp agents unpack` under a scratch `HOME`), never from a hand-written list that drifts with
  the pin.
- Prove the guard with a negative control that restores the offending text and watches the test
  fail naming the file and the agent; a guard that has only ever passed proved nothing.
- Say in the guard which cross-partition references are deliberate and why, and list them. A
  `skill://` mention conditioned on a role the reader cannot hold is inert and stays; the guard
  must not fail on it, and the next reader must not "fix" it.
- Every list that was implicitly "all of it" is now copied per partition: CI path filters, the
  release jobs' tracked paths, the boot gate's required prompt names. For each guard, name the
  paths whose change can break it, including the other partition's files it links into, not only
  the paths it belongs to; and either derive the copies from the one source (the prepack's
  partition) or add a check that holds them equal to it, as `check-image-trigger-paths.sh` does
  for the image's inputs.

## Evidence

sjawhar/legion#1831: the partition is one function in `scripts/pi-plugin-prepack.sh` (`plugin()`),
and `packages/pi-shared/test/skills-partition.test.ts` holds the two lists to an exact partition
of `skills/`. The Envoy partition's guard resolved `skill://` links against the repository's
`skills/`, where both partitions live, so a `task(agent="plan-gap-analyst")` in an Envoy skill
passed CI while the agent shipped only with the Legion package (review round 1, finding 1).
`packages/pi-shared/test/skills-guard.ts` gained `agentDispatches` (the daemon's own pattern,
`promptrefs.go:38-45`, fences included), `shippedAgents(<package root>)` and
`bundledAgents(<omp>)`; the Envoy guard's new test holds every dispatch to that union and, with
line 53 restored, fails `…/dispatch-first/SKILL.md dispatches plan-gap-analyst, which neither this
plugin nor Oh My Pi ships`. The six `skill://legion-*` mentions in the Envoy skills are listed
beside the test as deliberate.

The four `skill://dispatch/SKILL.md#…` anchors the Legion skills gained were checked only by the
Legion guard, whose `pi_legion` CI filter listed the eight Legion skill directories and not
`skills/dispatch/**`: a heading rename in the Envoy partition would merge green and fail the next
unrelated Legion change (round 1, finding 2; fixed in 8962eb69,
`.github/workflows/envoy-and-contracts.yaml:259-263`). Still open as fast-follow, same shape: no
check holds the skill lists in `release.yaml` and `envoy-and-contracts.yaml` to the prepack's
partition, and `promptreferences.go` now collects the gate's required names from the Legion
partition alone, so `dispatch-brainstorming` left the set the boot gate verifies.
