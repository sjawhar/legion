---
title: "Narrowing a verdict value: census every reader, code and agent text, and find each reader's guard by mutation"
category: daemon
tags:
  - classify
  - checks-verdict
  - fix-attempts
  - census
  - mutation
  - agent-instructions
date: 2026-10-26
status: active
module: packages/daemon/internal/classify
related_issues:
  - "INFRA-272"
  - "sjawhar/legion#2223"
---

# Narrowing a verdict value: census every reader, code and agent text, and find each reader's guard by mutation

Extends docs/solutions/daemon/a-rule-on-a-state-that-stands-for-the-head-is-applied-by-every-fact-that-can-make-it-stand-census-the-predicates-writers-LEGION-698.md.

That document has you list the code that **writes** a predicate's inputs. When a change narrows
what a verdict value means (here, `"green"` no longer covers a base that requires nothing), also
list the places that **read** the old value, and handle each one on purpose:

- **List every comparison against the value you narrow**, not only the projection.
  `grep -n '== "green"\|!= "green"\|HeadVerdict(' packages/daemon/internal` finds them. Any reader
  whose side effect must stay on the old cases gets an explicit predicate. A reader that stays
  quiet on the moved case changes behaviour without saying so.
- **Find each kept side effect's guard by mutation, and name only that test in the plan.** Delete
  the side effect, run the suites, and write down the test that goes red. A test that should guard
  it is not evidence that it does.
- **Agent-facing restatements are readers too.** A prompt, skill or doc sentence that restates the
  predicate copies every part of it: "a settled head whose base branch requires no check and no
  workflow", never "a head whose base requires no check". Grep the phrase across every copy before
  review: `packages/daemon/internal/prompts/go/`, `skills/`, the generated
  `packages/claude-envoy/legion/skills/` copy (rebuilt with `bun scripts/build.ts`), `docs/site/`,
  and Go doc comments.
- **A comment that says "this fails on main" quotes a run on main.** Before you write it, run the
  step against main's behaviour.

## Evidence

- In `classify`, `AdvancePullRequestHead` (`decisions.go`) was the only reader of `"green"`.
  `RedSendsBack`, `RedWithdrawsReady`, `FixAttemptCapHolds` and `BlockFixAttempt` compare to
  `"red"`. Moving "requires nothing" from green to `""` would have stopped clearing `HeadCounted` on
  a push onto such a head without saying so. `nothingRequired(pr)` keeps that reset.
- The plan named `classify/fixtures_test.go` as the guard of that reset, and it passed with
  `|| nothingRequired(pr)` removed. It builds `Required` from each fixture's `Failing`, so every
  fixture where nothing failed is a base that requires nothing, and none asserts the markers. Its
  green rows quietly became no-verdict rows. The real guards were the new table test in
  `decisions_export_test.go` and the live proof's step 4.
- Review round 1 sent back `architect-common.md:10` and `wake-routing.md`'s `pr-blocked` row, which
  said "a head whose base branch requires no check". That wording is wrong for three of the five
  rows the table test covers: a set never read, a head no settlement stands for, and a required
  workflow still running. All three keep the attempt. The same round found a live-test comment
  saying step 4 fails on main. It passes on main, because main read that head as green, which also
  cleared the mark.
