---
title: "Attribute a dependency's behaviour to a release by one probe on a real artefact at each release"
category: testing
tags:
  - dependencies
  - probes
  - regressions
  - evidence
date: 2026-11-01
status: active
module: packages/envoy
related_issues:
  - "sjawhar/legion#2444"
---

# Attribute a dependency's behaviour to a release by one probe on a real artefact at each release

Extends docs/solutions/testing/an-expected-gain-ratio-belongs-to-the-surface-it-was-measured-on-pr2479.md.

- Before you write that a library has always behaved some way, run the same probe at each candidate release. Run it on a real stored artefact of production size, and publish the counts for each version.
- A small hand-made input may never reach the code that changed. If the same probe gives the same answer at two releases, that shows only that this input does not tell them apart. It does not show the releases behave alike.

## Evidence

- In sjawhar/legion#2444's round 11, the implementer cut a small hand-made row at 57 points. The probe gave the same messages on ygo sami.4, sami.7 and the release candidate, and the implementer wrote that the doubled refusal was old ygo wording.
- Tester round 11 cut a real 182 KB stored row at 200 points. sami.4, sami.7 and sami.9 named `encoding: unexpected end of input` for all 200 cuts, and the candidate dropped the reason for 196. That made it a regression of the candidate. Its new pending check frames structs first and returns a bare `ErrInvalidUpdate`, which `wrapUpdateErr` then wraps in itself.
- The same probe at the tag (`be665c15`) and at sami.3 gave 196 of 200 again, so `invalidOnce` stayed.
