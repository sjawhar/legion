---
title: "A name a package stops shipping is swept in every text the remaining package emits, not only in what it imports"
category: legion
tags:
  - package-split
  - rename
  - injected-skill
  - tool-result
  - committed-bundle
  - census
  - review-rounds
date: 2026-10-07
status: active
module: packages/pi-envoy
applies_when:
  - A package stops shipping an agent, skill, command or file that another text still names
  - A skill is injected into every session and names something only one install has
  - A Contract change census or a rename sweep is being written
related_issues:
  - "LEGION-247"
  - "sjawhar/legion#1831"
---

# A name a package stops shipping is swept in every text the remaining package emits

- When a package stops shipping something, list every text the package that remains *emits* to
  a session, not only what its code imports: the skills it injects, the tool results it returns,
  the nudges and reminders it appends, the strings its committed bundles inline, and the
  changelog line that describes the feature. Each is a channel to the same reader, and a fix in
  one channel contradicts the others until all are fixed in one commit.
- Classify every hit by the install that reads it. A sentence conditioned on a role the reader
  cannot hold is inert and may stay; an instruction every reader follows must resolve in the
  smallest install the package is meant for.
- Scope the sweep by nothing: never `--include=*.ts`, never `--exclude-dir=docs`. A runnable
  script can live under `docs/`, and a prompt string lives in a TypeScript constant.
- Make the sweep permanent as a committed check with an exact-path allowlist whose every entry
  carries its reason (`.github/scripts/check-plugin-names.sh` is the shape), and write the
  `## Contract change census` into the PR body when the pull request opens, not when a reviewer
  asks: the census's own searches are what find the caller the migration sweep missed.

## Evidence

sjawhar/legion#1831 split `@sjawhar/pi-legion-envoy` into `@sjawhar/pi-envoy` (every session)
and `@sjawhar/pi-legion` (Legion panes), and `plan-gap-analyst` moved to the Legion package's
`agents/`. Three channels still told a pi-envoy-only session to run it:

1. `skills/dispatch-first/SKILL.md:53`, which the Envoy entry injects into every Dispatch
   session's requests (review round 1, finding 1). Fixed in 8962eb69: the sentence names the
   bundled `scout` for every session and the Legion plugin's `plan-gap-analyst` only for a Legion
   architect's spec, and `packages/pi-envoy/src/skills-guard.test.ts` now fails on a
   `task(agent="…")` in the Envoy skills naming an agent neither the plugin nor the pinned Oh My
   Pi ships.
2. `SPEC_CHECK_REMINDER` in `packages/envoy-client/src/dispatch-execute.ts:191`, appended to the
   `dispatch_issue` and `dispatch_request_approval` tool results, plus the two committed
   `packages/claude-envoy/dist` bundles that inline it (round 2, finding 1; thread 4206015406).
   The round-1 guard could not see it: it reads staged skills and matches `task(agent="…")`, and
   this is prose in a tool result. Fixed in adc58ad2 with the bundles rebuilt.
3. Two `[Unreleased]` changelog entries still describing the old text (round 3, left as
   fast-follow).

The census, written in round 1 only after the reviewer asked for it (finding 3), found a fourth
hit the migration sweep had missed because that sweep excluded `docs/`:
`docs/site/media/walkthroughs/legion-operator/record-casts.sh:112` called
`install-plugin-profile.sh` without the now-required `--package`. The reviewer's role prompt
makes a missing census a finding (`packages/daemon/internal/prompts/roles/core/reviewer.md:13`);
nothing in the implementer's prompt says to write one, which is why it cost a round here.
