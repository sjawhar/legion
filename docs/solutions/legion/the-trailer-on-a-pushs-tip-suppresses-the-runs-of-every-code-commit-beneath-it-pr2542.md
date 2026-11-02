---
title: "A `skip-checks` trailer on a push's tip suppresses the runs of every code commit beneath it; cure it with the next push, never a rewrite"
category: legion
tags:
  - skip-checks
  - handoff
  - ci
  - implementer
  - push-shape
date: 2026-10-30
status: active
module: .legion
related_issues:
  - "sjawhar/legion#2542"
---

GitHub decides whether a push starts a workflow from the **tip commit's message**, not from the
push's file set. Nothing strips an inherited trailer. So a push whose tip carries
`skip-checks: true` starts **no run at all**, however much code it carries beneath that tip.
Before sjawhar/legion#2354 that tip was usually another role's handoff commit. Since then a
handoff is a record on the Dispatch issue, no role pushes a handoff commit, and no push carries
the trailer.

## Rules

- **Read the tip's message before every code push.**
  `jj -R "$LEGION_WORKSPACE" log -r @- --no-graph -T description` — refuse to push while it ends
  with `skip-checks: true`. Before sjawhar/legion#2354 a role resuming a round inherited the
  previous role's trailered handoff commit as `@-`, so the trailer arrived without anybody writing
  one. Now no role writes it, so a trailer at `@-` is a mistake to take out before the push.
- **The cure is a further push whose head carries no trailer.** Not a rewrite: a pushed commit is
  rewritten only under *Rewriting pushed commits* in
  `skill://legion-worker/references/conflicts-and-rewrites.md`. Before sjawhar/legion#2354 a
  rewrite of only your own commits also left the other role's trailered handoff as the tip (still
  zero runs), and a rewrite of that commit rewrote the one the daemon recorded as that role's
  handoff. The daemon records a handoff on Dispatch now, not as a commit.
- **"No check runs" had two causes that looked identical in the API.** Before
  sjawhar/legion#2354 one was a handoff-only head with a trailer, where the code beneath it was
  checked at its own commit (the designed shape then,
  `docs/solutions/legion/a-base-with-no-required-check-makes-the-ci-verdict-a-hand-read-against-a-branch-line-baseline-INFRA-117.md`),
  and the other this one, where the code was checked nowhere. Every pushed head is a code head
  now, so a head with no check runs is this case, or a conflicting head
  (`docs/solutions/github/zero-check-runs-on-a-pushed-head-is-a-conflicting-head-not-a-trailer-pr2504.md`).
  Read the head commit's own check runs:
  `gh api repos/sjawhar/legion/commits/<sha>/check-runs --jq '.total_count'`.

## What this adds to the pages already on the topic

`docs/solutions/legion/an-implement-handoff-push-runs-ci-in-full-and-cancels-the-code-heads-runs-cite-the-tips-runs-and-give-the-lane-the-code-heads-image-LEGION-631.md`
says, in its first rule, "a push that carries code and a handoff together, or a handoff commit
pushed without the trailer, carries no trailer and runs CI at its head". That was true as a
**discipline** a role had to keep, and it was not a mechanism: nothing removed the trailer when
code joined the push. sjawhar/legion#2354 superseded that page, since no push carries a handoff
now. The rules above still say what happens when a trailer reaches a tip.

`docs/solutions/legion/gh-run-list-commit-takes-the-full-sha-and-an-empty-answer-is-not-a-measurement-INFRA-92.md`
records, in its evidence, a force-push ordered to drop a trailer where reading the code commit's
check runs showed the content **was** checked. The case here is the other one: the code was checked
nowhere, and the remedy was still not a rewrite.

## Evidence

On sjawhar/legion#2542, round 1's push carried seven code commits and ended on the implement
handoff commit, which carried `skip-checks: true`: GitHub started no run, and the code head had
zero check runs. The tester failed the round for it. Round 2's push ended on a commit with no
trailer and started the base's required checks over every code commit of the branch (runs
`41022625255` and `41022625206` queued at that head).
